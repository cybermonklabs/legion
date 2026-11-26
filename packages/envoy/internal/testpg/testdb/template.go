package testdb

import (
	"context"
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

	"github.com/sjawhar/envoy/internal/testwait"
)

// Server is the name of a variable that names the Postgres server a lane gives the tests. Only a
// fixture reads it, and no test is handed the database the variable's URL names: every test gets a
// database of its own on that server (Database, or a Template's Clone), so two test binaries
// sharing the server never see each other's rows.
type Server string

const (
	// DispatchServer is the server Dispatch's tests run against, and the one testpg.URL takes a
	// database from when it is set.
	DispatchServer Server = "DISPATCH_TEST_DATABASE_URL"
	// BrokerServer is the server the broker's tests run against.
	BrokerServer Server = "BROKER_TEST_DATABASE_URL"
	// LegionServer is the test server of a Legion pod and of CI's test job, which the daemon's
	// tests and the load tests run against.
	LegionServer Server = "LEGION_TEST_PG_DSN"
)

// Available reports whether the variable is set. It is for a test that makes no database itself
// and skips where the tests that do would skip.
func (s Server) Available() bool {
	return os.Getenv(string(s)) != ""
}

// Database creates an empty database of t's own on the server s names, drops it when t ends, and
// returns its URL (DatabaseOn, run on that server's postgres database). It skips t when the
// variable is unset.
func (s Server) Database(t testing.TB, prefix string) string {
	t.Helper()
	server := s.url(t)
	return DatabaseOn(t, databaseURL(server, "postgres"), prefix)
}

// AdminURL is the URL of an empty database of t's own on the server s names (Database), for a
// test that reads or cleans up what spans the cluster (a role, pg_roles) rather than what one
// database holds: a role is seen and dropped from any database of the server. A test takes it
// before it makes its other databases, so it outlives them: its drop is registered first, and so
// runs last. It carries the same connect bound as Database's, and skips t when the variable is
// unset.
func (s Server) AdminURL(t testing.TB) string {
	t.Helper()
	return s.Database(t, "admin")
}

// url is the server the variable names, skipping t when it is unset, with connect_timeout set to
// t's deadline (WithConnectDeadline), so every database, clone and pool made from it carries the
// binary's deadline as its connect bound. The variable's database path is never used: statements
// that make or drop a database run on the server's postgres database.
func (s Server) url(t testing.TB) url.URL {
	t.Helper()
	return s.urlAt(t, time.Now())
}

// urlAt is url with the time left before t's deadline counted from now, so a test of it names
// the moment rather than reading the clock.
func (s Server) urlAt(t testing.TB, now time.Time) url.URL {
	t.Helper()
	raw := os.Getenv(string(s))
	if raw == "" {
		t.Skip(string(s) + " must be set to run Postgres-backed tests")
	}
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		t.Fatalf("parse %s: %v", s, errors.Unwrap(err))
	}
	setParameter(server, "connect_timeout", connectTimeout(t, now))
	return *server
}

// Template is a database a test binary migrates once, on its first Clone, and copies for every
// test that asks for a migrated database of its own. Running every migration is most of the
// Postgres work such a test would otherwise do, so a package's wall time would scale with its test
// count times the speed of the Postgres it runs against; a clone costs one create database. A test
// whose database another migrate brings to its schema clones a template of that migrate's key
// instead (CloneFrom), made the same way beside the Template's own, once per key.
//
// A clone copies the template's files (strategy file_copy). Postgres's default from 15, strategy
// wal_log, writes every page of the template into the write-ahead log instead: 6.4 MB of
// Dispatch's 12.7 MB template per clone, 16 GiB over one run of its api and docs packages. A file
// copy writes a few hundred bytes of log; Postgres forces a checkpoint before and after the copy.
//
// The template changes no Postgres setting: fsync, full_page_writes and max_wal_size belong to
// the whole server, which can hold other databases (the one packages/envoy/scripts/dev-postgres.sh
// runs holds the local Dispatch's), and a change to them outlives the run.
//
// A test's database is dropped in the background once the test ends, and Main waits for the
// drops. Dropping a database forces a checkpoint and waits for it, and every package testing
// against the same server queues on its one checkpointer: a test that waited for its own drop
// spent that queue's time on its clock.
type Template struct {
	server   Server
	prefix   string
	mainCall string
	migrate  func(ctx context.Context, databaseURL string) error

	// main is set by Main before the package's tests run; Clone refuses to run without it, since
	// only Main drops the templates and waits for the drops of the clones.
	main      bool
	once      sync.Once
	err       error
	serverURL url.URL
	admin     *pgxpool.Pool

	// own is the template Clone copies, and keyed each one CloneFrom copies, by key; each is made by
	// the first test that asks for it, and Main drops every one that was made.
	own     madeTemplate
	keyedMu sync.Mutex
	keyed   map[string]*madeTemplate

	otherOnce sync.Once
	otherErr  error
	other     string

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
}

// madeTemplate is one template database: once makes it, name is its database once it exists, and
// err is why it could not be made.
type madeTemplate struct {
	once sync.Once
	name string
	err  error
}

// NewTemplate is a Template on the server server names. prefix starts the name of the template
// and of every database made from it (<prefix>_template_<hex>, <prefix>_test_<hex>); mainCall is
// the TestMain a package whose tests clone it needs, which Clone names when the package lacks it;
// migrate brings the empty database databaseURL names to the schema every clone starts with.
func NewTemplate(server Server, prefix, mainCall string, migrate func(ctx context.Context, databaseURL string) error) *Template {
	return &Template{server: server, prefix: prefix, mainCall: mainCall, migrate: migrate}
}

// Clone creates a database of t's own from the template, drops it after t ends, and returns its
// URL: the variable's URL with its database replaced and its query kept, connect_timeout set as
// the binary's first Clone found it (Server's url). The binary's first Clone makes and migrates
// the template. It skips t when the variable is unset.
func (tp *Template) Clone(t testing.TB) string {
	t.Helper()
	return tp.clone(t, tp.template(t, &tp.own, tp.migrate))
}

// CloneFrom is Clone for a database migrate brings to its schema instead of the Template's own
// migrate: it creates a database of t's own from the template of key, drops it after t ends, and
// returns its URL. The template of a key is made once in the test binary, by the first test that
// asks: a new empty database whose URL migrate is given, which migrate migrates and leaves with no
// connection open. Main drops it beside the Template's own. A template whose migrate failed, or
// ended its test, fails every test that asks for it again. It skips t when the variable is unset.
func (tp *Template) CloneFrom(t testing.TB, key string, migrate func(ctx context.Context, databaseURL string) error) string {
	t.Helper()
	tp.keyedMu.Lock()
	if tp.keyed == nil {
		tp.keyed = map[string]*madeTemplate{}
	}
	entry := tp.keyed[key]
	if entry == nil {
		entry = &madeTemplate{}
		tp.keyed[key] = entry
	}
	tp.keyedMu.Unlock()
	return tp.clone(t, tp.template(t, entry, migrate))
}

// clone creates a database of t's own from the template database named template, drops it after t
// ends, and returns its URL.
func (tp *Template) clone(t testing.TB, template string) string {
	t.Helper()
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	name := tp.prefix + "_test_" + suffix
	if _, err := tp.admin.Exec(context.Background(), cloneStatement(name, template)); err != nil {
		t.Fatalf("clone isolated database from template %s: %v", template, err)
	}
	// Registered before the caller opens a pool on the clone, so it runs after that pool's close.
	t.Cleanup(func() { tp.drop(name) })
	return databaseURL(tp.serverURL, name)
}

// template is the name of entry's template database, which the binary's first call for entry makes
// with migrate (makeTemplate). It fails t when that could not be made.
func (tp *Template) template(t testing.TB, entry *madeTemplate, migrate func(ctx context.Context, databaseURL string) error) string {
	t.Helper()
	tp.ready(t)
	entry.once.Do(func() {
		// Set first, and replaced only once makeTemplate returns, so a migrate that ends its test
		// (t.Fatal) leaves a template no later test clones.
		entry.err = errors.New("the test that made it ended before its migrate returned")
		entry.err = tp.makeTemplate(entry, migrate)
	})
	if entry.err != nil {
		t.Fatalf("prepare migrated template database: %v", entry.err)
	}
	return entry.name
}

// Other is the URL of a second database of the binary's own on the template's server: empty, made
// on the first call and dropped by Main. It is for a test that needs a database beside its clone,
// such as one that holds a transaction elsewhere on the instance. Every test of the binary that
// asks gets the same one, so a test writes nothing there.
func (tp *Template) Other(t testing.TB) string {
	t.Helper()
	tp.ready(t)
	tp.otherOnce.Do(func() {
		suffix, err := randomSuffix()
		if err != nil {
			tp.otherErr = err
			return
		}
		name := tp.prefix + "_other_" + suffix
		if _, err := tp.admin.Exec(context.Background(), "create database "+name+" strategy file_copy"); err != nil {
			tp.otherErr = fmt.Errorf("create the binary's other database: %w", err)
			return
		}
		tp.other = name
	})
	if tp.otherErr != nil {
		t.Fatalf("%v", tp.otherErr)
	}
	return databaseURL(tp.serverURL, tp.other)
}

// ready fails t when the package's TestMain does not run Main, skips t when the variable is unset,
// and otherwise opens the admin pool on the binary's first call.
func (tp *Template) ready(t testing.TB) {
	t.Helper()
	if !tp.main {
		t.Fatal("this database needs the package's TestMain to be: " + tp.mainCall)
	}
	server := tp.server.url(t)
	tp.once.Do(func() { tp.err = tp.openAdmin(server) })
	if tp.err != nil {
		t.Fatalf("open the admin pool: %v", tp.err)
	}
}

// cloneStatement returns the create database that makes name a copy of template's files rather
// than a copy through the write-ahead log (Template says why).
func cloneStatement(name, template string) string {
	return "create database " + name + " template " + template + " strategy file_copy"
}

// Main runs the package's tests through run, waits for their databases to drop, then drops the
// binary's other database and every template that was made, and returns run's code, made non-zero
// by a drop that failed. A package whose tests clone the template returns it from TestMain.
func (tp *Template) Main(run func() int) int {
	tp.main = true
	code := run()
	if tp.admin == nil {
		return code
	}
	defer tp.admin.Close()
	if tp.other != "" {
		tp.drop(tp.other)
	}
	tp.drops.Wait()
	for _, failure := range tp.dropFails {
		fmt.Fprintf(os.Stderr, "testdb: %s\n", failure)
		if code == 0 {
			code = 1
		}
	}
	templates := []string{tp.own.name}
	tp.keyedMu.Lock()
	for _, entry := range tp.keyed {
		templates = append(templates, entry.name)
	}
	tp.keyedMu.Unlock()
	for _, name := range templates {
		if name == "" {
			continue
		}
		// A template takes no connection (makeTemplate closes it to them), so its forced drop ends
		// nothing, and dropDatabase waits out a backend of its migration that has not exited yet.
		if err := dropDatabase(tp.admin, name, testwait.InMain); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: drop template database %s: %v\n", name, err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// SQLSTATE codes a drop reads: Postgres refuses to end another role's backend to a login without
// pg_signal_backend (insufficient_privilege), and refuses a drop of a database a backend is still
// connected to (object_in_use), whatever words it puts on either message.
const (
	insufficientPrivilege = "42501"
	objectInUse           = "55006"
)

// execer is what dropDatabase needs of a connection or a pool: one statement.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// dropDatabase drops name through admin. It is every drop of a test database here: a Template's of
// each clone, of the binary's other database and of each template itself, and, through
// DropDatabaseOn, DatabaseOn's and a drop of a database another package's tests made themselves.
//
// `with (force)` ends whatever is still connected. A test's own pool is closed before its drop
// runs (DatabaseOn's and Clone's cleanups run after the caller's), but Postgres tears a backend
// down after its client has closed the socket, so a drop can land while one is still going. The
// forced drop waits only a few seconds for the backends it ended to exit, and answers
// object_in_use while one has not, which a loaded machine reaches. Ending a backend of another
// role needs pg_signal_backend, which a login that only owns the database lacks; Postgres refuses
// such a login, and the drop is then asked plainly, which Postgres refuses with object_in_use for
// as long as any connection is still there.
//
// So the drop is asked again for as long as Postgres answers object_in_use, through wait: the
// caller's shared wait, testwait.InMain in Main and testwait.Eventually in a test's cleanup
// (WaitIn), which gives up only at the test binary's deadline less testwait's margin. Any other
// answer is final.
//
// `if exists`, because a cleanup the test registered later runs first and may have dropped the
// database already: the cleanup of a role the test created there drops the database before the
// role, which nothing in the database then holds (internal/dispatch/store's
// dropRoleAfterTestDatabase).
func dropDatabase(admin execer, name string, wait func(what string, holds func() bool) error) error {
	ctx := context.Background()
	statement := "drop database if exists " + name + " with (force)"
	var err, forced error
	waited := wait("every backend of database "+name+" to exit, for its drop", func() bool {
		_, err = admin.Exec(ctx, statement)
		if forced == nil && sqlState(err) == insufficientPrivilege {
			forced, statement = err, "drop database if exists "+name
			_, err = admin.Exec(ctx, statement)
		}
		return sqlState(err) != objectInUse
	})
	if waited != nil {
		err = fmt.Errorf("%w; Postgres last answered: %v", waited, err)
	}
	if err != nil && forced != nil {
		err = fmt.Errorf("%w, after the forced drop answered: %v", err, forced)
	}
	return err
}

// DropDatabaseOn drops the database name on the server adminURL names, over a connection of its
// own to adminURL, as every drop here is made (dropDatabase): `with (force)`, asked again through
// wait for as long as Postgres answers object_in_use. DatabaseOn's cleanup drops its database
// here, and so does a package whose tests make databases of their own on the server outside
// DatabaseOn and Template (internal/dispatch/store's migrated templates and their copies). wait is
// the caller's shared wait: testwait.InMain in TestMain, and WaitIn(t) in a test's cleanup. A
// failure names the server's host and the database, never adminURL, which carries a credential.
func DropDatabaseOn(adminURL, name string, wait func(what string, holds func() bool) error) error {
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		return fmt.Errorf("parse the admin URL of the Postgres server: %w", errors.Unwrap(err))
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to the Postgres server at %s: %w", server.Host, err)
	}
	defer conn.Close(ctx)
	return dropDatabase(conn, name, wait)
}

// WaitIn is the wait dropDatabase and DropDatabaseOn take, in t: the shared wait
// (testwait.Eventually), which fails t when it gives up at the test binary's deadline less
// testwait's margin, so the func it returns returns nil whenever it returns. It runs on the
// goroutine that calls it, which must be t's own, as a test's cleanup is.
func WaitIn(t testing.TB) func(what string, holds func() bool) error {
	return func(what string, holds func() bool) error {
		testwait.Eventually(t, what, holds)
		return nil
	}
}

// drop drops name without waiting for it (dropDatabase); Main waits for every drop. Drops in
// flight together share one checkpoint.
func (tp *Template) drop(name string) {
	tp.drops.Go(func() {
		err := dropDatabase(tp.admin, name, testwait.InMain)
		if err == nil {
			return
		}
		tp.dropMu.Lock()
		tp.dropFails = append(tp.dropFails, fmt.Sprintf("drop isolated database %s: %v", name, err))
		tp.dropMu.Unlock()
	})
}

// openAdmin opens the admin pool, on the postgres database of server, that every database and
// template is created and dropped through.
func (tp *Template) openAdmin(server url.URL) error {
	tp.serverURL = server
	config, err := pgxpool.ParseConfig(databaseURL(server, "postgres"))
	if err != nil {
		return fmt.Errorf("parse admin database URL: %w", err)
	}
	// Drops run concurrently on this pool. Its default size is the CPU count, and on a large
	// machine the packages testing against one server at once would exceed its connection limit.
	config.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	tp.admin = admin
	return nil
}

// makeTemplate makes entry's template database: a file copy of template1, named in entry once it
// exists, so Main drops exactly the templates that were made, then migrated by migrate and closed
// to connections.
func (tp *Template) makeTemplate(entry *madeTemplate, migrate func(ctx context.Context, databaseURL string) error) error {
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := tp.prefix + "_template_" + suffix
	ctx := context.Background()
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := tp.admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	entry.name = name
	if err := migrate(ctx, databaseURL(tp.serverURL, name)); err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := tp.admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
		return fmt.Errorf("close template database to connections: %w", err)
	}
	return nil
}

// sqlState is the SQLSTATE of a Postgres error, and "" for nil or any other error.
func sqlState(err error) string {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		return pgError.Code
	}
	return ""
}
