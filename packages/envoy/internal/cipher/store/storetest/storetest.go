// Package storetest gives each Postgres-backed broker test a migrated database of its own on the
// BROKER_TEST_DATABASE_URL server, dropped when the test ends. Every broker package's tests run
// against one server at once, and the poller's passes act on every row of their tables, so tests
// that shared tables would act on each other's rows; with a database per test no test can see
// another's rows, in its own package, a sibling package, or a concurrent run of either.
//
// Each test's database is a file copy of a template migrated once per migration set on the
// server (template), so a test pays one create database, not the broker's whole migration set.
// A database of its own also gives each test its own copy of the broker's database-wide advisory
// locks: Migrate's lock (store.go's lockAndPin) and the serving beat's (enroll's servingLock). In
// one shared database, every test's migration, every broker's boot and every beat waited for those
// of every other test of every package of the run, and the tests of a package could not run at
// once.
//
// Dispatch's storetest and the daemon's testpg each make a template of the test binary's own and
// drop it, and their clones, in the package's TestMain. This harness differs in three ways:
//   - The template is the server's, not the binary's, and nothing drops it: every broker test
//     binary of a run, and of every later run of the same migration set on that server, copies the
//     one template, so no binary owns it, and none could drop it while another copies it. CI gives
//     each job a server of its own. A server that outlives runs (a dev box's) keeps one template per
//     migration set, which drop database reclaims when no run is using it. The name comes from the
//     embedded migration files alone (store.MigrationsDigest), so a change to how Migrate applies
//     them that leaves the files as they are reuses the template the earlier code built; drop it
//     there to build it again.
//   - A test drops its own database in its own cleanup and waits for the drop's checkpoint, where
//     Dispatch's drops run in the background and its TestMain waits for them: no package of the
//     broker needs a TestMain for its databases, so nothing would wait for a drop left running.
//   - Each create and drop opens a connection of its own (testdb.DatabaseOn), so no pool is held
//     that a TestMain would have to close.
//
// A test binary that -timeout ends leaves the databases of the tests it was running behind
// (broker_test_<hex>), as every harness here does on a kill; on a long-lived server they are
// dropped by hand when no run is going (docs/testing.md, "A database a Go test creates").
//
// It also writes the rows only a database writer other than the broker could (Exec,
// ForgeRequestSignature, CopyAsOtherKind), for the tests that pin what a release or an
// authentication refuses. It is an ordinary package rather than a _test.go file so every broker
// package can import it.
package storetest

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

// serverURL is the DatabaseURLEnv server every schema and database here is made on, skipping t
// when the variable is unset: a run with no server is skipped rather than failed, which is the
// contract URL, Open and DatabaseURL all state.
func serverURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed broker tests")
	}
	return raw
}

// testSchema is the schema the template's tables are migrated into, and every clone's connection
// string names as its search_path, as each test's own schema was named before.
const testSchema = "broker_test"

// templates is the template each server's migration set was found or made under, by the server's
// URL, so a test process asks the server once.
var templates sync.Map

type templateDatabase struct {
	once sync.Once
	name string
	err  error
}

// templateLockKey is the session advisory lock under which a process looks for the template and
// makes it when it is missing: every broker test binary of a run, and of every other run on the
// server, asks at once. Postgres keeps an advisory lock per database, so the lock is taken on a
// connection to the server's postgres database (lockDatabase), whatever database the server's URL
// names: two binaries that name the same server through different databases take the one lock,
// and the sweep of unfinished builds below, which drops every build it finds, never drops one that
// another process is building.
const templateLockKey = "envoy broker storetest template"

// lockDatabase is the database of the server templateLockKey is taken in: one every Postgres
// server has, which the template is never copied from (a database a session is connected to
// cannot be a template's source, so template1 would be the wrong one).
const lockDatabase = "postgres"

// template is the name of the database every test's own database is copied from: the server's
// broker_template_<16 hex digits of store.MigrationsDigest>, migrated once by the first process to
// ask and left on the server for the next run of the same migration set. Under templateLockKey it
// uses that database when it exists; otherwise it drops what a process that died while building
// left, builds the template under a name of its own, migrates it and closes it to connections
// (a database anyone is connected to cannot be copied), and only then renames it, so a database of
// the template's name is always a whole one.
func template(t testing.TB, raw string) string {
	t.Helper()
	entry, _ := templates.LoadOrStore(raw, &templateDatabase{})
	tmpl := entry.(*templateDatabase)
	tmpl.once.Do(func() { tmpl.name, tmpl.err = makeTemplate(raw) })
	if tmpl.err != nil {
		t.Fatalf("broker test template database: %v", tmpl.err)
	}
	return tmpl.name
}

func makeTemplate(raw string) (string, error) {
	digest, err := store.MigrationsDigest()
	if err != nil {
		return "", err
	}
	name := "broker_template_" + digest[:16]
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		return "", fmt.Errorf("%s does not parse", DatabaseURLEnv)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testdb.DatabaseURL(*server, lockDatabase))
	if err != nil {
		return "", fmt.Errorf("connect to the %s database of %s: %w", lockDatabase, DatabaseURLEnv, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "select pg_advisory_lock(hashtext($1))", templateLockKey); err != nil {
		return "", fmt.Errorf("take the template lock: %w", err)
	}
	// The lock is the connection's: closing the connection releases it whatever happens below.
	var exists bool
	if err := conn.QueryRow(ctx, "select exists (select from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look for the template: %w", err)
	}
	if exists {
		return name, nil
	}
	// Builds only happen under the lock, which is the server's, so any build database on the server
	// now is one whose process died before it renamed it.
	rows, err := conn.Query(ctx, "select datname from pg_database where datname like 'broker_template_build_%'")
	if err != nil {
		return "", fmt.Errorf("list unfinished template builds: %w", err)
	}
	leftovers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("list unfinished template builds: %w", err)
	}
	for _, leftover := range leftovers {
		if _, err := conn.Exec(ctx, "drop database if exists "+pgx.Identifier{leftover}.Sanitize()+" with (force)"); err != nil {
			return "", fmt.Errorf("drop the unfinished template build %s: %w", leftover, err)
		}
	}
	build, err := testdb.NewDatabaseName("broker_template_build")
	if err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, "create database "+build+" strategy file_copy"); err != nil {
		return "", fmt.Errorf("create the template build: %w", err)
	}
	// The migrations run as production's do, with JIT off. The copies of the template take the
	// setting themselves (testdb.DatabaseOn): a copy does not keep its template's settings.
	if _, err := conn.Exec(ctx, testdb.WithoutJIT(build)); err != nil {
		return "", fmt.Errorf("turn JIT off in the template build: %w", err)
	}
	if err := migrateTemplate(ctx, server, build); err != nil {
		return "", err
	}
	for _, statement := range []string{
		"select pg_terminate_backend(pid) from pg_stat_activity where datname = '" + build + "' and pid <> pg_backend_pid()",
		"alter database " + build + " with allow_connections false",
		"alter database " + build + " rename to " + name,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return "", fmt.Errorf("finish the template (%s): %w", statement, err)
		}
	}
	return name, nil
}

// migrateTemplate makes testSchema in the database build and migrates it there, closing every
// connection it opened before it returns.
func migrateTemplate(ctx context.Context, server *url.URL, build string) error {
	conn, err := pgx.Connect(ctx, testdb.DatabaseURL(*server, build))
	if err != nil {
		return fmt.Errorf("connect to the template build: %w", err)
	}
	_, err = conn.Exec(ctx, "create schema "+testSchema)
	conn.Close(ctx)
	if err != nil {
		return fmt.Errorf("create the template's schema: %w", err)
	}
	st, err := store.Open(ctx, testdb.DatabaseURL(*server, build, "search_path="+testSchema))
	if err != nil {
		return fmt.Errorf("open the template build: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate the template build: %w", err)
	}
	return nil
}

// URL creates a database of the test's own on the DatabaseURLEnv server, a file copy of the
// migrated template (template), drops it when t ends, and returns a connection string whose
// connections use its migrated schema (testdb.DatabaseOn). params are extra connection parameters
// as raw query pairs, key=value (e.g. "pool_max_conns=2"), each replacing any pair of its key the
// server's URL holds. It skips t when DatabaseURLEnv is unset. The server's login needs CREATEDB.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	raw := serverURL(t)
	set := []string{"search_path=" + testSchema}
	for _, param := range params {
		if _, err := url.ParseQuery(param); err != nil {
			t.Fatalf("connection parameter %q: %v", param, err)
		}
		set = append(set, param)
	}
	return testdb.DatabaseOn(t, raw, template(t, raw), "broker_test", set...)
}

// Open is store.Open on a fresh URL(t, params...), closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), URL(t, params...))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// RepeatableReadByDefault opens a second pool on st's database whose connections begin every
// transaction at REPEATABLE READ unless it names a level (default_transaction_isolation), closed
// when t ends. A transaction that asks for READ COMMITTED by name reads alike on either pool, so a
// test that runs the code under test on this one fails once the name is taken away, which on the
// server's own default, READ COMMITTED, it would not.
func RepeatableReadByDefault(t testing.TB, st *store.Store) *pgxpool.Pool {
	t.Helper()
	config := st.Pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a REPEATABLE READ pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var level string
	if err := pool.QueryRow(context.Background(), `show transaction_isolation`).Scan(&level); err != nil || level != "repeatable read" {
		t.Fatalf("the REPEATABLE READ pool's transactions run at %q, %v; want repeatable read, or the test that uses it proves nothing", level, err)
	}
	return pool
}

// PerPodLoginSchema applies by hand the one statement the release after this one migrates:
// dropping launcher_credentials_live_subject, 0015's one-live-credential-per-subject index,
// which migration 0029 deliberately leaves in place so that a roll of the broker serves the
// previous generation and this one against one database (0029's own comment). Until it is
// dropped a subject holds one live credential whatever its pod, which is what this release
// ships; a test that drives the per-pod machine login calls this first, so it runs against the
// schema the next release leaves, which enroll.Service.IssueTokenLoginTx reads for itself.
func PerPodLoginSchema(t testing.TB, st *store.Store) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), `drop index if exists launcher_credentials_live_subject`); err != nil {
		t.Fatalf("drop launcher_credentials_live_subject: %v", err)
	}
}

// DatabaseURL creates a fresh, UNMIGRATED database on the DatabaseURLEnv server, drops it when t
// ends, and returns its DSN. URL gives each test a database of its own too, already migrated;
// DatabaseURL is for a test that needs one nothing has migrated: a LISTEN/NOTIFY test, which reads
// no table at all, and a test that starts two broker processes against a database neither has
// migrated yet, which is the race those two processes are there to settle. A caller that wants
// tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	return testdb.DatabaseOn(t, serverURL(t), "", "broker_test")
}

// Querier is what AwaitLockWaiters and LockWaitersBehind read through: a pool, or a connection of
// its own when the pool's connections may all be waiting.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AwaitLockWaiters waits until n backends wait on a lock behind holder's backend and answers their
// pids. It waits through testwait.Until, so its one bound is the test binary's deadline less
// testwait's margin, and a wait that gives up prints the waiters it read last. A backend counts
// when it waits on a lock (wait_event_type 'Lock') that holder's backend holds, or that a backend
// already counted holds or is ahead of it in the queue for: a second waiter for one row waits on
// the first waiter's tuple lock, and pg_blocking_pids names only that first waiter, never the
// holder. Waiters behind other holders, in other tests on the same server, never count.
func AwaitLockWaiters(t *testing.T, q Querier, holder pgx.Tx, n int) []uint32 {
	t.Helper()
	pid := holder.Conn().PgConn().PID()
	waiting := testwait.Until(t, fmt.Sprintf("%d backends to wait on a lock behind %d", n, pid), func() ([]int32, bool) {
		waiting := LockWaitersBehind(t, q, pid)
		return waiting, len(waiting) >= n
	})
	pids := make([]uint32, len(waiting))
	for i, w := range waiting {
		pids[i] = uint32(w)
	}
	return pids
}

// LockWaitersBehind is every backend that waits on a lock behind pid's backend, read once and in
// pid order: one waiting on a lock pid's backend holds, or on one a backend already counted holds
// or is ahead of it for, as AwaitLockWaiters counts them. It is the read AwaitLockWaiters polls,
// for a test whose other way out is the waiter returning, which AwaitLockWaiters would fail after
// ten seconds with no waiter.
func LockWaitersBehind(t testing.TB, q Querier, pid uint32) []int32 {
	t.Helper()
	var waiting []int32
	if err := q.QueryRow(context.Background(), `with recursive waiting(pid) as (
			select pid from pg_stat_activity where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid))
			union
			select a.pid from pg_stat_activity a, waiting w where a.wait_event_type = 'Lock' and w.pid = any(pg_blocking_pids(a.pid))
		)
		select coalesce(array_agg(pid order by pid), '{}') from waiting`, int32(pid)).Scan(&waiting); err != nil {
		t.Fatalf("read the backends waiting behind %d: %v", pid, err)
	}
	return waiting
}

// Exec is a tamper that runs statements as a writer other than the broker would: $1 is the
// record id wherever a statement names it, and an update must change exactly one row.
func Exec(statements ...string) func(t testing.TB, st *store.Store, recordID string) {
	return func(t testing.TB, st *store.Store, recordID string) {
		t.Helper()
		for _, statement := range statements {
			var args []any
			if strings.Contains(statement, "$1") {
				args = []any{recordID}
			}
			tag, err := st.Pool.Exec(context.Background(), statement, args...)
			if err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
			if strings.HasPrefix(statement, "update") && tag.RowsAffected() != 1 {
				t.Fatalf("%s changed %d rows, want 1", statement, tag.RowsAffected())
			}
		}
	}
}

// ForgeRequestSignature writes a copy of record recordID whose embedded request object carries an
// altered signature, under the id its own body hashes to (the append-only trigger refuses updates,
// not inserts), with an approved event by the record's own approver, and returns the copy's id.
// Every link of that chain but the requester's signature holds, so only the request object's
// re-verification can refuse it.
func ForgeRequestSignature(t testing.TB, st *store.Store, recordID string) string {
	t.Helper()
	body, kind := readRecord(t, st, recordID)
	parts := strings.Split(body.Request, ".")
	if len(parts) != 3 {
		t.Fatalf("request object has %d parts, want 3", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 {
		t.Fatalf("decode request object signature: %v", err)
	}
	signature[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	body.Request = strings.Join(parts, ".")
	return insertApprovedCopy(t, st, recordID, body, kind)
}

// CopyAsOtherKind writes a copy of record recordID under the other record kind ('agent_secret'
// for a 'launcher_credential' record, and the reverse), its body differing only in an expiry one
// second later so it hashes to an id of its own, with an approved event by the record's own
// approver, and returns the copy's id. Every link of that chain holds but the kind, so only a
// chain verifier's scope to its own kind can refuse it.
func CopyAsOtherKind(t testing.TB, st *store.Store, recordID string) string {
	t.Helper()
	body, kind := readRecord(t, st, recordID)
	body.ExpiresAt = body.ExpiresAt.Add(time.Second)
	other := "agent_secret"
	if kind == other {
		other = "launcher_credential"
	}
	return insertApprovedCopy(t, st, recordID, body, other)
}

func readRecord(t testing.TB, st *store.Store, recordID string) (record.Body, string) {
	t.Helper()
	var canonical, kind string
	if err := st.Pool.QueryRow(context.Background(), `select body, kind from credential_requests where id=$1`, recordID).Scan(&canonical, &kind); err != nil {
		t.Fatalf("read record %s: %v", recordID, err)
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		t.Fatalf("parse record %s: %v", recordID, err)
	}
	return body, kind
}

// insertApprovedCopy writes body as a record of kind under the id it hashes to, its other columns
// copied from recordID, with an approved event by the body's approver, and returns the copy's id.
func insertApprovedCopy(t testing.TB, st *store.Store, recordID string, body record.Body, kind string) string {
	t.Helper()
	ctx := context.Background()
	copyID := body.ID()
	if _, err := st.Pool.Exec(ctx, `insert into credential_requests (id, body, kind, approver, enrollment_id, code, created_at, expires_at)
		select $2, $3, $4, approver, enrollment_id, code, created_at, expires_at from credential_requests where id=$1`,
		recordID, copyID, body.Canonical(), kind); err != nil {
		t.Fatalf("insert record copy: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, login, actor) values ($1, 'approved', $2, $3)`,
		copyID, body.Approver, record.HumanActor(body.Approver)); err != nil {
		t.Fatalf("insert record copy's approval: %v", err)
	}
	return copyID
}
