package ompsessions

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// PoolImage is ghcr.io/cybermonklabs/legion/ci, the copy of PgBouncer 1.25.2 (.github/ci-images.yaml),
// the pool TestEveryPoolPinsItsSearchPath starts in transaction mode in front of its test server.
// 1.25.2 refuses search_path as a startup parameter, and 1.26.0 and later accept it, so it stays
// at 1.25.2 while a pool the session database is reached through does. CI's daemon-go job pulls it
// before its tests run (.github/actions/pull-test-images reads this constant).
const PoolImage = "ghcr.io/cybermonklabs/legion/ci@sha256:7d7a27d9e90985cab5cf42256f5c13a3120baa4b055b69df37beb272b89b2340"

// Postgres16Image is ghcr.io/cybermonklabs/legion/ci, the copy of PostgreSQL 16.16
// (.github/ci-images.yaml, its postgres-3950ad7c row), the server TestTheClientHoldsUnderEveryLeftSetting
// starts with docker, directly and behind PoolImage. 16.4 is the first 16 release with
// restrict_nonsystem_relation_kind, which every transaction of the session client sets, so it stays
// at 16.4 or later. CI's daemon-go job pulls it before its tests run (.github/actions/pull-test-images
// reads this constant).
const Postgres16Image = "ghcr.io/cybermonklabs/legion/ci@sha256:3950ad7c3148ca2175ef1b6c36c9efbf78957ae8fbe8b3a5d7c35af50500382f"

// containerReadiness bounds the wait for a server the test started with docker, the pool or
// PostgreSQL 16, to answer a connection once docker started it.
const containerReadiness = 60 * time.Second

// noDocker is what TestEveryPoolPinsItsSearchPath says when LEGION_TEST_PG_DSN names a server and
// no docker is on PATH (dockerFor): a skip outside CI, and a failure on GitHub Actions, whose runners
// have docker, so a skip there would take the pooled leg out of the one run that holds it.
const noDocker = "no docker on PATH, so the pooled leg cannot start the PgBouncer it runs through. The session database's " +
	"pods reach it through a PgBouncer pool in transaction mode, which refuses search_path as a startup parameter and " +
	"gives a server connection, with the session settings another client left on it, to the next client, so a pin a " +
	"direct connection alone holds would pass here and fail there. Run the test where docker is on PATH: it starts the pool itself."

// dockerFor is the docker on PATH, for a test that starts its servers with it; why is what the test
// says without one. Outside CI the test skips with why, and under GITHUB_ACTIONS=true it fails with
// it, the module's rule for an input CI provides (testbin's binary): CI's runners have docker, so a
// skip there would take the test out of the one run that must hold it.
func dockerFor(t *testing.T, why string) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal(why)
		}
		t.Skip(why)
	}
	return docker
}

// pinnedPath is search_path as every transaction of the package must read it: pg_catalog, and
// the session's own temporary schema, which a name of a function or an operator never reaches.
// No schema another login can create in is on it.
const pinnedPath = "pg_catalog, pg_temp"

// transaction is what the pin check runs its read in: a helper that opens a transaction on conn,
// read-only where readOnly, and answers search_path as a statement inside it reads it.
type transaction func(ctx context.Context, conn *Conn, readOnly bool) (string, error)

// readSearchPath is the statement each transaction of the pin check reads search_path with.
const readSearchPath = "SELECT pg_catalog.current_setting('search_path')"

// pinnedRead is the package's own helper, inTransaction, reading search_path in its transaction.
func pinnedRead(ctx context.Context, conn *Conn, readOnly bool) (path string, err error) {
	err = inTransaction(ctx, conn, readOnly, func(p pinned) error {
		return p.queryRow(ctx, readSearchPath).Scan(&path)
	})
	return path, err
}

// unpinned is a transaction helper without the pin, the control TestEveryPoolPinsItsSearchPath
// must catch.
func unpinned(ctx context.Context, conn *Conn, readOnly bool) (path string, err error) {
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	err = pgx.BeginTxFunc(ctx, conn.c, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, readSearchPath).Scan(&path)
	})
	return path, err
}

// startupPinned opens dsn with search_path as a startup parameter: a pin a direct connection
// holds for its whole session, and a pool in transaction mode refuses.
func startupPinned(ctx context.Context, dsn string) (*Conn, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.RuntimeParams["search_path"] = pinnedPath
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	return &Conn{c: conn}, nil
}

// pinFailure is what is wrong with a client's pin on the database dsn names, "" for nothing. The
// client opens its connection with open, and a read-only and a read-write transaction run through
// within must each read pinnedPath as search_path inside it, where a statement of the package
// runs: outside a transaction a client that pins each one reads the default.
func pinFailure(ctx context.Context, dsn string, open func(context.Context, string) (*Conn, error), within transaction) string {
	conn, err := open(ctx, dsn)
	if err != nil {
		return fmt.Sprintf("the client cannot connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, readOnly := range []bool{true, false} {
		kind := "read write"
		if readOnly {
			kind = "read only"
		}
		path, err := within(ctx, conn, readOnly)
		if err != nil {
			return fmt.Sprintf("a %s transaction cannot read search_path: %v", kind, err)
		}
		if path != pinnedPath {
			return fmt.Sprintf("a %s transaction reads search_path %q, want %q", kind, path, pinnedPath)
		}
	}
	return ""
}

// pool is how startPool sets a PgBouncer up: its pool_mode, its server_lifetime in seconds, and
// its default_pool_size, which an entry with a pool_size of its own does not take (poolEntry).
type pool struct {
	mode     string
	lifetime int
	size     int
}

var (
	// fresh is the strictest stand-in for the session database's pool: transaction mode, and
	// server_lifetime 0, so each server connection serves one transaction and is closed, and a
	// setting one transaction leaves on its session reaches no later one.
	fresh = pool{mode: "transaction", lifetime: 0, size: 20}
	// reusing is transaction mode with one server connection kept for an hour, so every client
	// gets the server session the one before it left, with whatever setting it left on it.
	reusing = pool{mode: "transaction", lifetime: 3600, size: 1}
	// sessionMode is session mode, the control of modeFailure: a client keeps its server session.
	sessionMode = pool{mode: "session", lifetime: 0, size: 20}
)

// modeFailure is what is wrong with the pool dsn names as a stand-in for the session database's
// pool, "" for nothing: a client sets search_path in one statement and reads it in the next, and
// in transaction mode with server_lifetime 0 the next statement gets a server session of its own,
// with the default, where session mode, or a server connection kept, reads the change.
func modeFailure(ctx context.Context, dsn string) string {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Sprintf("the client cannot connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		return fmt.Sprintf("the client cannot set search_path: %v", err)
	}
	var path string
	if err := conn.QueryRow(ctx, readSearchPath).Scan(&path); err != nil {
		return fmt.Sprintf("the client cannot read search_path: %v", err)
	}
	if path == "pg_catalog" {
		return "the next statement read the search_path the one before it set, so the pool kept the server session from one " +
			"transaction to the next: it is not in transaction mode with server_lifetime 0"
	}
	return ""
}

// leaveSetting has a client of the reusing pool dsn names leave search_path = public on the server
// session, outside any transaction, as an unpinned client of the same login can, and answers what
// is wrong with the leg, "" for nothing: a second client must get the same server backend and read
// the setting the first left, or the leg shows no reuse.
func leaveSetting(ctx context.Context, dsn string) string {
	backend := func(set bool) (int32, string, error) {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = conn.Close(ctx) }()
		if set {
			if _, err := conn.Exec(ctx, "SET search_path = public"); err != nil {
				return 0, "", err
			}
		}
		var pid int32
		var path string
		err = conn.QueryRow(ctx, "SELECT pg_catalog.pg_backend_pid(), pg_catalog.current_setting('search_path')").Scan(&pid, &path)
		return pid, path, err
	}
	first, _, err := backend(true)
	if err != nil {
		return fmt.Sprintf("the first client: %v", err)
	}
	second, path, err := backend(false)
	if err != nil {
		return fmt.Sprintf("the second client: %v", err)
	}
	if second != first || path != "public" {
		return fmt.Sprintf("the second client got backend %d reading search_path %q, after the first got backend %d and set "+
			"public: the pool did not hand the server session on", second, path, first)
	}
	return ""
}

// poolPort names the unix socket a pool listens on, in a directory of the test's own: PgBouncer
// calls it .s.PGSQL.<listen_port>, as Postgres does, and binds no TCP port for it.
const poolPort = "6432"

// poolURL is the URL of database through the pool whose socket is in socketDir, signed in as user
// with password (socketURL).
func poolURL(user, password, database, socketDir string) (string, error) {
	return socketURL(user, password, database, socketDir, poolPort)
}

// socketURL is the URL of database on the server whose unix socket .s.PGSQL.<port> is in socketDir,
// signed in as user with password. url.Values writes a space as `+`, which pgconn keeps as a `+`
// rather than reading it as a space, so each `+` the encoding leaves is spelt %20: a literal `+` is
// %2B by then. A socketDir that holds a comma is refused: pgx reads host= as a comma-separated list
// of hosts, however the comma is encoded, so no URL can name that directory to it.
func socketURL(user, password, database, socketDir, port string) (string, error) {
	if strings.Contains(socketDir, ",") {
		return "", fmt.Errorf("the socket directory %q holds a comma, and pgx splits a URL's host= at each comma into "+
			"hosts of their own, so no client can reach the socket there: run the test with a TMPDIR whose path holds none", socketDir)
	}
	return (&url.URL{
		Scheme: "postgres", User: url.UserPassword(user, password), Path: "/" + database,
		RawQuery: strings.ReplaceAll(url.Values{"host": {socketDir}, "port": {port}, "sslmode": {"disable"}}.Encode(), "+", "%20"),
	}).String(), nil
}

// TestThePoolURLReadsBackToItsSocketDirectory: pgconn reads poolURL back to the directory the
// pool's socket is in, a space and a `+` in its name included, to the pool's port and to the login
// and database it was given, a password with a space and a `+` included.
func TestThePoolURLReadsBackToItsSocketDirectory(t *testing.T) {
	const dir, password = "/tmp/a pool+dir/lg1", "p@ss word+"
	address, err := poolURL("postgres", password, "db", dir)
	if err != nil {
		t.Fatalf("poolURL refused %q: %v", dir, err)
	}
	config, err := pgconn.ParseConfig(address)
	if err != nil {
		t.Fatalf("pgconn cannot read the pool's URL: %v", err)
	}
	if config.Host != dir || fmt.Sprint(config.Port) != poolPort || config.User != "postgres" || config.Password != password || config.Database != "db" {
		t.Fatalf("the pool's URL reads back as host %q port %d user %q password %q database %q; want %q, %s, postgres, %q, db",
			config.Host, config.Port, config.User, config.Password, config.Database, dir, poolPort, password)
	}
}

// TestThePoolURLRefusesASocketDirectoryWithAComma: a socket directory whose path holds a comma is
// refused by a message that names pgx's split of host=, and the split itself is still pgx's: it
// reads the directory, percent-encoded, as more than one host.
func TestThePoolURLRefusesASocketDirectoryWithAComma(t *testing.T) {
	const dir = "/tmp/a,b/lg1"
	if _, err := poolURL("postgres", "pw", "db", dir); err == nil || !strings.Contains(err.Error(), "pgx splits a URL's host= at each comma") {
		t.Fatalf("poolURL(%q) = %v; want a refusal naming pgx's split of host= at a comma", dir, err)
	}
	config, err := pgconn.ParseConfig("postgres:///db?host=" + url.QueryEscape(dir) + "&port=" + poolPort + "&sslmode=disable")
	if err == nil && config.Host == dir {
		t.Fatalf("pgx now reads host=%s as the one directory %q, so the refusal is no longer needed", url.QueryEscape(dir), dir)
	}
}

// poolUser is the --user the pool's container runs as, and PostgreSQL 16's
// (TestTheClientHoldsUnderEveryLeftSetting), the test's own uid and gid, so that each server can make
// its socket in the test's directory, which is mode 0700, and the pool can reach PostgreSQL 16's
// there. A test run as root is refused here: PgBouncer stops at a uid of 0 ("PgBouncer should not run
// as root"), as PostgreSQL does, so the pool would end before it answered a connection and say only
// that.
func poolUser(uid, gid int) (string, error) {
	if uid == 0 {
		return "", errors.New("the test runs as root, and PgBouncer refuses to (\"PgBouncer should not run as root\"), while " +
			"the pool must run as the test's own user to make its socket in the test's own directory: run the test as another user")
	}
	return fmt.Sprintf("%d:%d", uid, gid), nil
}

// TestThePoolRefusesToRunAsRoot: the pool's user is the test's own, and root is refused by a
// message that names PgBouncer's own refusal.
func TestThePoolRefusesToRunAsRoot(t *testing.T) {
	if user, err := poolUser(1001, 118); err != nil || user != "1001:118" {
		t.Fatalf("poolUser(1001, 118) = %q, %v; want 1001:118", user, err)
	}
	if _, err := poolUser(0, 0); err == nil || !strings.Contains(err.Error(), "PgBouncer should not run as root") {
		t.Fatalf("poolUser(0, 0) = %v; want a refusal naming PgBouncer's own refusal of root", err)
	}
}

// poolEntry is one [databases] entry of a pool startPool starts: its name, which a client names as
// its database; the server's database it reaches; the login it signs in to the server as, whatever
// login signed in to the pool; and its pool_size, the most server connections it keeps.
type poolEntry struct {
	name, database, user string
	size                 int
}

// poolTarget is the server a pool startPool starts reaches, and who signs in to the pool. host is a
// TCP host, or the directory of the server's unix socket on this machine, which docker mounts into
// the container at /server; port is the server's. entries are the pool's [databases] entries, and
// none is one wildcard entry that serves every database of the server under its own name, as the
// login that signed in to the pool. auth is its auth_type and users the logins of its auth_file,
// each with its password; admins is its admin_users, "" for none; lifo writes server_round_robin =
// 0, PgBouncer's own default, which hands out the server connection released last first, so the
// file names the order a test rests on. ready is the database through the pool that startPool waits
// to open a connection to, as the first of users.
type poolTarget struct {
	host    string
	port    uint16
	entries []poolEntry
	auth    string
	users   [][2]string
	admins  string
	lifo    bool
	ready   string
}

// startPool starts PgBouncer 1.25.2 (PoolImage) with docker in front of the server target names,
// set up as settings says, and answers the directory of the pool's unix socket, once a connection
// through it has opened (poolURL reaches a database through it). The container is removed when the
// test ends, and its removal is registered before docker runs (runContainer).
//
// The pool listens on a unix socket alone, in a directory of the test's own (testtmp.Dir) that
// docker mounts into the container, and on no TCP port. A port is the machine's: one the test
// probed and released for PgBouncer to bind by number was taken by another process in between on
// a shared box (dispatch://INFRA-3904), and a port Docker picks comes from its own record, which
// picked one already held (docs/testing.md, on the host side of a port Docker publishes). The
// socket's directory is the test's alone, mode 0700, so no other user's process can bind there
// before PgBouncer does, and PgBouncer runs as the test's own user (poolUser) so that it can, and so
// that it can reach a server's socket in another directory of the test's, which docker mounts at
// /server. The container runs with SELinux labelling off (--security-opt label=disable), so that on
// a host that enforces SELinux it can make its socket in the mounted directory, which carries no
// container label; on a host without SELinux the option changes nothing. The mount needs docker's
// daemon on this machine, as the host network below already does.
//
// The container runs on the host's network, so it reaches a TCP server as the test does. Its
// configuration reaches it through its environment, never its command line, so no process list
// holds a password. It runs with 1 CPU and 256 MiB, as docs/testing.md asks of every container a
// test starts.
func startPool(t *testing.T, docker string, target poolTarget, settings pool) string {
	t.Helper()
	socketDir := testtmp.Dir(t, ".s.PGSQL."+poolPort)
	address, err := poolURL(target.users[0][0], target.users[0][1], target.ready, socketDir)
	if err != nil {
		t.Fatal(err)
	}
	user, err := poolUser(os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	host, mounts := target.host, []string{"--mount", "type=bind,src=" + socketDir + ",dst=/pool"}
	if strings.HasPrefix(target.host, "/") {
		host, mounts = "/server", append(mounts, "--mount", "type=bind,src="+target.host+",dst=/server")
	}
	lines := []string{"[databases]"}
	if len(target.entries) == 0 {
		lines = append(lines, fmt.Sprintf("* = host=%s port=%d", host, target.port))
	}
	for _, entry := range target.entries {
		lines = append(lines, fmt.Sprintf("%s = host=%s port=%d dbname=%s user=%s pool_size=%d",
			entry.name, host, target.port, entry.database, entry.user, entry.size))
	}
	lines = append(lines, "", "[pgbouncer]", "listen_port = "+poolPort, "unix_socket_dir = /pool", "auth_type = "+target.auth,
		"auth_file = /tmp/userlist.txt")
	if target.admins != "" {
		lines = append(lines, "admin_users = "+target.admins)
	}
	lines = append(lines, "pool_mode = "+settings.mode, fmt.Sprintf("server_lifetime = %d", settings.lifetime), "max_client_conn = 500",
		fmt.Sprintf("default_pool_size = %d", settings.size), "max_prepared_statements = 1000")
	if target.lifo {
		lines = append(lines, "server_round_robin = 0")
	}
	config := strings.Join(append(lines, ""), "\n")
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	users := make([]string, len(target.users))
	for i, login := range target.users {
		users[i] = quote(login[0]) + " " + quote(login[1])
	}
	args := append([]string{"--network", "host", "--cpus", "1", "--memory", "256m", "--user", user, "--security-opt", "label=disable"}, mounts...)
	name := runContainer(t, docker, "legion-ompsessions-pool", "the pool", []string{"POOL_CONFIG=" + config, "POOL_USERS=" + strings.Join(users, "\n")},
		append(args, "-e", "POOL_CONFIG", "-e", "POOL_USERS", "--entrypoint", "sh", PoolImage, "-c",
			`printf "%s\n" "$POOL_USERS" > /tmp/userlist.txt && printf "%s" "$POOL_CONFIG" > /tmp/pgbouncer.ini && exec pgbouncer /tmp/pgbouncer.ini`)...)
	awaitConnection(t, docker, name, "the pool", address)
	return socketDir
}

// poolInFrontOf starts PgBouncer 1.25.2 (startPool) in front of the server direct names, set up as
// settings says, and answers the URL that reaches direct's database through it. One wildcard
// [databases] entry serves every database on the server under its own name, and the pool's
// auth_file holds the one login direct names, so a login that is no superuser, such as a Legion
// pod's test login, signs in as well as CI's postgres. The pool reaches the server as the test does,
// with localhost read as 127.0.0.1 (PgBouncer tries every address a name resolves to, and a server
// published on IPv4 alone refuses the connections it sends to ::1).
func poolInFrontOf(t *testing.T, docker, direct string, settings pool) string {
	t.Helper()
	server, err := pgconn.ParseConfig(direct)
	if err != nil {
		t.Fatalf("read the test database's URL: %v", err)
	}
	if strings.HasPrefix(server.Host, "/") {
		t.Fatal("LEGION_TEST_PG_DSN names a unix socket, which a pool in a container cannot reach: name a TCP host")
	}
	host := server.Host
	if host == "localhost" {
		host = "127.0.0.1"
	}
	socketDir := startPool(t, docker, poolTarget{host: host, port: server.Port, auth: "scram-sha-256",
		users: [][2]string{{server.User, server.Password}}, ready: server.Database}, settings)
	address, err := poolURL(server.User, server.Password, server.Database, socketDir)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// runContainer starts a container of the test's own, named from prefix, with docker run -d and
// args, the image and its command among them, and answers its name; what names the container in a
// failure ("the pool"). env is put in docker's own environment, for args that pass a value by
// -e NAME alone, so that no process list holds it. The container is removed with its anonymous
// volumes when the test ends (docs/testing.md, "Containers a test starts"), and its removal is
// registered before docker runs, so a container docker made but could not start goes too.
func runContainer(t *testing.T, docker, prefix, what string, env []string, args ...string) string {
	t.Helper()
	name, err := testdb.RandomName(prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, err := exec.Command(docker, "rm", "-f", "-v", name).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container") {
			t.Errorf("remove %s's container %s: %v: %s", what, name, err, out)
		}
	})
	run := exec.Command(docker, append([]string{"run", "-d", "--name", name}, args...)...)
	run.Env = append(os.Environ(), env...)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("start %s: docker run: %v: %s", what, err, out)
	}
	return name
}

// awaitConnection waits until a connection to address opens, for at most containerReadiness, and
// fails the test with the log of the container name, which serves it, where the container exits
// first or the bound passes; what names the server in the failure.
func awaitConnection(t *testing.T, docker, name, what, address string) {
	t.Helper()
	logs := func() string {
		out, _ := exec.Command(docker, "logs", name).CombinedOutput()
		return string(out)
	}
	deadline := time.Now().Add(containerReadiness)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := pgx.Connect(ctx, address)
		if err == nil {
			_ = conn.Close(ctx)
			cancel()
			return
		}
		cancel()
		running, _ := exec.Command(docker, "container", "inspect", "-f", "{{.State.Running}}", name).Output()
		if strings.TrimSpace(string(running)) != "true" {
			t.Fatalf("%s's container %s exited before it answered a connection (%v); its log:\n%s", what, name, err, logs())
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s in container %s answered no connection within %s (%v); its log:\n%s", what, name, containerReadiness, err, logs())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Every transaction of the package reads search_path as pinnedPath, on a direct connection and
// through PgBouncer 1.25.2 in transaction mode, the pool the pods reach the session database
// through: inTransaction's first statement is SET LOCAL search_path = pg_catalog, pg_temp.
// TestNoStatementRunsOutsideTheHelper holds every statement to that helper, so this read covers
// each. It runs through two pools: fresh, with server_lifetime 0, the strictest stand-in; and
// reusing, which keeps one server connection, after a client of the same login left search_path =
// public on its session. Its controls run in every run:
//
//   - the same helper without the pin, which must fail on every leg naming what it read: the
//     server's default directly and through fresh, and through reusing the public the first client
//     left;
//   - a client that sets search_path as a startup parameter, which holds on a direct connection
//     and which each pool refuses;
//   - modeFailure, which must find fresh in transaction mode with server_lifetime 0 and a pool in
//     session mode not, so fresh is the pool the leg says it is;
//   - leaveSetting, which must see reusing hand the first client's server session, with its
//     setting, to the second;
//   - and the package's own client, which passes every leg.
//
// The test starts the pools itself with docker (poolInFrontOf). A pod has no docker, so in a pod the
// test skips, saying so, and its pooled legs run in CI's daemon-go job, where the test fails
// rather than skips without docker (dockerFor). Every other test of the package runs in a pod as
// usual.
func TestEveryPoolPinsItsSearchPath(t *testing.T) {
	// The database first: a host with no LEGION_TEST_PG_DSN skips here, as every test that needs
	// a database does. Where there is one and no docker, the test skips outside CI and fails on
	// GitHub Actions (dockerFor).
	direct := testdb.PlainDatabase(t, "legion_ompsessions_pin")
	docker := dockerFor(t, noDocker)
	ctx := context.Background()
	throughFresh, throughReusing := poolInFrontOf(t, docker, direct, fresh), poolInFrontOf(t, docker, direct, reusing)

	if failure := modeFailure(ctx, throughFresh); failure != "" {
		t.Fatalf("the fresh pool: %s", failure)
	}
	if failure := modeFailure(ctx, poolInFrontOf(t, docker, direct, sessionMode)); failure == "" {
		t.Fatal("control: modeFailure found a pool in session mode in transaction mode, so it cannot tell the fresh pool's mode")
	} else {
		t.Logf("control: a pool in session mode: %s", failure)
	}
	if failure := leaveSetting(ctx, throughReusing); failure != "" {
		t.Fatalf("the reusing pool: %s", failure)
	}

	const (
		directly = "direct"
		pooled   = "through the fresh pool"
		reused   = "through the reusing pool, after a client left search_path = public"
	)
	legs := []struct{ name, dsn string }{{directly, direct}, {pooled, throughFresh}, {reused, throughReusing}}
	for _, client := range []struct {
		name   string
		open   func(context.Context, string) (*Conn, error)
		within transaction
		// fails is, by leg, what the check must name in its failure, and a leg it holds
		// nothing for one the client must pass.
		fails map[string]string
	}{
		{"the package's client (ConnectURL, inTransaction)", ConnectURL, pinnedRead, nil},
		{"control: the helper without the pin", ConnectURL, unpinned, map[string]string{
			directly: `transaction reads search_path "\"$user\", public"`,
			pooled:   `transaction reads search_path "\"$user\", public"`,
			reused:   `transaction reads search_path "public"`,
		}},
		{"control: search_path as a startup parameter", startupPinned, unpinned, map[string]string{
			pooled: "unsupported startup parameter: search_path",
			reused: "unsupported startup parameter: search_path",
		}},
	} {
		for _, leg := range legs {
			failure := pinFailure(ctx, leg.dsn, client.open, client.within)
			switch want, fails := client.fails[leg.name]; {
			case !fails && failure != "":
				t.Errorf("%s, %s: %s", client.name, leg.name, failure)
			case fails && !strings.Contains(failure, want):
				t.Errorf("%s, %s: the check found %q, want a failure naming %q", client.name, leg.name, failure, want)
			default:
				t.Logf("%s, %s: %q", client.name, leg.name, failure)
			}
		}
	}
}

// TestEveryPoolPinsItsSearchPath, run in a test binary of its own, skips where there is no
// database to test against, as every test of the module that needs one does; skips saying so
// where there is a database and no docker on PATH outside CI; and fails there under
// GITHUB_ACTIONS, where a skip would take the pooled leg out of the one run that must hold it.
// TestTheClientHoldsUnderEveryLeftSetting, which reads no LEGION_TEST_* variable and starts its
// servers itself, skips saying so with no docker on PATH outside CI, and fails so under
// GITHUB_ACTIONS, through the same rule (dockerFor). Each child is this test binary, run with one
// test alone, with LEGION_TEST_PG_DSN and GITHUB_ACTIONS taken out of its environment and PATH an
// empty directory of the test's, and the case's own settings put back. Each case looks for the
// first line of the message it wants, since `go test -v` indents the lines after it. The two cases
// of the pin check with a database need one themselves, and skip without it.
func TestThePinCheckSkipsWithoutADatabaseOrDockerAndFailsWithoutDockerOnCI(t *testing.T) {
	const pinTest, leftTest = "TestEveryPoolPinsItsSearchPath", "TestTheClientHoldsUnderEveryLeftSetting"
	firstLine := func(message string) string {
		line, _, _ := strings.Cut(message, "\n")
		return line
	}
	noPath := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(noPath, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, test string, set ...string) (string, error) {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1", "-test.v")
		child.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return slices.Contains([]string{"LEGION_TEST_PG_DSN", "GITHUB_ACTIONS", "PATH"}, name)
		}), append([]string{"PATH=" + noPath}, set...)...)
		out, err := child.CombinedOutput()
		return string(out), err
	}
	t.Run("no database", func(t *testing.T) {
		out, err := run(t, pinTest)
		if err != nil || !strings.Contains(out, "--- SKIP: "+pinTest) || !strings.Contains(out, firstLine(testdb.SkipMessage)) {
			t.Errorf("with no LEGION_TEST_PG_DSN the run = %v; want it to skip naming LEGION_TEST_PG_DSN:\n%s", err, out)
		}
	})
	database := os.Getenv("LEGION_TEST_PG_DSN")
	t.Run("a database and no docker outside CI", func(t *testing.T) {
		if database == "" {
			t.Skip(testdb.SkipMessage)
		}
		out, err := run(t, pinTest, "LEGION_TEST_PG_DSN="+database)
		if err != nil || !strings.Contains(out, "--- SKIP: "+pinTest) || !strings.Contains(out, firstLine(noDocker)) {
			t.Errorf("with LEGION_TEST_PG_DSN and no docker on PATH outside CI the run = %v; want it to skip saying so:\n%s", err, out)
		}
	})
	t.Run("a database and no docker on GitHub Actions", func(t *testing.T) {
		if database == "" {
			t.Skip(testdb.SkipMessage)
		}
		out, err := run(t, pinTest, "LEGION_TEST_PG_DSN="+database, "GITHUB_ACTIONS=true")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: "+pinTest) || !strings.Contains(out, firstLine(noDocker)) {
			t.Errorf("with LEGION_TEST_PG_DSN, no docker on PATH and GITHUB_ACTIONS=true the run = %v; want it to fail saying so:\n%s", err, out)
		}
	})
	t.Run("the left-setting test with no docker outside CI", func(t *testing.T) {
		out, err := run(t, leftTest)
		if err != nil || !strings.Contains(out, "--- SKIP: "+leftTest) || !strings.Contains(out, firstLine(noDockerForLeftSettings)) {
			t.Errorf("with no docker on PATH outside CI the left-setting run = %v; want it to skip saying so:\n%s", err, out)
		}
	})
	t.Run("the left-setting test with no docker on GitHub Actions", func(t *testing.T) {
		out, err := run(t, leftTest, "GITHUB_ACTIONS=true")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: "+leftTest) || !strings.Contains(out, firstLine(noDockerForLeftSettings)) {
			t.Errorf("with no docker on PATH and GITHUB_ACTIONS=true the left-setting run = %v; want it to fail saying so:\n%s", err, out)
		}
	})
}
