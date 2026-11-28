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

// embeddedMigrations is how many migrations the store embeds: the .up.sql files of ../migrations,
// the directory store embeds. It fails t where there is none, since a copy's count of applied
// migrations would then have nothing to be compared with.
func embeddedMigrations(t *testing.T) int {
	t.Helper()
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
	return want
}

// newLogin makes a login of the test's own on server, with CREATEDB and a random password, named
// prefix, an underscore and 16 random hex digits, and returns server's URL as that login. No
// template of the login is on the server until something asks for one (template). When t ends it
// drops, as the login, which owns them, every database the login still owns: its template and
// every copy or build of it that no other cleanup dropped. Then it drops the login. Its cleanup is
// registered before the login makes anything, so it runs after the drop of each database the
// login makes (testdb.DatabaseOn's cleanup).
//
// Its own statements, as server's login and as the new one, run in an empty database of the test's
// own (prefix_admin_<hex>, testdb.DatabaseOn) that it makes before the login, never in the
// database server names or in the server's postgres database: logins and pg_database are the
// server's, the same from any of its databases, and PUBLIC may connect to a new database, so the
// new login can too. server's login owns that database, so the cleanup leaves it, and its drop,
// registered before the cleanup, runs after it.
func newLogin(t *testing.T, server url.URL, prefix string) url.URL {
	t.Helper()
	ctx := context.Background()
	role, err := testdb.NewDatabaseName(prefix)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	adminURL := testdb.DatabaseOn(t, server.String(), "", prefix+"_admin")
	ownerURL, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatal("the URL of the test's own database does not parse")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the test's own database on the server: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create role "+role+" login createdb password '"+password+"'"); err != nil {
		t.Fatalf("create the login %s: %v", role, err)
	}
	login := server
	login.User = url.UserPassword(role, password)
	ownerURL.User = login.User
	t.Cleanup(func() {
		ctx := context.Background()
		owner, err := pgx.Connect(ctx, ownerURL.String())
		if err != nil {
			t.Errorf("connect as the login %s to drop what it owns: %v", role, err)
		} else {
			rows, err := owner.Query(ctx, "select datname from pg_database where datdba = (select oid from pg_roles where rolname = current_user)")
			var owned []string
			if err == nil {
				owned, err = pgx.CollectRows(rows, pgx.RowTo[string])
			}
			if err != nil {
				t.Errorf("list the databases the login %s owns: %v", role, err)
			}
			for _, name := range owned {
				statement := "drop database if exists " + pgx.Identifier{name}.Sanitize() + " with (force)"
				if _, err := owner.Exec(ctx, statement); err != nil {
					t.Errorf("%s: %v", statement, err)
				}
			}
			owner.Close(ctx)
		}
		admin, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect to drop the login %s: %v", role, err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "drop role if exists "+role); err != nil {
			t.Errorf("drop the login %s: %v", role, err)
		}
	})
	return login
}

// holdTemplateLock takes the template lock, templateLockKey as takeTemplateLock takes it, on a
// connection of its own to databaseURL and returns that connection, which a cleanup closes when t
// ends, giving the lock back where the test has not. Postgres keeps an advisory lock per database,
// so builders wait for what it holds only where databaseURL names the server's lockDatabase.
func holdTemplateLock(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect the holder of the template lock: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "select pg_advisory_lock(hashtext($1))", templateLockKey); err != nil {
		t.Fatalf("take the template lock: %v", err)
	}
	return conn
}
