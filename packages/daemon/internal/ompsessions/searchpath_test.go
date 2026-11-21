package ompsessions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// PoolImage is ghcr.io/cybermonklabs/legion/ci, the copy of PgBouncer 1.25.2 (.github/ci-images.yaml),
// the pool TestEveryPoolPinsItsSearchPath starts in transaction mode in front of its test server.
// 1.25.2 refuses search_path as a startup parameter, and 1.26.0 and later accept it, so it stays
// at 1.25.2 while a pool the session database is reached through does. CI's daemon-go job pulls it
// before its tests run (.github/actions/pull-test-images reads this constant).
const PoolImage = "ghcr.io/cybermonklabs/legion/ci@sha256:7d7a27d9e90985cab5cf42256f5c13a3120baa4b055b69df37beb272b89b2340"

// poolReadiness bounds the wait for the pool to answer a connection once docker started it.
const poolReadiness = 60 * time.Second

// noDocker is what TestEveryPoolPinsItsSearchPath says when LEGION_TEST_PG_DSN names a server and
// no docker is on PATH: a skip outside CI, and a failure on GitHub Actions, whose runners have
// docker, so a skip there would take the pooled leg out of the one run that holds it.
const noDocker = "no docker on PATH, so the pooled leg cannot start the PgBouncer it runs through. The session database's " +
	"pods reach it through a PgBouncer pool in transaction mode, which refuses search_path as a startup parameter and " +
	"carries no setting from one transaction to the next, so a pin a direct connection alone holds would pass here and " +
	"fail there. Run the test where docker is on PATH: it starts the pool itself."

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
// its default_pool_size.
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

// poolStarts bounds how many times startPool starts a pool whose port another process took
// between the free-port probe and PgBouncer's bind.
const poolStarts = 5

// startPool starts PgBouncer 1.25.2 (PoolImage) with docker in front of the server direct names,
// set up as settings says, and answers the URL that reaches direct's database through it, once a
// connection through it has opened. The container is removed when the test ends, and its removal
// is registered before docker runs, so a container docker made but could not start goes too. The
// port is one free when the test asked; a pool that then fails to bind it, another process having
// taken it first, is removed and started again on another port, up to poolStarts times.
//
// One wildcard [databases] entry serves every database on the server under its own name, and the
// pool's auth_file holds the one login direct names, so a login that is no superuser, such as a
// Legion pod's test login, signs in as well as CI's postgres. The container runs on the host's
// network, so it reaches the server as the test does, with localhost read as 127.0.0.1 (PgBouncer
// tries every address a name resolves to, and a server published on IPv4 alone refuses the
// connections it sends to ::1), and it listens on 127.0.0.1. Its configuration reaches it through
// its environment, never its command line, so no process list holds the password. It runs with 1
// CPU and 256 MiB, as docs/testing.md asks of every container a test starts.
func startPool(t *testing.T, docker, direct string, settings pool) string {
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
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for start := 1; ; start++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find a free port for the pool: %v", err)
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			t.Fatalf("free the pool's port: %v", err)
		}
		config := strings.Join([]string{
			"[databases]",
			fmt.Sprintf("* = host=%s port=%d", host, server.Port),
			"",
			"[pgbouncer]",
			"listen_addr = 127.0.0.1",
			"listen_port = " + port,
			"unix_socket_dir =",
			"auth_type = scram-sha-256",
			"auth_file = /tmp/userlist.txt",
			"pool_mode = " + settings.mode,
			fmt.Sprintf("server_lifetime = %d", settings.lifetime),
			"max_client_conn = 500",
			fmt.Sprintf("default_pool_size = %d", settings.size),
			"max_prepared_statements = 1000",
			"",
		}, "\n")
		name, err := testdb.RandomName("legion-ompsessions-pool")
		if err != nil {
			t.Fatal(err)
		}
		remove := func() error {
			out, err := exec.Command(docker, "rm", "-f", "-v", name).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "No such container") {
				return fmt.Errorf("remove the pool's container %s: %v: %s", name, err, out)
			}
			return nil
		}
		t.Cleanup(func() {
			if err := remove(); err != nil {
				t.Error(err)
			}
		})
		run := exec.Command(docker, "run", "-d", "--name", name, "--network", "host", "--cpus", "1", "--memory", "256m",
			"-e", "POOL_CONFIG", "-e", "POOL_USERS", "--entrypoint", "sh", PoolImage, "-c",
			`printf "%s\n" "$POOL_USERS" > /tmp/userlist.txt && printf "%s" "$POOL_CONFIG" > /tmp/pgbouncer.ini && exec pgbouncer /tmp/pgbouncer.ini`)
		run.Env = append(os.Environ(), "POOL_CONFIG="+config, "POOL_USERS="+quote(server.User)+" "+quote(server.Password))
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("start the pool: docker run: %v: %s", err, out)
		}
		address := (&url.URL{
			Scheme: "postgres", User: url.UserPassword(server.User, server.Password),
			Host: net.JoinHostPort("127.0.0.1", port), Path: "/" + server.Database, RawQuery: "sslmode=disable",
		}).String()
		logs := func() string {
			out, _ := exec.Command(docker, "logs", name).CombinedOutput()
			return string(out)
		}
		deadline := time.Now().Add(poolReadiness)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := pgx.Connect(ctx, address)
			if err == nil {
				_ = conn.Close(ctx)
				cancel()
				return address
			}
			cancel()
			running, _ := exec.Command(docker, "container", "inspect", "-f", "{{.State.Running}}", name).Output()
			if strings.TrimSpace(string(running)) != "true" {
				log := logs()
				// The image's C library is musl, whose words for EADDRINUSE are "Address in use"
				// (PgBouncer logs "bind(): Address in use"); glibc's are "Address already in use".
				taken := strings.Contains(log, "Address in use") || strings.Contains(log, "Address already in use")
				if taken && start < poolStarts {
					t.Logf("the pool's port %s was taken before PgBouncer bound it: starting it again on another", port)
					if err := remove(); err != nil {
						t.Fatal(err)
					}
					break
				}
				t.Fatalf("the pool's container %s exited before it answered a connection (%v); its log:\n%s", name, err, log)
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pool in container %s answered no connection within %s (%v); its log:\n%s", name, poolReadiness, err, logs())
			}
			time.Sleep(500 * time.Millisecond)
		}
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
// The test starts the pools itself with docker (startPool). A pod has no docker, so in a pod the
// test skips, saying so, and its pooled legs run in CI's daemon-go job, where the test fails
// rather than skips without docker. Every other test of the package runs in a pod as usual.
func TestEveryPoolPinsItsSearchPath(t *testing.T) {
	// The database first: a host with no LEGION_TEST_PG_DSN skips here, as every test that needs
	// a database does. Where there is one and no docker, the test skips outside CI and fails on
	// GitHub Actions, the module's rule for an input CI provides (testbin's binary).
	direct := testdb.Database(t, "legion_ompsessions_pin")
	docker, err := exec.LookPath("docker")
	if err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal(noDocker)
		}
		t.Skip(noDocker)
	}
	ctx := context.Background()
	throughFresh, throughReusing := startPool(t, docker, direct, fresh), startPool(t, docker, direct, reusing)

	if failure := modeFailure(ctx, throughFresh); failure != "" {
		t.Fatalf("the fresh pool: %s", failure)
	}
	if failure := modeFailure(ctx, startPool(t, docker, direct, sessionMode)); failure == "" {
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
// The child is this test binary, run with that test alone, with LEGION_TEST_PG_DSN and
// GITHUB_ACTIONS taken out of its environment and PATH an empty directory of the test's, and
// the case's own settings put back. Each case looks for the first line of the message it wants,
// since `go test -v` indents the lines after it. The two cases with a database need one
// themselves, and skip without it.
func TestThePinCheckSkipsWithoutADatabaseOrDockerAndFailsWithoutDockerOnCI(t *testing.T) {
	const pinTest = "TestEveryPoolPinsItsSearchPath"
	firstLine := func(message string) string {
		line, _, _ := strings.Cut(message, "\n")
		return line
	}
	noPath := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(noPath, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, set ...string) (string, error) {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^"+pinTest+"$", "-test.count=1", "-test.v")
		child.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return slices.Contains([]string{"LEGION_TEST_PG_DSN", "GITHUB_ACTIONS", "PATH"}, name)
		}), append([]string{"PATH=" + noPath}, set...)...)
		out, err := child.CombinedOutput()
		return string(out), err
	}
	t.Run("no database", func(t *testing.T) {
		out, err := run(t)
		if err != nil || !strings.Contains(out, "--- SKIP: "+pinTest) || !strings.Contains(out, firstLine(testdb.SkipMessage)) {
			t.Errorf("with no LEGION_TEST_PG_DSN the run = %v; want it to skip naming LEGION_TEST_PG_DSN:\n%s", err, out)
		}
	})
	database := os.Getenv("LEGION_TEST_PG_DSN")
	t.Run("a database and no docker outside CI", func(t *testing.T) {
		if database == "" {
			t.Skip(testdb.SkipMessage)
		}
		out, err := run(t, "LEGION_TEST_PG_DSN="+database)
		if err != nil || !strings.Contains(out, "--- SKIP: "+pinTest) || !strings.Contains(out, firstLine(noDocker)) {
			t.Errorf("with LEGION_TEST_PG_DSN and no docker on PATH outside CI the run = %v; want it to skip saying so:\n%s", err, out)
		}
	})
	t.Run("a database and no docker on GitHub Actions", func(t *testing.T) {
		if database == "" {
			t.Skip(testdb.SkipMessage)
		}
		out, err := run(t, "LEGION_TEST_PG_DSN="+database, "GITHUB_ACTIONS=true")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !strings.Contains(out, "--- FAIL: "+pinTest) || !strings.Contains(out, firstLine(noDocker)) {
			t.Errorf("with LEGION_TEST_PG_DSN, no docker on PATH and GITHUB_ACTIONS=true the run = %v; want it to fail saying so:\n%s", err, out)
		}
	})
}
