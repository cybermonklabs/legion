// Package storetest gives each Postgres-backed broker test a migrated schema of its own on the
// BROKER_TEST_DATABASE_URL server, dropped when the test ends. Every broker package's tests run
// against one server at once, and the poller's passes act on every row of their tables, so tests
// that shared tables would act on each other's rows; with a schema per test no test can see
// another's rows, in its own package, a sibling package, or a concurrent run of either.
//
// The schemas of one test binary are made in a database of that binary's own
// (testdb.ProcessDatabaseOn), which Main drops after the binary's tests. Every migration takes the
// broker's advisory lock (store.go's lockAndPin), and Postgres holds an advisory lock for the
// whole database: with every package's schemas in one database, each migration, a broker's at
// its boot among them, waited for the migrations of every other package of the run and of every
// other run on the server. In a database of the binary's own it waits only for its own binary's
// tests. So a package that uses URL or Open runs its tests through Main.
//
// It also writes the rows only a database writer other than the broker could (Exec,
// ForgeRequestSignature, CopyAsOtherKind), for the tests that pin what a release or an
// authentication refuses. It is an ordinary package rather than a _test.go file so every broker
// package can import it.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
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

// randomName mints the name of the schema one test gets of its own. It is random rather than
// derived from t.Name() because two concurrent runs of one test - this package's tests and a
// sibling package's, against the one server - would otherwise mint the same name and act on each
// other's rows.
func randomName(t testing.TB) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	return "broker_test_" + hex.EncodeToString(suffix[:])
}

// URL creates a fresh schema in the test binary's own database on the DatabaseURLEnv server
// (testdb.ProcessDatabaseOn: the package comment says why), migrates it, drops it when t ends, and
// returns a connection string whose connections use that schema. params are extra connection
// parameters as key=value (e.g. "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset, and
// fails it when the package's TestMain does not run its tests through Main. The server's login
// needs CREATEDB.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	raw := testdb.ProcessDatabaseOn(t, serverURL(t), "broker_test_binary")
	ctx := context.Background()
	schema := randomName(t)
	admin(t, raw, "create schema "+schema)
	t.Cleanup(func() { admin(t, raw, "drop schema "+schema+" cascade") })

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	for _, param := range params {
		values, err := url.ParseQuery(param)
		if err != nil {
			t.Fatalf("connection parameter %q: %v", param, err)
		}
		for key := range values {
			query.Set(key, values.Get(key))
		}
	}
	parsed.RawQuery = query.Encode()
	dsn := parsed.String()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test schema: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return dsn
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

// Main runs the package's tests and then drops the database URL made their schemas in. A package
// whose tests call URL or Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return testdb.Main(m) }

// Run is Main for a package whose TestMain also runs another harness's Main, or runs its tests
// more than once (testdb.Run).
func Run(run func() int) int { return testdb.Run(run) }

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
// ends, and returns its DSN. URL remains the right helper for everything else: a schema is
// cheaper than a database and isolates rows just as well. A test needs a database of its own
// only when what it exercises is per-database rather than per-schema, and two things are.
// LISTEN/NOTIFY is one: a channel name is a database-wide identifier, so every schema on one
// server hears every other's notifications on the one channel constant a feature listens to, and
// a test on a schema would hear a sibling test's sends. The broker's advisory locks are the
// other: an advisory lock's key is database-wide, so two processes migrating their own schemas of
// one database still serialize on Migrate's lock, and a test that holds Migrate's lock or a
// serving beat's lock (enroll's servingLock) while it waits holds up every other test's broker on
// that database for as long.
//
// The DSN is unmigrated because some callers need it so: a LISTEN/NOTIFY test, which reads no
// table at all, and a test that starts two broker processes against a database neither has
// migrated yet, which is the race those two processes are there to settle. A caller that wants
// tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	return testdb.DatabaseOn(t, serverURL(t), "broker_test")
}

func admin(t testing.TB, databaseURL, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", DatabaseURLEnv, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
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
