package testpg_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// testdb.Drop tries the plain `drop database` first, and again while Postgres answers SQLSTATE
// 55006 (object_in_use), and reaches `with (force)` only when a backend is still attached after
// that window. A plain drop needs no right over another backend: the server ends the database's
// own autovacuum worker itself. `with (force)` ends that worker and every other backend as the
// dropping login, and ending a backend of another role needs pg_signal_backend or the superuser
// (PostgreSQL 18.6's TerminateOtherDBBackends, in src/backend/storage/ipc/procarray.c).
//
// So the right is the fallback's, not the fixture's. On a server that holds it — CI's test login
// is a superuser — this proves the fallback: a connection left open makes the plain drop answer
// 55006, and testdb.Drop clears it anyway. On a server that does not — a Legion issue pod's
// legion_test holds neither — the fallback cannot run, which is not a broken server, because
// every drop of a run there should be plain and the line testpg.Main prints counts them; so the
// test says which server it found and skips.
//
// The right is not dead, and the skip's message says so: internal/launcher/testpg.go's probe,
// packages/daemon/docker/worker.Dockerfile's test server and the sibling module's
// packages/envoy/internal/dispatch/store/storetest all still force, so
// packages/daemon/docker/legion-testpg goes on granting it.
func TestTheForcedFallbackNeedsARightThisLoginMayNotHold(t *testing.T) {
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	admin, database := adminURL(t, dsn)
	conn, err := pgx.Connect(t.Context(), admin)
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	defer conn.Close(context.Background())

	// pg_has_role's 'usage' is has_privs_of_role, which answers true for a superuser before it
	// looks at any membership, so this one read covers both ways the right can be held.
	var mayTerminate bool
	var login string
	if err := conn.QueryRow(t.Context(),
		`select current_user, pg_has_role(current_user, 'pg_signal_backend', 'usage')`,
	).Scan(&login, &mayTerminate); err != nil {
		t.Fatalf("read the login and whether it may terminate a backend: %v", err)
	}
	if !mayTerminate {
		t.Skipf("%s is no superuser and does not hold pg_signal_backend, so testdb.Drop's last attempt, "+
			"`drop database … with (force)`, answers SQLSTATE 42501 here: the autovacuum worker it ends "+
			"belongs to another role. What the absence costs this server is the leaked-connection case and "+
			"nothing else — a test that left a connection open past the retry window is not recovered, and "+
			"its drop fails the run. Every ordinary drop is plain and needs no right, and the line "+
			"testpg.Main prints at the end of a run counts how many were plain, how many were retried and "+
			"how many reached the fallback; that count is the evidence this assertion would have given. The "+
			"right is still wanted elsewhere — internal/launcher/testpg.go's probe, docker/worker.Dockerfile's "+
			"test server and the sibling module's storetest all force — so packages/daemon/docker/legion-testpg "+
			"goes on granting it", login)
	}

	// A connection held open on the database is what the force is for; the plain drop must answer
	// 55006 for it, and testdb.Drop must clear it all the same.
	name, err := testdb.RandomName("legion_testpg_fallback_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	held, err := pgx.Connect(t.Context(), testdb.DatabaseURL(database, name))
	if err != nil {
		_, _ = conn.Exec(context.Background(), "drop database "+name+" with (force)")
		t.Fatalf("hold a connection on %s: %v", name, err)
	}
	defer held.Close(context.Background())

	_, plain := conn.Exec(t.Context(), "drop database "+name)
	var pgError *pgconn.PgError
	if !errors.As(plain, &pgError) || pgError.Code != "55006" {
		_, _ = conn.Exec(context.Background(), "drop database "+name+" with (force)")
		t.Fatalf("the plain drop of %s with a connection held answered %v, want SQLSTATE 55006", name, plain)
	}
	if err := testdb.Drop(t.Context(), conn, name); err != nil {
		t.Fatalf("%s holds pg_signal_backend, so testdb.Drop's forced last attempt should have cleared the held connection: %v", login, err)
	}
}

// adminURL is dsn pointed at the admin database, and dsn's server for naming a database on it.
func adminURL(t testing.TB, dsn string) (string, url.URL) {
	t.Helper()
	server, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", errors.Unwrap(err))
	}
	admin := *server
	admin.Path = "/postgres"
	return admin.String(), *server
}
