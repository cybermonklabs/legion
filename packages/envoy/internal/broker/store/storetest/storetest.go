// Package storetest gives each Postgres-backed broker test a fully migrated database of its own,
// waits for backends to queue behind a lock a test holds (AwaitLockWaiters), and writes the rows
// only a database writer other than the broker could (Exec, ForgeRequestSignature,
// CopyAsOtherKind), for the tests that pin what a release or an authentication refuses. It is an
// ordinary package rather than a _test.go file so every broker package can import it.
//
// URL clones each database from one migrated template, which the first test process to want it
// builds and every other shares, through the harness internal/testpg/pgtemplate implements for
// both stores: its package comment says how the template is named, built, copied and kept, and
// how a test's database is dropped. Migrate applies the whole set inside one transaction under a
// database-wide advisory lock (internal/broker/store), so tests that each migrated a schema of
// their own on one shared database both re-ran every migration and queued on that one lock,
// which was most of the Postgres work a broker test did (dispatch://INFRA-1075). A clone costs
// one create database. Nothing is handed from one test to the next: a test's database is dropped
// once the test ends, so no test can be given a database another test wrote in.
//
// A package whose tests call URL or Open uses Main as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }. Main waits for the tests' drops. A
// package that installs none still works: each call makes a database of its own, migrates it and
// drops it with the test, a whole migration run per test, as each test's own schema cost before
// this harness.
package storetest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/testpg"
	"github.com/sjawhar/envoy/internal/testpg/pgtemplate"
)

// DatabaseURLEnv names the Postgres server the broker's tests run against.
const DatabaseURLEnv = "BROKER_TEST_DATABASE_URL"

// harness clones every database URL hands out from the broker's template.
var harness = pgtemplate.New(pgtemplate.Broker, store.MigrationsFingerprint, migrate)

// migrate applies every migration to the database a build of the template runs in.
func migrate(ctx context.Context, url string) error {
	st, err := store.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("open the template database: %w", err)
	}
	defer st.Close()
	return st.Migrate(ctx)
}

// serverURL is the DatabaseURLEnv server every database here is made on, skipping t when the
// variable is unset: a run with no server is skipped rather than failed, which is the contract
// URL, Open and DatabaseURL all state.
func serverURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed broker tests")
	}
	return raw
}

// URL returns the DSN of a database of t's own, carrying every migration and nothing another test
// wrote, dropped when t ends. params are extra connection parameters as key=value (e.g.
// "pool_max_conns=2"). It skips t when DatabaseURLEnv is unset.
func URL(t testing.TB, params ...string) string {
	t.Helper()
	raw := serverURL(t)
	if !harness.InMain() {
		// Only Main waits for a clone's drop, so a package that installs none migrates a
		// database of its own instead.
		return ownMigratedURL(t, raw, params)
	}
	return withParams(harness.Clone(t, raw, "broker_test_"), params...)
}

// Open is store.Open on a fresh URL(t, params...), closed when t ends.
func Open(t testing.TB, params ...string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), URL(t, params...))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	// The store closes before the drop URL registered, which t.Cleanup's reverse order gives, so
	// the drop meets no session of this store's.
	t.Cleanup(st.Close)
	return st
}

// ownMigratedURL is what a package with no Main gets: a database of its own (testpg.DatabaseOn),
// migrated, and dropped with the test - a whole migration run per test, as each test's own schema
// cost before this harness.
func ownMigratedURL(t testing.TB, raw string, params []string) string {
	t.Helper()
	// The database is a file copy of template1 (docs/testing.md, "A database a Go test creates").
	dsn := withParams(testpg.DatabaseOn(t, raw, "broker_test"), params...)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	return dsn
}

// Main runs the package's tests and waits for their databases to drop (pgtemplate.Harness.Main).
func Main(m *testing.M) int { return harness.Main(m) }

// withParams is dsn carrying params as extra connection parameters, each given as key=value.
func withParams(dsn string, params ...string) string {
	if len(params) == 0 {
		return dsn
	}
	database, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, which carries a credential, so only
		// the error it wraps is printed.
		panic("storetest: parse a test database's URL: " + errors.Unwrap(err).Error())
	}
	query := database.Query()
	for _, param := range params {
		values, err := url.ParseQuery(param)
		if err != nil {
			panic("storetest: connection parameter " + param + ": " + err.Error())
		}
		for key := range values {
			query.Set(key, values.Get(key))
		}
	}
	database.RawQuery = query.Encode()
	return database.String()
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
// ends, and returns its DSN. URL remains the right helper for everything else: it hands out a
// database already carrying every migration. The DSN is unmigrated because that is what both
// callers need: a LISTEN/NOTIFY test, which reads no table at all, and a test that starts two
// broker processes against a database neither has migrated yet, which is the race those two
// processes are there to settle. A caller that wants tables runs store.Migrate on it itself.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	return testpg.DatabaseOn(t, serverURL(t), "broker_test")
}

// Querier is what AwaitLockWaiters polls through: a pool, or a connection of its own when the
// pool's connections may all be waiting.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AwaitLockWaiters waits until n backends wait on a lock behind holder's backend and answers their
// pids, failing t after ten seconds. A backend counts when it waits on a lock (wait_event_type
// 'Lock') that holder's backend holds, or that a backend already counted holds or is ahead of it
// in the queue for: a second waiter for one row waits on the first waiter's tuple lock, and
// pg_blocking_pids names only that first waiter, never the holder. Waiters behind other holders,
// in other tests on the same server, never count.
func AwaitLockWaiters(t testing.TB, q Querier, holder pgx.Tx, n int) []uint32 {
	t.Helper()
	ctx := context.Background()
	pid := holder.Conn().PgConn().PID()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting []int32
		if err := q.QueryRow(ctx, `with recursive waiting(pid) as (
				select pid from pg_stat_activity where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid))
				union
				select a.pid from pg_stat_activity a, waiting w where a.wait_event_type = 'Lock' and w.pid = any(pg_blocking_pids(a.pid))
			)
			select coalesce(array_agg(pid order by pid), '{}') from waiting`, int32(pid)).Scan(&waiting); err != nil {
			t.Fatalf("read the backends waiting behind %d: %v", pid, err)
		}
		if len(waiting) >= n {
			pids := make([]uint32, len(waiting))
			for i, w := range waiting {
				pids[i] = uint32(w)
			}
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waited on a lock behind %d, want %d", len(waiting), pid, n)
		}
	}
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
