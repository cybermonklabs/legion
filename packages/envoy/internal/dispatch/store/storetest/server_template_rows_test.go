package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file call serverTemplate in this process, each on connections of its own: a
// connection is a session to Postgres, as a process's template connection is, and Postgres never
// makes a session wait on an advisory lock it holds itself, so a row's "other process" is another
// connection. Each row makes its templates under a scheme of its own (rowScheme), with a fake
// migrate that creates one table (rowMigrate), so it never holds, waits on or sweeps a real
// template, and no real build or listing meets one of its own; and it drops every database it made
// once it ends (rowAdmin).

// rowScheme is a scheme of the row's own: the prefix storetest_row_ and 8 random hex, and lock
// classes that carry the same hex, so the row's templates, builds and locks meet no real template's
// and no other row's, and a sweep of the row's drops only the row's own.
func rowScheme(t *testing.T) scheme {
	t.Helper()
	tag := randomHex(t, 4)
	return scheme{
		prefix:     "storetest_row_" + tag + "_",
		buildClass: "storetest row build " + tag,
		holdClass:  "storetest row hold " + tag,
	}
}

// rowDigest is the digest of a fake migration set of the row's own: 64 random hex, whose first 16
// name its template (templateName).
func rowDigest(t *testing.T) string {
	t.Helper()
	return randomHex(t, 32)
}

// randomHex is n random bytes in hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	random := make([]byte, n)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	return hex.EncodeToString(random)
}

// rowMigrate is the rows' fake migrate: it creates the one table storetest_row_set in the build
// (migrateRowSet), with no gate.
func rowMigrate(databaseURL string) error {
	return migrateRowSet(databaseURL, "")
}

// rowAdmin opens the row's own connection to the server's postgres database, beside the
// connections it hands serverTemplate, for what the row reads and makes by hand. When t ends it
// drops on it every database whose name begins with s's prefix, the row's templates, builds and
// clones, then closes it. It skips t where DISPATCH_TEST_DATABASE_URL is unset (ready).
func rowAdmin(t *testing.T, s scheme) *pgx.Conn {
	t.Helper()
	ready(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres")))
	if err != nil {
		t.Fatalf("open the row's own connection to the server: %v", err)
	}
	testcleanup.Add(t, func() { _ = conn.Close(context.Background()) })
	testcleanup.Add(t, func() {
		names, err := databasesUnder(conn, s.prefix)
		if err != nil {
			t.Errorf("list the row's databases to drop them: %v", err)
			return
		}
		dropDatabases(t, conn, names...)
	})
	return conn
}

// databasesUnder lists, in order, every database on conn's server whose name begins with prefix.
func databasesUnder(conn *pgx.Conn, prefix string) ([]string, error) {
	rows, err := conn.Query(context.Background(), "select datname from pg_database where starts_with(datname, $1) order by datname", prefix)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// rowDatabases is databasesUnder for a row, which fails t where the read does.
func rowDatabases(t *testing.T, conn *pgx.Conn, prefix string) []string {
	t.Helper()
	names, err := databasesUnder(conn, prefix)
	if err != nil {
		t.Fatalf("list the databases under %s: %v", prefix, err)
	}
	return names
}

// dropDatabases drops each of names that exists, on conn, ending any session still on it, as a
// sweep does: a row drops what it made whatever state it failed in. A drop that fails fails t and
// leaves the rest to be tried.
func dropDatabases(t *testing.T, conn *pgx.Conn, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := conn.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("drop the row's database %s: %v", name, err)
		}
	}
}

// databaseExists reports whether a database named name is on conn's server.
func databaseExists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var found bool
	if err := conn.QueryRow(context.Background(), "select exists (select from pg_database where datname = $1)", name).Scan(&found); err != nil {
		t.Fatalf("look for the database %s: %v", name, err)
	}
	return found
}

// databaseOwner is the login that owns the database name.
func databaseOwner(t *testing.T, conn *pgx.Conn, name string) string {
	t.Helper()
	var owner string
	if err := conn.QueryRow(context.Background(), "select pg_get_userbyid(datdba)::text from pg_database where datname = $1", name).Scan(&owner); err != nil {
		t.Fatalf("read the owner of %s: %v", name, err)
	}
	return owner
}

// currentLogin is conn's login, current_user, whose key names its templates (loginKey).
func currentLogin(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var login string
	if err := conn.QueryRow(context.Background(), "select current_user::text").Scan(&login); err != nil {
		t.Fatalf("read the row's login: %v", err)
	}
	return login
}

// createRowDatabase makes, on conn, a database named name by hand, as a file copy of template1 that
// runs with JIT off, as every database a test makes is (docs/testing.md, "A database a Go test
// creates").
func createRowDatabase(t *testing.T, conn *pgx.Conn, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()+" strategy file_copy"); err != nil {
		t.Fatalf("make the database %s by hand: %v", name, err)
	}
	if _, err := conn.Exec(ctx, testdb.WithoutJIT(name)); err != nil {
		t.Fatalf("turn JIT off on %s: %v", name, err)
	}
}

// cloneRowTemplate copies template into a database of the row's own under s's prefix, on conn, as
// Open's clone does (cloneStatement), as the login conn signs in as, and answers its name. The
// clone's name holds no 16 hex and login key after the prefix, so no sweep takes it for a
// template; rowAdmin drops it with the rest.
func cloneRowTemplate(t *testing.T, conn *pgx.Conn, s scheme, template string) string {
	t.Helper()
	ctx := context.Background()
	clone := s.prefix + "clone_" + randomHex(t, 8)
	if _, err := conn.Exec(ctx, cloneStatement(clone, template)); err != nil {
		t.Fatalf("clone the template %s: %v", template, err)
	}
	if _, err := conn.Exec(ctx, testdb.WithoutJIT(clone)); err != nil {
		t.Fatalf("turn JIT off on the clone %s: %v", clone, err)
	}
	return clone
}

// rowTemplateConn opens a template connection on the server raw names (openTemplateConn), its
// connect bounded by t's connect end, and closes it when t ends: one process's, to Postgres.
func rowTemplateConn(t *testing.T, raw string) *templateConn {
	t.Helper()
	end, bounded := testdb.ConnectEnd(t)
	c, err := openTemplateConn(raw, end, bounded)
	if err != nil {
		t.Fatalf("open a template connection: %v", err)
	}
	testcleanup.Add(t, func() { closeTemplateConn(c) })
	return c
}

// closeTemplateConn closes c, and with it every hold it took, once no call of serverTemplate holds
// it. A row that takes the build lock itself registers its release after its template connections
// (holdBuildLock), so the release runs first and no call still waits on it here.
func closeTemplateConn(c *templateConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.Close(context.Background())
}

// closeAndAwaitEnd closes c, as a process that exits closes its template connection, and waits,
// reading conn, until Postgres lists neither its backend nor a lock of it: the backend lets its
// advisory locks go as it ends, and only then does the hold c took stop keeping its template from a
// sweep.
func closeAndAwaitEnd(t *testing.T, conn *pgx.Conn, c *templateConn) {
	t.Helper()
	pid := int(c.conn.PgConn().PID())
	closeTemplateConn(c)
	awaitBackendEnd(t, conn, pid, "the closed template connection")
}

// awaitBackendEnd waits, reading conn, until Postgres lists neither the backend pid, which what
// names, nor a lock of it.
func awaitBackendEnd(t *testing.T, conn *pgx.Conn, pid int, what string) {
	t.Helper()
	testwait.Until(t, "the backend "+fmt.Sprint(pid)+" of "+what+" to end and let its locks go", func() (string, bool) {
		var listed bool
		if err := conn.QueryRow(context.Background(), `select exists (select from pg_stat_activity where pid = $1)
			or exists (select from pg_locks where pid = $1)`, pid).Scan(&listed); err != nil {
			t.Fatalf("read whether the backend %d is listed: %v", pid, err)
		}
		return fmt.Sprintf("listed: %t", listed), !listed
	})
}

// askRowTemplate has c find or make s's template of digest, as serverTemplate does for a process,
// with the rows' fake migrate and t's connect end, and fails t where it errs.
func askRowTemplate(t *testing.T, c *templateConn, s scheme, digest string) (name string, made bool, swept []string) {
	t.Helper()
	end, bounded := testdb.ConnectEnd(t)
	name, made, swept, err := serverTemplate(c, s, digest, rowMigrate, end, bounded)
	if err != nil {
		t.Fatalf("find or make the template of the set %s under %s: %v", digest[:16], s.prefix, err)
	}
	return name, made, swept
}

// makeRowTemplate is askRowTemplate for a set no connection has made yet, whose template c must
// make.
func makeRowTemplate(t *testing.T, c *templateConn, s scheme, digest string) string {
	t.Helper()
	name, made, _ := askRowTemplate(t, c, s, digest)
	if !made {
		t.Fatalf("the template %s of a set no connection had made was found, not made", name)
	}
	return name
}

// holdBuildLock takes s's build lock on a connection of the row's own, which keeps it until it
// answers release or t ends, whichever comes first. It is registered after the row's template
// connections, so its release runs before their close (closeTemplateConn).
func holdBuildLock(t *testing.T, s scheme) (release func()) {
	t.Helper()
	ctx := context.Background()
	holder, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, "postgres")))
	if err != nil {
		t.Fatalf("open the connection that holds the build lock: %v", err)
	}
	testcleanup.Add(t, func() { _ = holder.Close(context.Background()) })
	if _, err := holder.Exec(ctx, "select pg_advisory_lock(hashtext($1), 0)", s.buildClass); err != nil {
		t.Fatalf("take the build lock %s: %v", buildLock(s), err)
	}
	return func() {
		if _, err := holder.Exec(context.Background(), "select pg_advisory_unlock(hashtext($1), 0)", s.buildClass); err != nil {
			t.Fatalf("release the build lock %s: %v", buildLock(s), err)
		}
	}
}

// buildLockState is how one backend stands on a scheme's build lock, as pg_locks lists its one
// request for it: granted the lock, or queued behind it, and whether the request is in the
// server's postgres database. Neither is true where the backend asked for none.
type buildLockState struct {
	granted, queued, inPostgres bool
}

func (b buildLockState) String() string {
	switch {
	case b.granted:
		return fmt.Sprintf("granted, in postgres: %t", b.inPostgres)
	case b.queued:
		return fmt.Sprintf("queued, in postgres: %t", b.inPostgres)
	}
	return "no request"
}

// buildLockOf reads, on conn, how the backend pid stands on s's build lock. Postgres lists
// pg_advisory_lock(hashtext(<class>), 0) as an advisory lock of classid hashtext(<class>), objid 0
// and objsubid 2, the two-key form. It reads the backend's own request, never the wait event
// (docs/testing.md, "A test reads a lock wait from pg_locks").
func buildLockOf(t *testing.T, conn *pgx.Conn, s scheme, pid int) buildLockState {
	t.Helper()
	var granted, inPostgres bool
	err := conn.QueryRow(context.Background(), `select granted, database = (select oid from pg_database where datname = 'postgres')
		from pg_locks
		where pid = $1 and locktype = 'advisory' and classid = hashtext($2)::oid and objid = 0 and objsubid = 2`, pid, s.buildClass).Scan(&granted, &inPostgres)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return buildLockState{}
	case err != nil:
		t.Fatalf("read the build lock's request of the backend %d from pg_locks: %v", pid, err)
	}
	return buildLockState{granted: granted, queued: !granted, inPostgres: inPostgres}
}

// namesOrNone is names, ", "-joined, or "none".
func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// TestAnotherMigrationSetMakesATemplateOfItsOwn: a process whose migrations differ from those a
// server's template was made of makes a template of its own beside it and leaves the other one
// (R1: "A test process whose migrations differ … makes a template of its own"). One connection
// makes set D1's template and keeps its hold, as a running process does; a second connection asks
// for set D2 and must make a second template, whose name, written by the one function that names
// them (templateName), differs from D1's in its digest part alone, sweep nothing, and leave D1's on
// the server.
func TestAnotherMigrationSetMakesATemplateOfItsOwn(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	login := currentLogin(t, admin)
	raw := shared.server.String()
	first, second := rowDigest(t), rowDigest(t)
	firstName := makeRowTemplate(t, rowTemplateConn(t, raw), s, first)
	secondName, made, swept := askRowTemplate(t, rowTemplateConn(t, raw), s, second)
	if !made {
		t.Fatalf("set D2's template %s was found, not made, beside set D1's %s", secondName, firstName)
	}
	if firstName != s.templateName(first, login) || secondName != s.templateName(second, login) {
		t.Fatalf("the templates are %s and %s; want %s and %s, named by their digests and the login %q", firstName, secondName, s.templateName(first, login), s.templateName(second, login), login)
	}
	if firstName[len(s.prefix):len(s.prefix)+16] == secondName[len(s.prefix):len(s.prefix)+16] ||
		firstName[len(s.prefix)+16:] != secondName[len(s.prefix)+16:] {
		t.Fatalf("the templates %s and %s do not differ in their digest part alone", firstName, secondName)
	}
	if len(swept) != 0 {
		t.Fatalf("set D2's build swept %s while a connection held set D1's template %s", namesOrNone(swept), firstName)
	}
	if held := rowDatabases(t, admin, s.prefix); !slices.Equal(held, []string{firstName, secondName}) && !slices.Equal(held, []string{secondName, firstName}) {
		t.Fatalf("the server holds %s under the row's prefix; want both templates, %s and %s", namesOrNone(held), firstName, secondName)
	}
	t.Logf("set D1's template %s stays beside set D2's %s", firstName, secondName)
}

// TestATemplateAProcessHoldsIsNotSwept: a template is never dropped while a test process may still
// clone it (R1), and a sweep is what drops one (D5). Three sets of one scheme, each made by a
// connection of its own: set D1's, whose connection keeps its hold, as a running process does;
// set D0's, whose connection then closes, as a process that exits does, so no process holds it
// (D1's is made first, since each build sweeps); and set D2's, which sweeps. The sweep must drop
// D0's alone, which shows it ran, and leave D1's, which a clone of then succeeds.
func TestATemplateAProcessHoldsIsNotSwept(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	raw := shared.server.String()
	held := makeRowTemplate(t, rowTemplateConn(t, raw), s, rowDigest(t))
	gone := rowTemplateConn(t, raw)
	unheld := makeRowTemplate(t, gone, s, rowDigest(t))
	closeAndAwaitEnd(t, admin, gone)
	next, made, swept := askRowTemplate(t, rowTemplateConn(t, raw), s, rowDigest(t))
	if !made {
		t.Fatalf("set D2's template %s was found, not made", next)
	}
	if !slices.Equal(swept, []string{unheld}) {
		t.Fatalf("set D2's build swept %s; want %s alone, the template no connection holds, and not %s, which one holds", namesOrNone(swept), unheld, held)
	}
	if !databaseExists(t, admin, held) {
		t.Fatalf("the template %s, which a connection holds, is gone after set D2's build", held)
	}
	clone := cloneRowTemplate(t, admin, s, held)
	t.Logf("set D2's build swept %s and left %s, which a connection holds and which %s then cloned", unheld, held, clone)
}

// TestATemplateNoProcessHoldsIsSweptByTheNextBuild: a server that outlives runs keeps the templates
// in use plus the newest (D5). The connection that made set D1's template closes, as a process
// that exits does; once its backend has let its locks go, a build of set D2 by another connection
// must drop D1's template, report it among the swept, and leave D2's.
func TestATemplateNoProcessHoldsIsSweptByTheNextBuild(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	raw := shared.server.String()
	gone := rowTemplateConn(t, raw)
	first := makeRowTemplate(t, gone, s, rowDigest(t))
	closeAndAwaitEnd(t, admin, gone)
	second, made, swept := askRowTemplate(t, rowTemplateConn(t, raw), s, rowDigest(t))
	if !made {
		t.Fatalf("set D2's template %s was found, not made", second)
	}
	if !slices.Equal(swept, []string{first}) {
		t.Fatalf("set D2's build swept %s; want %s, the template no connection holds", namesOrNone(swept), first)
	}
	if databaseExists(t, admin, first) {
		t.Fatalf("the template %s is still on the server, though set D2's build reports it swept", first)
	}
	if !databaseExists(t, admin, second) {
		t.Fatalf("set D2's template %s is not on the server", second)
	}
	t.Logf("set D2's build made %s and swept %s", second, first)
}

// TestAnUnfinishedBuildIsSweptUnderTheLock: builds happen only under the build lock, so a build of
// this login's that a maker finds under the lock is a dead maker's, which it drops before it builds
// (D5); another login's builds are that login's to drop. The row makes by hand a database named
// like a build of its login (buildPrefix) and one named like a build of another login's key, both
// owned by its login; the next build must drop the first and leave the second.
func TestAnUnfinishedBuildIsSweptUnderTheLock(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	dead := s.buildPrefix(currentLogin(t, admin)) + randomHex(t, 8)
	others := s.buildPrefix("another login, whose builds are its own to drop") + randomHex(t, 8)
	createRowDatabase(t, admin, dead)
	createRowDatabase(t, admin, others)
	name := makeRowTemplate(t, rowTemplateConn(t, shared.server.String()), s, rowDigest(t))
	if databaseExists(t, admin, dead) {
		t.Fatalf("the build of this login's %s, which no maker holds the build lock for, is still on the server after the build of %s", dead, name)
	}
	if !databaseExists(t, admin, others) {
		t.Fatalf("the database %s, named like a build of another login's, was dropped by this login's build of %s", others, name)
	}
	t.Logf("the build of %s dropped %s and left %s", name, dead, others)
}

// TestAFoundTemplateIsUsedWithoutTheLock: a process that finds the template uses it without the
// build lock (D3), so a process never waits on a maker of another set, or on one that made the
// template a moment before. One connection makes the template; another connection of the row's
// takes the build lock and keeps it; a third connection then asks for the template, with no bound
// on any wait of its own, and must answer it, found. A look that took the lock first never returns,
// and the row's wait for it fails by name.
func TestAFoundTemplateIsUsedWithoutTheLock(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	rowAdmin(t, s)
	raw := shared.server.String()
	digest := rowDigest(t)
	name := makeRowTemplate(t, rowTemplateConn(t, raw), s, digest)
	looker := rowTemplateConn(t, raw)
	holdBuildLock(t, s)
	var found string
	var made bool
	var err error
	testwait.Returns(t, "a look for the template "+name+", which exists, to return while another connection holds the build lock "+buildLock(s), func() {
		found, made, _, err = serverTemplate(looker, s, digest, rowMigrate, time.Time{}, false)
	})
	if err != nil {
		t.Fatalf("the look for the template %s failed: %v", name, err)
	}
	if found != name || made {
		t.Fatalf("the look answered %s, made %t; want %s, found", found, made, name)
	}
	t.Logf("the look found %s while another connection held the build lock", name)
}

// TestTheTemplateLockIsTheServersWhateverDatabaseTheURLNames: Postgres keeps an advisory lock per
// database, so two processes whose URLs name different databases meet on the build lock only where
// each takes it in one database: the server's postgres database, whatever database the URL names
// (D3). The row holds the build lock on a connection to postgres, then has a template connection
// whose URL names a database of the row's own make the template: the build must wait on the lock,
// its request in postgres (pg_locks.database), and make the template once the row lets it go. A
// lock taken in the URL's database would not meet the row's, and the build would return without
// waiting, which fails the row by name.
func TestTheTemplateLockIsTheServersWhateverDatabaseTheURLNames(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	elsewhere := s.prefix + "elsewhere"
	createRowDatabase(t, admin, elsewhere)
	c := rowTemplateConn(t, databaseURL(shared.server, elsewhere))
	pid := int(c.conn.PgConn().PID())
	release := holdBuildLock(t, s)
	type answer struct {
		name string
		made bool
		err  error
	}
	answered := make(chan answer, 1)
	end, bounded := testdb.ConnectEnd(t)
	digest := rowDigest(t)
	go func() {
		name, made, _, err := serverTemplate(c, s, digest, rowMigrate, end, bounded)
		answered <- answer{name, made, err}
	}()
	state := testwait.Until(t, "the build, whose URL names "+elsewhere+", to wait on the build lock "+buildLock(s)+" the row holds in postgres", func() (buildLockState, bool) {
		select {
		case got := <-answered:
			t.Fatalf("the build returned (%s, made %t, %v) without waiting on the build lock the row holds in the server's postgres database", got.name, got.made, got.err)
		default:
		}
		state := buildLockOf(t, admin, s, pid)
		return state, state.queued
	})
	if !state.inPostgres {
		t.Fatalf("the build waits on the build lock in a database other than postgres (%s)", state)
	}
	release()
	got := testwait.Receive(t, "the build to make the template once the row let the build lock go", answered)
	if got.err != nil || !got.made {
		t.Fatalf("the build answered %s, made %t, %v; want the template made", got.name, got.made, got.err)
	}
	t.Logf("the build, whose URL names %s, waited on the build lock in postgres and then made %s", elsewhere, got.name)
}

// TestASecondLoginMakesAndCopiesItsOwnTemplate: Postgres copies a database not marked a template
// only for its owner or a superuser, so each login makes and copies a template of its own (R1, D2).
// The row's login makes the set's template; the row then makes a login of its own, with a password,
// since a sign-in over TCP needs one, and has a template connection of that login ask for the same
// set: it must make a template named with its own login key, owned by it, that it then clones,
// sweep nothing, and leave the first login's untouched. The row grants the new role to its own
// login (with inherit), so it can drop that role's databases and the role when it ends, on a server
// where it is no superuser. It skips where the server refuses its login a role (no CREATEROLE).
func TestASecondLoginMakesAndCopiesItsOwnTemplate(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	ctx := context.Background()
	first := currentLogin(t, admin)
	digest := rowDigest(t)
	firstName := makeRowTemplate(t, rowTemplateConn(t, shared.server.String()), s, digest)
	role := "storetest_row_" + randomHex(t, 4)
	password := randomHex(t, 16)
	if _, err := admin.Exec(ctx, "create role "+role+" login createdb password '"+password+"'"); err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "42501" {
			t.Skipf("the server refuses the login %s a role of the row's own (%s), so the row has no second login", first, pgError.Message)
		}
		t.Fatalf("make the login %s: %v", role, err)
	}
	testcleanup.Add(t, func() {
		rows, err := admin.Query(context.Background(), "select datname from pg_database where datdba = (select oid from pg_roles where rolname = $1)", role)
		var owned []string
		if err == nil {
			owned, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			t.Errorf("list the databases of the login %s to drop them: %v", role, err)
			return
		}
		dropDatabases(t, admin, owned...)
		if _, err := admin.Exec(context.Background(), "drop role if exists "+role); err != nil {
			t.Errorf("drop the login %s: %v", role, err)
		}
	})
	if _, err := admin.Exec(ctx, "grant "+role+" to current_user with inherit true"); err != nil {
		t.Fatalf("grant the login %s to %s, so the row can drop what it makes: %v", role, first, err)
	}
	server := shared.server
	server.User = url.UserPassword(role, password)
	second := rowTemplateConn(t, server.String())
	secondName, made, swept := askRowTemplate(t, second, s, digest)
	if want := s.templateName(digest, role); secondName != want || secondName == firstName || !made {
		t.Fatalf("the login %s answered the template %s, made %t; want %s, made, beside the login %s's %s", role, secondName, made, want, first, firstName)
	}
	if len(swept) != 0 {
		t.Fatalf("the login %s's build swept %s", role, namesOrNone(swept))
	}
	if owner := databaseOwner(t, admin, secondName); owner != role {
		t.Fatalf("the template %s is owned by %s; want the login that made it, %s", secondName, owner, role)
	}
	clone := cloneRowTemplate(t, second.conn, s, secondName)
	if !databaseExists(t, admin, firstName) {
		t.Fatalf("the login %s's template %s is gone after the login %s made its own", first, firstName, role)
	}
	if owner := databaseOwner(t, admin, firstName); owner != first {
		t.Fatalf("the login %s's template %s is owned by %s", first, firstName, owner)
	}
	t.Logf("the login %s made %s and cloned it into %s; the login %s's %s stays", role, secondName, clone, first, firstName)
}

// TestAWaitForTheTemplateLockEndsByItsEnd: a maker's wait for the build lock ends by its end (D3),
// so a binary whose TestMain cannot get the lock fails its tests with the error, by name, rather
// than holding the job. Another connection of the row's holds the build lock; a make whose end the
// row sets 300 ms ahead must return an error that names the lock wait, Postgres's lock_timeout
// (SQLSTATE 55P03), make nothing, and leave its connection open with its hold on the template. The
// verdict is the error; testwait bounds the row's wait for it.
func TestAWaitForTheTemplateLockEndsByItsEnd(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	c := rowTemplateConn(t, shared.server.String())
	holdBuildLock(t, s)
	digest := rowDigest(t)
	var err error
	testwait.Returns(t, "a make whose end is 300 ms ahead to return while another connection holds the build lock "+buildLock(s), func() {
		_, _, _, err = serverTemplate(c, s, digest, rowMigrate, time.Now().Add(300*time.Millisecond), true)
	})
	var pgError *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "wait for the template build lock "+buildLock(s)) || !errors.As(err, &pgError) || pgError.Code != "55P03" {
		t.Fatalf("the make answered %v; want the wait for the template build lock %s ended by its lock_timeout (SQLSTATE 55P03)", err, buildLock(s))
	}
	if made := rowDatabases(t, admin, s.prefix); len(made) != 0 {
		t.Fatalf("the make whose wait ended left %s on the server", namesOrNone(made))
	}
	var held bool
	if err := admin.QueryRow(context.Background(), `select exists (select from pg_locks
		where pid = $1 and locktype = 'advisory' and granted and mode = 'ShareLock'
		and classid = hashtext($2)::oid and objid = hashtext($3)::oid and objsubid = 2)`,
		int(c.conn.PgConn().PID()), s.holdClass, s.templateName(digest, currentLogin(t, admin))).Scan(&held); err != nil {
		t.Fatalf("read the template connection's hold from pg_locks: %v", err)
	}
	if !held {
		t.Fatalf("the template connection holds no hold on the template after its wait ended: the wait's end closed it")
	}
	t.Logf("the make answered: %v", err)
}
