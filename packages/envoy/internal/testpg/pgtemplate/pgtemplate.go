// Package pgtemplate is the test-database harness the module's two stores share: Dispatch's
// (internal/dispatch/store/storetest, and internal/dispatch/store's own tests, which cannot import
// storetest) and the broker's (internal/broker/store/storetest). Each test gets a database of its
// own, a file copy of one migrated template, and that database is dropped once the test ends, so
// no test can be given a database another test wrote in. It imports neither store: a harness is
// handed its migration set's fingerprint and the function that migrates a database (New).
//
// The template is named for the migration set it carries (Template.Prefix and the set's
// fingerprint), built under an advisory lock and under a name of its own until it is whole, and
// never dropped: the test processes of one run start minutes apart, and a process that dropped it
// would take it from the ones still cloning. A set that gains, loses or edits a migration is a
// template of its own, so a stale one is never cloned - it is only left on the server. The
// Postgres services of .github/workflows/ci.yaml are containers deleted with the job; a server
// that outlives a run, the one packages/envoy/scripts/dev-postgres.sh runs, keeps one template per
// migration set a run there has tested, which `drop database` by hand reclaims (docs/testing.md).
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead: 6.4 MB of the
// 12.7 MB Dispatch template per clone, 16 GiB over one run of the api and docs packages. A file
// copy writes a few hundred bytes of log; Postgres forces a checkpoint before and after the copy,
// and the drop at the end of the test forces one more. What makes that affordable is the server
// rather than any machinery here: the Postgres services of .github/workflows/ci.yaml run with
// fsync, full_page_writes and synchronous_commit off, where a forced checkpoint writes nothing
// durable (dispatch://INFRA-1075).
//
// The harness changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to the
// whole server, which can hold other databases (the container dev-postgres.sh runs holds the local
// Dispatch's), and a change to them outlives the run.
//
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops before it returns, so no test database outlives the package. Dropping a database forces a
// checkpoint and waits for it, and every package testing against the same server queues on its
// one checkpointer: a test that waited for its own drop spent that queue's time on its clock.
package pgtemplate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Template names one migrated template on a server.
type Template struct {
	// Prefix opens the template's name, which the migration set's fingerprint ends, and the name
	// a build of it runs under (Prefix, "building_" and a random suffix), so that clearing what a
	// failed build left names no other database.
	Prefix string
	// LockKey is the advisory lock a process holds while it looks for the template or builds one
	// (Harness.lock).
	LockKey int64
}

// Dispatch and Broker are the module's two templates. Dispatch's has two harnesses, storetest's
// and the one internal/dispatch/store's own tests hold, since those tests cannot import
// storetest, which imports the store: declared once here, the two clone one template instead of
// each building its own. The keys differ because the two templates share nothing and need not
// queue on each other, and neither is a key a migration run takes inside the database it migrates.
var (
	Dispatch = Template{Prefix: "dispatch_template_", LockKey: 8150101}
	Broker   = Template{Prefix: "broker_template_", LockKey: 8330101}
)

// Harness hands one test process databases cloned from one template, and drops them. A package
// holds one, made by New, and its TestMain returns the harness's Main.
type Harness struct {
	template    Template
	fingerprint func() (string, error)
	migrate     func(ctx context.Context, url string) error

	// main is set by Main before the package's tests run: only Main waits for the drops.
	main bool

	adminOnce sync.Once
	adminErr  error
	server    url.URL
	admin     *pgxpool.Pool

	templateOnce sync.Once
	templateErr  error
	name         string // the template's, once prepare has found or built it

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
}

// New returns a harness of template. fingerprint names the migration set a build carries
// (store.MigrationsFingerprint). migrate applies that set to the database url names and closes
// every connection it opened before it returns: the build renames that database next, and
// Postgres refuses to rename a database another session is connected to.
func New(template Template, fingerprint func() (string, error), migrate func(ctx context.Context, url string) error) *Harness {
	return &Harness{template: template, fingerprint: fingerprint, migrate: migrate}
}

// Main runs the package's tests and waits for their databases to drop. A package whose tests take
// a database from h uses it as its TestMain: func TestMain(m *testing.M) { os.Exit(h.Main(m)) }.
// A drop that failed fails the run, naming the database.
//
// The template stays: it is named after the migration set and shared by every test process on the
// server (prepare), so a process that dropped it would take it from the processes still cloning
// it.
func (h *Harness) Main(m *testing.M) int {
	h.main = true
	code := m.Run()
	if h.admin == nil {
		return code
	}
	defer h.admin.Close()
	h.drops.Wait()
	for _, failure := range h.dropFails {
		fmt.Fprintf(os.Stderr, "pgtemplate: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// InMain reports whether Main runs this process's tests. Clone and Unmigrated fail a test without
// it, since only Main waits for the drops they start.
func (h *Harness) InMain() bool { return h.main }

// Clone returns the URL of a new database of t's own on the server raw names, a file copy of the
// migrated template, dropped once t ends. Its name is prefix and sixteen random hex digits: random
// because every test of every package sharing the server takes a name there. raw's database path
// is not used, since every statement here runs on the server's postgres database (openAdmin), and
// its query is kept. The first call finds the template, or builds it when no process has.
func (h *Harness) Clone(t testing.TB, raw, prefix string) string {
	t.Helper()
	h.open(t, raw)
	h.templateOnce.Do(func() { h.name, h.templateErr = h.prepare() })
	if h.templateErr != nil {
		t.Fatalf("prepare the migrated template database: %v", h.templateErr)
	}
	return h.copyOf(t, h.name, prefix)
}

// Unmigrated is Clone of template1 rather than of the template: a database carrying no migration,
// for a test of the migration runner itself. It neither looks for the template nor builds it.
func (h *Harness) Unmigrated(t testing.TB, raw, prefix string) string {
	t.Helper()
	h.open(t, raw)
	return h.copyOf(t, "template1", prefix)
}

// open opens the admin pool on the server raw names, once.
func (h *Harness) open(t testing.TB, raw string) {
	t.Helper()
	if !h.main {
		t.Fatal("pgtemplate: a test database needs the package's TestMain to return its harness's Main, which waits for the database's drop")
	}
	h.adminOnce.Do(func() { h.adminErr = h.openAdmin(raw) })
	if h.adminErr != nil {
		t.Fatalf("open the admin pool on the test Postgres server: %v", h.adminErr)
	}
}

// openAdmin opens the pool every create, drop and lock of the harness runs on. It connects to the
// server's postgres database: neither create database nor drop database may run against the
// database it names, and the template lock is taken in this database (lock).
func (h *Harness) openAdmin(raw string) error {
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, which carries a credential, so only
		// the error it wraps is kept.
		return fmt.Errorf("parse the server's URL: %w", errors.Unwrap(err))
	}
	h.server = *server
	config, err := pgxpool.ParseConfig(h.url("postgres"))
	if err != nil {
		return fmt.Errorf("parse the admin database URL: %w", err)
	}
	// Every clone and every drop runs on this pool, one of each per test rather than one per
	// package, so it is sized for the tests a package runs at once rather than for the drops
	// alone. It is still bounded: several package binaries test against the one server and each
	// holds a pool of its own.
	config.MaxConns = 8
	admin, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return fmt.Errorf("open the admin pool: %w", err)
	}
	h.admin = admin
	return nil
}

// copyOf makes a database of t's own named prefix and a random suffix, a file copy of source, and
// drops it in the background once t ends; Main waits for the drop.
func (h *Harness) copyOf(t testing.TB, source, prefix string) string {
	t.Helper()
	name := randomName(prefix)
	if _, err := h.admin.Exec(context.Background(), cloneStatement(name, source)); err != nil {
		t.Fatalf("copy %s into the test database %s: %v", source, name, err)
	}
	t.Cleanup(func() { h.drop(name) })
	return h.url(name)
}

// cloneStatement returns the create database that makes name a copy of source's files rather
// than a copy through the write-ahead log (the package comment says why).
func cloneStatement(name, source string) string {
	return "create database " + name + " template " + source + " strategy file_copy"
}

// dropWindow and dropPause bound the plain drop's retry: how long a drop is tried again while
// Postgres says the database is still in use, and the wait between tries.
const (
	dropWindow = 15 * time.Second
	dropPause  = 200 * time.Millisecond
)

// drop drops name without waiting for it; Main waits for every drop before it returns. Dropping a
// database forces a checkpoint and waits for it, which a test that waited would spend on its own
// clock (the package comment); drops in flight together share one checkpoint.
//
// `with (force)` terminates whatever is still connected, which needs pg_signal_backend: a login
// that is only the database's owner is refused `permission denied to terminate process`. A test's
// own connections close before this runs, since the cleanups that close them are registered after
// the clone and t.Cleanup runs the last registered first, but Postgres tears a backend down after
// the client has closed its socket, so a drop that lands inside that window finds a connection
// and, without that right, cannot clear it. So the drop is tried plainly and tried again while
// Postgres answers SQLSTATE 55006 (object_in_use), the code it gives whatever words it puts on the
// message, up to dropWindow; `force` is the last attempt, for a connection that really is
// somebody else's or one a test left open.
func (h *Harness) drop(name string) {
	h.drops.Go(func() {
		deadline := time.Now().Add(dropWindow)
		var err error
		for {
			if _, err = h.admin.Exec(context.Background(), "drop database "+name); err == nil {
				return
			}
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "55006" || time.Now().After(deadline) {
				break
			}
			time.Sleep(dropPause)
		}
		_, forced := h.admin.Exec(context.Background(), "drop database "+name+" with (force)")
		if forced == nil {
			return
		}
		// The forced drop is what ended the cleanup, so its error is the one reported; the
		// plain drop's says what the force was for.
		h.dropMu.Lock()
		h.dropFails = append(h.dropFails,
			fmt.Sprintf("drop the test database %s: %v, after the plain drop answered: %v", name, forced, err))
		h.dropMu.Unlock()
	})
}

// prepare finds the template every Clone copies: the one database named after this build's
// migration set, built here when no process has built it yet.
func (h *Harness) prepare() (string, error) {
	fingerprint, err := h.fingerprint()
	if err != nil {
		return "", err
	}
	name := h.template.Prefix + fingerprint
	ctx := context.Background()
	// One builder at a time, whatever set it builds: the lock also covers the databases a failed
	// build leaves behind, which the builder holding it clears.
	unlock, err := h.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	built, err := h.exists(ctx, name)
	if err != nil {
		return "", err
	}
	if !built {
		if err := h.build(ctx, name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// lock holds the template's LockKey on a connection of its own and returns the release. A
// session-level lock rather than a transaction's, because the build it covers runs on another
// connection, in transactions of its own.
//
// Postgres tags an advisory lock with the database of the session that takes it, so two sessions
// wait on one key only when both take it in one database. Every process takes this one on the
// server's postgres database, where the admin pool connects (openAdmin), and that is what makes
// every test process on the server queue on it: a process that took it in another database would
// wait for no other process, and the two could each build a template.
func (h *Harness) lock(ctx context.Context) (func(), error) {
	conn, err := h.admin.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire a connection for the template lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", h.template.LockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take the template lock: %w", err)
	}
	return func() {
		if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1)", h.template.LockKey); err != nil {
			fmt.Fprintf(os.Stderr, "pgtemplate: release the template lock: %v\n", err)
		}
		conn.Release()
	}, nil
}

func (h *Harness) exists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := h.admin.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("look for database %s: %w", name, err)
	}
	return exists, nil
}

// buildingPrefix opens the name a template is migrated under, before the rename that gives it the
// name the clones read.
func (h *Harness) buildingPrefix() string { return h.template.Prefix + "building_" }

// build migrates a database of its own and renames it to name, under the lock prepare holds. A
// template exists under its own name only once it carries every migration, so a build that fails
// or a process that dies mid-build leaves no half-migrated database for the next process to
// clone.
func (h *Harness) build(ctx context.Context, name string) error {
	if err := h.dropStaleBuilds(ctx); err != nil {
		return err
	}
	building := randomName(h.buildingPrefix())
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := h.admin.Exec(ctx, "create database "+building+" strategy file_copy"); err != nil {
		return fmt.Errorf("create the template database: %w", err)
	}
	if err := h.migrate(ctx, h.url(building)); err != nil {
		return fmt.Errorf("migrate the template database: %w", err)
	}
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := h.admin.Exec(ctx, "alter database "+building+" with allow_connections false"); err != nil {
		return fmt.Errorf("close the template database to connections: %w", err)
	}
	if _, err := h.admin.Exec(ctx, "alter database "+building+" rename to "+name); err != nil {
		return fmt.Errorf("name the migrated template %s: %w", name, err)
	}
	return nil
}

// dropStaleBuilds removes what a build that failed or a process that died mid-build left behind.
// It runs under the template lock, which no other build holds, so every such database is finished
// with.
func (h *Harness) dropStaleBuilds(ctx context.Context) error {
	// starts_with rather than `like $1 || '%'`: each `_` of the prefix is a LIKE wildcard, and
	// what this query selects is dropped with (force) on a server that may hold other databases.
	rows, err := h.admin.Query(ctx, "select datname from pg_database where starts_with(datname, $1)", h.buildingPrefix())
	if err != nil {
		return fmt.Errorf("look for half-built templates: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("look for half-built templates: %w", err)
	}
	for _, name := range stale {
		if _, err := h.admin.Exec(ctx, "drop database "+name+" with (force)"); err != nil {
			return fmt.Errorf("drop the half-built template %s: %w", name, err)
		}
	}
	return nil
}

// url is the server's URL pointed at database, its query kept.
func (h *Harness) url(database string) string {
	server := h.server
	server.Path = "/" + database
	return server.String()
}

// randomName is prefix and sixteen random hex digits.
func randomName(prefix string) string {
	var suffix [8]byte
	rand.Read(suffix[:]) // crypto/rand's Read never returns an error
	return prefix + hex.EncodeToString(suffix[:])
}
