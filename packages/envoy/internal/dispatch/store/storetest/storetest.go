// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database, and
// waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an ordinary
// package rather than a _test.go file so every package whose tests need a Dispatch database can
// import it; Go cannot share test-only code across packages.
//
// Open clones each database from one migrated template, which the first test process to want it
// builds and every other shares, through the harness internal/testpg/pgtemplate implements for
// both stores: its package comment says how the template is named, built, copied and kept, and
// how a test's database is dropped. Running every migration in its declared execution mode is
// most of the Postgres work a test would otherwise do, so a package's wall time would scale with
// its test count times the speed of the Postgres it runs against; a clone costs one create
// database. Nothing is handed from one test to the next: a test's database is dropped when that
// test ends, so no test can be given a database another test wrote in.
package storetest

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/pgtemplate"
)

// DatabaseURLEnv names the Postgres server the tests run against. Its database path is ignored:
// the admin connection uses the postgres database, and every test gets a database of its own.
const DatabaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

// testPoolSize bounds the shared pool of every store Open hands a test, in place of production's
// sharedPoolSize. A store per test database against one Postgres is what the harness holds, and
// that server bounds them all: the Postgres services of .github/workflows/ci.yaml start with
// max_connections=500, less the 3 slots it reserves for superusers, and the envoy-go job's unit
// call runs eight package binaries at once, each running up to twelve of its tests at once
// (`go test -p 8 -parallel 12`), so the live stores are many, not one. Sixteen is above what any
// one test is known to hold at once: the deepest lock-wait rig queues three writers behind a held
// transaction while the rest of the pool serves the test's reads (WaitForLockWaiters's callers).
// The pools Pool.separate opens - the rooms, lease, outbox, listener, health, compaction and
// horizon pools - keep their own sizes, so this bounds the shared pool alone.
//
// It is not a free knob: a test writes against the number. Halving it to eight failed
// TestConcurrentSendsUnderOneKeyMakeOneMessage (internal/dispatch/api), which holds a table lock
// and waits for the other fifteen connections of this pool to queue behind it. Lowering it means
// reading that test with it.
const testPoolSize = 16

// harness clones every database Open hands out from Dispatch's template. The store package's own
// tests hold a harness of their own on the same template (pgtemplate.Dispatch), since they cannot
// import this package, which imports theirs.
var harness = pgtemplate.New(pgtemplate.Dispatch, store.MigrationsFingerprint, migrate)

// migrate applies every migration to the database a build of the template runs in.
func migrate(ctx context.Context, url string) error {
	database, err := store.Open(ctx, url, store.WithMaxConns(testPoolSize))
	if err != nil {
		return fmt.Errorf("open the template database: %w", err)
	}
	defer database.Pool.Close()
	return database.Migrate(ctx)
}

// Open returns a store on a new database carrying every migration, dropped after t ends. It skips
// t when DISPATCH_TEST_DATABASE_URL is unset.
func Open(t testing.TB) *store.Store {
	t.Helper()
	if !harness.InMain() {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }")
	}
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	database, err := store.Open(context.Background(), harness.Clone(t, raw, "dispatch_test_"), store.WithMaxConns(testPoolSize))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	// Registered after the clone's drop, so it runs first (t.Cleanup's reverse order): the drop
	// meets no session of this store's.
	t.Cleanup(database.Pool.Close)
	return database
}

// Main runs the package's tests and waits for their databases to drop (pgtemplate.Harness.Main).
// A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return harness.Main(m) }
