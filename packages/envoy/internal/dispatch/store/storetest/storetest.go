// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database, and
// waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an ordinary
// package rather than a _test.go file so every package whose tests need a Dispatch database can
// import it; Go cannot share test-only code across packages.
//
// Open clones each database from a template this test process migrates once. Running every
// migration in its declared execution mode is most of the Postgres work a test would
// otherwise do, so a package's wall time would scale with its test count times the speed of the
// Postgres it runs against; a clone costs one create database. Open's template is one of the
// templates OpenFrom makes, each once per key: Open's key is openKey, and this tree's runner
// migrates it. A filled template (Filled) is a copy of Open's into which a Fill writes rows once,
// for tests that each need the same rows; OpenFilled clones it.
//
// Under Main, the default, each template is made by the first test that asks for it, and that test
// pays for it: Open's takes 3 to 8 s. A package whose TestMain is MainEager (or RunEager) has
// Open's template, and each filled template it names, made in TestMain before any test runs, so no
// test pays for them; any other template is still made by the first test that asks.
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
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops, up to a bound (Run). Dropping a database forces a checkpoint and waits for it, and every
// package testing against the same server queues on its one checkpointer: a test that waited for
// its own drop spent that queue's time on its clock.
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
	"github.com/sjawhar/envoy/internal/testwait"
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
	return throughStore(databaseURL, func(ctx context.Context, database *store.Store) error {
		return database.Migrate(ctx)
	})
}

// throughStore runs write on a store opened on the database databaseURL names, and closes the
// store's pool before it returns, so write leaves the database with no connection of the store's:
// a template's migrate (migrateWithThisTree) and a Filled's Fill.
func throughStore(databaseURL string, write func(ctx context.Context, database *store.Store) error) error {
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	return write(ctx, database)
}

// ready makes sure the package's TestMain runs Run, skips t when DISPATCH_TEST_DATABASE_URL is
// unset, and opens the admin pool, once: here, in the first test that asks, unless RunEager opened
// it before the tests. It makes no template: RunEager makes Open's and the filled templates its
// TestMain names, and the first test that asks makes any other (OpenFrom, OpenFilled). The
// server's URL is kept as DISPATCH_TEST_DATABASE_URL names it, with no bound of this package's
// (shared.server), so no connect inherits the bound of whoever opened the pool. Each connect string
// this package hands out instead takes, when it is made, the seconds left until the test binary's
// deadline less testwait's margin (testdb.WithConnectDeadline), not pgx's default (two minutes in
// a pool, no bound through pgx.Connect): a clone's store, Empty's URL, and the URL each template's
// migrate or Fill is given, made by whoever makes the template when it makes it (makeTemplate:
// the test that asks, or RunEager, whose bound is testwait.MainGiveUpAt). The admin pool's
// connections take that bound as each is made (openAdmin). A pool opened from such a string
// measures its bound from when the string was made: a connection it makes s seconds later can end
// s seconds past the binary's deadline less the margin, which a migrate, made and run at once, and
// a test's own store keep within the margin unless the test runs on for longer than it. The admin
// URL is read before the Once, so a URL that cannot be read fails each test that asks, and never
// leaves the Once done with no admin pool for a later test to call.
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

// clone returns a store on a new database cloned from template, dropped after t ends.
func clone(t testing.TB, template string) *store.Store {
	t.Helper()
	name := randomName(t, "dispatch_test_")
	if err := adminExec(t, cloneStatement(name, template)); err != nil {
		t.Fatalf("clone isolated database from template %s: %v", template, err)
	}
	testcleanup.Add(t, func() { dropDatabase(name) })
	if err := adminExec(t, testdb.WithoutJIT(name)); err != nil {
		t.Fatalf("turn JIT off on the clone %s: %v", name, err)
	}
	database, err := store.Open(context.Background(), testdb.WithConnectDeadline(t, databaseURL(shared.server, name)))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	testcleanup.Add(t, database.Pool.Close)
	return database
}

// Empty returns the connection string of a new database that holds nothing, not even the
// version table, dropped after t ends: for a test that migrates it itself, as the two-build
// suite has the builds it runs migrate theirs, in production's order. It makes no template.
func Empty(t testing.TB) string {
	t.Helper()
	ready(t)
	name := randomName(t, "dispatch_test_")
	if err := adminExec(t, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	testcleanup.Add(t, func() { dropDatabase(name) })
	if err := adminExec(t, testdb.WithoutJIT(name)); err != nil {
		t.Fatalf("turn JIT off on %s: %v", name, err)
	}
	return testdb.WithConnectDeadline(t, databaseURL(shared.server, name))
}

// templates are the templates this test binary made, by key, each once: Open's (openKey), each
// OpenFrom caller's and each Filled's. Run drops every one of them.
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

// emptyTemplate is the database Open's template and each OpenFrom caller's copy: template1, the
// one Postgres copies where a create database names none.
const emptyTemplate = "template1"

// OpenFrom is Open for a database migrate brings to its schema instead of this tree's runner
// alone: it returns a store on a new database cloned from the template of key, dropped after t
// ends. The template is made once per key in the test binary (makeTemplate), by the first test
// that asks, or, for Open's key, by RunEager before the tests: a new empty database whose
// connection string migrate is given, which migrate migrates and leaves with no connection open.
// That string's connect bound is the seconds left, when the template is made, until the binary's
// deadline less testwait's margin (testdb.WithConnectEnd with the connect end of the test that
// asks, or RunEager's), so a template first asked for long after the admin pool opened still fails
// on pgx's error inside the margin. A template whose migrate failed, or ended its test, fails every
// test that asks for it again.
func OpenFrom(t testing.TB, key string, migrate func(databaseURL string) error) *store.Store {
	t.Helper()
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	name, err := makeTemplate(key, end, bounded, emptyTemplate, "migrate", migrate)
	if err != nil {
		t.Fatalf("prepare template database %q: %v", key, err)
	}
	return clone(t, name)
}

// Filled is a template that holds rows, for the tests of a package that each need the same rows:
// Fill writes them once, into a copy of Open's template, and each test that asks (OpenFilled)
// clones the result instead of writing them itself.
//
// Key names the template in the test binary, in the one namespace OpenFrom's keys are in, so no
// OpenFrom call and no other Filled names it. The template is made once per Key, with the Fill of
// whoever makes it: RunEager, where the package's TestMain names the Filled, otherwise the first
// test that asks, as OpenFrom's templates are.
//
// The template is a file copy of Open's (cloneStatement), so it holds every migration and runs
// none again, and it runs with JIT off (testdb.WithoutJIT). Fill writes through database, a store
// opened on the template whose connects end by the maker's connect end (testdb.WithConnectEnd);
// ctx has no deadline, as a migrate's context has none. Once Fill returns the store's pool is
// closed, and the template is closed to connections. Fill closes anything else it opens before it
// returns: Postgres refuses to clone a database another session holds, so a pool or a connection
// Fill leaves open fails every test that asks. A filled template whose Fill failed, or whose Open
// template could not be made, fails every test that asks for it, with the error.
type Filled struct {
	// Key names the template in the test binary.
	Key string
	// Fill writes the template's rows through database.
	Fill func(ctx context.Context, database *store.Store) error
}

// OpenFilled is Open for a test that needs filled's rows: it returns a store on a new database
// cloned from filled's template, dropped after t ends. It skips t when DISPATCH_TEST_DATABASE_URL
// is unset. Where RunEager has not made them, the first test that asks makes Open's template, then
// filled's (Filled).
func OpenFilled(t testing.TB, filled Filled) *store.Store {
	t.Helper()
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	name, err := makeFilled(filled, end, bounded)
	if err != nil {
		t.Fatalf("prepare template database %q: %v", filled.Key, err)
	}
	return clone(t, name)
}

// makeOpen answers Open's template, made where no caller has made it yet (makeTemplate).
func makeOpen(end time.Time, bounded bool) (string, error) {
	return makeTemplate(openKey, end, bounded, emptyTemplate, "migrate", migrateWithThisTree)
}

// makeFilled answers filled's template, made where no caller has made it yet (makeTemplate), as a
// copy of Open's, which it makes first where no caller has. Where Open's could not be made it
// answers Open's error, which is every later caller's too, since Open's is made once.
func makeFilled(filled Filled, end time.Time, bounded bool) (string, error) {
	open, err := makeOpen(end, bounded)
	if err != nil {
		return "", fmt.Errorf("prepare the template it copies, %q: %w", openKey, err)
	}
	return makeTemplate(filled.Key, end, bounded, open, "fill", func(databaseURL string) error {
		return throughStore(databaseURL, filled.Fill)
	})
}

// makeTemplate answers the template of key, which it makes once per key in the test binary, through
// the one entry Open, OpenFrom, OpenFilled and RunEager share (templates), where no caller has made
// it yet: a new database that copies from's files (cloneStatement), runs with JIT off, is brought
// to what the template holds by fill, given its connection string, and is then closed to
// connections. end and bounded are its maker's connect end, a test's (testdb.ConnectEnd) or
// RunEager's (testwait.MainGiveUpAt): they bound each of its admin statements' wait
// (adminExecUntil) and the connects of fill's connection string (testdb.WithConnectEnd). step
// names fill in fill's error ("<step> template database: …") and in the error a fill that ended
// its test leaves. A caller after the maker gets what the maker got: the template's database, or
// why it could not be made.
func makeTemplate(key string, end time.Time, bounded bool, from, step string, fill func(databaseURL string) error) (string, error) {
	templates.Lock()
	entry := templates.byKey[key]
	if entry == nil {
		entry = &templateDatabase{}
		templates.byKey[key] = entry
	}
	templates.Unlock()
	entry.once.Do(func() {
		// Set first, and cleared only once fill returns, so a fill that ends its test
		// (t.Fatal) leaves a template no later test clones.
		entry.err = fmt.Errorf("the test that made it ended before its %s returned", step)
		suffix, err := randomSuffix()
		if err != nil {
			entry.err = err
			return
		}
		name := "dispatch_template_" + suffix
		// The template is a file copy of from, for the reason each clone of it is one.
		if err := adminExecUntil(end, bounded, cloneStatement(name, from)); err != nil {
			entry.err = fmt.Errorf("create template database: %w", err)
			return
		}
		// Named once it exists, so Run drops exactly the templates that were made.
		entry.name = name
		if err := adminExecUntil(end, bounded, testdb.WithoutJIT(name)); err != nil {
			entry.err = fmt.Errorf("turn JIT off on the template database: %w", err)
			return
		}
		connect, err := testdb.WithConnectEnd(databaseURL(shared.server, name), end, bounded)
		if err != nil {
			entry.err = err
			return
		}
		if err := fill(connect); err != nil {
			entry.err = fmt.Errorf("%s template database: %w", step, err)
			return
		}
		// Postgres refuses to clone a database another session is connected to. Refusing
		// connections to the template keeps any stray session from making a clone wait or fail.
		if err := adminExecUntil(end, bounded, "alter database "+name+" with allow_connections false"); err != nil {
			entry.err = fmt.Errorf("close template database to connections: %w", err)
			return
		}
		entry.err = nil
	})
	return entry.name, entry.err
}

// cloneStatement returns the create database that makes name a copy of template's files rather
// than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name, template string) string {
	return "create database " + name + " template " + template + " strategy file_copy"
}

// adminExec runs statement, one a test makes (clone, Empty), on the admin pool, its wait for one of
// the pool's connections bounded by t's connect end (testdb.ConnectEnd): adminExecUntil, which
// says why.
func adminExec(t testing.TB, statement string) error {
	end, bounded := testdb.ConnectEnd(t)
	return adminExecUntil(end, bounded, statement)
}

// adminExecUntil runs statement on the admin pool: one a test makes (adminExec, and makeTemplate
// for the test that asks for a template), or one RunEager makes in TestMain (makeTemplate). Its
// wait for one of the pool's connections ends by end, the connect end of the test
// (testdb.ConnectEnd) or of TestMain (testwait.MainGiveUpAt), or a second from now where less than
// one is left: testdb.TimeoutUntil, the rule each new connection's connect bound follows
// (openAdmin). That one bound ends the wait whatever it waits for: the pool's semaphore, while
// drops or other statements hold all four of its connections (MaxConns); a new connection's
// connect, where the pool has none idle; and pgxpool's ping of a connection idle more than a
// second. So past the connect end the wait for a busy pool and that ping get one second, and a new
// connection's connect has a second of its own. A drop holds a connection for up to
// testdb.NoDeadlineConnectTimeout while the server holds its login, so a statement whose wait had
// no bound waited for four such drops past the connect end, into go test's alarm
// (TestATestsAdminStatementEndsByTheConnectEndWhileDropsHoldThePool, and in CI
// TestATestsAdminStatementEndsByTheConnectEndWhileEveryAdminConnectionIsHeld). The statement
// itself then runs with no bound of this package's, so a test that makes one after its connect
// end, on a server that answers, still gets its database once its wait has a connection within
// that second (TestATestsAdminStatementPastTheConnectEndStillRuns): a deadline on the statement
// failed docs' leak-check child, whose template, under load, was still being made 5 s past its
// connect end. So a statement that hangs once it has its connection, on a server that accepted it
// and never answers, is bounded only by go test's alarm, as at main, and in TestMain, where m.Run
// has not started that alarm yet, by cmd/go's kill: the connect end is too short a bound for it,
// and this package names no other. Without bounded (a t that names no deadline, or TestMain under
// -timeout 0) the wait has none. A failed wait says how long it waited and its bound ("wait for an
// admin connection (waited 1.001s of a 1s bound)"), so a reader can tell a wait that ran out from
// one that never waited (TestATestsAdminStatementPastTheConnectEndStillRuns skips only on the
// first). The wait is measured from before its deadline is set, so one that ran out reads at least
// its bound.
func adminExecUntil(end time.Time, bounded bool, statement string) error {
	began := time.Now()
	ctx, cancel := context.Background(), func() {}
	bound := "no"
	if bounded {
		timeout := testdb.TimeoutUntil(end)
		bound = "a " + timeout.String()
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	connection, err := shared.admin.Acquire(ctx)
	waited := time.Since(began)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for an admin connection (waited %s of %s bound): %w", waited.Round(time.Millisecond), bound, err)
	}
	defer connection.Release()
	_, err = connection.Exec(context.Background(), statement)
	return err
}

// DefaultParallel is how many tests of a package run at once when its TestMain calls
// SetDefaultParallel and the command line names no -parallel: four, the parallelism the
// build-cluster measurement of those packages ran at (a 4-CPU pod) and go test's default on CI's
// 4-CPU runner. Left to go test's default of GOMAXPROCS, a machine with more CPUs would run more of
// them at once than any run has measured. A package measured at a number of its own sets that
// number in its own TestMain, with its reason (internal/dispatch/api, internal/dispatch/docs).
const DefaultParallel = 4

// SetDefaultParallel sets the test binary's default -parallel to DefaultParallel. A package's
// TestMain calls it before Main, Run, MainEager or RunEager, and so before the command line is
// parsed (by m.Run, or by RunEager before it), where a -parallel still wins.
func SetDefaultParallel() {
	if err := flag.Set("test.parallel", strconv.Itoa(DefaultParallel)); err != nil {
		panic("storetest: set the default -parallel: " + err.Error())
	}
}

// Main runs the package's tests, then drops every template they were cloned from beside their
// databases' drops, and waits for all of them. A package whose tests call Open uses it as its
// TestMain: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }. It is the default: each
// template is made by the first test that asks for it, which pays for it. MainEager makes Open's
// and the filled templates before the tests instead.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose tests also need another harness's Main, which runs m.Run
// itself: run runs the package's tests through that Main, and Run then does what Main does after
// them. internal/dispatch/delivery shares one NATS server across its tests (testnats.Main):
// func TestMain(m *testing.M) { os.Exit(storetest.Run(func() int { return testnats.Main(m) })) }.
// As under Main, each template is made by the first test that asks for it (RunEager is the form
// that makes them before run).
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

// MainEager is Main for a package whose tests should not pay for its templates: before the tests
// it makes Open's template, then each of filled's (Filled), as RunEager says. Such a package's
// TestMain is func TestMain(m *testing.M) { os.Exit(storetest.MainEager(m, filled...)) }, where
// filled are the Filled its tests pass to OpenFilled, or none.
func MainEager(m *testing.M, filled ...Filled) int { return RunEager(m.Run, filled...) }

// RunEager is Run, but before run it makes Open's template, then each of filled's templates, in
// order, each once, through the entries Open, OpenFrom and OpenFilled read (makeTemplate), so the
// tests find them made and none pays for them. Under Main the first test that calls Open pays for
// Open's template, 3 to 8 s; under RunEager TestMain pays it. The time moves from that test to
// TestMain and is not saved: it stays in the package's time, which go test prints beside the
// package. So a run of one test by name, even one that opens no database, pays for every template
// RunEager makes, where under Main it pays only for those it asks for.
//
// After each template it writes one line to stderr: `storetest: template "<key>" made in <d>`, or
// `storetest: template "<key>" could not be made after <d>: <error>`. A template that could not be
// made does not stop the run: its entry keeps the error, and each test that asks for the template
// fails with it, as each test that asks for one whose first maker failed does under Main. It makes
// nothing, and the tests skip or fail as they do under Main, where DISPATCH_TEST_DATABASE_URL is
// unset or cannot be parsed, and where the command line names -test.list: go test -list runs
// TestMain and no test, and packages/envoy/scripts/database-skips.sh runs it in CI.
//
// It reads the command line (-test.list, -test.timeout) before run, and so parses it, as m.Run
// would: a package whose TestMain calls SetDefaultParallel calls it before RunEager. Its connect
// end is testwait.MainGiveUpAt, the -timeout flag's deadline from the process's start less
// testwait's margin, which is no later than any test's (testdb.ConnectEnd). It opens the admin
// pool with that end, as ready would with a test's (openAdmin), so the pool's new connections take
// it for the rest of the run; each of its admin statements' wait ends by it (adminExecUntil); and
// each template's migrate and Fill are given a connection string bounded by it
// (testdb.WithConnectEnd). go test's alarm starts only in m.Run, so a statement that hangs once it
// has its connection is bounded here only by cmd/go's kill; the time RunEager takes is the
// binary's before Run, which testdb.TeardownGrace's margin under that kill counts.
func RunEager(run func() int, filled ...Filled) int {
	if raw, ok := eagerServer(); ok {
		makeEagerly(raw, filled)
	}
	return Run(run)
}

// eagerServer is the server RunEager makes its templates on, DISPATCH_TEST_DATABASE_URL, and false
// where it makes none: the variable is unset or cannot be parsed, for which ready skips or fails
// each test that asks, or the command line names -test.list, under which m.Run lists the tests and
// runs none. It parses the command line when nothing has yet, as m.Run would.
func eagerServer() (string, bool) {
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		return "", false
	}
	if _, err := url.Parse(raw); err != nil {
		return "", false
	}
	if !flag.Parsed() {
		flag.Parse()
	}
	list := flag.Lookup("test.list")
	return raw, list == nil || list.Value.String() == ""
}

// makeEagerly opens the admin pool on raw, as ready would, and makes Open's template, then each of
// filled's, under TestMain's connect end (testwait.MainGiveUpAt), writing one line to stderr for
// each. Where the admin pool could not be opened it makes none, and each line says why; ready then
// fails each test that asks with the same error, as under Main.
func makeEagerly(raw string, filled []Filled) {
	end, bounded := testwait.MainGiveUpAt()
	shared.once.Do(func() { shared.err = openAdmin(raw, end, bounded) })
	made := func(key string, build func() (string, error)) {
		began := time.Now()
		var err error
		if shared.err != nil {
			err = fmt.Errorf("open the admin pool: %w", shared.err)
		} else {
			_, err = build()
		}
		took := time.Since(began).Round(time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "storetest: template %q could not be made after %s: %v\n", key, took, err)
			return
		}
		fmt.Fprintf(os.Stderr, "storetest: template %q made in %s\n", key, took)
	}
	made(openKey, func() (string, error) { return makeOpen(end, bounded) })
	for _, each := range filled {
		made(each.Key, func() (string, error) { return makeFilled(each, end, bounded) })
	}
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

// teardownKey marks teardown, the context of every drop: dropDatabase's, which a test's cleanup
// starts and Run waits for after the tests, and Run's template drops. A connection the admin pool
// makes for a statement under it takes testdb.NoDeadlineConnectTimeout, not the tests' bound
// (openAdmin). pgxpool hands BeforeConnect a context with the values of the call that asked for
// the connection (puddle's constructor context, at pgx v5.13.0 and puddle v2.2.3).
type teardownKey struct{}

var teardown = context.WithValue(context.Background(), teardownKey{}, true)

// goDrop runs drop, the drop of database, in the background, as one of the drops Run waits for:
// what it answers is a failure Run prints ("" for none), and until it returns Run names database
// at its bound (shared.dropping).
func goDrop(database string, drop func() (failure string)) {
	shared.dropMu.Lock()
	if shared.dropping == nil {
		shared.dropping = map[string]string{}
	}
	shared.dropping[database] = "drop database " + database
	shared.dropMu.Unlock()
	shared.drops.Go(func() {
		failure := drop()
		shared.dropMu.Lock()
		defer shared.dropMu.Unlock()
		delete(shared.dropping, database)
		if failure != "" {
			shared.dropFails = append(shared.dropFails, failure)
		}
	})
}

// teardownExec runs statement, a drop of database, on the admin pool under teardown, and keeps it
// as the statement database's drop runs (shared.dropping).
func teardownExec(database, statement string) error {
	shared.dropMu.Lock()
	shared.dropping[database] = statement
	shared.dropMu.Unlock()
	_, err := shared.admin.Exec(teardown, statement)
	return err
}

// dropDatabase drops name without waiting for it; Main waits for every drop, up to its bound.
// Drops in flight together share one checkpoint.
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
// this package's. The pool outlives whoever opens it, the first test that asks (ready) or RunEager,
// and makes connections for later tests, so a connection's connect timeout is not one fixed when
// the pool opened: each new connection a test's statement asks for takes the seconds left until
// end, the opener's connect end, as it is made (testdb.TimeoutUntil), so one made late ends on
// pgx's own error with testwait's margin left, not after go test's alarm.
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
