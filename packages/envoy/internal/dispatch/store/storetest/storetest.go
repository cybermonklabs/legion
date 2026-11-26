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
//
// Each database's name carries the run of the binary that made it, and the binary holds a lock
// keyed by that run for as long as it runs (openAdmin). A database a binary leaves on the server,
// because its teardown stopped waiting for its drop (Run), is dropped by the next storetest
// binary on that server once that lock is free (sweepOrphans).
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
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
	// run names this binary's databases (randomName), and owner is the connection that holds
	// the lock keyed by it for as long as the binary runs (claimRun).
	run   string
	owner *pgx.Conn

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
	// pending are the databases whose drop has started and not returned, under dropMu.
	pending map[string]bool
	// sweepFails are what the sweep could not do, under dropMu; teardown reports them.
	sweepFails []string
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
	testcleanup.Add(t, func() { dropDatabase(name) })
	database, err := store.Open(ctx, databaseURL(shared.server, name))
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
	if _, err := shared.admin.Exec(context.Background(), "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	testcleanup.Add(t, func() { dropDatabase(name) })
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
		name := "dispatch_template_" + shared.run + "_" + suffix
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

// Main runs the package's tests, then drops every template they were cloned from and waits for
// the drops, the tests' own and the templates'. A package whose tests call Open uses it as its
// TestMain: func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }.
func Main(m *testing.M) int { return Run(m.Run) }

// Run is Main for a package whose tests also need another harness's Main, which runs m.Run
// itself: run runs the package's tests through that Main, and Run then does what Main does after
// them. internal/dispatch/delivery shares one NATS server across its tests (testnats.Main):
// func TestMain(m *testing.M) { os.Exit(storetest.Run(func() int { return testnats.Main(m) })) }.
//
// The teardown is bounded (teardownDeadline), because a drop can wait for a reason outside this
// process. DROP DATABASE waits until every backend of the server, in any database, has accepted
// the barrier it raises (wait event IPC/ProcSignalBarrier). Measured on PostgreSQL 18.6, a
// backend the machine does not run (stopped with SIGSTOP), or one blocked writing to a client
// that does not read (wait event Client/ClientWrite), holds every drop on the server for as long
// as it stays so. A cancel request ends that wait and leaves the database where it was, and pgx
// sends one when a statement's context ends; a drop whose client goes away without one goes on,
// and the server finishes it once the barrier is accepted. So no drop here has a deadline: each
// runs on context.Background(), and the bound is Run's alone.
//
// At the bound Run stops waiting, and the run fails. It writes a line for each database still
// being dropped (leftLine) and one naming the server's other client backends, among them what the
// drops wait on (othersLine), then a FAIL line, and returns 1: go test prints a package's output
// only when it fails, so a run that passed would hide the lines from every CI job. The process
// then ends without a cancel request: a drop the server is running is finished by the server, and
// a database whose drop never reached it (the drop waited for one of the admin pool's connections)
// stays until the next storetest binary on the server drops it (sweepOrphans). A drop that fails,
// and a database the sweep could not drop, is reported and fails the run too, bound or not
// (teardown).
func Run(run func() int) int {
	shared.main = true
	started := time.Now()
	code := run()
	if shared.admin == nil {
		return code
	}
	// Every test has ended, so no clone is made from a template again: the templates' drops
	// start now, beside whatever drops of the tests' own databases still run.
	for _, name := range templateNames() {
		dropDatabase(name)
	}
	code, done := teardown(os.Stderr, code, started, teardownDeadline(started))
	if done {
		// Not at the bound: the admin pool's Close waits for every connection it lent, and a
		// drop still running holds one. Then the process ends with the pool, and with the owner
		// connection, which frees this binary's run for the next sweep.
		shared.admin.Close()
		_ = shared.owner.Close(context.Background())
	}
	return code
}

// teardown waits until deadline for every drop, the tests' own and the templates', and returns
// the binary's exit code from the tests' code, and whether every drop returned. A drop still
// running at the deadline is written to w with what becomes of it (reportLeftDrops) and fails the
// run, with a FAIL line; so does each drop that failed and each database the sweep could not drop.
func teardown(w io.Writer, code int, started, deadline time.Time) (int, bool) {
	dropped := make(chan struct{})
	go func() {
		shared.drops.Wait()
		close(dropped)
	}()
	bound := time.NewTimer(time.Until(deadline))
	defer bound.Stop()
	done := true
	select {
	case <-dropped:
	case <-bound.C:
		done = false
	}
	shared.dropMu.Lock()
	failures := append(append([]string(nil), shared.dropFails...), shared.sweepFails...)
	shared.dropMu.Unlock()
	for _, failure := range failures {
		fmt.Fprintf(w, "storetest: %s\n", failure)
	}
	if len(failures) > 0 && code == 0 {
		code = 1
	}
	if !done {
		left := reportLeftDrops(w, time.Since(started))
		fmt.Fprintf(w, "storetest: FAIL: %d databases were still being dropped at the teardown's bound, %s after the tests began; the lines above say what becomes of each\n",
			left, time.Since(started).Round(time.Second))
		if code == 0 {
			code = 1
		}
	}
	return code, done
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

// reportLeftDrops writes to w Run's lines for the databases still being dropped at the teardown's
// bound, elapsed after the tests began: one for each (leftLine) and one for the server's other
// client backends (othersLine), from one read of the server (readActivity). It returns how many
// databases it wrote of.
func reportLeftDrops(w io.Writer, elapsed time.Duration) int {
	names := pendingDrops()
	read, err := readActivity(names)
	if err != nil {
		fmt.Fprintf(w, "storetest: drop database %s: still running at the teardown's bound, %s after the tests began; the server could not be read: %v\n",
			strings.Join(names, ", "), elapsed.Round(time.Second), err)
		return len(names)
	}
	for _, name := range names {
		fmt.Fprintln(w, leftLine(name, elapsed, read))
	}
	fmt.Fprintln(w, othersLine(read))
	return len(names)
}

// pendingDrops are the databases whose drop has started and not returned, by name.
func pendingDrops() []string {
	shared.dropMu.Lock()
	defer shared.dropMu.Unlock()
	names := make([]string, 0, len(shared.pending))
	for name := range shared.pending {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// leftLine is Run's line for name, a database still being dropped at the teardown's bound,
// elapsed after the tests began: what becomes of it, as the server shows it (read).
func leftLine(name string, elapsed time.Duration, read activity) string {
	head := fmt.Sprintf("storetest: drop database %s: still running at the teardown's bound, %s after the tests began", name, elapsed.Round(time.Second))
	if read.gone[name] {
		return head + ", and the database is gone: its drop ended as the bound passed"
	}
	if drop, running := read.drops[name]; running {
		return head + ", so the drop is left to the server, which finishes it once what it waits on lets it, since this process ends without a cancel request: the drop: " + drop.drop()
	}
	return head + ", and its drop is not on the server (it waits for one of the admin pool's connections, or between its tries), so the database stays until the next storetest binary on this server drops it"
}

// othersLine is Run's line naming the server's other client backends at the teardown's bound, by
// pid, database, state and wait, and not by their statements, which are other sessions' text. What
// the drops wait on is among them: a backend the machine does not run (it shows the state it
// stopped in), or one waiting on Client/ClientWrite for a client that does not read.
func othersLine(read activity) string {
	return "storetest: the server's other client backends at the teardown's bound, among them what the drops wait on: " + listOrNone(read.others)
}

// backend is one client backend of the server, as pg_stat_activity shows it.
type backend struct {
	pid                          int
	database, state, wait, query string
}

// String names b by its pid, database, state and wait, without its statement.
func (b backend) String() string {
	return fmt.Sprintf("pid %d in %q, %s, waiting on %s", b.pid, b.database, b.state, b.wait)
}

// drop names b, a backend running one of this binary's drops, with its statement too.
func (b backend) drop() string {
	return fmt.Sprintf("%s, %q", b, b.query)
}

// activity is what the server shows of the drops of some databases: the backend running each
// one's drop, the databases that are gone, and the server's other client backends.
type activity struct {
	drops  map[string]backend
	gone   map[string]bool
	others []backend
}

// readActivity reads pg_stat_activity and pg_database for the drops of names, on a connection of
// its own (the admin pool's may all be held by drops), bounded by readActivityBound. A backend is
// a name's drop while it runs one of dropStatements(name); one that ran it and is idle is not.
func readActivity(names []string) (activity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readActivityBound)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL(shared.server, "postgres"))
	if err != nil {
		return activity{}, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	dropping := map[string]string{}
	for _, name := range names {
		for _, statement := range dropStatements(name) {
			dropping[statement] = name
		}
	}
	rows, err := conn.Query(ctx, `select pid, coalesce(datname, ''), coalesce(state, ''),
		coalesce(wait_event_type || '/' || wait_event, 'nothing'), coalesce(query, '')
		from pg_stat_activity where backend_type = 'client backend' and pid <> pg_backend_pid() order by pid`)
	if err != nil {
		return activity{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	backends, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (backend, error) {
		var b backend
		err := row.Scan(&b.pid, &b.database, &b.state, &b.wait, &b.query)
		return b, err
	})
	if err != nil {
		return activity{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	read := activity{drops: map[string]backend{}, gone: map[string]bool{}}
	for _, b := range backends {
		if name, ok := dropping[b.query]; ok && b.state == "active" {
			read.drops[name] = b
			continue
		}
		read.others = append(read.others, b)
	}
	rows, err = conn.Query(ctx, "select datname from pg_database where datname = any($1)", names)
	if err != nil {
		return activity{}, fmt.Errorf("read pg_database: %w", err)
	}
	present, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return activity{}, fmt.Errorf("read pg_database: %w", err)
	}
	for _, name := range names {
		read.gone[name] = true
	}
	for _, name := range present {
		delete(read.gone, name)
	}
	return read, nil
}

// readActivityBound bounds readActivity.
const readActivityBound = 10 * time.Second

// dropStatements are the statements that drop name: dropDatabase's two and sweepOrphans' one.
func dropStatements(name string) []string {
	return []string{"drop database " + name, "drop database " + name + " with (force)", "drop database if exists " + name}
}

func listOrNone(backends []backend) string {
	if len(backends) == 0 {
		return "none"
	}
	lines := make([]string, len(backends))
	for i, b := range backends {
		lines[i] = b.String()
	}
	return strings.Join(lines, "; ")
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

// dropDatabase drops name without waiting for it; Run waits for every drop. Drops in flight
// together share one checkpoint. No drop has a deadline (Run says why).
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
	shared.dropMu.Lock()
	shared.pending[name] = true
	shared.dropMu.Unlock()
	shared.drops.Go(func() {
		defer func() {
			shared.dropMu.Lock()
			delete(shared.pending, name)
			shared.dropMu.Unlock()
		}()
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
			fmt.Sprintf("drop database %s: %v, after the plain drop answered: %v", name, forced, err))
		shared.dropMu.Unlock()
	})
}

// openAdmin claims this binary's run (claimRun), opens the admin pool, on the server raw names,
// that every database and template is created and dropped through, and starts the sweep of the
// databases on the server that gone binaries left (sweepInBackground). The run is claimed first,
// so it is held before any database of this binary is made.
func openAdmin(raw string) error {
	server, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	shared.server = *server
	ctx := context.Background()
	owner, run, err := claimRun(ctx)
	if err != nil {
		return err
	}
	config, err := pgxpool.ParseConfig(databaseURL(shared.server, "postgres"))
	if err != nil {
		_ = owner.Close(ctx)
		return fmt.Errorf("parse admin database URL: %w", err)
	}
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_ = owner.Close(ctx)
		return fmt.Errorf("open admin pool: %w", err)
	}
	shared.owner, shared.run, shared.admin = owner, run, admin
	shared.pending = map[string]bool{}
	go sweepInBackground()
	return nil
}

// claimRun opens the owner connection and takes on it the session advisory lock of a new run, a
// random name every database of this binary carries (randomName): the lock is held for as long as
// the connection is open, so a sweep never takes a database of a binary that runs (sweepOrphans).
// A run whose lock is already held is drawn again. The guarantee rests on the owner session alone:
// if the server ends it while the binary runs (a restart, an idle_session_timeout, a lost
// network), the lock goes with it, and another binary's sweep may then drop this binary's
// templates and databases. Nothing here takes the lock again; on such a server the tests fail on
// their next statement anyway.
func claimRun(ctx context.Context) (*pgx.Conn, string, error) {
	owner, err := pgx.Connect(ctx, databaseURL(shared.server, "postgres"))
	if err != nil {
		return nil, "", fmt.Errorf("open the owner connection: %w", err)
	}
	for range 3 {
		run, err := randomSuffix()
		if err != nil {
			_ = owner.Close(ctx)
			return nil, "", err
		}
		var held bool
		if err := owner.QueryRow(ctx, "select pg_try_advisory_lock($1)", runKey(run)).Scan(&held); err != nil {
			_ = owner.Close(ctx)
			return nil, "", fmt.Errorf("take the owner lock: %w", err)
		}
		if held {
			return owner, run, nil
		}
	}
	_ = owner.Close(ctx)
	return nil, "", errors.New("take the owner lock: the locks of three random runs were all held")
}

// runName matches the name of a database a storetest binary made: its kind, its binary's run and
// a random part of its own (randomName, OpenFrom). Postgres reads it as a regular expression too.
var runName = regexp.MustCompile(`^dispatch_(?:test|template)_([0-9a-f]{16})_[0-9a-f]{16}$`)

// runKey is the advisory lock key of run, sixteen hex digits (randomSuffix, runName): its eight
// bytes read as one int64, every bit of them kept.
func runKey(run string) int64 {
	var key [8]byte
	_, _ = hex.Decode(key[:], []byte(run))
	return int64(binary.BigEndian.Uint64(key[:]))
}

// sweepInBackground runs sweepOrphans on a connection of its own, so a drop that waits holds no
// admin connection and no test. What it could not do goes to shared.sweepFails, which teardown
// reports and counts. Nothing waits for it: a database it leaves is the next sweep's.
func sweepInBackground() {
	ctx := context.Background()
	fail := func(err error) {
		shared.dropMu.Lock()
		shared.sweepFails = append(shared.sweepFails, fmt.Sprintf("sweep the databases gone binaries left: %v", err))
		shared.dropMu.Unlock()
	}
	conn, err := pgx.Connect(ctx, databaseURL(shared.server, "postgres"))
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := sweepOrphans(ctx, conn); err != nil {
		fail(err)
	}
}

// sweepOrphans drops, through conn and one at a time, every database on the server that a
// storetest binary made (runName) and left: one whose run's lock no session holds, so its binary
// is gone. Each binary holds its run's lock from before its first database is made until it ends
// (claimRun). The drop is `if exists`, so a database another sweep removed first costs nothing,
// and one whose drop the server still runs is waited for. It returns the databases it dropped.
func sweepOrphans(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, "select datname from pg_database where datname ~ $1 order by datname", runName.String())
	if err != nil {
		return nil, fmt.Errorf("list the databases: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the databases: %w", err)
	}
	gone := map[string]bool{}
	var dropped []string
	var failures []error
	for _, name := range names {
		run := runName.FindStringSubmatch(name)[1]
		if run == shared.run {
			continue
		}
		free, read := gone[run]
		if !read {
			if err := conn.QueryRow(ctx, "select case when pg_try_advisory_lock($1) then pg_advisory_unlock($1) else false end", runKey(run)).Scan(&free); err != nil {
				return dropped, fmt.Errorf("read the lock of run %s: %w", run, err)
			}
			gone[run] = free
		}
		if !free {
			continue
		}
		if _, err := conn.Exec(ctx, "drop database if exists "+name); err != nil {
			failures = append(failures, fmt.Errorf("drop %s: %w", name, err))
			continue
		}
		dropped = append(dropped, name)
	}
	return dropped, errors.Join(failures...)
}

func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

// randomName is a new database name of the kind prefix names: prefix, this binary's run and a
// random part (runName).
func randomName(t testing.TB, prefix string) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return prefix + shared.run + "_" + suffix
}

func randomSuffix() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return hex.EncodeToString(suffix[:]), nil
}
