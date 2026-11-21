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
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to the
// whole server DatabaseURLEnv names, which can hold other databases (the one
// packages/envoy/scripts/dev-postgres.sh runs holds the local Dispatch's), and a change to them
// outlives the run.
//
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops. Dropping a database forces a checkpoint and waits for it, and every package testing
// against the same server queues on its one checkpointer: a test that waited for its own drop
// spent that queue's time on its clock.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// asks for it (OpenFrom).
func ready(t testing.TB) {
	t.Helper()
	if !shared.main {
		t.Fatal("storetest.Open needs the package's TestMain to be: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }, or storetest.Run around another harness's Main")
	}
	raw := os.Getenv(DatabaseURLEnv)
	if raw == "" {
		t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")
	}
	shared.once.Do(func() { shared.err = openAdmin(raw) })
	if shared.err != nil {
		t.Fatalf("open the admin pool: %v", shared.err)
	}
}

// clone returns a store on a new database cloned from template, dropped after t ends.
func clone(t testing.TB, template string) *store.Store {
	t.Helper()
	ctx := context.Background()
	name := randomName(t, "dispatch_test_")
	if _, err := shared.admin.Exec(ctx, cloneStatement(name, template)); err != nil {
		t.Fatalf("clone isolated database from template %s: %v", template, err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	database, err := store.Open(ctx, databaseURL(shared.server, name))
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// Empty returns the connection string of a new database that holds nothing, not even the
// version table, dropped after t ends: for a test that migrates it itself, as the two-build
// suite has the builds it runs migrate theirs, in production's order. It makes no template.
func Empty(t testing.TB) string {
	t.Helper()
	ready(t)
	name := randomName(t, "dispatch_test_")
	if _, err := shared.admin.Exec(context.Background(), "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	t.Cleanup(func() { dropDatabase(name) })
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
// with no connection open. A template whose migrate failed, or ended its test, fails every test
// that asks for it again.
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
		ctx := context.Background()
		// The template is a file copy of template1, for the reason each clone of it is one.
		if _, err := shared.admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
			entry.err = fmt.Errorf("create template database: %w", err)
			return
		}
		// Named once it exists, so Run drops exactly the templates that were made.
		entry.name = name
		if err := migrate(databaseURL(shared.server, entry.name)); err != nil {
			entry.err = fmt.Errorf("migrate template database: %w", err)
			return
		}
		// Postgres refuses to clone a database another session is connected to. Refusing
		// connections to the template keeps any stray session from making a clone wait or fail.
		if _, err := shared.admin.Exec(ctx, "alter database "+entry.name+" with allow_connections false"); err != nil {
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

// Main runs the package's tests, waits for their databases to drop, then drops every template
// they were cloned from. A package whose tests call Open uses it as its TestMain:
// func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose tests also need another harness's Main, which runs m.Run
// itself: run runs the package's tests through that Main, and Run then does what Main does after
// them. internal/dispatch/delivery shares one NATS server across its tests (testnats.Main):
// func TestMain(m *testing.M) { os.Exit(storetest.Run(func() int { return testnats.Main(m) })) }.
//
// The teardown is bounded (teardownDeadline), because a drop can wait for a reason outside this
// process. DROP DATABASE waits until every backend of the server, in any database, has accepted
// the barrier it raises (wait event IPC/ProcSignalBarrier), and neither statement_timeout nor a
// cancel request ends that wait: measured on PostgreSQL 18.6, a drop with a 25 s
// statement_timeout went on waiting for the whole nine minutes that one backend of another
// database stayed stopped. So a backend that cannot run holds every drop on the server for as
// long as it cannot. A drop still waiting at the bound is left to the server: Run stops waiting,
// names the database and what its drop waits on, so a database the server keeps can be dropped by
// hand, and keeps the tests' exit code, since every test has reported by then and the wait is
// another backend's. A drop that fails is still reported, and fails the run, as before.
func Run(run func() int) int {
	shared.main = true
	started := time.Now()
	code := run()
	if shared.admin == nil {
		return code
	}
	ctx, cancel := context.WithDeadline(context.Background(), teardownDeadline(started))
	defer cancel()
	dropped := make(chan struct{})
	go func() {
		shared.drops.Wait()
		close(dropped)
	}()
	select {
	case <-dropped:
	case <-ctx.Done():
		// The templates' drops would wait on the same server, so none is started. The admin pool
		// is not closed: Close waits for every connection it lent, and the drops still in flight
		// hold theirs. The process ends with them.
		fmt.Fprintf(os.Stderr, "storetest: the tests' own databases were still being dropped at the teardown's bound, %s after the tests began; those drops are left to the server, and the templates %s are not dropped: %s\n",
			time.Since(started).Round(time.Second), strings.Join(templateNames(), ", "), waitingOn("drop database dispatch_test_"))
		return code
	}
	defer shared.admin.Close()
	for _, failure := range shared.dropFails {
		fmt.Fprintf(os.Stderr, "storetest: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	names := templateNames()
	for _, name := range names {
		if _, err := shared.admin.Exec(ctx, "drop database "+name); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "storetest: drop template database %s: still waiting at the teardown's bound, %s after the tests began, so the drop is left to the server: %s\n",
					name, time.Since(started).Round(time.Second), waitingOn("drop database "+name))
				continue
			}
			fmt.Fprintf(os.Stderr, "storetest: drop template database %s: %v\n", name, err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// templateNames are the databases of the templates this test binary made.
func templateNames() []string {
	templates.Lock()
	defer templates.Unlock()
	names := make([]string, 0, len(templates.byKey))
	for _, entry := range templates.byKey {
		if entry.name != "" {
			names = append(names, entry.name)
		}
	}
	return names
}

// teardownGrace and teardownUnbounded set Run's bound. cmd/go kills a test binary one minute past
// its -timeout ("ran too long"), and the binary's own alarm stops when m.Run returns, so nothing
// else ends a teardown that waits: the teardown ends teardownGrace past the -timeout, inside that
// minute. A binary run with -timeout 0 has no kill to stay inside, and gets teardownUnbounded
// from the end of its tests.
const (
	teardownGrace     = 30 * time.Second
	teardownUnbounded = 5 * time.Minute
)

// teardownDeadline is when Run's teardown gives up, for a test binary whose tests began at
// started.
func teardownDeadline(started time.Time) time.Time {
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		if timeout, ok := timeoutFlag.Value.(flag.Getter).Get().(time.Duration); ok && timeout > 0 {
			return started.Add(timeout + teardownGrace)
		}
	}
	return time.Now().Add(teardownUnbounded)
}

// waitingOn says what the server's backend running the statement that begins with prefix waits on,
// and lists the server's other client backends, by pid, database, state and wait, from a
// pg_stat_activity read on a connection of its own (the admin pool's may all be held by drops),
// bounded by waitingOnBound. A backend that holds a drop is among them, though nothing the server
// shows says which.
func waitingOn(prefix string) string {
	ctx, cancel := context.WithTimeout(context.Background(), waitingOnBound)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL(shared.server, "postgres"))
	if err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `select pid, coalesce(datname, ''), coalesce(state, ''), coalesce(wait_event_type, ''),
		coalesce(wait_event, ''), left(coalesce(query, ''), 60), starts_with(coalesce(query, ''), $1)
		from pg_stat_activity where backend_type = 'client backend' and pid <> pg_backend_pid() order by pid`, prefix)
	if err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	defer rows.Close()
	var drops, others []string
	for rows.Next() {
		var pid int
		var database, state, waitType, wait, query string
		var isDrop bool
		if err := rows.Scan(&pid, &database, &state, &waitType, &wait, &query, &isDrop); err != nil {
			return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
		}
		line := fmt.Sprintf("pid %d in %q, %s, waiting on %s/%s, %q", pid, database, state, waitType, wait, query)
		if isDrop {
			drops = append(drops, line)
		} else {
			others = append(others, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	return fmt.Sprintf("the drop: %s; the server's other client backends: %s", listOrNone(drops), listOrNone(others))
}

// waitingOnBound bounds waitingOn's read of pg_stat_activity.
const waitingOnBound = 10 * time.Second

func listOrNone(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

// dropDatabase drops name without waiting for it; Main waits for every drop. Drops in flight
// together share one checkpoint.
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
	shared.drops.Go(func() {
		deadline := time.Now().Add(dropWindow)
		var err error
		for {
			if _, err = shared.admin.Exec(context.Background(), "drop database "+name); err == nil {
				return
			}
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "55006" || time.Now().After(deadline) {
				break
			}
			time.Sleep(dropPause)
		}
		_, forced := shared.admin.Exec(context.Background(), "drop database "+name+" with (force)")
		if forced == nil {
			return
		}
		// The forced drop is what ended the cleanup, so its error is the one reported; the
		// plain drop's says what the force was for.
		shared.dropMu.Lock()
		shared.dropFails = append(shared.dropFails,
			fmt.Sprintf("drop isolated database %s: %v, after the plain drop answered: %v", name, forced, err))
		shared.dropMu.Unlock()
	})
}

// openAdmin opens the admin pool, on the server raw names, that every database and template is
// created and dropped through.
func openAdmin(raw string) error {
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
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	shared.admin = admin
	return nil
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
