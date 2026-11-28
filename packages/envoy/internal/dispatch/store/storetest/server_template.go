package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// scheme names one family of server templates: the names its templates and their builds take, the
// build lock its makers take, and the hold each process that clones one of its templates keeps on
// it. A sweep drops only names of its own scheme and its own login (serverTemplate), so a later
// scheme never drops a template an earlier binary holds, and a row of this package that makes
// templates under a scheme of its own never meets a real one.
type scheme struct {
	// prefix begins every database of the scheme. A template is prefix, the first 16 hex of its
	// digest, "_" and its login's key (templateName); a build is prefix, "build_", the login's key,
	// "_" and 16 random hex (buildPrefix).
	prefix string
	// buildClass is the build lock's class: pg_advisory_lock(hashtext(buildClass), 0).
	buildClass string
	// holdClass is the hold's class: pg_advisory_lock_shared(hashtext(holdClass),
	// hashtext(<template>)).
	holdClass string
}

// openScheme is production's scheme, the one Open's template takes (openSet). v1 is the form of the
// names, the build lock and the hold together: a change to any of them is v2, whose sweeps then
// never match a template a binary of v1 holds. Both locks take the two-key form, which never meets
// the broker's storetest lock (the one-key hashtext('envoy broker storetest template')) or
// Dispatch's own migration lock (8150001).
var openScheme = scheme{
	prefix:     "dispatch_shared_v1_",
	buildClass: "envoy dispatch storetest template build",
	holdClass:  "envoy dispatch storetest template hold",
}

// openSet is what Open's template is made of: its scheme, the function that answers the digest
// that names it, and the migrate that brings a build of it to its schema. Production's are
// openScheme, templateDigest, run once in the process (sync.OnceValues), and migrateWithThisTree.
// Only storetest's own TestMain changes them, in a copy of its test binary that a row starts with
// STORETEST_OPEN_SET (storetest_test.go), so the row's copies make, find and sweep templates of the
// row's own and never a real one.
var openSet = struct {
	scheme  scheme
	digest  func() (string, error)
	migrate func(databaseURL string) error
}{openScheme, sync.OnceValues(templateDigest), migrateWithThisTree}

// templateForm names each step storetest adds to Open's template outside the migrations, one a
// line, in the order it runs them, so a change to one of them moves the template's name
// (templateDigest) as a change to a migration file does, and a server never hands a binary a
// template built with other steps. A step storetest adds to the template is added here: its
// statement, after a newline, with the build's name written <template> where it takes one.
var templateForm = testdb.WithoutJIT("<template>") + "\n" + embeddingsStatistics

// templateDigest is the digest that names Open's template in production: the SHA-256, in hex, of
// the embedded migrations' digest (store.MigrationsDigest), a zero byte and templateForm. It covers
// the migration files and the steps templateForm names, not the runner's Go code:
// store.MigrationsDigest says what that costs on a server that outlives a run.
func templateDigest() (string, error) {
	migrations, err := store.MigrationsDigest()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(migrations + "\x00" + templateForm))
	return hex.EncodeToString(sum[:]), nil
}

// loginKey is the first 12 hex of the SHA-256 of login: the part of a template's name, and of its
// builds' names, that is the login's own. Postgres copies a database that is not marked a template
// only for its owner or a superuser, and a copy keeps the tables of its template's owner, so each
// login makes and copies a template of its own, as the broker's storetest does. A login's name may
// hold any character and run to 63 bytes, the most an identifier holds; its hash keeps every name
// identifier-safe and short: dispatch_shared_v1_<16 hex>_<12 hex> is 48 bytes, and a build's
// dispatch_shared_v1_build_<12 hex>_<16 hex> is 54.
func loginKey(login string) string {
	sum := sha256.Sum256([]byte(login))
	return hex.EncodeToString(sum[:6])
}

// templateName is the name of s's template of digest, the full hex digest whose first 16 hex it
// takes, for login. It is the one function that writes a template's name: the look, the hold, the
// rename that ends a build and the sweep's pattern (namesTemplate) all read it.
func (s scheme) templateName(digest, login string) string {
	return s.prefix + digest[:16] + "_" + loginKey(login)
}

// buildPrefix begins the name of each of login's builds under s, which 16 random hex end: the one
// function that writes it, for the build and for the drop of a dead maker's (serverTemplate).
func (s scheme) buildPrefix(login string) string {
	return s.prefix + "build_" + loginKey(login) + "_"
}

// namesTemplate reports whether name is one of s's templates, of any digest, of the login whose key
// is key: the form templateName writes, which no build's name and no other scheme's has.
func (s scheme) namesTemplate(name, key string) bool {
	digest, ok := strings.CutPrefix(name, s.prefix)
	if !ok {
		return false
	}
	digest, ok = strings.CutSuffix(digest, "_"+key)
	return ok && len(digest) == 16 && strings.Trim(digest, "0123456789abcdef") == ""
}

// templateConn is one process's connection to the server's postgres database, on which each look,
// build lock, build and sweep of a server template runs (serverTemplate), and which keeps the
// process's hold on each template it resolved for as long as it is open: until the process exits.
// Postgres keeps an advisory lock per database, so the build lock and the hold are taken in
// postgres whatever database DISPATCH_TEST_DATABASE_URL names. It names itself "storetest template
// <pid>" (application_name), so the rows that hold this binary's admin logins
// (adminApplicationName) never hold it. Its mutex guards the connection, which TestMain, a
// re-resolve (Open's clone) and Run's listing (listSharedTemplates) each use in turn.
type templateConn struct {
	mu sync.Mutex
	// server is the server's URL as DISPATCH_TEST_DATABASE_URL names it, with no bound of this
	// package's: each build's migrate is given it, on the build, bounded by its maker's end.
	server url.URL
	conn   *pgx.Conn
	// held is each template whose hold conn took. A sweep passes over them: Postgres never makes a
	// session's lock wait on one the same session holds, so the sweep's try of the hold key would
	// take it.
	held map[string]bool
}

// openTemplateConn opens a template connection on server, its connect bounded by end
// (testdb.WithConnectEnd), or by testdb.NoDeadlineConnectTimeout without bounded.
func openTemplateConn(server url.URL, end time.Time, bounded bool) (*templateConn, error) {
	conn, err := connectTemplate(server, end, bounded)
	if err != nil {
		return nil, err
	}
	return &templateConn{server: server, conn: conn, held: map[string]bool{}}, nil
}

// connectTemplate opens the connection a templateConn holds: to server's postgres database, named
// templateApplicationName of this process, its connect bounded by end.
func connectTemplate(server url.URL, end time.Time, bounded bool) (*pgx.Conn, error) {
	connect, err := testdb.WithConnectEnd(databaseURL(server, "postgres"), end, bounded)
	if err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(connect)
	if err != nil {
		return nil, fmt.Errorf("parse the template connection's URL: %w", err)
	}
	config.RuntimeParams["application_name"] = templateApplicationName(os.Getpid())
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("open the template connection: %w", err)
	}
	return conn, nil
}

// templateApplicationName is the application_name of the template connection of the test binary
// whose pid is pid: "storetest template" and the pid, which no other process on the machine holds.
func templateApplicationName(pid int) string {
	return "storetest template " + strconv.Itoa(pid)
}

// reconnectWhereLost opens c's connection again where it is closed or does not answer a ping
// within the seconds left until end (testdb.TimeoutUntil), as a connection whose backend the server
// ended does not: its holds went with it, so the new one holds none until serverTemplate takes
// them again.
func (c *templateConn) reconnectWhereLost(end time.Time, bounded bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.conn.IsClosed() {
		ctx, cancel := context.Background(), func() {}
		if bounded {
			ctx, cancel = context.WithTimeout(ctx, testdb.TimeoutUntil(end))
		}
		err := c.conn.Ping(ctx)
		cancel()
		if err == nil {
			return nil
		}
		closing, cancelClose := context.WithTimeout(context.Background(), time.Second)
		_ = c.conn.Close(closing) // a connection that failed its ping has nothing left to say
		cancelClose()
	}
	conn, err := connectTemplate(c.server, end, bounded)
	if err != nil {
		return err
	}
	c.conn, c.held = conn, map[string]bool{}
	return nil
}

// serverTemplate finds or makes, on c's server and as c's login, s's template of digest, and
// answers its name, whether it made it, and each database it swept.
//
// It takes c's hold on the template (pg_advisory_lock_shared(hashtext(s.holdClass),
// hashtext(name))) before its first look, and c keeps it, so no sweep drops the template while the
// process runs. A template it finds it uses without the build lock: a database of the template's
// name exists only once whole, after the rename that ends its build. Where it finds none it takes
// the build lock (pg_advisory_lock(hashtext(s.buildClass), 0), a session lock, since create
// database cannot run in a transaction and the hold must outlast any) and looks again, since
// another process may have made it meanwhile. Where the template is still missing it drops its
// login's unfinished builds (builds happen only under the lock, so one found there is a dead
// maker's), creates a build named s.buildPrefix and 16 random hex, a file copy of template1 with
// JIT off, has migrate bring it to the schema through a connection string bounded by end
// (testdb.WithConnectEnd), ends any session still on it, closes it to connections, renames it to
// the template's name and releases the lock.
//
// A maker that built then sweeps, outside the build lock, since each drop forces a checkpoint and
// waits on every backend's barrier, which no waiting builder should pay: it drops each of s's
// templates of its own login but its own and those c holds, whose hold key it can take without
// waiting (pg_try_advisory_lock), since no running process holds it and so none can clone it. It
// drops one with drop database if exists … with (force), releases the key, and writes
// "storetest: swept <name>, which no running test process holds" to stderr. A process whose hold
// waited on that key then finds its template gone and makes it again. A sweep that fails writes
// why and leaves the rest: the template is made all the same.
//
// The wait for the build lock ends by end, or a second from now where less is left
// (testdb.TimeoutUntil, the rule adminExecUntil's wait follows), through the connection's
// lock_timeout, which ends the wait and leaves the connection, and with it the hold, open; its
// error names the lock. Without bounded it has no bound. Every other statement runs with no bound
// of this package's, as each admin statement does (adminExecUntil says why), and the wait for the
// build's last sessions to end is testwait's (InHelper).
func serverTemplate(c *templateConn, s scheme, digest string, migrate func(databaseURL string) error, end time.Time, bounded bool) (name string, made bool, swept []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := context.Background()
	var login string
	if err := c.conn.QueryRow(ctx, "select current_user").Scan(&login); err != nil {
		return "", false, nil, fmt.Errorf("read the login: %w", err)
	}
	name = s.templateName(digest, login)
	if !c.held[name] {
		if _, err := c.conn.Exec(ctx, "select pg_advisory_lock_shared(hashtext($1), hashtext($2))", s.holdClass, name); err != nil {
			return "", false, nil, fmt.Errorf("take the hold on the template %s, pg_advisory_lock_shared(hashtext('%s'), hashtext('%s')): %w", name, s.holdClass, name, err)
		}
		c.held[name] = true
	}
	found, err := c.exists(name)
	if err != nil {
		return "", false, nil, err
	}
	if found {
		return name, false, nil, nil
	}
	if made, err = c.buildUnderLock(s, name, login, migrate, end, bounded); err != nil {
		return "", false, nil, err
	}
	if !made {
		return name, false, nil, nil
	}
	return name, true, c.sweep(s, name, login), nil
}

// exists reports whether a database named name is on c's server.
func (c *templateConn) exists(name string) (bool, error) {
	var found bool
	if err := c.conn.QueryRow(context.Background(), "select exists (select from pg_database where datname = $1)", name).Scan(&found); err != nil {
		return false, fmt.Errorf("look for the template %s: %w", name, err)
	}
	return found, nil
}

// buildUnderLock is serverTemplate's part under the build lock: it waits for the lock, looks for
// the template again, and where it is still missing builds it, and answers whether it did. It
// releases the lock before it returns.
func (c *templateConn) buildUnderLock(s scheme, name, login string, migrate func(databaseURL string) error, end time.Time, bounded bool) (made bool, err error) {
	if err := c.waitForBuildLock(s, end, bounded); err != nil {
		return false, err
	}
	defer func() {
		if unlockErr := c.releaseBuildLock(s); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()
	if found, err := c.exists(name); err != nil || found {
		return false, err
	}
	if err := c.dropUnfinishedBuilds(s, login); err != nil {
		return false, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return false, err
	}
	build := s.buildPrefix(login) + suffix
	ctx := context.Background()
	// A file copy of template1, for the reason each clone is one (the package comment).
	if _, err := c.conn.Exec(ctx, cloneStatement(build, emptyTemplate)); err != nil {
		return false, fmt.Errorf("create the template's build %s: %w", build, err)
	}
	if _, err := c.conn.Exec(ctx, testdb.WithoutJIT(build)); err != nil {
		return false, fmt.Errorf("turn JIT off on the template's build %s: %w", build, err)
	}
	connect, err := testdb.WithConnectEnd(databaseURL(c.server, build), end, bounded)
	if err != nil {
		return false, err
	}
	if err := migrate(connect); err != nil {
		return false, fmt.Errorf("migrate the template's build %s: %w", build, err)
	}
	if err := c.endSessionsOn(build); err != nil {
		return false, err
	}
	// Postgres refuses to clone a database another session is connected to, and to rename one,
	// so the template refuses every connection from here on.
	for _, statement := range []string{
		"alter database " + build + " with allow_connections false",
		"alter database " + build + " rename to " + name,
	} {
		if _, err := c.conn.Exec(ctx, statement); err != nil {
			return false, fmt.Errorf("finish the template (%s): %w", statement, err)
		}
	}
	return true, nil
}

// buildLock is the statement that takes s's build lock, as an error names the lock.
func buildLock(s scheme) string {
	return "pg_advisory_lock(hashtext('" + s.buildClass + "'), 0)"
}

// buildLockTake and buildLockRelease take and release a scheme's build lock, whose class is their
// one parameter: the build's (waitForBuildLock, releaseBuildLock), and a row's that holds the lock
// itself.
const (
	buildLockTake    = "select pg_advisory_lock(hashtext($1), 0)"
	buildLockRelease = "select pg_advisory_unlock(hashtext($1), 0)"
)

// releaseBuildLock releases s's build lock on c.
func (c *templateConn) releaseBuildLock(s scheme) error {
	if _, err := c.conn.Exec(context.Background(), buildLockRelease, s.buildClass); err != nil {
		return fmt.Errorf("release the template build lock %s: %w", buildLock(s), err)
	}
	return nil
}

// waitForBuildLock takes s's build lock on c, its wait bounded by end where bounded
// (serverTemplate). The bound is the connection's lock_timeout, set for the wait alone, which
// fails the statement and leaves the connection open: a context's end would close it, since pgx
// answers one by setting the socket's deadline (pgconn's DeadlineContextWatcherHandler, its
// default at v5.13.0), and the hold would end with it. A failed wait says how long it waited and
// its bound, as an admin connection's wait does (acquireAdminUntil).
func (c *templateConn) waitForBuildLock(s scheme, end time.Time, bounded bool) error {
	ctx := context.Background()
	bound := "no"
	if bounded {
		timeout := testdb.TimeoutUntil(end)
		bound = "a " + timeout.String()
		if _, err := c.conn.Exec(ctx, "select set_config('lock_timeout', $1, false)", strconv.FormatInt(timeout.Milliseconds(), 10)+"ms"); err != nil {
			return fmt.Errorf("bound the wait for the template build lock %s: %w", buildLock(s), err)
		}
	}
	began := time.Now()
	_, err := c.conn.Exec(ctx, buildLockTake, s.buildClass)
	waited := time.Since(began)
	if bounded {
		if _, resetErr := c.conn.Exec(ctx, "reset lock_timeout"); resetErr != nil && err == nil {
			// The lock is held: release it, since no build follows.
			_ = c.releaseBuildLock(s)
			err = fmt.Errorf("reset lock_timeout after the wait: %w", resetErr)
		}
	}
	if err != nil {
		return fmt.Errorf("wait for the template build lock %s (waited %s of %s bound): %w", buildLock(s), waited.Round(time.Millisecond), bound, err)
	}
	return nil
}

// dropUnfinishedBuilds drops each database of c's login whose name begins with s.buildPrefix for
// that login: under the build lock no other process is building, so each is a dead maker's. Only
// the login's own are read (datdba), since only a database's owner or a superuser may drop it, and
// another login's builds are that login's to drop when it next builds.
func (c *templateConn) dropUnfinishedBuilds(s scheme, login string) error {
	ctx := context.Background()
	rows, err := c.conn.Query(ctx, `select datname from pg_database
		where datdba = (select oid from pg_roles where rolname = current_user) and starts_with(datname, $1)`, s.buildPrefix(login))
	if err != nil {
		return fmt.Errorf("list the unfinished template builds: %w", err)
	}
	builds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the unfinished template builds: %w", err)
	}
	for _, build := range builds {
		if _, err := c.conn.Exec(ctx, "drop database if exists "+pgx.Identifier{build}.Sanitize()+" with (force)"); err != nil {
			return fmt.Errorf("drop the unfinished template build %s: %w", build, err)
		}
	}
	return nil
}

// endSessionsOn ends each client session of c's login still connected to build, and waits until
// Postgres lists no client session on it, through testwait (InHelper, whose one bound is the
// binary's deadline less testwait's margin). A migrate closes its connections before it returns,
// but Postgres ends a backend a moment after its client closes the connection, so one can still be
// listed; and one a migrate left open would make every clone of the template fail. Postgres itself
// ends an autovacuum worker on the build where the rename needs it to, and a non-superuser may not.
// A session of another login, which this login may not end, is waited for, and the wait then fails
// naming the build.
func (c *templateConn) endSessionsOn(build string) error {
	ctx := context.Background()
	var listed int
	var readErr error
	if err := testwait.InHelper("storetest", "the client sessions on the template's build "+build+" to end", func() bool {
		if _, readErr = c.conn.Exec(ctx, `select pg_terminate_backend(pid) from pg_stat_activity
			where datname = $1 and backend_type = 'client backend' and usename = current_user`, build); readErr != nil {
			return true
		}
		readErr = c.conn.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = $1 and backend_type = 'client backend'`, build).Scan(&listed)
		return readErr != nil || listed == 0
	}); err != nil {
		return err
	}
	if readErr != nil {
		return fmt.Errorf("end the sessions on the template's build %s: %w", build, readErr)
	}
	return nil
}

// sweep drops each of s's templates of login (namesTemplate) on c's server but name and those c
// holds, whose hold key c takes without waiting, and answers the ones it dropped (serverTemplate
// says why). It writes one line to stderr for each it drops, and one for each that fails, which
// leaves the rest unswept where the connection failed.
func (c *templateConn) sweep(s scheme, name, login string) []string {
	ctx := context.Background()
	rows, err := c.conn.Query(ctx, `select datname from pg_database
		where datdba = (select oid from pg_roles where rolname = current_user) and starts_with(datname, $1) and datname <> $2
		order by datname`, s.prefix, name)
	if err == nil {
		var names []string
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err == nil {
			return c.sweepEach(s, names, loginKey(login))
		}
	}
	fmt.Fprintf(os.Stderr, "storetest: could not list the templates to sweep after making %s: %v\n", name, err)
	return nil
}

// sweepEach is sweep's drop of each of names that is one of s's templates of key and that c does
// not hold.
func (c *templateConn) sweepEach(s scheme, names []string, key string) []string {
	ctx := context.Background()
	var swept []string
	for _, other := range names {
		if !s.namesTemplate(other, key) || c.held[other] {
			continue
		}
		var free bool
		if err := c.conn.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1), hashtext($2))", s.holdClass, other).Scan(&free); err != nil {
			fmt.Fprintf(os.Stderr, "storetest: could not sweep %s: take its hold key: %v\n", other, err)
			return swept
		}
		if !free {
			continue
		}
		_, dropErr := c.conn.Exec(ctx, "drop database if exists "+pgx.Identifier{other}.Sanitize()+" with (force)")
		_, unlockErr := c.conn.Exec(ctx, "select pg_advisory_unlock(hashtext($1), hashtext($2))", s.holdClass, other)
		if dropErr != nil {
			fmt.Fprintf(os.Stderr, "storetest: could not sweep %s: %v\n", other, dropErr)
		} else {
			swept = append(swept, other)
			fmt.Fprintf(os.Stderr, "storetest: swept %s, which no running test process holds\n", other)
		}
		if unlockErr != nil {
			fmt.Fprintf(os.Stderr, "storetest: could not release the hold key of %s after its sweep: %v\n", other, unlockErr)
			return swept
		}
	}
	return swept
}

// openConn is this process's template connection, opened by the first resolve of Open's template
// (resolveOpen) and kept until the process exits.
var openConn struct {
	sync.Mutex
	conn *templateConn
}

// resolveOpen finds or makes Open's template (openSet) on the server DISPATCH_TEST_DATABASE_URL
// names, under end, through serverTemplate on the process's template connection, which it opens
// where none is open yet and opens again where it was lost (reconnectWhereLost), and answers the
// template's name. Run calls it in TestMain (through makeOpen), and Open's clone again where its
// template was gone (clone). It writes one line to stderr, naming the server by the host and port
// of the URL alone: `storetest: Open's template <name> on <host:port> found in <d>`, `… made in
// <d>`, or `storetest: Open's template on <host:port> could not be made after <d>: <error>`.
func resolveOpen(end time.Time, bounded bool) (string, error) {
	began := time.Now()
	name, made, host, err := findOrMakeOpen(end, bounded)
	took := time.Since(began).Round(time.Millisecond)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "storetest: Open's template on %s could not be made after %s: %v\n", host, took, err)
	case made:
		fmt.Fprintf(os.Stderr, "storetest: Open's template %s on %s made in %s\n", name, host, took)
	default:
		fmt.Fprintf(os.Stderr, "storetest: Open's template %s on %s found in %s\n", name, host, took)
	}
	return name, err
}

// findOrMakeOpen is resolveOpen's work: the template's name, whether it was made, and the server's
// host and port.
func findOrMakeOpen(end time.Time, bounded bool) (name string, made bool, host string, err error) {
	raw, set, err := serverFromEnv()
	if err != nil {
		return "", false, "", err
	}
	if !set {
		return "", false, "", errors.New(DatabaseURLEnv + " is unset")
	}
	// serverFromEnv refused a URL that does not parse.
	server, _ := url.Parse(raw)
	c, err := templateConnection(*server, end, bounded)
	if err != nil {
		return "", false, server.Host, err
	}
	digest, err := openSet.digest()
	if err != nil {
		return "", false, server.Host, err
	}
	name, made, _, err = serverTemplate(c, openSet.scheme, digest, openSet.migrate, end, bounded)
	return name, made, server.Host, err
}

// templateConnection answers the process's template connection (openConn): opened on server where
// none is yet, and opened again where it was lost.
func templateConnection(server url.URL, end time.Time, bounded bool) (*templateConn, error) {
	openConn.Lock()
	defer openConn.Unlock()
	if openConn.conn != nil {
		return openConn.conn, openConn.conn.reconnectWhereLost(end, bounded)
	}
	c, err := openTemplateConn(server, end, bounded)
	if err != nil {
		return nil, err
	}
	openConn.conn = c
	return c, nil
}

// listSharedTemplates writes, where the process opened a template connection, one line to stderr:
// `storetest: shared templates on <host:port> after this binary's tests: <names>`, each database
// on the server whose name begins with Open's scheme's prefix (openSet), whatever its login, in
// order, ", "-joined, or "none". It reads them on the template connection, by bound (Run's, the
// teardown's: testdb.TeardownDeadline). A read that fails writes why instead.
func listSharedTemplates(bound time.Time) {
	openConn.Lock()
	c := openConn.conn
	openConn.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), bound)
	defer cancel()
	rows, err := c.conn.Query(ctx, "select datname from pg_database where starts_with(datname, $1)", openSet.scheme.prefix)
	var names []string
	if err == nil {
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "storetest: the shared templates on %s could not be read after this binary's tests: %v\n", c.server.Host, err)
		return
	}
	slices.Sort(names)
	fmt.Fprintf(os.Stderr, "storetest: shared templates on %s after this binary's tests: %s\n", c.server.Host, namesOrNone(names))
}

// namesOrNone is names, ", "-joined, or "none".
func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
