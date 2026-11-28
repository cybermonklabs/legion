// Package storetest gives each Postgres-backed Dispatch test its own fully migrated database, and
// waits for backends to queue behind a lock a test holds (WaitForLockWaiters). It is an ordinary
// package rather than a _test.go file so every package whose tests need a Dispatch database can
// import it; Go cannot share test-only code across packages.
//
// Open clones each database from a template migrated once for each Postgres server, set of
// migrations and login, which every test process that runs against that server as that login
// finds and clones. Running every migration in its declared execution mode is most of the
// Postgres work a test would otherwise do, so a package's wall time would scale with its test
// count times the speed of the Postgres it runs against; a clone costs one create database.
// Open's template is the server's (serverTemplate): its name, dispatch_shared_v1_<16 hex>_<12 hex>
// (templateName), holds the digest of the embedded migration files and of the steps storetest
// adds to them (templateDigest), and the login's key (loginKey). The first test process to look
// for a template no process has made makes it, with this tree's runner, under a lock the server
// holds; every other finds it. Each process holds the template it resolved for as long as it runs
// (templateConn), and resolves it again where it lost that hold and a sweep dropped the template
// (createFrom). Nothing drops a server template but a sweep: a process that made one then
// drops each other template of its scheme and login that no running process holds, so a server
// that outlives runs keeps the templates in use plus the newest (docs/testing.md, "A database a Go
// test creates", says how to drop one by hand). Every other template is the test binary's own,
// made once per key (makeTemplate) and dropped by Run: each OpenFrom caller's, and each filled
// template (Filled), a copy of Open's into which a Fill writes rows once, for tests that each need
// the same rows; OpenFilled clones it.
//
// Main and Run find or make Open's template in TestMain, before any test runs, so no test pays for
// it: making it took 3.8 to 8.5 s of the first test that asked in CI's envoy-go job, and finding it
// costs a look. A package whose TestMain is MainEager (or RunEager) has each filled template it
// names made there too; any other template is made by the first test that asks.
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
// JIT is off (testdb.WithoutJIT), as on production's database. Open's template also turns
// autovacuum off for one table, embeddings, which each clone copies (embeddingsStatistics).
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
	"strings"
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

// openKey is the key of Open's template in templates: the server's template of this tree's
// migrations (serverTemplate), which this tree's runner migrates where no process has made it yet
// (migrateWithThisTree).
const openKey = "this tree's runner (storetest.Open)"

// Open returns a store on a new database carrying every migration, dropped after t ends: a clone
// of the server's template, which TestMain resolved (Run). It skips t when
// DISPATCH_TEST_DATABASE_URL is unset. Where the clone finds the template gone, which a sweep can
// drop once this process's template connection, and with it its hold, was lost, it resolves the
// template again, once, and clones it (clone).
func Open(t testing.TB) *store.Store {
	t.Helper()
	return openTemplate(t, openKey, makeOpen, resolveOpen)
}

// migrateWithThisTree migrates the database databaseURL names with this tree's runner, and turns
// autovacuum off for embeddings (embeddingsStatistics): a build of Open's template. It closes its
// pool before it returns, so the build is left with no connection.
func migrateWithThisTree(databaseURL string) error {
	return throughStore(databaseURL, func(ctx context.Context, database *store.Store) error {
		if err := database.Migrate(ctx); err != nil {
			return err
		}
		if _, err := database.Pool.Exec(ctx, embeddingsStatistics); err != nil {
			return fmt.Errorf("turn autovacuum off for embeddings: %w", err)
		}
		return nil
	})
}

// throughStore runs write on a store opened on the database databaseURL names, and closes the
// store's pool before it returns, so write leaves the database with no connection of the store's:
// a template's migrate (migrateWithThisTree), and a Filled's Fill, which runs on a second store
// opened beside this one (fillThroughStore).
func throughStore(databaseURL string, write func(ctx context.Context, database *store.Store) error) error {
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	return write(ctx, database)
}

// fillThroughStore runs fill, a Filled's Fill, on a store opened on the template databaseURL
// names, closes every pool of that store before it returns, and answers the backend pid of each
// connection those pools closed. Postgres ends a backend a moment after its client closes the
// connection, so those backends can still be listed on the template as fill returns:
// noSessionLeftOpen waits for them to end rather than take them for sessions fill left open.
//
// Each pool records a connection as it closes it (pgxpool's BeforeClose). store.Open builds the
// production configuration and takes no hook, so the store fill is given is a second pool opened
// from the configuration of throughStore's, with the record added. The pools the given store opens
// on demand (store.Pool.Rooms and the rest) copy its configuration, record included, as does a
// pool fill opens from database.Pool.Config(); a pool fill leaves open closes nothing, so its
// connections are in no record. throughStore's own pool holds only the connection store.Open
// pinged on, recorded before fill runs, and closes as throughStore returns. A connection no pool
// closes is in no record either: one fill opens otherwise, or takes out of a pool
// (store.Pool.Listener hijacks one), and closes itself.
func fillThroughStore(databaseURL string, fill func(ctx context.Context, database *store.Store) error) ([]int32, error) {
	var closed closedConnections
	err := throughStore(databaseURL, func(ctx context.Context, opened *store.Store) error {
		for _, connection := range opened.Pool.AcquireAllIdle(ctx) {
			closed.record(connection.Conn())
			connection.Release()
		}
		config := opened.Pool.Config()
		config.BeforeClose = closed.record
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			return fmt.Errorf("open the Fill's pool: %w", err)
		}
		database := &store.Store{Pool: store.NewPool(pool)}
		defer database.Pool.Close()
		return fill(ctx, database)
	})
	return closed.backends(), err
}

// closedConnections holds the backend pid of each connection a Fill's pools closed
// (fillThroughStore). A pool closes its connections on goroutines of its own, and a pool the Fill
// left open, which copied the record, can close one after the record was read, so it is locked.
type closedConnections struct {
	mu   sync.Mutex
	pids []int32
}

// record keeps connection's backend pid. Postgres's pid is an int4, which pgconn hands out as a
// uint32.
func (c *closedConnections) record(connection *pgx.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pids = append(c.pids, int32(connection.PgConn().PID()))
}

// backends is a copy of the pids recorded so far.
func (c *closedConnections) backends() []int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pids)
}

// embeddingsStatistics turns autovacuum off for embeddings in Open's template, and so in every
// clone, which copies the table's options: the table's statistics are then only what the test
// writes and any analyze it runs itself.
//
// Meaning search's plan follows those statistics, and the plan decides its answer. Autovacuum
// analyzes a clone when its own schedule reaches it, which it did inside one unloaded run of a
// test a few seconds long. An analyze that lands after a test has written its windows and before
// their vectors reads every row as holding none, and the planner then takes every meaning arm
// through embeddings_cosine, pgvector's approximate HNSW index, where at any other moment it takes
// an exact scan and sort of a table this small. Measured on
// TestMeaningSearchFindsOtherItemsPastOneLongDocumentsWindows (INFRA-3149), whose long document
// holds a hundred identical vectors: with that analyze planted, 6 of 12 searches lost the short
// document and 1 lost the comment; analyzed once every vector was in, 12 of 12 found all three;
// and the document arm alone, forced onto the HNSW index, missed the short document in 13 of 33
// runs. A test whose subject is the planner's choice analyzes the table itself
// (search_index_test.go, docs' block_index_test.go).
const embeddingsStatistics = `alter table embeddings set (autovacuum_enabled = false, toast.autovacuum_enabled = false)`

// ready makes sure the package's TestMain runs Run, skips t when DISPATCH_TEST_DATABASE_URL is
// unset, and opens the admin pool, once: here, in the first test that asks, with that test's
// connect end, unless RunEager opened it before the tests to make the filled templates its
// TestMain names. It makes no template: Run resolves Open's in TestMain, on a connection of its
// own (templateConn) and not on the admin pool, RunEager makes the filled templates its TestMain
// names, and the first test that asks makes any other (OpenFrom, OpenFilled). The server's URL is
// kept as DISPATCH_TEST_DATABASE_URL names it, with no bound of this package's (shared.server), so
// no connect inherits the bound of whoever opened the pool. Each connect string this package hands
// out instead takes, when it is made, the seconds left until the test binary's deadline less
// testwait's margin (testdb.WithConnectDeadline), not pgx's default (two minutes in a pool, no
// bound through pgx.Connect): a clone's store, Empty's URL, and the URL each template's migrate or
// Fill is given, made by whoever makes the template when it makes it (makeTemplate and
// serverTemplate: the test that asks, or TestMain, whose bound is testwait.MainGiveUpAt). The admin
// pool's connections take that bound as each is made (openAdmin). A pool opened from such a string
// measures its bound from when the string was made: a connection it makes s seconds later can end
// s seconds past the binary's deadline less the margin, which a migrate, made and run at once, and
// a test's own store keep within the margin unless the test runs on for longer than it. The admin
// URL is read (serverFromEnv) before the Once, so a URL that cannot be read fails each test that
// asks, and never leaves the Once done with no admin pool for a later test to call.
func ready(t testing.TB) {
	t.Helper()
	if !shared.main {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }, or storetest.Run around another harness's Main")
	}
	raw, set, err := serverFromEnv()
	if !set {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	end, bounded := testdb.ConnectEnd(t)
	shared.once.Do(func() { shared.err = openAdmin(raw, end, bounded) })
	if shared.err != nil {
		t.Fatalf("open the admin pool: %v", shared.err)
	}
}

// serverFromEnv reads DISPATCH_TEST_DATABASE_URL for ready, eagerServer and findOrMakeOpen: raw is
// its value, set is whether it names one, and err, where it does not parse, is a fixed text that
// carries nothing of the URL. url.Parse's error quotes the URL, and even the error it wraps can (an
// invalid port's quotes what follows the authority's last colon, which for a password holding an
// unescaped '#', '/' or '?' is the password's first part).
func serverFromEnv() (raw string, set bool, err error) {
	raw = os.Getenv(DatabaseURLEnv)
	if raw == "" {
		return "", false, nil
	}
	if _, err = url.Parse(raw); err != nil {
		return "", true, errors.New(DatabaseURLEnv + " does not parse")
	}
	return raw, true, nil
}

// clone returns a store on a new database cloned from template, dropped after t ends, and logs
// `storetest: <database> cloned from <template> in <d>` through t, which go test prints under -v
// or -json, or where t failed: which test of a process opened a database first, and what one
// clone cost. The store's pool closes before the drop runs (closeThroughTheSharedWait), and the
// drop, which runs after it, ends the connections still held (dropDatabase's forced drop).
//
// again, where not nil, answers template once more, for Open's template, the server's: where the
// clone fails because template does not exist (SQLSTATE 3D000), clone has again resolve it, once,
// and clones what it answers (createFrom says when that comes true). A clone already running
// when a drop starts finishes first: drop database waits for the lock the clone's create database
// holds on its template. A binary's own templates take no again: nothing but Run drops them.
func clone(t testing.TB, template string, again func() (string, error)) *store.Store {
	t.Helper()
	name := randomName(t, "dispatch_test_")
	began := time.Now()
	template, err := createFrom(template, again, func(from string) error {
		return adminExec(t, cloneStatement(name, from))
	})
	if err != nil {
		t.Fatalf("clone isolated database from template %s: %v", template, err)
	}
	t.Logf("storetest: %s cloned from %s in %s", name, template, time.Since(began).Round(time.Millisecond))
	testcleanup.Add(t, func() { dropDatabase(name) })
	if err := adminExec(t, testdb.WithoutJIT(name)); err != nil {
		t.Fatalf("turn JIT off on the clone %s: %v", name, err)
	}
	database, err := store.Open(context.Background(), testdb.WithConnectDeadline(t, databaseURL(shared.server, name)))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	closeThroughTheSharedWait(t, "the test's database pool", database.Pool)
	return database
}

// createFrom runs create, a create database that copies from, and answers what it copied and its
// error. Where create fails because from does not exist (SQLSTATE 3D000) and again is not nil, it
// has again answer from once more, once, and runs create on what again answers: Open's template,
// which a sweep of another migration set by the same login can drop once this process's hold was
// lost with its template connection, is resolved again (resolveOpen: a new connection, the hold
// taken again, the template found or made again). Open's clone and a Filled's template, a copy of
// Open's, take it (clone, makeTemplate).
func createFrom(from string, again func() (string, error), create func(from string) error) (string, error) {
	err := create(from)
	var pgError *pgconn.PgError
	if again == nil || !errors.As(err, &pgError) || pgError.Code != "3D000" {
		return from, err
	}
	resolved, againErr := again()
	if againErr != nil {
		return from, fmt.Errorf("%w, and resolving it again failed: %w", err, againErr)
	}
	return resolved, create(resolved)
}

// closeThroughTheSharedWait closes pool after t ends, through the shared wait (testwait.Returns):
// store.Pool's Close returns only once every connection in use has come back, and a row that
// failed, or the defect it caught, can leave the code under test holding one for good, parked on a
// fake clock no one moves or a gate no one opens. The wait then fails naming what, the pool, where
// Close alone would hang the test binary to its timeout.
//
// The shared wait gives up at the connect end (testdb.ConnectEnd and testwait.GiveUpAt name the
// same moment), and past it asks once and fails, so in a cleanup that starts at or past that end,
// as the cleanups of a test that makes its statements past it do
// (TestATestsAdminStatementPastTheConnectEndStillRuns), it would give up on a Close still
// destroying the pool's idle connections. There Close waits until the hang guard's moment
// (hangGuardAt) instead: a correct Close returns once each idle connection is destroyed, and only
// a connection never given back holds it, which the guard then names.
func closeThroughTheSharedWait(t testing.TB, what string, pool *store.Pool) {
	testcleanup.Add(t, func() {
		end, bounded := testdb.ConnectEnd(t)
		if !bounded || time.Now().Before(end) {
			testwait.Returns(t, "every connection of "+what+" to come back, so the pool closes", pool.Close)
			return
		}
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			pool.Close()
		}()
		guard := time.NewTimer(time.Until(hangGuardAt(end)))
		defer guard.Stop()
		select {
		case <-closed:
		case <-guard.C:
			t.Fatalf("storetest: every connection of %s had not come back at the hang guard, %s past the connect end, so a connection was never given back and the pool did not close", what, testwait.Margin-hangGuardMargin)
		}
	})
}

// hangGuardMargin is what a hang guard leaves before go test's alarm (the binary's deadline), for
// the failure it reports and the cleanups after it.
const hangGuardMargin = 5 * time.Second

// hangGuardAt is the moment a wait past the connect end (end, testdb.ConnectEnd) gives up: past
// that end the shared wait cannot serve, since it gives up there itself, so such a wait runs until
// the binary's deadline less hangGuardMargin. Only a call that never returns reaches it: a guard
// against a hang, never a bound a row proves.
func hangGuardAt(end time.Time) time.Time {
	return end.Add(testwait.Margin - hangGuardMargin)
}

// untilHangGuard asks holds until it reports true, at the shared wait's pace, or until the hang
// guard's moment past end, and reports whether it held: a wait for a fact past the connect end,
// where the shared wait cannot serve.
func untilHangGuard(end time.Time, holds func() bool) bool {
	guard := hangGuardAt(end)
	for !holds() {
		if !time.Now().Before(guard) {
			return holds()
		}
		time.Sleep(testwait.Poll)
	}
	return true
}

// Empty returns the connection string of a new database that holds nothing, not even the
// version table, dropped after t ends: for a test that migrates it itself, as the two-build
// suite has the builds it runs migrate theirs, in production's order. It makes no template.
func Empty(t testing.TB) string {
	t.Helper()
	ready(t)
	name := randomName(t, "dispatch_test_")
	if err := adminExec(t, cloneStatement(name, emptyTemplate)); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	testcleanup.Add(t, func() { dropDatabase(name) })
	if err := adminExec(t, testdb.WithoutJIT(name)); err != nil {
		t.Fatalf("turn JIT off on %s: %v", name, err)
	}
	return testdb.WithConnectDeadline(t, databaseURL(shared.server, name))
}

// templates are the templates this test binary's tests clone, by key, each resolved once: Open's
// (openKey), which is the server's (serverTemplate) and which Run leaves on the server, and each
// OpenFrom caller's and each Filled's, which the binary made and Run drops.
var templates = struct {
	sync.Mutex
	byKey map[string]*templateDatabase
}{byKey: map[string]*templateDatabase{}}

// templateDatabase is one template: once makes it, or finds it, name is its database, and err is
// why it could not be made.
type templateDatabase struct {
	once sync.Once
	name string
	err  error
}

// templateEntry is the entry of key in templates, made where none is yet: the one place an entry
// is made, for makeOpen and makeTemplate.
func templateEntry(key string) *templateDatabase {
	templates.Lock()
	defer templates.Unlock()
	entry := templates.byKey[key]
	if entry == nil {
		entry = &templateDatabase{}
		templates.byKey[key] = entry
	}
	return entry
}

// emptyTemplate is the database each build of Open's template (serverTemplate), each OpenFrom
// caller's template and Empty's database copy: template1, the one Postgres copies where a create
// database names none.
const emptyTemplate = "template1"

// OpenFrom is Open for a database migrate brings to its schema instead of this tree's runner
// alone: it returns a store on a new database cloned from the template of key, dropped after t
// ends. The template is made once per key in the test binary (makeTemplate), by the first test
// that asks, and Run drops it: a new empty database whose connection string migrate is given,
// which migrate migrates and leaves with no connection open. That string's connect bound is the
// seconds left, when the template is made, until the binary's deadline less testwait's margin
// (testdb.WithConnectEnd with the connect end of the test that asks), so a template first asked
// for long after the admin pool opened still fails on pgx's error inside the margin. A template
// whose migrate failed, or ended its test, fails every test that asks for it again.
func OpenFrom(t testing.TB, key string, migrate func(databaseURL string) error) *store.Store {
	t.Helper()
	return openTemplate(t, key, func(end time.Time, bounded bool) (string, error) {
		return makeTemplate(key, end, bounded, emptyTemplate, nil, "migrate", func(_, databaseURL string) error {
			return migrate(databaseURL)
		})
	}, nil)
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
// The template is a file copy of Open's (cloneStatement), resolved again where a sweep dropped it
// (makeFilled), so it holds every migration and runs none again, and it runs with JIT off
// (testdb.WithoutJIT). Fill writes through database, a store opened on the template whose connects
// end by the maker's connect end (testdb.WithConnectEnd); ctx has no deadline, as a migrate's
// context has none. Once Fill returns the store's pools are closed, and the template is closed to
// connections. Fill closes anything else it opens before it returns: Postgres refuses to clone a
// database another session is connected to. So before the template is closed, storetest reads the
// sessions still connected to it (noSessionLeftOpen), and where Fill left one open the template is
// not made: its error names Key and each such session's application_name, state and query.
// storetest ends none of them, since ending one would hide the leak. It first waits for Postgres to
// end the backends of the connections that database's pools closed as Fill returned, and of any
// pool Fill opened from database.Pool.Config() and closed: each of those pools records the
// connections it closes (fillThroughStore). A connection Fill opens otherwise, or takes out of a
// pool (store.Pool.Listener), and closes is in no record, and Postgres can still list it when
// storetest reads, which fails the template as a session left open does; a Fill that needs a
// connection of its own takes it from such a pool. A filled template whose Fill failed or left a
// session open, or whose Open template could not be made, fails every test that asks for it, with
// the error.
type Filled struct {
	// Key names the template in the test binary.
	Key string
	// Fill writes the template's rows through database.
	Fill func(ctx context.Context, database *store.Store) error
}

// OpenFilled is Open for a test that needs filled's rows: it returns a store on a new database
// cloned from filled's template, dropped after t ends. It skips t when DISPATCH_TEST_DATABASE_URL
// is unset. Where RunEager has not made it, the first test that asks makes filled's template
// (Filled), from Open's, which TestMain resolved (Run).
func OpenFilled(t testing.TB, filled Filled) *store.Store {
	t.Helper()
	return openTemplate(t, filled.Key, func(end time.Time, bounded bool) (string, error) {
		return makeFilled(filled, end, bounded)
	}, nil)
}

// openTemplate is what Open, OpenFrom and OpenFilled share: it makes sure t can have a database
// (ready), has maker answer the template of key under t's connect end (testdb.ConnectEnd), failing
// t where it could not be made, and returns a store on a new database cloned from it, dropped after
// t ends. again, Open's alone, answers the template once more under the same end where the clone
// finds it gone (clone).
func openTemplate(t testing.TB, key string, maker, again func(end time.Time, bounded bool) (string, error)) *store.Store {
	t.Helper()
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	name, err := maker(end, bounded)
	if err != nil {
		t.Fatalf("prepare template database %q: %v", key, err)
	}
	var resolveAgain func() (string, error)
	if again != nil {
		resolveAgain = func() (string, error) { return again(end, bounded) }
	}
	return clone(t, name, resolveAgain)
}

// makeOpen answers Open's template, the server's, which it resolves once in the test binary
// (resolveOpen) where no caller has: Run does before the tests wherever a test could ask
// (eagerServer), under TestMain's connect end, so a test finds it resolved; were none to have, the
// first test that asks would, under its own. Its entry is openKey's, for Open, makeFilled and Run,
// which Run leaves on the server.
func makeOpen(end time.Time, bounded bool) (string, error) {
	entry := templateEntry(openKey)
	entry.once.Do(func() { entry.name, entry.err = resolveOpen(end, bounded) })
	return entry.name, entry.err
}

// makeFilled answers filled's template, made where no caller has made it yet (makeTemplate), as a
// copy of Open's, which it resolves first where no caller has (makeOpen), and again, once, where
// the copy finds it gone (createFrom, through resolveOpen), as Open's clone does. Where Open's
// could not be made it answers Open's error, which is every later caller's too, since Open's is
// resolved once. Once the Fill has returned, and before the template is closed to connections,
// the template fails where the Fill left a session connected to it (noSessionLeftOpen).
func makeFilled(filled Filled, end time.Time, bounded bool) (string, error) {
	open, err := makeOpen(end, bounded)
	if err != nil {
		return "", fmt.Errorf("prepare the template it copies, %q: %w", openKey, err)
	}
	again := func() (string, error) { return resolveOpen(end, bounded) }
	return makeTemplate(filled.Key, end, bounded, open, again, "fill", func(name, databaseURL string) error {
		closed, err := fillThroughStore(databaseURL, filled.Fill)
		if err != nil {
			return err
		}
		return noSessionLeftOpen(filled.Key, name, closed, end, bounded)
	})
}

// leftOpenQueryShown is how many characters of a session's query noSessionLeftOpen's error
// quotes; a longer query is cut there and ends in "…". Postgres keeps up to
// track_activity_query_size of it, 1 kB by default, and an error that quoted that much of each
// session would bury the key.
const leftOpenQueryShown = 200

// noSessionLeftOpen answers an error where a Filled's Fill, whose key is key, returned leaving a
// session connected to its template, the database template names. Postgres refuses to clone a
// database another session is connected to: each clone waits five seconds for the session to end,
// then fails with an error that names only the template's random database ("is being accessed by
// other users"), not the Filled whose Fill left the session. The error names key and each session
// found, by its application_name, its state and its query (cut to leftOpenQueryShown). It ends
// none of them: ending one would hide the Fill's leak.
//
// It reads pg_stat_activity on an admin connection, whose wait is adminExecUntil's (bounded by end
// where bounded). That connection is on the server's postgres database, so it is never among the
// sessions read, and only client backends count: an autovacuum worker can be on the template, and
// a clone ends one rather than fail. closed are the backends of the connections the Fill's pools
// closed (fillThroughStore), which Postgres can still list for a moment. It waits through testwait
// (InHelper, whose one bound is the binary's deadline less testwait's margin) until none of them is
// listed, which comes true as soon as Postgres has ended them; any session listed then is one the
// Fill left open, and it fails at once.
//
// Only a Fill is checked. A migrate, this tree's runner (migrateWithThisTree) or an OpenFrom
// caller's, closes connections no pool records: the runner closes the ones it takes out of its
// pool for a migration that runs outside a transaction, and an OpenFrom caller's migrate opens its
// own from the URL. The check could not tell their backends, still ending, from a session left
// open, so it would fail migrates that left none.
func noSessionLeftOpen(key, template string, closed []int32, end time.Time, bounded bool) error {
	connection, err := acquireAdminUntil(end, bounded)
	if err != nil {
		return err
	}
	defer connection.Release()
	var sessions []templateSession
	var readErr error
	what := fmt.Sprintf("Postgres to end the backends of the %d connections that the pools of Filled %q closed as its Fill returned, on its template %s", len(closed), key, template)
	if err := testwait.InHelper("storetest", what, func() bool {
		sessions, readErr = templateSessions(connection, template)
		return readErr != nil || !slices.ContainsFunc(sessions, func(session templateSession) bool {
			return slices.Contains(closed, session.pid)
		})
	}); err != nil {
		return err
	}
	if readErr != nil {
		return fmt.Errorf("read the sessions connected to the template database: %w", readErr)
	}
	if len(sessions) == 0 {
		return nil
	}
	found := make([]string, len(sessions))
	for i, session := range sessions {
		found[i] = fmt.Sprintf("application_name %q, state %q, query %q", session.application, session.state, session.query)
	}
	return fmt.Errorf("the Fill of Filled %q returned with %d session(s) still connected to its template %s, which Postgres refuses to clone; a Fill closes each connection it opens before it returns, and storetest ends none: %s",
		key, len(sessions), template, strings.Join(found, "; "))
}

// templateSession is a client session connected to a template database, as pg_stat_activity lists
// it.
type templateSession struct {
	pid                       int32
	application, state, query string
}

// templateSessions reads the client sessions connected to template, oldest first, each query cut
// to leftOpenQueryShown characters.
func templateSessions(connection *pgxpool.Conn, template string) ([]templateSession, error) {
	rows, err := connection.Query(context.Background(), `
		select pid, coalesce(application_name, ''), coalesce(state, ''),
			left(coalesce(query, ''), $2) || case when length(query) > $2 then '…' else '' end
		from pg_stat_activity
		where datname = $1 and backend_type = 'client backend'
		order by backend_start, pid`, template, leftOpenQueryShown)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (templateSession, error) {
		var session templateSession
		err := row.Scan(&session.pid, &session.application, &session.state, &session.query)
		return session, err
	})
}

// makeTemplate answers the template of key, which it makes once per key in the test binary,
// through the entries OpenFrom, OpenFilled and RunEager share (templates), where no caller has
// made it yet: a new database that copies from's files (cloneStatement), or, where from is gone
// and again is not nil, the files of what again answers (createFrom: OpenFilled's from is Open's
// template, the server's; OpenFrom's is template1, and takes none), runs with JIT off, is
// brought to what the template holds by fill, given its name and its connection string, and is
// then closed to connections. Each template it makes is the binary's own, which Run drops; Open's,
// the server's, is serverTemplate's. end and bounded are its maker's connect end, a test's
// (testdb.ConnectEnd) or RunEager's (testwait.MainGiveUpAt): they bound each of its admin
// statements' wait (adminExecUntil) and the connects of fill's connection string
// (testdb.WithConnectEnd). step names fill in fill's error ("<step> template database: …") and in
// the error a fill that ended its test leaves. A caller after the maker gets what the maker got:
// the template's database, or why it could not be made.
func makeTemplate(key string, end time.Time, bounded bool, from string, again func() (string, error), step string, fill func(name, databaseURL string) error) (string, error) {
	entry := templateEntry(key)
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
		if _, err := createFrom(from, again, func(from string) error {
			return adminExecUntil(end, bounded, cloneStatement(name, from))
		}); err != nil {
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
		if err := fill(name, connect); err != nil {
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
	connection, err := acquireAdminUntil(end, bounded)
	if err != nil {
		return err
	}
	defer connection.Release()
	_, err = connection.Exec(context.Background(), statement)
	return err
}

// acquireAdminUntil is adminExecUntil's wait for one of the admin pool's connections, with its
// bound and its error, for a caller that runs more than one statement on the connection
// (noSessionLeftOpen). The caller releases it.
func acquireAdminUntil(end time.Time, bounded bool) (*pgxpool.Conn, error) {
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
		return nil, fmt.Errorf("wait for an admin connection (waited %s of %s bound): %w", waited.Round(time.Millisecond), bound, err)
	}
	return connection, nil
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
// parsed (by Run before the tests, where DISPATCH_TEST_DATABASE_URL is set, or by m.Run), where a
// -parallel still wins.
func SetDefaultParallel() {
	if err := flag.Set("test.parallel", strconv.Itoa(DefaultParallel)); err != nil {
		panic("storetest: set the default -parallel: " + err.Error())
	}
}

// Main finds or makes Open's template on the server, runs the package's tests, then drops every
// template of the binary's own they were cloned from beside their databases' drops, and waits for
// all of them (Run). Open's template, the server's, stays for the next test process. A package
// whose tests call Open uses it as its TestMain: func TestMain(m *testing.M) {
// os.Exit(storetest.Main(m)) }. It is the default: each other template is made by the first test
// that asks for it, which pays for it. MainEager makes the filled templates before the tests too.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose tests also need another harness's Main, which runs m.Run
// itself: run runs the package's tests through that Main, and Run does what Main does before and
// after them. internal/dispatch/delivery shares one NATS server across its tests (testnats.Main):
// func TestMain(m *testing.M) { os.Exit(storetest.Run(func() int { return testnats.Main(m) })) }.
//
// Before run it finds or makes Open's template on the server DISPATCH_TEST_DATABASE_URL names
// (resolveOpen), so no test pays for it, and writes one line to stderr, naming the server by the
// URL's host and port alone: `storetest: Open's template <name> on <host:port> found in <d>`,
// `storetest: Open's template <name> on <host:port> made in <d>`, or `storetest: Open's template
// on <host:port> could not be made after <d>: <error>`. A template that could not be made does not
// stop the run: its entry keeps the error, and each test that asks for it fails with it. It uses
// the process's template connection alone (templateConn), which it opens, and opens no admin pool:
// the first test that asks opens that (ready), with its own connect end. Its connect end is
// testwait.MainGiveUpAt, the -timeout flag's deadline from the process's start less testwait's
// margin, which is no later than any test's (testdb.ConnectEnd): it bounds the template
// connection's connect, the wait for the build lock and the connects of the migrate's connection
// string (serverTemplate). go test's alarm starts only in m.Run, so a statement that hangs once it
// has its connection is bounded here only by cmd/go's kill; the time this takes is the binary's
// before the tests, which testdb.TeardownGrace's margin under that kill counts. It makes nothing,
// and the tests skip or fail as they would, where DISPATCH_TEST_DATABASE_URL is unset or cannot be
// parsed, and where the command line names -test.list: go test -list runs TestMain and no test,
// and packages/envoy/scripts/database-skips.sh runs it in CI (eagerServer). It reads the command
// line (-test.list, -test.timeout) before run, and so parses it, as m.Run would: a package whose
// TestMain calls SetDefaultParallel calls it before Run. So a run of one test by name, even one
// that opens no database, looks for Open's template, and makes it where the server holds none of
// this migration set for this login.
//
// After run its wait for the drops is bounded, as testdb.Run's is and for its reasons
// (testdb.TeardownDeadline: testdb.TeardownGrace past the -timeout, counted from the start of the
// tests, and five minutes from their end under -timeout 0). go test's alarm stops when the tests
// return, so a wait with no bound for a drop the server never answers held the binary until
// cmd/go's kill ("ran too long", which names no drop and no database), and for ever under
// -timeout 0 (TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered). No drop has a deadline of its
// own: a cancelled drop leaves its database on the server, as testdb.Run says. The drops of the
// binary's own templates, every one but Open's, start as soon as the tests return, beside the drops
// their cleanups started: a clone is a database of its own, so no template's drop waits on one.
// One wait, under the one bound, covers them all, so at the bound Run names every database whose
// drop is still in flight, templates among them, with the statement it ran; a template's drop that
// could start only after the clones' left every template unnamed and its drop unsent once a
// clone's drop reached the bound. It then reads once what the server's backends wait on
// (testdb.WaitingOn), and returns the tests' exit code with the failures of the drops that ended,
// since every test has reported by then. It leaves the admin pool open, because closing it waits
// for the connections those drops hold; the process's exit closes them, and a database whose drop
// the server never read stays on the server under the name Run printed.
//
// Where every drop ended and the process opened a template connection, it then writes one more
// line, read on that connection under the same bound (listSharedTemplates): `storetest: shared
// templates on <host:port> after this binary's tests: <names>`, each database on the server whose
// name begins with Open's scheme's prefix. Where a drop did not end it writes none.
func Run(run func() int) int { return runPackage(run, nil) }

// MainEager is Main for a package whose tests should not pay for its filled templates either:
// before the tests it resolves Open's template, as Main does, then makes each of filled's
// (Filled), as RunEager says. Such a package's TestMain is func TestMain(m *testing.M) {
// os.Exit(storetest.MainEager(m, filled...)) }, where filled are the Filled its tests pass to
// OpenFilled, or none.
func MainEager(m *testing.M, filled ...Filled) int { return RunEager(m.Run, filled...) }

// RunEager is Run, but before run, once Open's template is resolved, it also makes each of
// filled's templates, in order, each once, through the entries OpenFilled reads (makeTemplate), so
// the tests find them made and none pays for them. Under Main the first test that calls OpenFilled
// pays for its template; under RunEager TestMain pays it. The time moves from that test to
// TestMain and is not saved: it stays in the package's time, which go test prints beside the
// package. So a run of one test by name, even one that opens no database, pays for every template
// RunEager makes, where under Main it pays only for those it asks for. With no filled it is Run.
//
// Where filled names a template, it opens the admin pool before the tests with TestMain's connect
// end, as ready would with a test's (openAdmin), so the pool's new connections take it for the
// rest of the run; each of its admin statements' wait ends by it (adminExecUntil); and each Fill
// is given a connection string bounded by it (testdb.WithConnectEnd). With no filled, the first
// test that asks opens the pool, as under Main. After each filled template it writes one line to
// stderr: `storetest: template "<key>" made in <d>`, or `storetest: template "<key>" could not be
// made after <d>: <error>`. A template that could not be made does not stop the run: its entry
// keeps the error, and each test that asks for the template fails with it, as each test that asks
// for one whose first maker failed does under Main. It makes nothing where Run makes nothing.
func RunEager(run func() int, filled ...Filled) int { return runPackage(run, filled) }

// runPackage is what Run and RunEager do: before run, where a test could ask for a database
// (eagerServer), the step makeEagerly takes; then run; then the drops of the binary's own
// templates beside its databases', the wait for all of them under the teardown's bound, and the
// listing of the server's templates.
func runPackage(run func() int, filled []Filled) int {
	shared.main = true
	if raw, ok := eagerServer(); ok {
		makeEagerly(raw, filled)
	}
	started := time.Now()
	code := run()
	bound := testdb.TeardownDeadline(started)
	ended := true
	if shared.admin != nil {
		templates.Lock()
		names := make([]string, 0, len(templates.byKey))
		for key, entry := range templates.byKey {
			if entry.name != "" && key != openKey {
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
		if ended = awaitDrops(started, bound); ended {
			shared.admin.Close()
		}
	}
	if ended {
		listSharedTemplates(bound)
	}
	return withDropFails(code)
}

// eagerServer is the server Run resolves Open's template on, and RunEager makes its filled
// templates on, DISPATCH_TEST_DATABASE_URL, and false where they make none: the variable is unset
// or cannot be parsed, for which ready skips or fails each test that asks, or the command line
// names -test.list, under which m.Run lists the tests and runs none. It parses the command line
// when nothing has yet, as m.Run would.
func eagerServer() (string, bool) {
	raw, set, err := serverFromEnv()
	if !set || err != nil {
		return "", false
	}
	if !flag.Parsed() {
		flag.Parse()
	}
	list := flag.Lookup("test.list")
	return raw, list == nil || list.Value.String() == ""
}

// makeEagerly is the step Run and RunEager take before the tests, on the server raw names, under
// TestMain's connect end (testwait.MainGiveUpAt): it resolves Open's template (makeOpen) on the
// template connection alone, and, where filled names a template, opens the admin pool, as ready
// would, and makes each of filled's, writing one line to stderr for each. Where the admin pool
// could not be opened it makes none of filled's, and each line says why; ready then fails each
// test that asks with the same error, as under Main.
func makeEagerly(raw string, filled []Filled) {
	end, bounded := testwait.MainGiveUpAt()
	// Its error stays in Open's entry, for each test that asks; resolveOpen wrote its line.
	_, _ = makeOpen(end, bounded)
	if len(filled) == 0 {
		return
	}
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

// openAdmin opens the admin pool, on the server raw names, that every test's database and every
// template of the binary's own is created and dropped through, and keeps raw as the server's URL
// (shared.server), with no bound of this package's; Open's template, the server's, is made on the
// template connection instead (templateConn). The pool outlives whoever opens it, the first test
// that asks (ready) or RunEager where it makes filled templates, and makes connections for later
// tests, so a connection's connect timeout is not one fixed when the pool opened: each new
// connection a test's statement asks for takes the seconds left until end, the opener's connect
// end, as it is made (testdb.TimeoutUntil), so one made late ends on pgx's own error with
// testwait's margin left, not after go test's alarm.
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
