package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

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

// AdminURL is the URL of the postgres database of the server s names, for a test that reads or
// cleans up what spans the cluster (a role, pg_roles) rather than what one database holds. It
// carries the same connect bound as Database's, and skips t when the variable is unset.
func (s Server) AdminURL(t testing.TB) string {
	t.Helper()
	return databaseURL(s.url(t), "postgres")
}

// url is the server the variable names, skipping t when it is unset, with connect_timeout set to
// t's deadline (WithConnectDeadline), so every database, clone and pool made from it carries the
// binary's deadline as its connect bound. The variable's database path is never used: statements
// that make or drop a database run on the server's postgres database.
func (s Server) url(t testing.TB) url.URL {
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
	setParameter(server, "connect_timeout", connectTimeout(t))
	return *server
}

// Template is a database a test binary migrates once, on its first Clone, and copies for every
// test that asks for a migrated database of its own. Running every migration is most of the
// Postgres work such a test would otherwise do, so a package's wall time would scale with its test
// count times the speed of the Postgres it runs against; a clone costs one create database.
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
	// only Main drops the template and waits for the drops of the clones.
	main      bool
	once      sync.Once
	err       error
	serverURL url.URL
	admin     *pgxpool.Pool
	name      string

	otherOnce sync.Once
	otherErr  error
	other     string

	drops     sync.WaitGroup
	dropMu    sync.Mutex
	dropFails []string
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
	tp.ready(t)
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	name := tp.prefix + "_test_" + suffix
	if _, err := tp.admin.Exec(context.Background(), tp.cloneStatement(name)); err != nil {
		t.Fatalf("clone isolated database from template: %v", err)
	}
	// Registered before the caller opens a pool on the clone, so it runs after that pool's close.
	t.Cleanup(func() { tp.drop(name) })
	return databaseURL(tp.serverURL, name)
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
// and otherwise makes the template on the binary's first call.
func (tp *Template) ready(t testing.TB) {
	t.Helper()
	if !tp.main {
		t.Fatal("this database needs the package's TestMain to be: " + tp.mainCall)
	}
	server := tp.server.url(t)
	tp.once.Do(func() { tp.err = tp.prepare(server) })
	if tp.err != nil {
		t.Fatalf("prepare migrated template database: %v", tp.err)
	}
}

// cloneStatement returns the create database that makes name a copy of the template's files
// rather than a copy through the write-ahead log (Template says why).
func (tp *Template) cloneStatement(name string) string {
	return "create database " + name + " template " + tp.name + " strategy file_copy"
}

// Main runs the package's tests through run, waits for their databases to drop, then drops the
// binary's other database and the template, and returns run's code, made non-zero by a drop that
// failed. A package whose tests clone the template returns it from TestMain.
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
	if tp.name == "" {
		return code
	}
	// The template takes no connection (prepare closes it to them), so its forced drop ends
	// nothing, and dropDatabase waits out a backend of its migration that has not exited yet.
	if err := dropDatabase(tp.admin, tp.name, testwait.InMain); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: drop template database %s: %v\n", tp.name, err)
		if code == 0 {
			code = 1
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

// dropDatabase drops name through admin. It is every drop of a test database here: DatabaseOn's,
// and a Template's of each clone, of the binary's other database and of the template itself.
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
// caller's shared wait, testwait.InMain in Main and testwait.Eventually in a test's cleanup, which
// gives up only at the test binary's deadline less testwait's margin. Any other answer is final.
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

// prepare opens the admin pool and makes the template every Clone copies.
func (tp *Template) prepare(server url.URL) error {
	tp.serverURL = server
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL(server, "postgres"))
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
	tp.admin = admin

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := tp.prefix + "_template_" + suffix
	// The template is a file copy of template1, for the reason each clone of it is one.
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}
	tp.name = name
	if err := tp.migrate(ctx, databaseURL(server, name)); err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	// Postgres refuses to clone a database another session is connected to. Refusing connections
	// to the template keeps any stray session from making a clone wait or fail.
	if _, err := admin.Exec(ctx, "alter database "+name+" with allow_connections false"); err != nil {
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
