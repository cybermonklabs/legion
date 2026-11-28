package storetest

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A second login with CREATEDB runs the broker's tests on a server where another login already
// made the template of the same migration set: URL says the server's login needs CREATEDB, and
// every later run of the same migration set on that server finds the template that is there.
// Postgres copies a database only for its owner, for a superuser, or for any login where the
// database is marked a template (datistemplate), and a template is left unmarked, so the one the
// first login made refuses every other login ("permission denied to copy database"): the second
// login's test database is a copy of a template of that login's own (template), and records every
// migration the store embeds. The test makes the second login, has the server URL's login find or
// make its template first, then makes a test database as the second login the way URL does
// (urlOn), and reads the migrations it holds.
func TestASecondLoginGetsADatabaseWhereAnotherLoginMadeTheTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatalf("%s does not parse", DatabaseURLEnv)
	}
	want := embeddedMigrations(t)
	// The server URL's login finds or makes the template first, as the first binary of a run does.
	template(t, raw)

	// Made before the second login makes anything, so newLogin's cleanup, which drops the second
	// login's template and every copy it still owns and then the login, runs after each copy's own
	// drop.
	second := newLogin(t, *server, "broker_login_test")
	dsn := urlOn(t, second.String())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as the second login to its test database: %v", err)
	}
	defer conn.Close(ctx)
	var recorded int
	if err := conn.QueryRow(ctx, "select count(*) from broker_schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("read the second login's copy's applied migrations: %v", err)
	}
	if recorded != want {
		t.Fatalf("the second login's copy records %d migrations, want the %d the store embeds", recorded, want)
	}
}
