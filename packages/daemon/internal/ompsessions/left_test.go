package ompsessions

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/lockwaittest"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// noDockerForLeftSettings is what TestTheClientHoldsUnderEveryLeftSetting says with no docker on
// PATH (dockerFor): a skip outside CI, and a failure on GitHub Actions, whose runners have docker.
const noDockerForLeftSettings = "no docker on PATH, so the test cannot start the PostgreSQL 16 server and the PgBouncer in front of it " +
	"that it runs the session client against, under each setting another client can leave on a server connection. It reads no " +
	"LEGION_TEST_* variable: CI's daemon-go job runs it in a step of its own, and elsewhere it runs where docker is on PATH, " +
	"starting both servers itself."

// The test's two waits, which give a left timeout of 50 ms time to act inside a transaction of the
// client, whose own statements end before one would. The admin holds the row of the case's session
// FOR UPDATE for leftRowHold while the client's Append waits for it, which a left
// statement_timeout or lock_timeout of 50 ms cancels; and the tracer waits leftPause, sending
// nothing, before the second statement of the case's first Read transaction, in which a left
// idle_in_transaction_session_timeout of 50 ms ends the connection.
const (
	leftRowHold = 150 * time.Millisecond
	leftPause   = 150 * time.Millisecond
)

// leftBound bounds each of the test's waits for a backend to leave pg_stat_activity once its
// client's Terminate went, and for the server's log to hold a line the test's own statement wrote.
const leftBound = 10 * time.Second

// leftServerPort is PostgreSQL 16's port, which names its unix socket in its directory:
// .s.PGSQL.5432.
const leftServerPort = 5432

// postgres16Start is the command PostgreSQL 16's container runs as the test's own uid (poolUser):
// initdb, with trust for every connection, since the socket's directory is the test's alone, mode
// 0700; then the server, on its unix socket in the mounted directory alone, with the log settings
// the log rule reads (clientStatementsInLog), which testdata/pg16-statement-log.jsonl's capture used
// too: jsonlog, one file (log/server.json, never rotated), a line for each statement when it ends
// (log_min_duration_statement = 0), and the statement's text on each ERROR line. initdb looks the
// running user up, and the image's /etc/passwd lacks the test's uid, so where it is missing the
// command names it postgres through the image's nss_wrapper, as the image's own entrypoint does
// (docker_init_database_dir in docker-entrypoint.sh).
const postgres16Start = `set -e
if ! getent passwd "$(id -u)" >/dev/null; then
	for wrapper in /usr/lib/*/libnss_wrapper.so; do
		if [ -s "$wrapper" ]; then
			printf 'postgres:x:%s:%s:PostgreSQL:/tmp:/bin/false\n' "$(id -u)" "$(id -g)" >/tmp/passwd
			printf 'postgres:x:%s:\n' "$(id -g)" >/tmp/group
			export LD_PRELOAD="$wrapper" NSS_WRAPPER_PASSWD=/tmp/passwd NSS_WRAPPER_GROUP=/tmp/group
			break
		fi
	done
fi
initdb --pgdata=/tmp/data --username=postgres --auth=trust --encoding=UTF8 --no-sync >/dev/null
exec postgres -D /tmp/data -c listen_addresses= -c unix_socket_directories=/server \
	-c log_destination=jsonlog -c logging_collector=on -c log_directory=/server/log -c log_filename=server.log \
	-c log_rotation_age=0 -c log_rotation_size=0 -c log_min_duration_statement=0 -c log_min_error_statement=error`

// The PgBouncer entries of the pooled run (startLeftServers), each to the server as its superuser,
// postgres, so that a client of a pool may leave every entry and send SET SESSION AUTHORIZATION:
// leftset and leftset4 reach the one database leftset with one server connection and with four,
// and leftset_ro reaches leftset_ro with one.
const (
	poolSet   = "leftset"
	poolSet4  = "leftset4"
	poolSetRO = "leftset_ro"
)

// leftNames are the logins and databases prepareLeftServer makes on the server: client, the
// client's login on the direct run, which makes and owns the tables in database as a pod's agent
// login may; lockedOut, a login with CONNECT and no right on public or the tables; and readOnly,
// which defaults each transaction to read-only. The role with no right on the tables is
// LeftSettings' own (noRightsRole).
type leftNames struct {
	client, lockedOut, database, readOnly string
}

// leftNamesOnPostgres16 are leftNames on the PostgreSQL 16 the test starts.
var leftNamesOnPostgres16 = leftNames{client: "legion_client", lockedOut: "left_locked_out", database: "leftset", readOnly: "leftset_ro"}

// leftServer is how the case runner reaches the servers: the URLs of the test's superuser,
// directly, on names.database (admin), on names.readOnly (adminRO) and on a database no case
// writes (maintenance); of the client's login on names.database and on names.readOnly, and of
// names.lockedOut on names.database, directly; of the PgBouncer entries and the admin console; and
// the path of the server's jsonlog file.
type leftServer struct {
	names                              leftNames
	admin, adminRO, maintenance        string
	client, clientRO, lockedOut        string
	pooled, pooled4, pooledRO, console string
	log                                string
}

// startLeftServers starts PostgreSQL 16 (Postgres16Image) with docker, as startPool starts the
// pool: the test's own uid, 1 CPU and 256 MiB, removal registered before docker runs, and a unix
// socket alone, in a directory of the test's own (testtmp.Dir) that docker mounts, with no network
// at all. It makes the logins and databases (prepareLeftServer), and then starts PgBouncer 1.25.2
// (PoolImage) in front of it through the same socket: pool_mode = transaction,
// max_prepared_statements = 1000, server_round_robin = 0, its three entries, and postgres both its
// one login, which it trusts (its socket's directory is the test's alone too), and its admin, so
// that the test reads SHOW VERSION and sends RECONNECT on its admin console.
func startLeftServers(t *testing.T, docker string) leftServer {
	t.Helper()
	dir := testtmp.Dir(t, ".s.PGSQL."+strconv.Itoa(leftServerPort))
	user, err := poolUser(os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	direct := func(login, database string) string {
		address, err := socketURL(login, "", database, dir, strconv.Itoa(leftServerPort))
		if err != nil {
			t.Fatal(err)
		}
		return address
	}
	names := leftNamesOnPostgres16
	server := leftServer{
		names: names, admin: direct("postgres", names.database), adminRO: direct("postgres", names.readOnly),
		maintenance: direct("postgres", "postgres"), client: direct(names.client, names.database),
		clientRO: direct(names.client, names.readOnly), lockedOut: direct(names.lockedOut, names.database),
		log: filepath.Join(dir, "log", "server.json"),
	}
	name := runContainer(t, docker, "legion-ompsessions-pg16", "PostgreSQL 16", nil, "--network", "none", "--cpus", "1",
		"--memory", "256m", "--user", user, "--security-opt", "label=disable", "--mount", "type=bind,src="+dir+",dst=/server",
		"--entrypoint", "sh", Postgres16Image, "-c", postgres16Start)
	awaitConnection(t, docker, name, "PostgreSQL 16", server.maintenance)
	prepareLeftServer(t, server)

	poolDir := startPool(t, docker, poolTarget{
		host: dir, port: leftServerPort, auth: "trust", users: [][2]string{{"postgres", ""}}, admins: "postgres", lifo: true, ready: poolSet,
		entries: []poolEntry{
			{name: poolSet, database: names.database, user: "postgres", size: 1},
			{name: poolSet4, database: names.database, user: "postgres", size: 4},
			{name: poolSetRO, database: names.readOnly, user: "postgres", size: 1},
		},
	}, pool{mode: "transaction", lifetime: 3600, size: 1})
	through := func(entry string) string {
		address, err := poolURL("postgres", "", entry, poolDir)
		if err != nil {
			t.Fatal(err)
		}
		return address
	}
	server.pooled, server.pooled4, server.pooledRO, server.console = through(poolSet), through(poolSet4), through(poolSetRO), through("pgbouncer")
	return server
}

// noRightsRole is the role LeftSettings' role entry leaves, SET ROLE <role>, which
// prepareLeftServer makes with no right on the tables.
func noRightsRole(t *testing.T) string {
	t.Helper()
	for _, entry := range LeftSettings {
		if role, ok := strings.CutPrefix(entry.Left, "SET ROLE "); ok && entry.Name == "role" {
			return role
		}
	}
	t.Fatal("LeftSettings' role entry leaves no SET ROLE <role>, so the test cannot make the role it leaves")
	return ""
}

// readOnlyID and readOnlySession are the session leftNames.readOnly holds, which prepareLeftServer
// writes before the database defaults to read-only: readOnlyContent, last written at readOnlyAt.
const (
	readOnlyID      = "left-read-only"
	readOnlySession = leftFolder + "/2026-11-27T11-00-00-000Z_" + readOnlyID + ".jsonl"
	readOnlyContent = "{\"type\":\"session\",\"version\":3,\"id\":\"left-read-only\"}\n{\"type\":\"message\",\"message\":{\"content\":\"schön\"}}\n"
)

// readOnlyAt is when readOnlySession was written, as its row's mtime_ms holds it.
var readOnlyAt = time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC).UnixMilli()

// prepareLeftServer makes, as the test's superuser, what the cases need: the client's login, to
// which the role without rights (noRightsRole) is granted, so that SET ROLE of it is allowed; the
// login locked out of every table; and the two databases, each made as docs/testing.md asks
// (strategy file_copy, then testdb.WithoutJIT). In each, public grants nothing to PUBLIC and USAGE
// and CREATE to the client's login, so that the client makes and owns the tables in names.database,
// as a pod's agent login may, and neither the login locked out nor the role without rights reaches a
// table. names.readOnly gets Oh My Pi's two tables, which the client's login may read and write,
// with readOnlySession in them, and only then default_transaction_read_only = on, so that no session
// starts on it before the default does: a database default reaches only the sessions that start
// after it, and on names.database it would make every later case read-only.
func prepareLeftServer(t *testing.T, s leftServer) {
	t.Helper()
	ctx := context.Background()
	n, noRights := s.names, noRightsRole(t)
	login := func(role string) string { return "CREATE ROLE " + role + " LOGIN" }
	run := func(address string, statements ...string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, address)
		if err != nil {
			t.Fatalf("connect to prepare the server: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		for _, statement := range statements {
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Fatalf("prepare the server: %s: %v", statement, err)
			}
		}
	}
	run(s.maintenance, login(n.client), "CREATE ROLE "+noRights, "GRANT "+noRights+" TO "+n.client, login(n.lockedOut),
		"create database "+n.database+" strategy file_copy", testdb.WithoutJIT(n.database),
		"create database "+n.readOnly+" strategy file_copy", testdb.WithoutJIT(n.readOnly))
	schema := []string{"REVOKE ALL ON SCHEMA public FROM PUBLIC", "GRANT USAGE, CREATE ON SCHEMA public TO " + n.client}
	run(s.admin, schema...)
	run(s.adminRO, slices.Concat(schema, createTables, []string{
		"GRANT SELECT, INSERT, UPDATE, DELETE ON public.omp_session_files, public.omp_session_files_parts TO " + n.client,
		fmt.Sprintf("INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ('%s', '%s', %d, %d)",
			readOnlySession, readOnlyContent, readOnlyAt, len(readOnlyContent)),
	})...)
	run(s.maintenance, "ALTER DATABASE "+n.readOnly+" SET default_transaction_read_only = on")
}

// leftFolder is the session folder of every session the test writes.
const leftFolder = podSessions + "/--legion-workspaces-left-setting--"

// leftSession is the path and the id of the session of the run's case n.
func leftSession(n int) (path, id string) {
	id = fmt.Sprintf("left-%03d", n)
	return leftFolder + "/2026-11-27T12-00-00-000Z_" + id + ".jsonl", id
}

// What each case writes, each holding a multi-byte character: the session Import copies, what
// Append adds to it and what Replace writes in its place, each at its own mtime; and content with a
// NUL byte, which no text value holds, so the Import of it is refused and writes nothing.
var (
	leftFirst      = []byte("{\"type\":\"session\",\"version\":3,\"id\":\"left\"}\n{\"type\":\"message\",\"message\":{\"content\":\"grüß dich\"}}\n")
	leftAppended   = []byte("{\"type\":\"message\",\"message\":{\"content\":\"größer\"}}\n")
	leftReplaced   = []byte("{\"type\":\"session\",\"version\":3,\"id\":\"left\"}\n{\"type\":\"compaction\",\"summary\":\"über alles\"}\n")
	leftNUL        = []byte("{\"type\":\"session\"}\x00\n")
	leftFirstAt    = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	leftAppendedAt = leftFirstAt.Add(time.Minute)
	leftReplacedAt = leftFirstAt.Add(2 * time.Minute)
)

// poolerResets are the six parameters every PgBouncer release resets for each client: DateStyle,
// client_encoding, TimeZone, standard_conforming_strings and application_name, which it always
// tracks, and IntervalStyle, which track_extra_parameters adds (src/varcache.c, src/main.c,
// src/proto.c and src/objects.c at pgbouncer_1_24_1). PgBouncer sets each on the server connection
// to the next client's own value before that client's query, so what another client left of them
// never reaches the client, and the pooled run has no case for them.
var poolerResets = []string{"DateStyle", "client_encoding", "TimeZone", "standard_conforming_strings", "application_name", "IntervalStyle"}

// leftCase is one case a run leaves on the server connection the client then gets: its subtest's
// name; its LeftSettings entry, "" for none; the session-level statement that leaves it; and probe,
// an SQL expression of one text value that reads the entry as the backend then has it, "" for none.
// superuser has the direct run's client sign in as the test's superuser, the one login that may send
// the left statement, where it signs in as the client's login otherwise; unrepairable is a left
// statement under which the client cannot repair the connection, which the case wants closed
// (closed) where an entry's case wants it kept (pid). mode is the query mode of the client's Conn,
// 0 for the one ConnectURL gives it (tracedConn).
type leftCase struct {
	name, entry, left, probe string
	superuser, unrepairable  bool
	mode                     pgx.QueryExecMode
}

// leftCases are a run's cases of LeftSettings: one for each parameter the server has (has, its
// names of context user), for role and for each temporary table, less poolerResets on the pooled
// run. lacks are the parameters the server lacks, and resets the ones the pooled run leaves out.
func leftCases(has map[string]bool, pooled bool) (cases []leftCase, lacks, resets []string) {
	for _, entry := range LeftSettings {
		c := leftCase{name: entry.Name, entry: entry.Name, left: entry.Left}
		switch table, isTable := strings.CutPrefix(entry.Name, temporaryTable); {
		case isTable:
			c.probe = "(pg_catalog.to_regclass('pg_temp." + table + "') IS NOT NULL)::text"
		case entry.Name == "role":
			c.probe = "pg_catalog.current_setting('role')"
		case !has[entry.Name]:
			lacks = append(lacks, entry.Name)
			continue
		case pooled && slices.Contains(poolerResets, entry.Name):
			resets = append(resets, entry.Name)
			continue
		default:
			c.probe = "pg_catalog.current_setting('" + entry.Name + "')"
		}
		cases = append(cases, c)
	}
	return cases, lacks, resets
}

// userSettings are the names of the server's parameters of context user, read on conn, a
// connection that loaded no library: a DO block would load PL/pgSQL, whose four parameters of that
// context then show in its backend alone. A name LeftSettings lacks fails the test here, before any
// case runs: the list must hold the whole class of the server the test runs on.
func userSettings(t *testing.T, conn *pgx.Conn) map[string]bool {
	t.Helper()
	rows, err := conn.Query(context.Background(), "SELECT name FROM pg_catalog.pg_settings WHERE context = 'user' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, entry := range LeftSettings {
		listed[entry.Name] = true
	}
	has := map[string]bool{}
	var unlisted []string
	for _, name := range names {
		has[name] = true
		if !listed[name] {
			unlisted = append(unlisted, name)
		}
	}
	if len(unlisted) > 0 {
		t.Fatalf("the server has %d parameters of context user that LeftSettings lacks, so another client can leave them and no case "+
			"holds the client under them: %s", len(unlisted), strings.Join(unlisted, ", "))
	}
	return has
}

// The conditions of a case, each computed apart and named when it fails:
// operations, each operation answered what it reports; data, the admin reads in the tables what the
// client reported writing, byte for byte; temp, each temporary table of the client's backend holds
// no row; app, the client's transaction reads legion-session-client as its application_name, and
// so does pg_stat_activity for its backend while it waits for a row; isolation, the transaction
// reads read committed; pid, after the case the client's backend is the one the entry was left on,
// which still holds the entry; and closed, each backend the client could not repair is gone.
const (
	condOperations = "operations"
	condData       = "data"
	condTemp       = "temp"
	condApp        = "app"
	condIsolation  = "isolation"
	condPID        = "pid"
	condClosed     = "closed"
)

// leftConditions are the conditions in the order a case reports them.
var leftConditions = []string{condOperations, condData, condTemp, condApp, condIsolation, condPID, condClosed}

// leftOutcome is what a case found: the conditions it computed, with what is wrong with each, none
// for one that held, and the error each operation returned, nil for one that succeeded.
type leftOutcome struct {
	checked  map[string]bool
	failures map[string][]string
	errs     map[string]error
}

// newLeftOutcome is a case's outcome, which computes conditions.
func newLeftOutcome(conditions ...string) *leftOutcome {
	o := &leftOutcome{checked: map[string]bool{}, failures: map[string][]string{}, errs: map[string]error{}}
	for _, condition := range conditions {
		o.checked[condition] = true
	}
	return o
}

// fail records what is wrong with condition.
func (o *leftOutcome) fail(condition, format string, args ...any) {
	o.checked[condition] = true
	o.failures[condition] = append(o.failures[condition], fmt.Sprintf(format, args...))
}

// failed reports whether condition failed.
func (o *leftOutcome) failed(condition string) bool { return len(o.failures[condition]) > 0 }

// failure is what is wrong with condition, "" where it held.
func (o *leftOutcome) failure(condition string) string {
	return strings.Join(o.failures[condition], "; ")
}

// ran records the error operation returned, nil where it succeeded, and answers it.
func (o *leftOutcome) ran(operation string, err error) error {
	o.errs[operation] = err
	return err
}

// carries reports whether an operation's error carries SQLSTATE code.
func (o *leftOutcome) carries(code string) bool {
	for _, err := range o.errs {
		if sqlstate(err) == code {
			return true
		}
	}
	return false
}

// summary is each condition the case computed, with pass or fail.
func (o *leftOutcome) summary() string {
	var parts []string
	for _, condition := range leftConditions {
		switch {
		case o.failed(condition):
			parts = append(parts, condition+"=fail")
		case o.checked[condition]:
			parts = append(parts, condition+"=pass")
		}
	}
	return strings.Join(parts, " ")
}

// sqlstate is the SQLSTATE err carries, "" for none.
func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// leftClient runs a case's operations: the package's own functions on a Conn (packageClient), or,
// for a control, copies of their statements in the helper as it stood before the openings
// (controlClient).
type leftClient interface {
	written(ctx context.Context, path string) (time.Time, bool, error)
	findByID(ctx context.Context, id string) (string, bool, error)
	read(ctx context.Context, path string) ([]byte, bool, error)
	createTables(ctx context.Context) error
	importSession(ctx context.Context, path string, content []byte, mtime time.Time) (Outcome, error)
	appendTo(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error
	replace(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error
	// inside reads, in a read-only transaction of the client's helper, the transaction's
	// application_name and isolation level and the backend it runs on.
	inside(ctx context.Context) (app, isolation string, pid uint32, err error)
	// conn is the pgx connection the client holds now.
	conn() *pgx.Conn
}

// insideRead is the statement leftClient's inside sends, one of the test's own.
const insideRead = testMark + "SELECT pg_catalog.current_setting('application_name'), pg_catalog.current_setting('transaction_isolation'), " +
	"pg_catalog.pg_backend_pid()"

// packageClient is the package's own client: each operation is the exported function of its name on
// c, and inside runs through inTransaction, as a function of the package does.
type packageClient struct{ c *Conn }

func (p packageClient) written(ctx context.Context, path string) (time.Time, bool, error) {
	return Written(ctx, p.c, path)
}

func (p packageClient) findByID(ctx context.Context, id string) (string, bool, error) {
	return FindByID(ctx, p.c, id)
}

func (p packageClient) read(ctx context.Context, path string) ([]byte, bool, error) {
	return Read(ctx, p.c, path)
}

func (p packageClient) createTables(ctx context.Context) error { return CreateTables(ctx, p.c) }

func (p packageClient) importSession(ctx context.Context, path string, content []byte, mtime time.Time) (Outcome, error) {
	return Import(ctx, p.c, path, content, mtime)
}

func (p packageClient) appendTo(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error {
	return Append(ctx, p.c, path, saved, content, mtime)
}

func (p packageClient) replace(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error {
	return Replace(ctx, p.c, path, saved, content, mtime)
}

func (p packageClient) inside(ctx context.Context) (app, isolation string, pid uint32, err error) {
	var backend int32
	err = inTransaction(ctx, p.c, true, func(pin pinned) error {
		return pin.queryRow(ctx, insideRead).Scan(&app, &isolation, &backend)
	})
	return app, isolation, uint32(backend), err
}

func (p packageClient) conn() *pgx.Conn { return p.c.c }

// beforeOpenings is the control's helper, the client's inTransaction as it stood before the
// openings (main at 17767e49): pgx.BeginTxFunc with the access mode, then SET LOCAL search_path =
// pg_catalog, pg_temp, in a read SET LOCAL row_security = off, then SET LOCAL
// restrict_nonsystem_relation_kind = 'view, foreign-table', each a statement of its own, and
// ROLLBACK on every error. It sets nothing else another client could have left, and closes no
// connection.
func beforeOpenings(ctx context.Context, conn *pgx.Conn, readOnly bool, fn func(pgx.Tx) error) error {
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		pins := []string{"SET LOCAL search_path = pg_catalog, pg_temp"}
		if readOnly {
			pins = append(pins, "SET LOCAL row_security = off")
		}
		for _, pin := range append(pins, "SET LOCAL restrict_nonsystem_relation_kind = 'view, foreign-table'") {
			if _, err := tx.Exec(ctx, pin); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

// controlClient is the control's client: copies of the statements of the package's functions, in
// beforeOpenings on the one connection c, as the client ran them before the openings.
type controlClient struct{ c *pgx.Conn }

func (m controlClient) written(ctx context.Context, path string) (time.Time, bool, error) {
	var mtime int64
	err := beforeOpenings(ctx, m.c, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT mtime_ms FROM public.omp_session_files WHERE path = $1", path).Scan(&mtime)
	})
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.UnixMilli(mtime), true, nil
}

func (m controlClient) findByID(ctx context.Context, id string) (string, bool, error) {
	suffix := "_" + id + ".jsonl"
	var path string
	err := beforeOpenings(ctx, m.c, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT path FROM public.omp_session_files WHERE right(path, $1) = $2 ORDER BY mtime_ms DESC, path ASC LIMIT 1",
			utf8.RuneCountInString(suffix), suffix).Scan(&path)
	})
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return path, err == nil, err
}

func (m controlClient) read(ctx context.Context, path string) ([]byte, bool, error) {
	var buffer bytes.Buffer
	ok := false
	err := beforeOpenings(ctx, m.c, true, func(tx pgx.Tx) error {
		buffer.Reset()
		ok = false
		rows, err := tx.Query(ctx, "SELECT 0 AS kind, 0::bigint AS start_offset, content FROM public.omp_session_files WHERE path = $1 "+
			"UNION ALL SELECT 1, start_offset, content FROM public.omp_session_files_parts WHERE path = $1 ORDER BY kind, start_offset", path)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind int
			var offset int64
			var piece string
			if err := rows.Scan(&kind, &offset, &piece); err != nil {
				return err
			}
			ok = ok || kind == 0
			buffer.WriteString(piece)
		}
		return rows.Err()
	})
	if isUndefinedTable(err) || (err == nil && !ok) {
		return nil, false, nil
	}
	return buffer.Bytes(), err == nil, err
}

func (m controlClient) createTables(ctx context.Context) error {
	return beforeOpenings(ctx, m.c, false, func(tx pgx.Tx) error {
		for _, statement := range createTables {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m controlClient) importSession(ctx context.Context, path string, content []byte, mtime time.Time) (Outcome, error) {
	if err := m.createTables(ctx); err != nil {
		return "", err
	}
	inserted := false
	err := beforeOpenings(ctx, m.c, false, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4) "+
			"ON CONFLICT (path) DO NOTHING", path, string(content), mtime.UnixMilli(), len(content))
		if err != nil {
			return err
		}
		if inserted = tag.RowsAffected() == 1; !inserted {
			return nil
		}
		_, err = tx.Exec(ctx, "DELETE FROM public.omp_session_files_parts WHERE path = $1", path)
		return err
	})
	switch {
	case err != nil:
		return "", err
	case inserted:
		return Copied, nil
	}
	return Identical, nil
}

func (m controlClient) appendTo(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error {
	return m.write(ctx, path, saved, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO public.omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
			path, saved, string(content)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE public.omp_session_files SET byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, saved+int64(len(content)), mtime.UnixMilli())
		return err
	})
}

func (m controlClient) replace(ctx context.Context, path string, saved int64, content []byte, mtime time.Time) error {
	return m.write(ctx, path, saved, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE public.omp_session_files SET content = '', byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, len(content), mtime.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM public.omp_session_files_parts WHERE path = $1", path); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO public.omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
			path, 0, string(content))
		return err
	})
}

// write is the package's write in beforeOpenings: the row at path locked and its byte_len compared
// with saved, which a test's write here never leaves Absent, before apply.
func (m controlClient) write(ctx context.Context, path string, saved int64, apply func(pgx.Tx) error) error {
	return beforeOpenings(ctx, m.c, false, func(tx pgx.Tx) error {
		held := Absent
		err := tx.QueryRow(ctx, "SELECT byte_len FROM public.omp_session_files WHERE path = $1 FOR UPDATE", path).Scan(&held)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if held != saved {
			return &MovedError{Path: path, Saved: saved, Held: held}
		}
		return apply(tx)
	})
}

func (m controlClient) inside(ctx context.Context) (app, isolation string, pid uint32, err error) {
	var backend int32
	err = beforeOpenings(ctx, m.c, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, insideRead).Scan(&app, &isolation, &backend)
	})
	return app, isolation, uint32(backend), err
}

func (m controlClient) conn() *pgx.Conn { return m.c }

// clientOf is the client a case runs on conn: the package's own, or the control's on its pgx
// connection.
func clientOf(conn *Conn, control bool) leftClient {
	if control {
		return controlClient{conn.c}
	}
	return packageClient{conn}
}

// pauseInNextTransaction has trace wait leftPause, sending nothing, before the second statement
// of the next transaction its connection opens, so that a left idle_in_transaction_session_timeout
// acts inside it: under the client, between the opening and the work; under the control, between
// begin and its first SET LOCAL.
func pauseInNextTransaction(trace *tracer) {
	opened, paused := false, false
	trace.hook(func(_ *pgx.Conn, sql string) {
		switch {
		case paused:
		case opened:
			paused = true
			time.Sleep(leftPause)
		case sql == OpenRead || sql == OpenWrite || strings.HasPrefix(sql, "begin"):
			opened = true
		}
	})
}

// backendReads is the backend a statement on conn reaches and probe, an expression of one text
// value, as it reads there ("" for no probe), in one statement of the test's own.
func backendReads(ctx context.Context, conn *pgx.Conn, probe string) (uint32, string, error) {
	if probe == "" {
		probe = "''"
	}
	var pid int32
	var value string
	err := conn.QueryRow(ctx, testMark+"SELECT pg_catalog.pg_backend_pid(), "+probe).Scan(&pid, &value)
	return uint32(pid), value, err
}

// stored is the row the tables hold at path, as admin reads it byte for byte, with its parts in
// offset order: "no row" where they hold none.
func stored(ctx context.Context, admin *pgx.Conn, path string) string {
	var content string
	var byteLen, mtime int64
	row := ""
	switch err := admin.QueryRow(ctx, "SELECT content, byte_len, mtime_ms FROM public.omp_session_files WHERE path = $1", path).Scan(&content, &byteLen, &mtime); {
	case errors.Is(err, pgx.ErrNoRows):
		row = "no row"
	case err != nil:
		return fmt.Sprintf("a row the admin cannot read (%v)", err)
	default:
		row = fmt.Sprintf("content %q, byte_len %d, mtime_ms %d", content, byteLen, mtime)
	}
	rows, err := admin.Query(ctx, "SELECT start_offset, content FROM public.omp_session_files_parts WHERE path = $1 ORDER BY start_offset", path)
	if err != nil {
		return fmt.Sprintf("%s, parts the admin cannot read (%v)", row, err)
	}
	parts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var offset int64
		var part string
		err := row.Scan(&offset, &part)
		return fmt.Sprintf("%d:%q", offset, part), err
	})
	if err != nil {
		return fmt.Sprintf("%s, parts the admin cannot read (%v)", row, err)
	}
	return fmt.Sprintf("%s, parts [%s]", row, strings.Join(parts, " "))
}

// storedRow is what stored answers for a row of content, byteLen and mtime, with parts, each
// <offset>:<content>.
func storedRow(content string, byteLen int, mtime time.Time, parts ...[2]string) string {
	shown := make([]string, len(parts))
	for i, part := range parts {
		shown[i] = fmt.Sprintf("%s:%q", part[0], part[1])
	}
	return fmt.Sprintf("content %q, byte_len %d, mtime_ms %d, parts [%s]", content, byteLen, mtime.UnixMilli(), strings.Join(shown, " "))
}

// noRow is what stored answers for a path the tables hold nothing at.
const noRow = "no row, parts []"

// temporaryFailure is what is wrong with the temporary tables of the backend conn reaches, "" for
// nothing: each must hold no row, since every statement of the client names public.<table> and
// pg_temp is last on the search_path the openings pin.
func temporaryFailure(ctx context.Context, conn *pgx.Conn) string {
	rows, err := conn.Query(ctx, testMark+"SELECT c.relname::text FROM pg_catalog.pg_class c "+
		"WHERE c.relnamespace = pg_catalog.pg_my_temp_schema() AND c.relkind = 'r' ORDER BY c.relname")
	if err != nil {
		return fmt.Sprintf("list the backend's temporary tables: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Sprintf("list the backend's temporary tables: %v", err)
	}
	var failures []string
	for _, name := range names {
		var held int64
		if err := conn.QueryRow(ctx, testMark+"SELECT count(*) FROM pg_temp."+pgx.Identifier{name}.Sanitize()).Scan(&held); err != nil {
			failures = append(failures, fmt.Sprintf("count the rows of pg_temp.%s: %v", name, err))
		} else if held != 0 {
			failures = append(failures, fmt.Sprintf("the temporary table %s holds %d rows, want none: a statement of the client wrote it", name, held))
		}
	}
	return strings.Join(failures, "; ")
}

// leftRun is one run of the test, direct or through PgBouncer, and what it counted: its cases, the
// ones that failed and the ones that skipped; its controls, and the ones that failed as named; and
// what the log rule found for the backends the client's transactions ran on (pids), which must log
// at least one statement for each opening the client sent (openings).
type leftRun struct {
	name     string
	pooled   bool
	server   leftServer
	noRights string
	// admin is the test's superuser on the cases' database, directly, and watcher a second such
	// connection, which reads pg_stat_activity while admin holds a row; adminRO is the superuser on
	// the read-only database; console is PgBouncer's admin console, nil on the direct run.
	admin, watcher, adminRO, console *pgx.Conn
	sessions                         int
	pids                             map[uint32]bool
	openings                         int
	cases, failed, skipped           int
	controls, caught                 int
	statements, other                int
}

// newLeftRun is the run name of the test on server, through PgBouncer where pooled.
func newLeftRun(t *testing.T, name string, pooled bool, server leftServer) *leftRun {
	t.Helper()
	r := &leftRun{name: name, pooled: pooled, server: server, noRights: noRightsRole(t), pids: map[uint32]bool{}}
	r.admin, r.watcher, r.adminRO = connectFor(t, server.admin), connectFor(t, server.admin), connectFor(t, server.adminRO)
	if pooled {
		r.console = connectFor(t, server.console)
	}
	return r
}

// connectFor is a connection to address of the test's own, closed when the test ends.
func connectFor(t *testing.T, address string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), address)
	if err != nil {
		t.Fatalf("connect for the test's own statements: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// session is the path and the id of the run's next session, one of the case's own.
func (r *leftRun) session() (path, id string) {
	r.sessions++
	return leftSession(r.sessions)
}

// record keeps, for the log rule, the backends a case's client ran on and the openings it sent:
// on the direct run each backend the tracer saw a statement go to, and through PgBouncer, whose
// client connections carry no server's pid, the server backends the test read.
func (r *leftRun) record(sent []sentTo, backends ...uint32) {
	r.openings += len(openings(sent))
	if !r.pooled {
		for _, s := range sent {
			r.pids[s.pid] = true
		}
	}
	for _, pid := range backends {
		if pid != 0 {
			r.pids[pid] = true
		}
	}
}

// gone reports whether every one of pids left pg_stat_activity within leftBound.
func (r *leftRun) gone(t *testing.T, pids ...uint32) bool {
	t.Helper()
	return testwait.Holds(leftBound, 20*time.Millisecond, func() bool {
		for _, pid := range pids {
			if alive(t, r.watcher, pid) {
				return false
			}
		}
		return true
	})
}

// onBackend runs read on a connection to the backend the client's server connection is: directly
// the client's own, and through PgBouncer a further client of the pool address, which PgBouncer
// hands the pool's one server connection.
func (r *leftRun) onBackend(t *testing.T, client leftClient, address string, read func(*pgx.Conn)) {
	t.Helper()
	if !r.pooled {
		read(client.conn())
		return
	}
	ctx := context.Background()
	further, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatalf("connect a further client of the pool: %v", err)
	}
	defer func() { _ = further.Close(ctx) }()
	read(further)
}

// operate runs a case's operations on client, at a session of the case's own,
// and records in o what each answered (operations), what the tables hold after each write (data),
// and the client's application_name and isolation level (app, isolation). backend is the backend
// the entry was left on, which the client's transactions must run on. It answers the backend the
// client's own read inside its transaction ran on.
func (r *leftRun) operate(t *testing.T, client leftClient, trace *tracer, backend uint32, o *leftOutcome) uint32 {
	t.Helper()
	ctx := context.Background()
	path, id := r.session()
	refused := strings.TrimSuffix(path, ".jsonl") + "-nul.jsonl"
	if _, err := r.admin.Exec(ctx, "DROP TABLE IF EXISTS public.omp_session_files, public.omp_session_files_parts"); err != nil {
		t.Fatalf("drop the tables: %v", err)
	}
	if at, found, err := client.written(ctx, path); o.ran("Written with the tables missing", err) != nil || found {
		o.fail(condOperations, "Written with the tables missing = %v, %t, %v; want no session", at, found, err)
	}
	if err := o.ran("CreateTables", client.createTables(ctx)); err != nil {
		o.fail(condOperations, "CreateTables = %v", err)
	}
	if outcome, err := client.importSession(ctx, path, leftFirst, leftFirstAt); o.ran("Import", err) != nil || outcome != Copied {
		o.fail(condOperations, "Import = %q, %v; want %q", outcome, err, Copied)
	}
	if got, want := stored(ctx, r.admin, path), storedRow(string(leftFirst), len(leftFirst), leftFirstAt); got != want {
		o.fail(condData, "after Import the tables hold %s; want %s", got, want)
	}
	if at, found, err := client.written(ctx, path); o.ran("Written", err) != nil || !found || !at.Equal(leftFirstAt) {
		o.fail(condOperations, "Written = %v, %t, %v; want the session at %v", at, found, err, leftFirstAt)
	}
	if got, found, err := client.findByID(ctx, id); o.ran("FindByID", err) != nil || !found || got != path {
		o.fail(condOperations, "FindByID(%q) = %q, %t, %v; want %q", id, got, found, err, path)
	}
	pauseInNextTransaction(trace)
	content, found, err := client.read(ctx, path)
	trace.hook(nil)
	if o.ran("Read", err) != nil || !found || !bytes.Equal(content, leftFirst) {
		o.fail(condOperations, "Read = %q, %t, %v; want %q", content, found, err, leftFirst)
	}
	if _, err := client.importSession(ctx, refused, leftNUL, leftFirstAt); o.ran("Import of a NUL byte", err) == nil ||
		!strings.Contains(err.Error(), "invalid byte sequence") {
		o.fail(condOperations, "Import of content with a NUL byte = %v; want it refused as an invalid byte sequence", err)
	}
	if got := stored(ctx, r.admin, refused); got != noRow {
		o.fail(condData, "after the refused Import the tables hold %s at its path; want nothing", got)
	}

	app, isolation, inside, err := client.inside(ctx)
	switch {
	case o.ran("the read inside a transaction", err) != nil:
		o.fail(condApp, "the read inside the client's transaction failed: %v", err)
		o.fail(condIsolation, "the read inside the client's transaction failed: %v", err)
	default:
		if app != clientName {
			o.fail(condApp, "inside the client's transaction application_name reads %q, want %q", app, clientName)
		}
		if isolation != "read committed" {
			o.fail(condIsolation, "inside the client's transaction transaction_isolation reads %q, want read committed", isolation)
		}
	}
	watched := cmp.Or(inside, backend)
	shown, err := r.heldAppend(ctx, client, path, watched, o)
	if err != nil || shown == "" {
		o.fail(condApp, "pg_stat_activity never showed backend %d waiting for the row the admin held (%v)", watched, err)
	} else if shown != clientName {
		o.fail(condApp, "while it waited for the row, pg_stat_activity showed backend %d's application_name as %q, want %q", watched, shown, clientName)
	}
	saved := int64(len(leftFirst))
	if got, want := stored(ctx, r.admin, path), storedRow(string(leftFirst), len(leftFirst)+len(leftAppended), leftAppendedAt,
		[2]string{strconv.Itoa(len(leftFirst)), string(leftAppended)}); got != want {
		o.fail(condData, "after Append the tables hold %s; want %s", got, want)
	}
	saved += int64(len(leftAppended))
	if err := o.ran("Replace", client.replace(ctx, path, saved, leftReplaced, leftReplacedAt)); err != nil {
		o.fail(condOperations, "Replace = %v", err)
	}
	if got, want := stored(ctx, r.admin, path), storedRow("", len(leftReplaced), leftReplacedAt, [2]string{"0", string(leftReplaced)}); got != want {
		o.fail(condData, "after Replace the tables hold %s; want %s", got, want)
	}
	if content, found, err := client.read(ctx, path); o.ran("Read after Replace", err) != nil || !found || !bytes.Equal(content, leftReplaced) {
		o.fail(condOperations, "Read after Replace = %q, %t, %v; want %q", content, found, err, leftReplaced)
	}
	return inside
}

// heldAppend runs the client's Append to the session at path while the admin holds its row FOR
// UPDATE for leftRowHold, and answers the application_name the watcher read in pg_stat_activity for
// backend while it waited for that row, "" where it never saw it wait, or why the admin could not
// hold the row.
func (r *leftRun) heldAppend(ctx context.Context, client leftClient, path string, backend uint32, o *leftOutcome) (string, error) {
	lock, err := r.admin.Begin(ctx)
	if err != nil {
		return "", err
	}
	var one int
	if err := lock.QueryRow(ctx, "SELECT 1 FROM public.omp_session_files WHERE path = $1 FOR UPDATE", path).Scan(&one); err != nil {
		_ = lock.Rollback(ctx)
		if err := o.ran("Append", client.appendTo(ctx, path, int64(len(leftFirst)), leftAppended, leftAppendedAt)); err != nil {
			o.fail(condOperations, "Append = %v", err)
		}
		return "", fmt.Errorf("hold the session's row: %w", err)
	}
	held := time.Now()
	done := make(chan error, 1)
	go func() { done <- client.appendTo(ctx, path, int64(len(leftFirst)), leftAppended, leftAppendedAt) }()
	shown := ""
	for shown == "" && time.Since(held) < leftRowHold {
		var name string
		var waiting bool
		if err := r.watcher.QueryRow(ctx, "SELECT application_name, "+lockwaittest.Waiting+" FROM pg_catalog.pg_stat_activity WHERE pid = $1",
			int64(backend)).Scan(&name, &waiting); err == nil && waiting {
			shown = name
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(time.Until(held.Add(leftRowHold)))
	if err := lock.Commit(ctx); err != nil {
		_ = lock.Rollback(ctx)
		return shown, fmt.Errorf("release the session's row: %w", err)
	}
	select {
	case err = <-done:
	case <-time.After(leftBound):
		err = fmt.Errorf("Append did not return within %s of the row's release", leftBound)
	}
	if o.ran("Append", err) != nil {
		o.fail(condOperations, "Append while the admin held the row for %s = %v", leftRowHold, err)
	}
	return shown, nil
}

// entryCase runs c, a case of an entry (or the empty search_path, or the unrepairable server
// connection): it leaves c on the server connection the client then gets, runs the operations on
// the package's client, or, where control, on the control's, but for a case that names a query
// mode, which a control runs the package's own client in, and computes the case's conditions. A
// control records nothing for the log rule. Directly, the client's Conn is built around a
// connection on which the test sent c's left statement, with a tracer in its configuration, which
// each connection the client opens in its place keeps. Through PgBouncer, the test sends the
// statement from a client of the pool leftset and closes it, and the client then connects through
// the same pool, whose one server connection PgBouncer hands it. Either way the Conn sends in c's
// query mode (tracedConnIn).
func (r *leftRun) entryCase(t *testing.T, c leftCase, control bool) *leftOutcome {
	t.Helper()
	ctx := context.Background()
	o := newLeftOutcome(condOperations, condData, condTemp, condApp, condIsolation)
	trace := &tracer{}
	var conn *Conn
	var left uint32
	var leftValue string
	var err error
	if r.pooled {
		leaving, err := pgx.Connect(ctx, r.server.pooled)
		if err != nil {
			t.Fatalf("connect the client that leaves %s: %v", c.name, err)
		}
		left, leftValue, err = leaveOn(ctx, leaving, c)
		_ = leaving.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conn = tracedConnIn(t, r.server.pooled, trace, c.mode)
	} else {
		address := r.server.client
		if c.superuser {
			address = r.server.admin
		}
		conn = tracedConnIn(t, address, trace, c.mode)
		if left, leftValue, err = leaveOn(ctx, conn.c, c); err != nil {
			t.Fatal(err)
		}
	}
	client := clientOf(conn, control && c.mode == 0)
	trace.take()
	inside := r.operate(t, client, trace, left, o)

	var after uint32
	r.onBackend(t, client, r.server.pooled, func(backend *pgx.Conn) {
		if failure := temporaryFailure(ctx, backend); failure != "" {
			o.fail(condTemp, "%s", failure)
		}
		if c.unrepairable {
			after, _, _ = backendReads(ctx, backend, "")
			return
		}
		pid, value, err := backendReads(ctx, backend, c.probe)
		after = pid
		switch {
		case err != nil:
			o.fail(condPID, "the read on the client's backend after the case failed: %v", err)
		case pid != left:
			o.fail(condPID, "after the case the client's server connection is backend %d, want %d, the one the entry was left on", pid, left)
		case value != leftValue:
			o.fail(condPID, "after the case backend %d reads the entry as %q, want %q, the value left on it", pid, value, leftValue)
		case !r.pooled && c.entry == "application_name" && value != "left":
			o.fail(condPID, "after the case the backend's own application_name reads %q, want left", value)
		}
		o.checked[condPID] = true
	})
	sent, connects := trace.take()
	if !control {
		r.record(sent, left, inside, after)
	}
	if c.unrepairable {
		o.checked[condClosed] = true
		now := cmp.Or(inside, after)
		if !r.pooled {
			now = client.conn().PgConn().PID()
		}
		switch {
		case connects == 0:
			o.fail(condClosed, "the client opened no new connection, want one in place of backend %d, which it cannot repair", left)
		case now == left:
			o.fail(condClosed, "the client's transactions still run on backend %d, which it cannot repair", left)
		case !r.gone(t, left):
			o.fail(condClosed, "backend %d, which the client cannot repair, is still in pg_stat_activity %s after the case", left, leftBound)
		}
	}
	return o
}

// leaveOn sends c's left statement on conn, one of the test's own, and answers the backend it went
// to and c's probe as that backend then reads it.
func leaveOn(ctx context.Context, conn *pgx.Conn, c leftCase) (uint32, string, error) {
	if _, err := conn.Exec(ctx, testMark+c.left); err != nil {
		return 0, "", fmt.Errorf("leave %s with %s: %w", c.name, c.left, err)
	}
	pid, value, err := backendReads(ctx, conn, c.probe)
	if err != nil {
		return 0, "", fmt.Errorf("read %s back after leaving it: %w", c.name, err)
	}
	return pid, value, nil
}

// fourInARow is the direct run's case of four unrepairable server connections in a row: the
// client's connection is left_locked_out's, which reaches no table, and so is each one it opens in
// its place, so each try of its first call answers 42501 and is closed. The call fails after 4
// closes, naming 42501 and the 4 tries, and the 4 backends are gone.
func (r *leftRun) fourInARow(t *testing.T) *leftOutcome {
	t.Helper()
	ctx := context.Background()
	o := newLeftOutcome(condOperations, condClosed)
	trace := &tracer{}
	conn := tracedConn(t, r.server.lockedOut, trace)
	trace.take()
	path, _ := r.session()
	at, found, err := Written(ctx, conn, path)
	o.ran("Written", err)
	sent, connects := trace.take()
	pids := openings(sent)
	r.record(sent)
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 42501") || !strings.Contains(err.Error(), fmt.Sprintf("%d tries", tries)) {
		o.fail(condOperations, "Written as a login locked out of every table = %v, %t, %v; want it to fail after %d tries naming SQLSTATE 42501",
			at, found, err, tries)
	}
	if connects != tries-1 || len(pids) != tries || !distinct(pids) {
		o.fail(condClosed, "the client opened %d new connections and its transaction on backends %v; want %d opened and %d distinct backends",
			connects, pids, tries-1, tries)
	}
	if !r.gone(t, pids...) {
		o.fail(condClosed, "the backends %v the client closed are not all gone from pg_stat_activity", pids)
	}
	if !conn.IsClosed() {
		o.fail(condClosed, "after the fourth failure the Conn is open, want its connection closed")
	}
	return o
}

// poolOfFour is the pooled run's case of a pool of four server connections, three unrepairable
// (pool leftset4): the test holds four clients of the pool, each in BEGIN and so on a server
// connection of its own, sends SET SESSION AUTHORIZATION of the role without rights in three, and
// commits the clean one first and then the three, so that PgBouncer, which hands out the server
// connection released last first (server_round_robin = 0), gives the client the three first. One
// write then succeeds after exactly 3 closes, on the clean one, and the three are gone.
func (r *leftRun) poolOfFour(t *testing.T) *leftOutcome {
	t.Helper()
	ctx := context.Background()
	o := newLeftOutcome(condOperations, condData, condClosed)
	for _, statement := range createTables {
		if _, err := r.admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	held := make([]*pgx.Conn, 4)
	pids := make([]uint32, len(held))
	for i := range held {
		conn, err := pgx.Connect(ctx, r.server.pooled4)
		if err != nil {
			t.Fatalf("connect client %d of the pool of four: %v", i+1, err)
		}
		defer func() { _ = conn.Close(ctx) }()
		held[i] = conn
		if _, err := conn.Exec(ctx, testMark+"BEGIN"); err != nil {
			t.Fatal(err)
		}
		if pids[i], _, err = backendReads(ctx, conn, ""); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if _, err := conn.Exec(ctx, testMark+"SET SESSION AUTHORIZATION "+r.noRights); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !distinct(pids) {
		t.Fatalf("the four clients of the pool of four in BEGIN hold backends %v, want four server connections", pids)
	}
	for _, conn := range held {
		if _, err := conn.Exec(ctx, testMark+"COMMIT"); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close(ctx)
	}
	trace := &tracer{}
	conn := tracedConn(t, r.server.pooled4, trace)
	trace.take()
	path, _ := r.session()
	if err := o.ran("Replace", Replace(ctx, conn, path, Absent, leftReplaced, leftReplacedAt)); err != nil {
		o.fail(condOperations, "Replace through a pool of four with three server connections it cannot repair = %v", err)
	}
	_, _, now, err := packageClient{conn}.inside(ctx)
	sent, connects := trace.take()
	r.record(sent, append(slices.Clone(pids), now)...)
	if err != nil || now != pids[0] {
		o.fail(condClosed, "after the write the client's server connection is backend %d (%v), want %d, the clean one", now, err, pids[0])
	}
	if connects != 3 {
		o.fail(condClosed, "the write opened %d new connections, want exactly 3: one in place of each server connection left under "+
			"SET SESSION AUTHORIZATION %s, which PgBouncer hands out before the clean one", connects, r.noRights)
	}
	if !r.gone(t, pids[1:]...) {
		o.fail(condClosed, "the backends %v the client closed are not all gone from pg_stat_activity", pids[1:])
	}
	if got, want := stored(ctx, r.admin, path), storedRow("", len(leftReplaced), leftReplacedAt, [2]string{"0", string(leftReplaced)}); got != want {
		o.fail(condData, "after the write the tables hold %s; want %s", got, want)
	}
	return o
}

// readOnlyDefault is the case of the database's read-only default (names.readOnly, a database of
// its own: prepareLeftServer says why), on both runs: each write answers *ReadOnlyDefaultError, each
// read answers the session the database holds, the tables still hold their one row, and the
// client's backend is the one it started on and still in pg_stat_activity, with no connection
// opened in its place. The control's writes land, and it puts the tables back as they were.
func (r *leftRun) readOnlyDefault(t *testing.T, control bool) *leftOutcome {
	t.Helper()
	ctx := context.Background()
	o := newLeftOutcome(condOperations, condData, condPID)
	address := r.server.clientRO
	if r.pooled {
		address = r.server.pooledRO
	}
	trace := &tracer{}
	conn := tracedConn(t, address, trace)
	client := clientOf(conn, control)
	trace.take()
	if control {
		t.Cleanup(func() { r.restoreReadOnly(t) })
	}
	_, _, before, err := client.inside(ctx)
	if err != nil {
		t.Fatalf("read the client's backend: %v", err)
	}
	path, _ := r.session()
	refused := func(what string, err error) {
		var refusal *ReadOnlyDefaultError
		if o.ran(what, err); !errors.As(err, &refusal) {
			o.fail(condOperations, "%s under the database's read-only default = %v; want *ReadOnlyDefaultError", what, err)
		}
	}
	refused("CreateTables", client.createTables(ctx))
	_, err = client.importSession(ctx, path, leftFirst, leftFirstAt)
	refused("Import", err)
	refused("Append", client.appendTo(ctx, readOnlySession, int64(len(readOnlyContent)), leftAppended, leftAppendedAt))
	refused("Replace", client.replace(ctx, readOnlySession, int64(len(readOnlyContent)), leftReplaced, leftReplacedAt))
	if at, found, err := client.written(ctx, readOnlySession); o.ran("Written", err) != nil || !found || at.UnixMilli() != readOnlyAt {
		o.fail(condOperations, "Written under the read-only default = %v, %t, %v; want the session at %d", at, found, err, readOnlyAt)
	}
	if got, found, err := client.findByID(ctx, readOnlyID); o.ran("FindByID", err) != nil || !found || got != readOnlySession {
		o.fail(condOperations, "FindByID under the read-only default = %q, %t, %v; want %q", got, found, err, readOnlySession)
	}
	if content, found, err := client.read(ctx, readOnlySession); o.ran("Read", err) != nil || !found || string(content) != readOnlyContent {
		o.fail(condOperations, "Read under the read-only default = %q, %t, %v; want %q", content, found, err, readOnlyContent)
	}
	var rows, parts int
	if err := r.adminRO.QueryRow(ctx, "SELECT (SELECT count(*) FROM public.omp_session_files), (SELECT count(*) FROM public.omp_session_files_parts)").
		Scan(&rows, &parts); err != nil || rows != 1 || parts != 0 {
		o.fail(condData, "the read-only database's tables hold %d rows and %d parts (%v); want its one row and no part", rows, parts, err)
	}
	if got, want := stored(ctx, r.adminRO, readOnlySession), storedRow(readOnlyContent, len(readOnlyContent), time.UnixMilli(readOnlyAt)); got != want {
		o.fail(condData, "the read-only database holds %s; want %s", got, want)
	}
	_, _, after, err := client.inside(ctx)
	sent, connects := trace.take()
	if !control {
		r.record(sent, before, after)
	}
	if err != nil || after != before || connects != 0 || !alive(t, r.watcher, before) {
		o.fail(condPID, "after the writes the client runs on backend %d (%v) and opened %d connections; want backend %d, still in "+
			"pg_stat_activity, and none opened: the refusal keeps its connection", after, err, connects, before)
	}
	return o
}

// restoreReadOnly puts the read-only database's tables back as prepareLeftServer wrote them, after
// the control wrote through the default: in a transaction of its own that names READ WRITE.
func (r *leftRun) restoreReadOnly(t *testing.T) {
	ctx := context.Background()
	if err := pgx.BeginTxFunc(ctx, r.adminRO, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		for _, statement := range []string{"DELETE FROM public.omp_session_files_parts", "DELETE FROM public.omp_session_files"} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4)",
			readOnlySession, readOnlyContent, readOnlyAt, len(readOnlyContent))
		return err
	}); err != nil {
		t.Errorf("put the read-only database's tables back: %v", err)
	}
}

// leftFixed is one of a run's fixed cases: its name, how it runs, and whether it leaves what it
// left on the server connection of the pool leftset, which the pooled run then renews.
type leftFixed struct {
	name   string
	run    func(t *testing.T) *leftOutcome
	renews bool
}

// The names of the fixed cases a control repeats.
const (
	unrepairableCase = "an unrepairable server connection"
	readOnlyCase     = "the database's read-only default"
)

// unrepairable is the fixed case of one server connection the client cannot repair: SET SESSION
// AUTHORIZATION of the role without rights, which only a superuser may send, so that the
// transaction's SET LOCAL role TO DEFAULT runs it as that role. Directly, the client's connection is
// the test's superuser's, on which the test sent the statement, and the one the client opens in its
// place is clean; through PgBouncer, the test sends it through leftset.
func (r *leftRun) unrepairable() leftCase {
	return leftCase{name: unrepairableCase, left: "SET SESSION AUTHORIZATION " + r.noRights, superuser: true, unrepairable: true}
}

// fixedCases are the run's fixed cases: an empty search_path, one unrepairable server connection,
// four in a row directly or the pool of four through PgBouncer, and the database's read-only
// default.
func (r *leftRun) fixedCases() []leftFixed {
	empty := leftCase{name: "an empty search_path", entry: "search_path", left: "SET search_path = ''", probe: "pg_catalog.current_setting('search_path')"}
	cases := []leftFixed{
		{name: empty.name, run: func(t *testing.T) *leftOutcome { return r.entryCase(t, empty, false) }, renews: true},
		{name: unrepairableCase, run: func(t *testing.T) *leftOutcome { return r.entryCase(t, r.unrepairable(), false) }, renews: true},
	}
	if r.pooled {
		cases = append(cases, leftFixed{name: "a pool of four server connections, three unrepairable", run: r.poolOfFour})
	} else {
		cases = append(cases, leftFixed{name: "four unrepairable server connections in a row", run: r.fourInARow})
	}
	return append(cases, leftFixed{name: readOnlyCase, run: func(t *testing.T) *leftOutcome { return r.readOnlyDefault(t, false) }})
}

// leftControl is one control of a run, a fault the test must catch: an entry the helper as it stood
// before the openings breaks, or a fixed case it breaks, or, where mode is set, the case of entry
// with the package's own client sending in that query mode, which the control's name says; the
// condition the control wants to fail, and caught, which reports whether the operations failed as
// the control names. directOnly is a control the pooled run leaves out, and pooledOnly one the
// direct run leaves out.
type leftControl struct {
	entry, fixed, name, named string
	caught                    func(o *leftOutcome) bool
	directOnly, pooledOnly    bool
	mode                      pgx.QueryExecMode
}

// leftControls are the controls, ten on each run, each wanting its named condition to fail, and
// failing as the fault would. The first ten run in beforeOpenings: the case of each entry the helper
// as it stood before the openings breaks, and the two fixed cases it breaks; the pooled run leaves
// out client_encoding's, since PgBouncer sets it for each client (poolerResets). The last runs
// through PgBouncer alone: array_nulls' case with the package's own client in pgx's default query
// mode, a statement cache that names each statement after its text. PgBouncer 1.25.2 with
// max_prepared_statements keeps the name of a statement the server refused at Parse, so the
// client's Written with the tables missing (42P01) leaves its name there, the next Written of that
// text is refused FATAL (08P01, "prepared statement name is already in use"), and the client closes
// the server connection the entry was left on and runs on another (execMode in ompsessions.go). A
// server alone keeps no refused name, so the direct run has no such control.
var leftControls = []leftControl{
	{entry: "statement_timeout", named: condOperations, caught: func(o *leftOutcome) bool { return o.carries("57014") }},
	{entry: "lock_timeout", named: condOperations, caught: func(o *leftOutcome) bool { return o.carries("55P03") }},
	{entry: "idle_in_transaction_session_timeout", named: condOperations, caught: func(o *leftOutcome) bool {
		err := o.errs["Read"]
		code := sqlstate(err)
		return err != nil && (code == "" || code == "25P03")
	}},
	{entry: "exit_on_error", named: condPID, caught: func(*leftOutcome) bool { return true }},
	{entry: "client_encoding", named: condData, caught: func(*leftOutcome) bool { return true }, directOnly: true},
	{entry: "default_tablespace", named: condOperations, caught: func(o *leftOutcome) bool { return o.errs["CreateTables"] != nil }},
	{entry: "default_transaction_isolation", named: condIsolation, caught: func(*leftOutcome) bool { return true }},
	{entry: "role", named: condOperations, caught: func(o *leftOutcome) bool { return o.carries("42501") }},
	{fixed: unrepairableCase, named: condOperations, caught: func(o *leftOutcome) bool { return o.carries("42501") && o.failed(condClosed) }},
	{fixed: readOnlyCase, named: condOperations, caught: func(o *leftOutcome) bool {
		for _, write := range []string{"CreateTables", "Import", "Append", "Replace"} {
			if err, ran := o.errs[write]; ran && err == nil {
				return true
			}
		}
		return false
	}},
	{entry: "array_nulls", name: "pgx's default query mode", mode: pgx.QueryExecModeCacheStatement, named: condPID,
		caught: func(*leftOutcome) bool { return true }, pooledOnly: true},
}

// subtest runs a case as a subtest of t, which fails naming each condition that failed, and counts
// it, and whether it failed or skipped.
func (r *leftRun) subtest(t *testing.T, name string, run func(t *testing.T) *leftOutcome) {
	t.Run(name, func(t *testing.T) {
		r.cases++
		defer func() {
			if t.Failed() {
				r.failed++
			}
			if t.Skipped() {
				r.skipped++
			}
		}()
		o := run(t)
		t.Logf("left-setting case %s %q: %s", r.name, name, o.summary())
		for _, condition := range leftConditions {
			if failure := o.failure(condition); failure != "" {
				t.Errorf("%s: %s", condition, failure)
			}
		}
	})
}

// control runs a control as a subtest of t, which fails unless the control's named condition
// failed as the fault would, and counts it, and whether it was caught.
func (r *leftRun) control(t *testing.T, control leftControl, name string, run func(t *testing.T) *leftOutcome) {
	t.Run("control: "+name, func(t *testing.T) {
		r.controls++
		o := run(t)
		if o.failed(control.named) && control.caught(o) {
			r.caught++
			t.Logf("left-setting control %s %q caught, %s: %s", r.name, name, control.named, o.failure(control.named))
			return
		}
		runs := "the helper as it stood before the openings"
		if control.mode != 0 {
			runs = "the client"
		}
		t.Errorf("control: under %s %s passes the condition %s as the test reads it (%s), so the test cannot catch the fault it is for",
			name, runs, control.named, o.summary())
	})
}

// renew makes PgBouncer close the server connection of the pool leftset, on which the case before
// left what it left, so that the next case gets a new backend, which holds nothing of it: RECONNECT
// on the admin console marks it, and PgBouncer closes it with a Terminate rather than hand it out
// again ("obsolete connection", find_server in src/objects.c at pgbouncer_1_25_2). It answers what
// is wrong, "" for nothing: a client of the pool must then reach a new backend, and the old one
// must be gone from pg_stat_activity.
func (r *leftRun) renew(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	backend := func() (uint32, error) {
		conn, err := pgx.Connect(ctx, r.server.pooled)
		if err != nil {
			return 0, err
		}
		defer func() { _ = conn.Close(ctx) }()
		pid, _, err := backendReads(ctx, conn, "")
		return pid, err
	}
	old, err := backend()
	if err != nil {
		return fmt.Sprintf("read the pool's server connection before RECONNECT: %v", err)
	}
	if _, err := r.console.Exec(ctx, "RECONNECT "+poolSet); err != nil {
		return fmt.Sprintf("RECONNECT %s on PgBouncer's admin console: %v", poolSet, err)
	}
	renewed, err := backend()
	switch {
	case err != nil:
		return fmt.Sprintf("read the pool's server connection after RECONNECT: %v", err)
	case renewed == old:
		return fmt.Sprintf("after RECONNECT a client of the pool still reaches backend %d, which holds what the case before left", old)
	case !r.gone(t, old):
		return fmt.Sprintf("after RECONNECT backend %d, which holds what the case before left, is still in pg_stat_activity", old)
	}
	return ""
}

// run runs the run's cases (the entries it covers, then its fixed cases) and its controls, each a
// subtest of t, and at the end holds the log rule over what the server logged for the backends the
// client's transactions ran on (holdLog). Through PgBouncer, each subtest that left something on
// the pool leftset's one server connection is followed by renew, which closes it, so no case runs
// on what another left: a run whose renew fails stops there.
func (r *leftRun) run(t *testing.T, cases []leftCase, fixed []leftFixed, controls []leftControl) {
	t.Helper()
	after := func(renews bool) {
		if !r.pooled || !renews {
			return
		}
		if failure := r.renew(t); failure != "" {
			t.Fatalf("the pool's server connection cannot be renewed, so the next case would run on what the last left: %s", failure)
		}
	}
	for _, c := range cases {
		r.subtest(t, c.name, func(t *testing.T) *leftOutcome { return r.entryCase(t, c, false) })
		after(true)
	}
	for _, f := range fixed {
		r.subtest(t, f.name, f.run)
		after(f.renews)
	}
	for _, control := range controls {
		switch {
		case control.directOnly && r.pooled, control.pooledOnly && !r.pooled:
			continue
		case control.fixed == unrepairableCase:
			r.control(t, control, unrepairableCase, func(t *testing.T) *leftOutcome { return r.entryCase(t, r.unrepairable(), true) })
			after(true)
		case control.fixed == readOnlyCase:
			r.control(t, control, readOnlyCase, func(t *testing.T) *leftOutcome { return r.readOnlyDefault(t, true) })
		default:
			index := slices.IndexFunc(cases, func(c leftCase) bool { return c.entry == control.entry })
			if index < 0 {
				t.Errorf("control: the run has no case of %s, the entry the control leaves", control.entry)
				continue
			}
			c := cases[index]
			c.mode = control.mode
			r.control(t, control, cmp.Or(control.name, control.entry), func(t *testing.T) *leftOutcome { return r.entryCase(t, c, true) })
			after(true)
		}
	}
	r.holdLog(t)
}

// holdLog holds the server's log to naming the client on each of its statements, so that a reader
// of the log can tell them from any other client's: each statement of each transaction of the
// client shows legion-session-client (clientStatementsInLog over the backends the client's
// transactions ran on), and the log holds a statement for each opening the client sent at least, so
// it logged each of its transactions. It reads the log once a statement the watcher sent after the
// run is in it: the log collector writes what each backend sent it in order, and every backend of
// the client sent its lines before its answer, so by then the log holds them all.
func (r *leftRun) holdLog(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	mark := fmt.Sprintf("left-setting log mark %s %d", r.name, time.Now().UnixNano())
	if _, err := r.watcher.Exec(ctx, "SELECT '"+mark+"'"); err != nil {
		t.Fatalf("write the log's mark: %v", err)
	}
	var jsonlog []byte
	if !testwait.Holds(leftBound, 100*time.Millisecond, func() bool {
		var err error
		jsonlog, err = os.ReadFile(r.server.log)
		return err == nil && bytes.Contains(jsonlog, []byte(mark))
	}) {
		t.Fatalf("the server's log %s holds no line of the statement %q %s after it ran", r.server.log, mark, leftBound)
	}
	statements, other, problems := clientStatementsInLog(jsonlog, r.pids)
	r.statements, r.other = statements, len(other)
	t.Logf("left-setting %s log rule: %d statements of the client on %d backends, %d under another name, %d problems",
		r.name, statements, len(r.pids), len(other), len(problems))
	for _, entry := range slices.Concat(other, problems)[:min(20, len(other)+len(problems))] {
		t.Errorf("the server's log: %s", entry)
	}
	if statements < r.openings {
		t.Errorf("the server's log holds %d statements of the client, fewer than the %d openings it sent, so it did not log each of its "+
			"transactions on the backends the test read", statements, r.openings)
	}
}

// pgBouncerAnswer is PgBouncer's answer to SHOW VERSION on its admin console: PgBouncer
// <major>.<minor>.<patch> (PACKAGE_STRING, admin_show_version in src/admin.c).
var pgBouncerAnswer = regexp.MustCompile(`\APgBouncer ([0-9]+)\.([0-9]+)\.([0-9]+)\z`)

// pgBouncerVersion is PgBouncer's version as SHOW VERSION on its admin console answers it. The test
// fails on an answer of another shape, printing it, and at 1.26.0 or later, which resets
// default_transaction_read_only for each client and so would hide the fault the read-only cases are
// for (https://github.com/pgbouncer/pgbouncer/releases/tag/pgbouncer_1_26_0).
func pgBouncerVersion(t *testing.T, console *pgx.Conn) string {
	t.Helper()
	var answer string
	if err := console.QueryRow(context.Background(), "SHOW VERSION", pgx.QueryExecModeSimpleProtocol).Scan(&answer); err != nil {
		t.Fatalf("SHOW VERSION on PgBouncer's admin console: %v", err)
	}
	match := pgBouncerAnswer.FindStringSubmatch(answer)
	if match == nil {
		t.Fatalf("PgBouncer's admin console answered SHOW VERSION with %q, which does not read PgBouncer <major>.<minor>.<patch>", answer)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major > 1 || major == 1 && minor >= 26 {
		t.Fatalf("PgBouncer %s.%s.%s is 1.26.0 or later, which resets default_transaction_read_only for each client and so hides "+
			"the fault the read-only cases are for: run the test with a PgBouncer before 1.26.0 (PoolImage)", match[1], match[2], match[3])
	}
	return match[1] + "." + match[2] + "." + match[3]
}

// postgresVersion is the server's version, the first word of server_version. The test fails below
// PostgreSQL 16.4 (server_version_num 160004), the first 16 release with
// restrict_nonsystem_relation_kind, which every opening sets.
func postgresVersion(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var version string
	var number int
	if err := conn.QueryRow(context.Background(), "SELECT current_setting('server_version'), current_setting('server_version_num')::int").
		Scan(&version, &number); err != nil {
		t.Fatal(err)
	}
	if number < 160004 {
		t.Fatalf("the server is PostgreSQL %s, older than 16.4, the first 16 release with restrict_nonsystem_relation_kind", version)
	}
	return strings.Fields(version)[0]
}

// The client holds under each setting another client can leave on a server connection of the
// session database's pool, on PostgreSQL 16 directly and again through PgBouncer 1.25.2
// in transaction mode, each started by the test with docker (startLeftServers). Each run has one
// case for each entry of LeftSettings the server has, role and each temporary table included,
// which the server's own parameters of context user must not outgrow (userSettings); the pooled run
// leaves out the six parameters PgBouncer resets for each client (poolerResets). Each case leaves
// its entry with the entry's session-level statement on the server connection the client then gets
// and runs the client's operations on it (operate): Written with the tables missing, CreateTables,
// Import, Written, FindByID, Read, an Import PostgreSQL refuses, an Append while the admin holds
// the row, Replace and Read, each at a session of the case's own whose content holds a multi-byte
// character. It wants each condition to hold, each named when it fails: operations, data, temp,
// app, isolation and pid (leftConditions). Each run's fixed cases are an empty search_path, one
// server connection the client cannot repair, four in a row directly or a pool of four with three
// through PgBouncer, and the database's read-only default (fixedCases); and each run's count of
// cases must be its entries plus its fixed cases. Its controls run the helper as it stood before the
// openings (beforeOpenings) on the entries and the fixed cases it breaks, and, through PgBouncer,
// the client in pgx's default query mode on one entry, and each must fail the condition it names
// (leftControls).
// At the end of each run the server's jsonlog shows legion-session-client on every statement of
// every transaction of the client (holdLog). The test's last line is its summary, which CI's run
// of the test records.
//
// A pod has no docker, so there the test skips, saying so; under GITHUB_ACTIONS=true it fails
// instead (dockerFor). It reads no LEGION_TEST_* variable: it makes the servers it runs on.
func TestTheClientHoldsUnderEveryLeftSetting(t *testing.T) {
	docker := dockerFor(t, noDockerForLeftSettings)
	runLeftSettings(t, startLeftServers(t, docker))
}

// runLeftSettings is TestTheClientHoldsUnderEveryLeftSetting on server: the versions, the class,
// the direct run and the pooled run, their counts and the summary.
func runLeftSettings(t *testing.T, server leftServer) {
	maintenance := connectFor(t, server.maintenance)
	postgres := postgresVersion(t, maintenance)
	direct, pooled := newLeftRun(t, "direct", false, server), newLeftRun(t, "pooled", true, server)
	pgbouncer := pgBouncerVersion(t, pooled.console)
	has := userSettings(t, maintenance)
	for _, r := range []*leftRun{direct, pooled} {
		cases, lacks, resets := leftCases(has, r.pooled)
		fixed := r.fixedCases()
		t.Run(r.name, func(t *testing.T) {
			for _, name := range lacks {
				t.Logf("left-setting server lacks %s", name)
			}
			for _, name := range resets {
				t.Logf("left-setting pooler resets %s", name)
			}
			r.run(t, cases, fixed, leftControls)
		})
		if want := len(cases) + len(fixed); r.cases != want {
			t.Errorf("the %s run ran %d cases, want %d: %d entries the server has and %d fixed cases", r.name, r.cases, want, len(cases), len(fixed))
		}
	}
	if direct.controls+pooled.controls != direct.caught+pooled.caught {
		t.Errorf("of %d controls %d failed as named, so the test cannot catch each fault it is for", direct.controls+pooled.controls,
			direct.caught+pooled.caught)
	}
	t.Logf("left-setting summary postgresql=%s pgbouncer=%s direct cases=%d failed=%d skipped=%d statements=%d other-name=%d "+
		"pooled cases=%d failed=%d skipped=%d statements=%d other-name=%d controls=%d caught=%d", postgres, pgbouncer,
		direct.cases, direct.failed, direct.skipped, direct.statements, direct.other,
		pooled.cases, pooled.failed, pooled.skipped, pooled.statements, pooled.other,
		direct.controls+pooled.controls, direct.caught+pooled.caught)
}
