package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// A second login with CREATEDB runs the broker's tests on a server where another login already
// made the template of the same migration set: URL says the server's login needs CREATEDB, and
// every later run of the same migration set on that server copies the one template. At the merge
// base each test binary's database was a copy of template1, which any login may copy, so a second
// login ran the tests. Postgres copies a database only for its owner, for a superuser, or for any
// login where the database is marked a template (datistemplate), so a template that one login
// made and left unmarked refuses every other login ("permission denied to copy database"). The
// test makes the second login, has the server URL's login find or make the template first, then
// makes a test database as the second login the way URL does, and reads the migrations it holds.
func TestASecondLoginCopiesTheTemplateAnotherLoginMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := serverURL(t)
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatalf("%s does not parse", DatabaseURLEnv)
	}
	entries, err := os.ReadDir("../migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".up.sql") {
			want++
		}
	}
	if want == 0 {
		t.Fatal("../migrations holds no .up.sql file, so there is nothing to compare the copy with")
	}
	// The server URL's login finds or makes the template first, as the first binary of a run does.
	template(t, raw)

	role, err := testdb.NewDatabaseName("broker_login_test")
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	adminURL := testdb.DatabaseURL(*server, lockDatabase)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the %s database of %s: %v", lockDatabase, DatabaseURLEnv, err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create role "+role+" login createdb password '"+password+"'"); err != nil {
		t.Fatalf("create the second login: %v", err)
	}
	second := *server
	second.User = url.UserPassword(role, password)
	secondRaw := second.String()
	// Registered before the second login makes anything, so it runs after each of their drops: every
	// database the second login still owns (a template of its own, were there one) is dropped as that
	// login, which owns it, and then the login goes.
	t.Cleanup(func() {
		ctx := context.Background()
		owner, err := pgx.Connect(ctx, testdb.DatabaseURL(second, lockDatabase))
		if err != nil {
			t.Errorf("connect as the second login to drop what it owns: %v", err)
		} else {
			rows, err := owner.Query(ctx, "select datname from pg_database where datdba = (select oid from pg_roles where rolname = current_user)")
			var owned []string
			if err == nil {
				owned, err = pgx.CollectRows(rows, pgx.RowTo[string])
			}
			if err != nil {
				t.Errorf("list the databases the second login owns: %v", err)
			}
			for _, name := range owned {
				for _, statement := range []string{
					"alter database " + pgx.Identifier{name}.Sanitize() + " is_template false",
					"drop database if exists " + pgx.Identifier{name}.Sanitize() + " with (force)",
				} {
					if _, err := owner.Exec(ctx, statement); err != nil {
						t.Errorf("%s: %v", statement, err)
					}
				}
			}
			owner.Close(ctx)
		}
		admin, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect to drop the second login: %v", err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "drop role if exists "+role); err != nil {
			t.Errorf("drop the second login: %v", err)
		}
	})

	// URL's own body, for the second login's server URL.
	dsn := testdb.DatabaseOn(t, secondRaw, template(t, secondRaw), "broker_test", "search_path="+testSchema)
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
