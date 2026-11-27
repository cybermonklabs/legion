// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database, and
// waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an ordinary
// package rather than a _test.go file so every package whose tests need a Dispatch database can
// import it; Go cannot share test-only code across packages.
//
// Open clones each database from a template this test process migrates once. Running every
// migration in its declared execution mode is most of the Postgres work a test would
// otherwise do, so a package's wall time would scale with its test count times the speed of the
// Postgres it runs against; a clone costs one create database. Open's template is one of the
// templates OpenFrom makes, each once per key by the first test that asks for it: Open's key is
// openKey, and this tree's runner migrates it.
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead: 6.4 MB of the
// 12.7 MB template per clone, 16 GiB over one run of the api and docs packages. A file copy writes
// a few hundred bytes of log; Postgres forces a checkpoint before and after the copy.
//
// The harness changes no setting of the server: fsync, full_page_writes and max_wal_size belong
// to the whole server DatabaseURLEnv names, which can hold other databases (the one
// packages/envoy/scripts/dev-postgres.sh runs holds the local Dispatch's), and a change to them
// outlives the run. It sets one setting of each database it makes, which goes with the database:
// JIT is off (testdb.WithoutJIT), as on production's database.
//
// A test's database is not dropped when the test ends. Dropping a database takes a transaction id
// when it deletes the database's row and keeps it while it forces a checkpoint and waits for it,
// and every snapshot on the server is instance-wide: the live readers' event horizon
// (events.Horizon) waits for every transaction id open on the server, in any database, so a drop
// running while another test of the package watches a live view holds that view's frames for as
// long as the checkpoint takes, which under a loaded box passed the 30 seconds a test waits. So a
// database whose test ended waits, with the others, until a test asks for a database while no
// test of the package holds one, and dropBatch of them are waiting: that test drops them all
// before its own is made, and no other test can make one meanwhile (held). Main drops the rest
// once the package's tests end. Drops made together share one checkpoint, so the drops add one
// checkpoint for every dropBatch tests to the package's clock, and the disk holds at most
// dropBatch spare databases, plus those of the subtests of a test that still holds its own.
//
// The harness batches drops, not creates. A create takes a transaction id the same way, when it
// inserts the new database's row, and keeps it across both checkpoints a file copy forces, and
// creates run side by side with the package's other tests (hold runs create outside held's lock):
// so each test's create adds two checkpoints to the package's clock, and holds the live views of
// the tests running beside it while they run.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// DatabaseURLEnv names the Postgres server the tests run against. Its database path is ignored:
// the admin connection uses the postgres database, and every test gets a database of its own.
const DatabaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

var shared struct {
	// main is set by Run before the package's tests run; Open refuses to run without it, since
	// only Run drops the templates and waits for the per-test drops.
	main   bool
	once   sync.Once
	err    error
	server url.URL
	admin  *pgxpool.Pool

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
	// dropping is each drop not yet ended, by its database, with the statement it runs or last
	// ran (goDrop, teardownExec): what Run names at its bound.
	dropping map[string]string
}

// openKey is the key of the template Open clones, which this tree's runner migrates
// (migrateWithThisTree).
const openKey = "this tree's runner (storetest.Open)"

// Open returns a store on a new database carrying every migration, dropped after t ends. It skips
// t when DISPATCH_TEST_DATABASE_URL is unset.
func Open(t testing.TB) *store.Store {
	t.Helper()
	return OpenFrom(t, openKey, migrateWithThisTree)
}

// migrateWithThisTree migrates the database databaseURL names with this tree's runner: Open's
// template. It closes its pool before it returns, so the template is left with no connection.
func migrateWithThisTree(databaseURL string) error {
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	return database.Migrate(ctx)
}

// ready makes sure the package's TestMain runs Run, skips t when DISPATCH_TEST_DATABASE_URL is
// unset, and opens the admin pool, once. It makes no template: each is made by the first test that
// asks for it (OpenFrom). The server's URL is kept as DISPATCH_TEST_DATABASE_URL names it, with no
// bound of this package's (shared.server), so no connect inherits the bound of the test that opened
// the pool. Each connect string this package hands out instead takes, when it is made, the seconds
// left until the test binary's deadline less testwait's margin (testdb.WithConnectDeadline), not
// pgx's default (two minutes in a pool, no bound through pgx.Connect): a clone's store, Empty's
// URL, and the URL each template's migrate is given, made by the test that asks for the template
// when it asks (OpenFrom). The admin pool's connections take that bound as each is made
// (openAdmin). A pool opened from such a string measures its bound from when the string was made:
// a connection it makes s seconds later can end s seconds past the binary's deadline less the
// margin, which a migrate, made and run at once, and a test's own store keep within the margin
// unless the test runs on for longer than it. The admin URL is read before the Once, so a URL that
// cannot be read fails each test that asks, and never leaves the Once done with no admin pool for
// a later test to call.
func ready(t testing.TB) {
	t.Helper()
	if !shared.main {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }, or storetest.Run around another harness's Main")
	}
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	if _, err := url.Parse(raw); err != nil {
		// url.Parse's own error quotes the URL it was given, which carries a credential.
		t.Fatalf("parse %s: %v", DatabaseURLEnv, errors.Unwrap(err))
	}
	end, bounded := testdb.ConnectEnd(t)
	shared.once.Do(func() { shared.err = openAdmin(raw, end, bounded) })
	if shared.err != nil {
		t.Fatalf("open the admin pool: %v", shared.err)
	}
}

// held is every database of this package's tests that is not yet dropped: live counts the ones
// whose test has not ended, or is still being made, and finished names the rest. A batch drop
// runs only while live is 0, under held's lock, so no test makes a database, and so none watches
// a live view of one, while a drop runs.
var held struct {
	sync.Mutex
	live     int
	finished []string
}

// dropBatch is how many finished databases wait before a test that finds no other test holding a
// database drops them (the package comment says why). A variable so the package's own test can
// make every finished database drop at once.
var dropBatch = 32

// hold makes the database name with create as one of a test that has not ended, first dropping
// the finished ones if no test of the package holds a database and dropBatch of them wait, and
// marks it finished once t ends. create runs outside held's lock, with live counting it, so tests
// make their databases side by side and no batch drop starts meanwhile.
func hold(t testing.TB, name string, create func() error) error {
	held.Lock()
	if held.live == 0 && len(held.finished) >= dropBatch {
		dropNow(held.finished)
		held.finished = nil
	}
	held.live++
	held.Unlock()
	if err := create(); err != nil {
		held.Lock()
		held.live--
		held.Unlock()
		return err
	}
	t.Cleanup(func() {
		held.Lock()
		defer held.Unlock()
		held.live--
		held.finished = append(held.finished, name)
	})
	return nil
}

// dropNow drops names together and waits for them.
func dropNow(names []string) {
	for _, name := range names {
		dropDatabase(name)
	}
	shared.drops.Wait()
}

// clone returns a store on a new database cloned from template, dropped once no test of the
// package holds a database after t ends (hold).
func clone(t testing.TB, template string) *store.Store {
	t.Helper()
	name := randomName(t, "dispatch_test_")
	if err := hold(t, name, func() error {
		_, err := shared.admin.Exec(ctx, cloneStatement(name, template))
		return err
	}); err != nil {
		t.Fatalf("clone isolated database from template %s: %v", template, err)
	}
	database, err := store.Open(ctx, databaseURL(shared.server, name))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	testcleanup.Add(t, database.Pool.Close)
	return database
}

// Empty returns the connection string of a new database that holds nothing, not even the
// version table, dropped once no test of the package holds a database after t ends (hold): for a
// test that migrates it itself, as the two-build suite has the builds it runs migrate theirs, in
// production's order. It makes no template.
func Empty(t testing.TB) string {
	t.Helper()
	ready(t)
	name := randomName(t, "dispatch_test_")
	if err := hold(t, name, func() error {
		_, err := shared.admin.Exec(context.Background(), "create database "+name+" strategy file_copy")
		return err
	}); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	return databaseURL(shared.server, name)
}

// templates are the templates this test binary made, by key, each once: Open's (openKey) and each
// OpenFrom caller's. Run drops every one of them.
var templates = struct {
	sync.Mutex
	byKey map[string]*templateDatabase
}{byKey: map[string]*templateDatabase{}}

// templateDatabase is one template: once makes it, name is its database, and err is why it could
// not be made.
type templateDatabase struct {
	once sync.Once
	name string
	err  error
}

// OpenFrom is Open for a database migrate brings to its schema instead of this tree's runner
// alone: it returns a store on a new database cloned from the template of key, dropped after t
// ends. The template is made once per key in the test binary, by the first test that asks: a new
// empty database whose connection string migrate is given, which migrate migrates and leaves
// with no connection open. That string's connect bound is the seconds left, when the template is
// made, until the binary's deadline less testwait's margin (testdb.WithConnectDeadline with the
// test that asks), so a template first asked for long after the admin pool opened still fails on
// pgx's error inside the margin. A template whose migrate failed, or ended its test, fails every
// test that asks for it again.
func OpenFrom(t testing.TB, key string, migrate func(databaseURL string) error) *store.Store {
	t.Helper()
	ready(t)
	templates.Lock()
	entry := templates.byKey[key]
	if entry == nil {
		entry = &templateDatabase{}
		templates.byKey[key] = entry
	}
	templates.Unlock()
	entry.once.Do(func() {
		// Set first, and cleared only once migrate returns, so a migrate that ends its test
		// (t.Fatal) leaves a template no later test clones.
		entry.err = fmt.Errorf("the test that made it ended before its migrate returned")
		suffix, err := randomSuffix()
		if err != nil {
			entry.err = err
			return
		}
		name := "dispatch_template_" + suffix
		// The template is a file copy of template1, for the reason each clone of it is one.
		if err := adminExec(t, "create database "+name+" strategy file_copy"); err != nil {
			entry.err = fmt.Errorf("create template database: %w", err)
			return
		}
		// Named once it exists, so Run drops exactly the templates that were made.
		entry.name = name
		if err := adminExec(t, testdb.WithoutJIT(name)); err != nil {
			entry.err = fmt.Errorf("turn JIT off on the template database: %w", err)
			return
		}
		if err := migrate(testdb.WithConnectDeadline(t, databaseURL(shared.server, entry.name))); err != nil {
			entry.err = fmt.Errorf("migrate template database: %w", err)
			return
		}
		// Postgres refuses to clone a database another session is connected to. Refusing
		// connections to the template keeps any stray session from making a clone wait or fail.
		if err := adminExec(t, "alter database "+entry.name+" with allow_connections false"); err != nil {
			entry.err = fmt.Errorf("close template database to connections: %w", err)
			return
		}
		entry.err = nil
	})
	if entry.err != nil {
		t.Fatalf("prepare template database %q: %v", key, entry.err)
	}
	return clone(t, entry.name)
}

// cloneStatement returns the create database that makes name a copy of template's files rather
// than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name, template string) string {
	return "create database " + name + " template " + template + " strategy file_copy"
}

// Main runs the package's tests, drops their databases, then drops every template they were
// cloned from. A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose tests also need another harness's Main, which runs m.Run
// itself: run runs the package's tests through that Main, and Run then does what Main does after
// them. internal/dispatch/delivery shares one NATS server across its tests (testnats.Main):
// func TestMain(m *testing.M) { os.Exit(storetest.Run(func() int { return testnats.Main(m) })) }.
//
// Its wait for the drops is bounded, as testdb.Run's is and for its reasons
// (testdb.TeardownDeadline: testdb.TeardownGrace past the -timeout, counted from the start of the
// tests, and five minutes from their end under -timeout 0). go test's alarm stops when the tests
// return, so a wait with no bound for a drop the server never answers held the binary until
// cmd/go's kill ("ran too long", which names no drop and no database), and for ever under
// -timeout 0 (TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered). No drop has a deadline of its
// own: a cancelled drop leaves its database on the server, as testdb.Run says. The templates'
// drops start as soon as the tests return, beside the drops their cleanups started: a clone is a
// database of its own, so no template's drop waits on one. One wait, under the one bound, covers
// them all, so at the bound Run names every database whose drop is still in flight, templates
// among them, with the statement it ran; a template's drop that could start only after the
// clones' left every template unnamed and its drop unsent once a clone's drop reached the bound.
// It then reads once what the server's backends wait on (testdb.WaitingOn), and returns the tests'
// exit code with the failures of the drops that ended, since every test has reported by then. It
// leaves the admin pool open, because closing it waits for the connections those drops hold; the
// process's exit closes them, and a database whose drop the server never read stays on the server
// under the name Run printed.
func Run(run func() int) int {
	shared.main = true
	started := time.Now()
	code := run()
	if shared.admin == nil {
		return code
	}
	defer shared.admin.Close()
	held.Lock()
	dropNow(held.finished)
	held.finished = nil
	held.Unlock()
	shared.drops.Wait()
	for _, failure := range shared.dropFails {
		fmt.Fprintf(os.Stderr, "storetest: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	templates.Lock()
	names := make([]string, 0, len(templates.byKey))
	for _, entry := range templates.byKey {
		if entry.name != "" {
			names = append(names, entry.name)
		}
	}
	templates.Unlock()
	for _, name := range names {
		goDrop(name, func() string {
			if err := teardownExec(name, "drop database "+name); err != nil {
				return fmt.Sprintf("drop template database %s: %v", name, err)
			}
			return ""
		})
	}
	if awaitDrops(started, testdb.TeardownDeadline(started)) {
		shared.admin.Close()
	}
	return withDropFails(code)
}

// withDropFails prints each drop that failed and answers code, or 1 where code is 0 and one did.
func withDropFails(code int) int {
	shared.dropMu.Lock()
	defer shared.dropMu.Unlock()
	for _, failure := range shared.dropFails {
		fmt.Fprintf(os.Stderr, "storetest: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// awaitDrops waits for every drop started so far until bound, and answers whether they ended. At
// bound it names each database whose drop is still in flight and the statement that drop ran or
// waits to run, and what the server's backends wait on, read once (testdb.WaitingOn): those drops
// keep their connections.
func awaitDrops(started, bound time.Time) bool {
	ended := make(chan struct{})
	go func() {
		shared.drops.Wait()
		close(ended)
	}()
	timer := time.NewTimer(time.Until(bound))
	defer timer.Stop()
	select {
	case <-ended:
		return true
	case <-timer.C:
	}
	shared.dropMu.Lock()
	databases := slices.Sorted(maps.Keys(shared.dropping))
	statements := make([]string, 0, len(databases))
	for _, database := range databases {
		statements = append(statements, shared.dropping[database])
	}
	shared.dropMu.Unlock()
	if len(databases) == 0 {
		// Each drop ended as the bound came; Wait returns as their goroutines do.
		<-ended
		return true
	}
	after := time.Since(started).Round(time.Second)
	for i, database := range databases {
		fmt.Fprintf(os.Stderr, "storetest: drop database %s: still waiting at the teardown's bound, %s after the tests began, so the database may be left on the server (%q)\n", database, after, statements[i])
	}
	fmt.Fprintf(os.Stderr, "storetest: at the teardown's bound: %s\n", testdb.WaitingOn(databaseURL(shared.server, "postgres"), statements...))
	return false
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

// dropDatabase drops name without waiting for it; dropNow and Main wait for every drop. Drops in
// flight together share one checkpoint.
//
// `with (force)` terminates whatever is still connected, which needs pg_signal_backend: a login
// that is only the database's owner is refused `permission denied to terminate process`. The
// test's own pool is closed before this runs (Open's cleanups are LIFO), but Postgres tears a
// backend down after the client has closed its socket, so a drop that lands inside that window
// finds a connection and, without that right, cannot clear it. So the drop is tried plainly
// and tried again while Postgres answers SQLSTATE 55006 (object_in_use), the code it gives
// whatever words it puts on the message, up to dropWindow; `force` is the last attempt, for a
// connection that really is somebody else's.
func dropDatabase(name string) {
	goDrop(name, func() string {
		deadline := time.Now().Add(dropWindow)
		var err error
		for {
			if err = teardownExec(name, "drop database "+name); err == nil {
				return ""
			}
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "55006" || time.Now().After(deadline) {
				break
			}
			time.Sleep(dropPause)
		}
		forced := teardownExec(name, "drop database "+name+" with (force)")
		if forced == nil {
			return ""
		}
		// The forced drop is what ended the cleanup, so its error is the one reported; the
		// plain drop's says what the force was for.
		return fmt.Sprintf("drop isolated database %s: %v, after the plain drop answered: %v", name, forced, err)
	})
}

// openAdmin opens the admin pool, on the server raw names, that every database and template is
// created and dropped through, and keeps raw as the server's URL (shared.server), with no bound of
// this package's. The pool outlives the test that opens it and makes connections for later tests,
// so a connection's connect timeout is not one fixed when the pool opened: each new connection a
// test's statement asks for takes the seconds left until end as it is made (testdb.TimeoutUntil),
// so one made late ends on pgx's own error with testwait's margin left, not after go test's alarm.
// A connection a drop asks for (teardown) takes testdb.NoDeadlineConnectTimeout instead, as at
// main: Run waits for the drops after the tests, when go test's alarm has stopped, and once end
// has passed the tests' bound is a second (TimeoutUntil's floor), which a loaded server's login
// outlasts, failing the run and leaving the database and its template on the server. Two costs
// follow. A drop holds a pool connection for up to two minutes while its login is held, so drops
// a test's cleanups start can fill the pool's four; a test's own statement then fails by name at
// end (adminExec), as it did when the drops had the tests' bound, and no drop leaks. And a drop
// that starts late, where the tests end near -timeout, can take longer than Run waits: Run's wait
// ends at its bound, testdb.TeardownGrace past -timeout from the tests' start, before cmd/go's
// kill (testdb.TeardownGrace says when that comes), and names each drop still in flight, so a
// database such a drop leaves on the server is named. Without bounded (no deadline) raw's bound
// stands, pgxpool's two minutes where raw names none.
func openAdmin(raw string, end time.Time, bounded bool) error {
	server, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	shared.server = *server
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL(shared.server, "postgres"))
	if err != nil {
		return fmt.Errorf("parse admin database URL: %w", err)
	}
	// The name each admin connection gives the server, this binary's alone, so a row can hold
	// this pool's logins and no other client's (TestALaterAdminConnectionEndsByTheConnectEnd).
	config.ConnConfig.RuntimeParams["application_name"] = adminApplicationName()
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	if bounded {
		config.BeforeConnect = func(asked context.Context, connection *pgx.ConnConfig) error {
			if asked.Value(teardownKey{}) != nil {
				connection.ConnectTimeout = testdb.NoDeadlineConnectTimeout
				return nil
			}
			connection.ConnectTimeout = testdb.TimeoutUntil(end)
			return nil
		}
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	shared.admin = admin
	return nil
}

// adminApplicationName is the application_name of this test binary's admin connections:
// "storetest admin" and the binary's pid, which no other process on the machine holds.
func adminApplicationName() string {
	return "storetest admin " + strconv.Itoa(os.Getpid())
}

func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

func randomName(t testing.TB, prefix string) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return prefix + suffix
}

func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}
