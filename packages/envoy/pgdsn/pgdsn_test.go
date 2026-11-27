package pgdsn

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// urlDSN is a postgres URL with the user legion and password, built from parts, so no source line
// holds a URL with userinfo and its "@".
func urlDSN(password string) string {
	return "postgres://legion:" + password + "@" + "db.invalid:5432/legion"
}

// madeUp are the parts of the made-up passwords below; no refusal and no connect error of a DSN
// that spills holds any of them.
var madeUp = []string{"madeuphead", "zqtail", "48213", "db.invalid"}

// spilled are DSNs whose made-up password leaves part of itself after pgx's user part, each named
// by its shape: the "@" shapes leave the password's tail in the host list, the database or the
// query, the "/" shapes end pgx's search for the userinfo before the password's "@". The round-8
// shapes (`@…/` and after) passed the rule before it read every "@" after the user part, which cut
// the host list at the first "/" or "?" and read nothing after it once a userinfo was there.
var spilled = map[string]string{
	"@":              urlDSN("madeuphead0123@zqtail0123"),
	"/ after digits": urlDSN("48213579/zqtail0123"),
	"/ and ?":        "postgresql://legion:48213579/x?zqtail0123" + "@db.invalid/legion",
	"@…/":            urlDSN("madeuphead0123@zqtailone/zqtailtwo"),
	"@…?":            urlDSN("madeuphead0123@zqtailone?zqtailtwo"),
	"@…?k=":          urlDSN("madeuphead0123@zqtailone?k="),
	"@…?k=…":         urlDSN("madeuphead0123@zqtailone?k=zqtailtwo"),
	"@…,…/":          urlDSN("madeuphead0123@zqtailone,zqtailtwo/zqtailthree"),
	"@[…/…]":         urlDSN("madeuphead0123@[zqtailone/zqtailtwo]"),
	"48213/…?k=v":    urlDSN("48213/zqtailone?k=v"),
}

// Refusal refuses each spilled DSN by the setting alone, says to write the "@" as %40, and holds
// no part of the password.
func TestRefusalNamesTheSettingOfEachSpilledShape(t *testing.T) {
	for shape, dsn := range spilled {
		t.Run(shape, func(t *testing.T) {
			err := Refusal("postgres_dsn", dsn)
			if err == nil {
				t.Fatalf("Refusal = nil, want the %s shape refused", shape)
			}
			if !strings.HasPrefix(err.Error(), `postgres_dsn cannot be read: it holds an "@" after its user name and password`) ||
				!strings.Contains(err.Error(), "part of its user name, password or database name") || !strings.Contains(err.Error(), "%40") {
				t.Errorf("Refusal = %v, want it named by postgres_dsn with the %%40 remedy", err)
			}
			for _, part := range madeUp {
				if strings.Contains(err.Error(), part) {
					t.Errorf("Refusal = %v, which holds %q", err, part)
				}
			}
		})
	}
}

// Refusal keeps a DSN whose only "@" ends its userinfo, one whose "@" and "/" are percent-encoded,
// one whose password holds a "?" or a "#" (which pgx reads as the password), IPv6 and unix-socket
// hosts, keyword DSNs (a database whose name begins with "postgres", a password holding or
// beginning with "postgres:" or a scheme, an sslpassword alike, an application_name that is a URL,
// a database name holding a ":", a Cloud SQL IAM user holding an "@", a Cloud SQL socket for a
// project whose ID ends in "postgres", an IPv6 host and hostaddr, a host list of a socket, a name
// and an IPv6 address, a custom setting whose name holds "$" among them), the same in a URL's
// query, the empty DSN, which takes PG* from the environment, and the shapes the deployments
// write (the broker's, the daemon's and Dispatch's URL with its query, and the session database's,
// whose password holds a %40).
func TestRefusalKeepsADSNWithNoAtPastItsUserPart(t *testing.T) {
	at := "@"
	for _, dsn := range []string{
		urlDSN("legion"),
		urlDSN("pa?ss"),
		urlDSN("pa#ss"),
		urlDSN("p%40ss%2Fword"),
		"postgres://legion" + at + "[fe80::1%25eth0]:5432/legion",
		"postgres://[::1]:5432,db.invalid/legion?sslmode=disable",
		"postgres:///legion?host=/var/run/postgresql",
		"postgres:///legion?host=/cloudsql/p:r:i&user=sa%40project.iam",
		"postgres://broker:Abc123def456" + at + "127.0.0.1:5432/cipher?sslmode=disable&pool_max_conns=4",
		"postgres://sessions_w:Abc%40123" + at + "10.1.2.3:5432/sessions?sslmode=require&application_name=omp-sessions%20w",
		"postgresql://legion:Abc123def456" + at + "db.example:5432/legion",
		"host=db.invalid user=legion password=p@ss/word dbname=legion",
		"host=db.invalid user=postgres dbname=postgres sslmode=disable",
		"dbname=postgres_main host=/var/run/postgresql",
		"host=/var/run/postgresql dbname=legion",
		"host=db.invalid user=x password=postgresABC dbname=x",
		"host=/cloudsql/p:r:i user=sa" + at + "project.iam dbname=legion",
		"host=/cloudsql/acme-postgres:us-central1:db dbname=legion user=sa" + at + "acme-postgres.iam",
		"host=db.invalid user=legion password=Postgres:Admin1 dbname=legion",
		"host=db.invalid user=legion password='" + "Postgres:Admin1' dbname=legion",
		"host=db.invalid user=legion password=pgpostgresql:x dbname=legion",
		"host=db.invalid user=legion password=pg://x dbname=legion",
		"host=db.invalid dbname=legion myext.set$ting=1",
		"host=db.invalid dbname=legion options='-c search_path=app'",
		"host = db.invalid  port = 5432 dbname = legion password = 'with space'",
		"host=db.invalid user=legion password=https://x dbname=legion",
		"host=db.invalid user=legion sslpassword=Postgres:Admin1 sslkey=/k dbname=legion",
		"host=db.invalid user=legion sslpassword='" + "Postgres:Admin1' sslkey=/k dbname=legion",
		"host=db.invalid user=legion sslpassword=pg://x sslkey=/k dbname=legion",
		"host=db.invalid dbname=legion application_name=https://ci.example/run/1",
		"host=db.invalid dbname=app:prod",
		"host=::1 dbname=legion",
		"host=fe80::1%eth0 dbname=legion",
		"hostaddr=::1 dbname=legion",
		"host=/tmp,db.invalid,::1 port=5432,5433,5434 dbname=legion",
		"postgres:///legion?host=/cloudsql/acme-postgres:us-central1:db&user=sa%40acme-postgres.iam",
		"postgres://legion" + at + "[::1]:5432/legion?sslpassword=Postgres%3AAdmin1&sslkey=/k",
		"postgres://db.invalid/legion?application_name=https://ci.example/run/1",
		"postgres://db.invalid/legion?hostaddr=::1&dbname=app:prod",
		"host=db.invalid dbname=legion options='-c app.url=https://ci.example/run/1'",
		"postgres://db.invalid/legion?host= ::1",
		"postgres://db.invalid/legion?sslmode=disable&",
		"postgres://db.invalid/legion?options=-c%20app.url%3Dhttps://ci.example/run/1",
		"host=db.invalid user=legion sslpassword=https://u:Postgres" + at + "h sslkey=/k dbname=legion",
		"",
	} {
		if err := Refusal("postgres_dsn", dsn); err != nil {
			t.Errorf("Refusal(%q) = %v, want nil", dsn, err)
		}
	}
}

// Every host form pgx takes loads, in a keyword DSN, in a URL's host list and in a URL's query:
// a name, an IPv4 and an IPv6 address (with a zone too), a host list with one port or a port for
// each, the Cloud SQL socket of a project whose ID ends in "postgres", a list mixing a socket, a
// name and an IPv6 address, and an IPv4 or IPv6 hostaddr beside a host or alone. Each is kept by
// the host rule, whose ":" is refused only in an entry that is no IP address or socket directory,
// and pgx parses each.
func TestEveryHostFormPgxTakesIsKept(t *testing.T) {
	forms := []string{
		"host=db.invalid port=5432 dbname=legion",
		"host=127.0.0.1 port=5432 dbname=legion",
		"host=::1 port=5432 dbname=legion",
		"host=fe80::1%eth0 port=5432 dbname=legion",
		"host=db1.invalid,db2.invalid port=5432 dbname=legion",
		"host=db1.invalid,db2.invalid port=5432,5433 dbname=legion",
		"host=/cloudsql/acme-postgres:us-central1:db dbname=legion",
		"host=/tmp,db.invalid,::1 port=5432,5433,5434 dbname=legion",
		"hostaddr=::1 port=5432 dbname=legion",
		"hostaddr=127.0.0.1 port=5432 dbname=legion",
		"host=db.invalid hostaddr=2001:db8::1 port=5432 dbname=legion",
		"postgres://db.invalid:5432/legion",
		"postgres://127.0.0.1:5432/legion",
		"postgres://[::1]:5432/legion",
		"postgres://[fe80::1%25eth0]:5432/legion",
		"postgres://db1.invalid:5432,db2.invalid:5433/legion",
		"postgres://[2001:db8::1]:5432,db.invalid/legion",
		"postgres:///legion?host=db.invalid&port=5432",
		"postgres:///legion?host=::1&port=5432",
		"postgres:///legion?host=db1.invalid,db2.invalid&port=5432,5433",
		"postgres:///legion?host=/cloudsql/acme-postgres:us-central1:db",
		"postgres:///legion?host=/tmp,db.invalid,::1&port=5432,5433,5434",
		"postgres:///legion?hostaddr=::1&port=5432",
		"postgres://db.invalid/legion?hostaddr=2001:db8::1",
		"postgres:///legion?host= ::1&port=5432",
	}
	for _, dsn := range forms {
		if err := Refusal("postgres_dsn", dsn); err != nil {
			t.Errorf("Refusal(%q) = %v, want nil", dsn, err)
		}
		if _, err := pgconn.ParseConfig(dsn); err != nil {
			t.Errorf("ParseConfig(%q) = %v, want pgx to take it", dsn, err)
		}
	}
}

// printedShapes are DSNs Postgres or pgx prints with their made-up password, each with the start
// of the refusal that names it. A value that names the postgres scheme but that pgx reads as no
// URL (upper or mixed case, a space before it, or no "//"), and any value with a key no keyword or
// parameter name has (a misspelt scheme, a SQLAlchemy or JDBC URL, a BOM before the scheme, a key
// holding a space), pgx reads up to its first "=" as one keyword, sends that name to the server as
// a parameter's, and a server that admits the client answers `invalid configuration parameter name
// "<the URL>"`. A user, a database, a host or a hostaddr, in a keyword DSN or a URL's user info,
// path or query, pgx names in its connect error, and the server echoes the user and the database:
// one holding "://" anywhere (whatever stands before the scheme: a space, punctuation, `jdbc:`),
// a user holding a ":", a database holding an "@", and a host or hostaddr holding an "@", an "="
// or a ":" outside an IP address. Each DSN that names host=127.0.0.1 port=1, or 127.0.0.1:1 in a
// URL, is pointed at the echo server by its port.
var printedShapes = map[string]struct{ dsn, want string }{
	"an upper-case scheme":                             {"POSTGRES://legion:madeuphead0123" + "@" + "db.invalid:5432/legion?sslmode=require", "it names the postgres scheme"},
	"a mixed-case scheme":                              {"Postgresql://legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=require", "it names the postgres scheme"},
	"a space before it":                                {" postgres://legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=require", "it names the postgres scheme"},
	"a tab before it":                                  {"\tpostgresql://legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=require", "it names the postgres scheme"},
	"no // after it":                                   {"postgres:legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=require", "it names the postgres scheme"},
	"postgresql+psycopg2://":                           {"postgresql+psycopg2://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"postgresql+asyncpg://":                            {"postgresql+asyncpg://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"postgress://":                                     {"postgress://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"postgre://":                                       {"postgre://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"pgsql://":                                         {"pgsql://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"pg://":                                            {"pg://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"postgres//":                                       {"postgres//app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"postgres;//":                                      {"postgres;//app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"jdbc:postgresql://":                               {"jdbc:postgresql://db.invalid/app?user=app&password=madeuphead0123&sslmode=disable", keyMalformedWords},
	"a BOM before postgres:":                           {"\ufeffpostgres:legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=disable", keyMalformedWords},
	"a BOM before postgres://":                         {"\ufeffpostgres://legion:madeuphead0123" + "@" + "db.invalid/legion?sslmode=disable", keyMalformedWords},
	"a misspelt second key":                            {"host=127.0.0.1 port=1 postgress://app:madeuphead0123" + "@" + "db.invalid/app?sslmode=disable", keyMalformedWords},
	"a password of two words":                          {"host=127.0.0.1 port=1 user=legion password=madeuphead0123 zqtailtwo dbname=legion", keyMalformedWords},
	"a URL as dbname":                                  {"host=127.0.0.1 port=1 dbname=postgres://u:madeuphead0123" + "@" + "db.invalid/x", urlWords},
	"a URL as user":                                    {"host=127.0.0.1 port=1 user=Postgresql://u:madeuphead0123" + "@" + "db.invalid/x dbname=x", urlWords},
	"postgres:/ as dbname":                             {"host=127.0.0.1 port=1 dbname=postgres:/app:madeuphead0123" + "@" + "db/app", databaseWords},
	"postgres: as dbname":                              {"host=127.0.0.1 port=1 dbname=postgres:u:madeuphead0123" + "@" + "db.invalid", databaseWords},
	"postgress:// as dbname":                           {"host=127.0.0.1 port=1 dbname=postgress://u:madeuphead0123" + "@" + "db.invalid/x", urlWords},
	"pg:// as dbname":                                  {"host=127.0.0.1 port=1 dbname=pg://u:madeuphead0123" + "@" + "db.invalid/x", urlWords},
	"postgresql+asyncpg:// as dbname":                  {"host=127.0.0.1 port=1 dbname=postgresql+asyncpg://u:madeuphead0123" + "@" + "db.invalid/x", urlWords},
	"a JDBC URL as dbname":                             {"host=127.0.0.1 port=1 dbname=jdbc:postgresql://app:madeuphead0123" + "@" + "db.invalid/app", urlWords},
	"a quoted space before a URL":                      {"host=127.0.0.1 port=1 dbname=' pg://u:madeuphead0123" + "@" + "h'", urlWords},
	"an escaped space before a URL":                    {`host=127.0.0.1 port=1 dbname=\ postgres://u:madeuphead0123` + "@" + "h/x", urlWords},
	"a URL in parentheses":                             {"host=127.0.0.1 port=1 dbname=(postgres://u:madeuphead0123" + "@" + "h/x)", urlWords},
	"a URL in angle brackets":                          {"host=127.0.0.1 port=1 dbname=<postgres://u:madeuphead0123" + "@" + "h/x>", urlWords},
	"a URL with a token in its path":                   {"host=127.0.0.1 port=1 dbname=https://hooks.example/madeuphead0123", urlWords},
	"a quoted pgsql:// user":                           {"host=127.0.0.1 port=1 user='pgsql://u:madeuphead0123" + "@" + "h' dbname=x", urlWords},
	"a postgres: user":                                 {"host=127.0.0.1 port=1 user=postgres:madeuphead0123 dbname=x", userHoldsColonWords},
	"a user holding :":                                 {"host=127.0.0.1 port=1 user=app:madeuphead0123 dbname=app", userHoldsColonWords},
	"a user holding : and @":                           {"host=127.0.0.1 port=1 user=app:madeuphead0123" + "@" + "db.invalid dbname=app", userHoldsColonWords},
	"a database holding user info":                     {"host=127.0.0.1 port=1 dbname=app:madeuphead0123" + "@" + "db.invalid/app", databaseWords},
	"a database of a password and @":                   {"host=127.0.0.1 port=1 dbname=madeuphead0123" + "@" + "db.invalid", databaseWords},
	"a database key holding @":                         {"host=127.0.0.1 port=1 database=app:madeuphead0123" + "@" + "db.invalid", databaseWords},
	"a user and password host":                         {"host=app:madeuphead0123" + "@" + "127.0.0.1 port=1 dbname=app", hostWords},
	"a hostaddr holding @":                             {"hostaddr=app:madeuphead0123" + "@" + "127.0.0.1 port=1 dbname=app", hostWords},
	"a host missing a space":                           {"host=127.0.0.1password=madeuphead0123 port=1 dbname=x", hostWords},
	"a named host missing a space":                     {"host=db.invalidpassword=madeuphead0123 dbname=x", hostWords},
	"a hostaddr missing a space":                       {"hostaddr=127.0.0.1password=madeuphead0123 port=1 dbname=x", hostWords},
	"a host holding :":                                 {"host=app:madeuphead0123 port=1 dbname=app", hostWords},
	"a host list entry holding :":                      {"host=/tmp,app:madeuphead0123 port=1 dbname=app", hostWords},
	"a hostaddr holding :":                             {"hostaddr=app:madeuphead0123 port=1 dbname=app", hostWords},
	"a URL's ?user holding :":                          {"postgres://127.0.0.1:1/legion?user=app:madeuphead0123", userHoldsColonWords},
	"a URL's user info holding :":                      {"postgres://app%3Amadeuphead0123" + "@" + "127.0.0.1:1/legion", userHoldsColonWords},
	"a URL's ?dbname holding a URL":                    {"postgres://127.0.0.1:1/legion?dbname=pg://u:madeuphead0123%40h", urlWords},
	"a URL's ?dbname with a token path":                {"postgres://127.0.0.1:1/legion?dbname=https://hooks.example/madeuphead0123", urlWords},
	"a URL's path holding a URL":                       {"postgres://127.0.0.1:1/pg://u:madeuphead0123%40h", urlWords},
	"a URL's ?user holding a URL":                      {"postgres://127.0.0.1:1/legion?user=jdbc:postgresql://u:madeuphead0123%40h", urlWords},
	"a URL's ?dbname holding @":                        {"postgres://127.0.0.1:1/legion?dbname=app:madeuphead0123%40db.invalid", databaseWords},
	"a URL's ?database holding @":                      {"postgres://127.0.0.1:1/legion?database=madeuphead0123%40db.invalid", databaseWords},
	"a URL's path holding @":                           {"postgres://127.0.0.1:1/app:madeuphead0123%40db.invalid", databaseWords},
	"a URL's ?host holding :":                          {"postgres://127.0.0.1:1/legion?host=app:madeuphead0123", hostWords},
	"a URL's ?host holding @":                          {"postgres://127.0.0.1:1/legion?host=app:madeuphead0123%40127.0.0.1", hostWords},
	"a URL's ?hostaddr holding @":                      {"postgres://127.0.0.1:1/legion?hostaddr=app:madeuphead0123%40127.0.0.1", hostWords},
	"a URL's bracketed host with :":                    {"postgres://[app:madeuphead0123]:1/legion", hostWords},
	"a URL credential as a setting":                    {"host=127.0.0.1 port=1 user=legion dbname=legion datestyle=https://u:madeuphead0123" + "@" + "h", urlCredentialWords},
	"a URL credential in options":                      {"host=127.0.0.1 port=1 user=legion dbname=legion options='-c datestyle=https://u:madeuphead0123" + "@" + "h'", urlCredentialWords},
	"a URL credential glued in options":                {"host=127.0.0.1 port=1 user=legion dbname=legion options='-cdatestyle=https://u:madeuphead0123" + "@" + "h/x'", urlCredentialWords},
	"a URL's setting with a credential":                {"postgres://127.0.0.1:1/legion?user=legion&datestyle=https://u:madeuphead0123%40h", urlCredentialWords},
	"a URL's options with a credential":                {"postgres://127.0.0.1:1/legion?user=legion&options=-c%20datestyle%3Dhttps://u:madeuphead0123%40h", urlCredentialWords},
	"a URL's user after a space":                       {"postgres://127.0.0.1:1/legion?sslmode=disable& user=app:madeuphead0123", userHoldsColonWords},
	"a URL's user key with a space":                    {"postgres://127.0.0.1:1/legion?user =app:madeuphead0123", userHoldsColonWords},
	"a URL's dbname after a space":                     {"postgres://127.0.0.1:1/legion?sslmode=disable& dbname=app:madeuphead0123%40h", databaseWords},
	"a URL's host after a space":                       {"postgres://127.0.0.1:1/legion?sslmode=disable& host=app:madeuphead0123", hostWords},
	"a URL's query key holding a URL":                  {"postgres://127.0.0.1:1/legion?user=legion&https://u:madeuphead0123%40h=1", queryKeyWords},
	"a URL's setting with a %20 in its password":       {"postgres://127.0.0.1:1/legion?user=legion&datestyle=https://u:madeup%20madeuphead0123%40h", urlCredentialWords},
	"a URL's setting with a tab in its password":       {"postgres://127.0.0.1:1/legion?user=legion&datestyle=https://u:madeup%09madeuphead0123%40h", urlCredentialWords},
	"a quoted setting with a space in its password":    {"host=127.0.0.1 port=1 user=legion dbname=legion datestyle='https://u:madeup madeuphead0123" + "@" + "h'", urlCredentialWords},
	"a statement_timeout with a space in its password": {"host=127.0.0.1 port=1 user=legion dbname=legion statement_timeout='https://u:madeup madeuphead0123" + "@" + "h'", urlCredentialWords},
	"options with an escaped space in a password":      {`host=127.0.0.1 port=1 user=legion dbname=legion options='-c datestyle=https://u:madeup\\ madeuphead0123` + "@" + `h'`, urlCredentialWords},
	"a setting with a / in its password":               {"host=127.0.0.1 port=1 user=legion dbname=legion datestyle=https://u:madeup/madeuphead0123" + "@" + "h", urlCredentialWords},
	"a URL's setting with a %2F in its password":       {"postgres://127.0.0.1:1/legion?user=legion&datestyle=https://u:madeup%2Fmadeuphead0123%40h", urlCredentialWords},
	"a quoted setting with a ? in its password":        {"host=127.0.0.1 port=1 user=legion dbname=legion datestyle='https://u:madeup?madeuphead0123" + "@" + "h'", urlCredentialWords},
	"a quoted setting with a # in its password":        {"host=127.0.0.1 port=1 user=legion dbname=legion datestyle='https://u:madeup#madeuphead0123" + "@" + "h'", urlCredentialWords},
	"a statement_timeout with a / in its password":     {"host=127.0.0.1 port=1 user=legion dbname=legion statement_timeout=https://u:madeup/madeuphead0123" + "@" + "h", urlCredentialWords},
	"options with a / in a password":                   {"host=127.0.0.1 port=1 user=legion dbname=legion options='-c datestyle=https://u:madeup/madeuphead0123" + "@" + "h'", urlCredentialWords},
}

// A custom parameter's name holding a letter outside ASCII (`app.naïve`), which guc.c's own check
// admits and libpq refuses, is refused as libpq refuses it, in a keyword DSN and in a URL's query.
func TestANonASCIIParameterNameIsRefusedAsLibpqRefusesIt(t *testing.T) {
	for dsn, want := range map[string]string{
		"host=db.invalid dbname=legion app.naïve=1":     keyMalformedWords,
		"postgres://db.invalid/legion?app.na%C3%AFve=1": queryKeyWords,
	} {
		if err := Refusal("postgres_dsn", dsn); err == nil || !strings.HasPrefix(err.Error(), "postgres_dsn cannot be read: "+want) {
			t.Errorf("Refusal(%q) = %v, want it refused: %s", dsn, err, want)
		}
	}
}

const (
	keyMalformedWords   = "it is no postgres:// or postgresql:// URL, and a key of it holds a character"
	urlWords            = "a value of it other than a password, its user, database, host or hostaddr, holds \"://\""
	urlCredentialWords  = "a value of it other than a password holds a URL with a user name or a password"
	queryKeyWords       = "a key of its query holds a character no keyword or parameter name has"
	userHoldsColonWords = "its user holds a \":\""
	hostWords           = "its host or hostaddr holds an \"@\", an \"=\", or a \":\" in an entry that is no IP address or socket directory"
	databaseWords       = "its database name holds an \"@\""
)

// maskedShapes are keyword DSNs Refusal keeps, since every key and value is a keyword DSN's, whose
// password ConnectError masks wherever pgx or the server prints it: a user missing the space
// before password= (pgx reads it as the user, so the password it parsed is empty), with the
// password quoted, or holding a quote, two quotes in a row, a backtick, or a space pgx read from
// a backslash before it.
var maskedShapes = map[string]string{
	"a missing space before password=":    "host=127.0.0.1 port=1 user=apppassword=madeuphead0123 dbname=app",
	"a quoted password after a user":      "host=127.0.0.1 port=1 user=apppassword='" + "madeuphead0123' dbname=app",
	"a quote inside a password":           "host=127.0.0.1 port=1 user=apppassword=zq'madeuphead0123 dbname=app",
	"two single quotes inside a password": "host=127.0.0.1 port=1 user=apppassword=zq'" + "'madeuphead0123 dbname=app",
	"two double quotes inside a password": `host=127.0.0.1 port=1 user=apppassword=zq""madeuphead0123 dbname=app`,
	"a backtick inside a password":        "host=127.0.0.1 port=1 user=apppassword=zq`madeuphead0123 dbname=app",
	"an escaped space inside a password":  `host=127.0.0.1 port=1 user=apppassword=zq\ madeuphead0123 dbname=app`,
	"a database missing a space":          "host=127.0.0.1 port=1 dbname=xpassword=madeuphead0123",
	"a quoted password with an escape":    `host=127.0.0.1 port=1 user=apppassword='made\'uphead0123' dbname=app`,
}

// Each shape Postgres or pgx would print is refused by the setting alone, and ConnectError of it
// names "the configured Postgres", with none of the driver's text.
func TestRefusalRefusesEachShapePostgresWouldPrint(t *testing.T) {
	for name, row := range printedShapes {
		t.Run(name, func(t *testing.T) {
			err := Refusal("postgres_dsn", row.dsn)
			if err == nil || !strings.HasPrefix(err.Error(), "postgres_dsn cannot be read: "+row.want) {
				t.Fatalf("Refusal = %v, want it refused by postgres_dsn: %s", err, row.want)
			}
			driver := errors.New("server error: FATAL: invalid configuration parameter name \"" + row.dsn + "\" (SQLSTATE 42602)")
			shown := ConnectError("postgres_dsn", row.dsn, driver)
			if !strings.HasPrefix(shown.Error(), "connect to the configured Postgres: ") || !errors.Is(shown, driver) {
				t.Errorf("ConnectError = %v, want the configured Postgres named, unwrapping to the driver's error", shown)
			}
			for _, printed := range []string{err.Error(), shown.Error()} {
				if strings.Contains(printed, "madeuphead") || strings.Contains(printed, "zqtail") {
					t.Errorf("%q holds part of the password", printed)
				}
			}
		})
	}
}

// echoUser is the one role the echo server knows.
const echoUser = "legion"

// serverParameterName is the shape of a parameter name a Postgres server knows: a custom setting's
// takes "$" after its first character.
var serverParameterName = regexp.MustCompile(`^[a-z_][a-z0-9_.$]*$`)

// echoServer is a Postgres server for these rows, on a port of this machine: it admits every
// client without a password, as a trust-auth server does, and answers each startup with the FATAL
// error such a server gives, the text it echoes: `invalid configuration parameter name "<name>"`
// for a parameter no server has, `role "<user>" does not exist` for a user other than echoUser,
// `invalid value for parameter "DateStyle": "<value>"` for a datestyle that names no date style,
// given as a setting of its own or as `-c datestyle=<value>` in options, and `database
// "<database>" does not exist` otherwise. It declines TLS and GSS encryption.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveEcho(conn)
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

// serveEcho answers one client's startup as echoServer does.
func serveEcho(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	backend := pgproto3.NewBackend(conn, conn)
	for {
		message, err := backend.ReceiveStartupMessage()
		if err != nil {
			return
		}
		startup, ok := message.(*pgproto3.StartupMessage)
		if !ok {
			if _, err := conn.Write([]byte("N")); err != nil {
				return
			}
			continue
		}
		backend.Send(echoOf(startup.Parameters))
		_ = backend.Flush()
		return
	}
}

// echoOf is the FATAL error echoServer answers a startup's parameters with.
func echoOf(parameters map[string]string) *pgproto3.ErrorResponse {
	fatal := func(code, message string) *pgproto3.ErrorResponse {
		return &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: message}
	}
	for name := range parameters {
		if !serverParameterName.MatchString(name) {
			return fatal("42602", "invalid configuration parameter name \""+name+"\"")
		}
	}
	if user := parameters["user"]; user != echoUser {
		return fatal("28000", "role \""+user+"\" does not exist")
	}
	if style, ok := startupSetting(parameters, "datestyle"); ok && !dateStyle.MatchString(style) {
		return fatal("22023", "invalid value for parameter \"DateStyle\": \""+style+"\"")
	}
	if timeout, ok := startupSetting(parameters, "statement_timeout"); ok && !timeoutValue.MatchString(timeout) {
		return fatal("22023", "invalid value for parameter \"statement_timeout\": \""+timeout+"\"")
	}
	return fatal("3D000", "database \""+parameters["database"]+"\" does not exist")
}

// dateStyle is the shape of a DateStyle a server takes: words such as ISO and MDY, with commas.
var dateStyle = regexp.MustCompile(`^[A-Za-z, ]+$`)

// timeoutValue is the shape of a statement_timeout a server takes: a number and maybe a unit.
var timeoutValue = regexp.MustCompile(`^[0-9]+ *[a-z]*$`)

// startupSetting is the value a startup sets for name, as a parameter of its own or as `-c
// name=<value>` (or `-cname=<value>`) among its options, which the server splits at spaces a
// backslash does not take (pg_split_opts), and whether it sets one.
func startupSetting(parameters map[string]string, name string) (string, bool) {
	if value, ok := parameters[name]; ok {
		return value, true
	}
	words := optionWords(parameters["options"])
	for i, word := range words {
		setting, ok := strings.CutPrefix(word, "-c")
		if ok && setting == "" && i+1 < len(words) {
			setting = words[i+1]
		}
		if value, ok := strings.CutPrefix(setting, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// optionWords is options split as the server's pg_split_opts splits it: at spaces, a backslash
// taking the next character into the word.
func optionWords(options string) []string {
	var words []string
	var word strings.Builder
	escaped := false
	for _, r := range options {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ' ' || r == '\t':
			if word.Len() > 0 {
				words = append(words, word.String())
				word.Reset()
			}
		default:
			word.WriteRune(r)
		}
	}
	if word.Len() > 0 {
		words = append(words, word.String())
	}
	return words
}

// leaked reports whether text holds part of a made-up password of these rows.
func leaked(text string) bool {
	return strings.Contains(text, "uphead0123") || strings.Contains(text, "zqtailtwo")
}

// noServerWord is true for the shapes no server sees: a host or hostaddr pgx cannot use, and a
// password of two words pgx refuses to parse; pgx never sends hostaddr, so no server prints it
// either. The JDBC URL's password stands in the value of its misspelt key, which no server
// echoes, so it is refused for the key alone.
func noServerWord(name, want string) bool {
	return want == hostWords || name == "a password of two words" || name == "jdbc:postgresql://"
}

// atPort points dsn, a row's, at port: `port=1` of a keyword DSN, `127.0.0.1:1/` of a URL.
func atPort(dsn, port string) string {
	dsn = strings.Replace(dsn, "port=1 ", "port="+port+" ", 1)
	return strings.Replace(dsn, "127.0.0.1:1/", "127.0.0.1:"+port+"/", 1)
}

// Against a server that admits the client without a password, pgx's own connect error of each
// shape it sends to the server prints the made-up password, in the parameter name, the user or
// the database the server echoes (the row's control: a shape whose echo holds none of it is
// named, since the row would test nothing for it); ConnectError of each holds none of it, the
// refused shapes named "the configured Postgres" and the kept ones masked. The shapes pgx refuses
// to parse, or cannot connect with, are the next row's.
func TestConnectErrorOfAShapeAServerEchoesPrintsNoneOfIt(t *testing.T) {
	port := echoServer(t)
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", port)
	t.Setenv("PGUSER", echoUser)
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGCONNECT_TIMEOUT", "5")
	shapes := map[string]string{}
	for name, row := range printedShapes {
		if !noServerWord(name, row.want) {
			shapes[name] = row.dsn
		}
	}
	for name, dsn := range maskedShapes {
		shapes[name] = dsn
	}
	for name, dsn := range shapes {
		t.Run(name, func(t *testing.T) {
			dsn := atPort(dsn, port)
			_, err := pgconn.Connect(t.Context(), dsn)
			if err == nil {
				t.Fatal("pgconn.Connect to the echo server connected")
			}
			if !leaked(err.Error()) {
				t.Fatalf("pgx's own error %q holds none of the password, so this row tests nothing for it", err)
			}
			if shown := ConnectError("postgres_dsn", dsn, err); leaked(shown.Error()) {
				t.Errorf("ConnectError = %q, which holds part of the password", shown)
			}
		})
	}
}

// With nothing listening (port 1 on this machine refuses at once), pgx's own error of a DSN still
// prints a password the DSN puts where pgx names a value, in a keyword DSN or a URL: a database or
// user holding "://", a user holding a ":", a database holding an "@", a host holding the user
// name and password, a ":" or a password= missing the space before it, a password of two words,
// whose second word pgx refuses as a keyword, and each kept shape of maskedShapes. Each is the
// row's control: one whose pgx error holds none of the password is named. ConnectError of each
// holds none of it: the refused shapes are named "the configured Postgres", and the kept ones keep
// pgx's detail with every password masked (redactPassword).
func TestConnectErrorOfAShapePgxPrintsWithNoServerPrintsNoneOfIt(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "5")
	shapes := map[string]string{}
	for _, name := range []string{"a URL as dbname", "a URL as user", "postgres:/ as dbname", "postgres: as dbname",
		"postgress:// as dbname", "pg:// as dbname", "postgresql+asyncpg:// as dbname", "a JDBC URL as dbname",
		"a quoted space before a URL", "an escaped space before a URL", "a URL in parentheses", "a URL in angle brackets",
		"a quoted pgsql:// user", "a postgres: user", "a user holding :", "a user holding : and @",
		"a database holding user info", "a database of a password and @", "a database key holding @",
		"a user and password host", "a host missing a space", "a named host missing a space", "a host holding :",
		"a host list entry holding :", "a password of two words", "a URL's ?user holding :", "a URL's user info holding :",
		"a URL's ?dbname holding a URL", "a URL's path holding a URL", "a URL's ?user holding a URL",
		"a URL's ?dbname holding @", "a URL's ?database holding @", "a URL's path holding @", "a URL's ?host holding :",
		"a URL's ?host holding @", "a URL's bracketed host with :", "a URL with a token in its path",
		"a URL's ?dbname with a token path", "a URL's user after a space", "a URL's user key with a space",
		"a URL's dbname after a space", "a URL's host after a space"} {
		row, ok := printedShapes[name]
		if !ok {
			t.Fatalf("printedShapes holds no %q", name)
		}
		shapes[name] = row.dsn
	}
	for name, dsn := range maskedShapes {
		shapes[name] = dsn
	}
	for name, dsn := range shapes {
		t.Run(name, func(t *testing.T) {
			_, err := pgconn.Connect(t.Context(), dsn)
			if err == nil {
				t.Fatal("pgconn.Connect to port 1 connected")
			}
			if !leaked(err.Error()) {
				t.Fatalf("pgx's own error %q holds none of the password, so this row tests nothing for it", err)
			}
			if shown := ConnectError("postgres_dsn", dsn, err); leaked(shown.Error()) {
				t.Errorf("ConnectError = %q, which holds part of the password", shown)
			}
		})
	}
	for name, dsn := range maskedShapes {
		if err := Refusal("postgres_dsn", dsn); err != nil {
			t.Errorf("Refusal of %s = %v, want it kept: its keys and values are a keyword DSN's", name, err)
		}
	}
}

// Three keyword DSNs round 10 refused, which connected before it, are kept and reach the server:
// a password beginning with "Postgres:" and a custom setting whose name holds "$" each reach the
// echo server's answer to an admitted client (`database "legion" does not exist`), and a Cloud SQL
// socket for a project whose ID ends in "postgres" reaches the dial of its socket, not a refusal
// or a parse failure.
func TestAKeywordDSNRound10RefusedReachesTheServer(t *testing.T) {
	port := echoServer(t)
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGCONNECT_TIMEOUT", "5")
	for name, dsn := range map[string]string{
		"a password beginning with Postgres:": "host=127.0.0.1 port=" + port + " user=legion password=Postgres:Admin1 dbname=legion",
		"a custom setting holding $":          "host=127.0.0.1 port=" + port + " user=legion dbname=legion myext.set$ting=1",
	} {
		if err := Refusal("postgres_dsn", dsn); err != nil {
			t.Errorf("Refusal of %s = %v, want it kept", name, err)
			continue
		}
		if _, err := pgconn.Connect(t.Context(), dsn); err == nil || !strings.Contains(err.Error(), `database "legion" does not exist`) {
			t.Errorf("pgconn.Connect with %s = %v, want the server's answer to an admitted client", name, err)
		}
	}
	socket := "host=/cloudsql/acme-postgres:us-central1:db dbname=legion user=sa" + "@" + "acme-postgres.iam"
	if err := Refusal("postgres_dsn", socket); err != nil {
		t.Fatalf("Refusal of the Cloud SQL socket = %v, want it kept", err)
	}
	config, err := pgconn.ParseConfig(socket)
	if err != nil || config.Host != "/cloudsql/acme-postgres:us-central1:db" {
		t.Fatalf("ParseConfig of the Cloud SQL socket = %+v, %v; want its socket directory as the host", config, err)
	}
	var unparsed *pgconn.ParseConfigError
	if _, err := pgconn.Connect(t.Context(), socket); err == nil || errors.As(err, &unparsed) || !strings.Contains(err.Error(), "/cloudsql/acme-postgres:us-central1:db") {
		t.Errorf("pgconn.Connect with the Cloud SQL socket = %v, want the dial of its socket", err)
	}
}

// Values no driver prints are held to no rule of the four printed values, and a DSN holding one
// reaches the server: a password and an sslpassword beginning with "Postgres:" (pgx uses an
// sslpassword only as a client key's passphrase, and sends or prints it never) and an
// application_name that is a URL, as keyword values and as a URL's query values, each reach the
// echo server's answer to an admitted client (`database "legion" does not exist`). An IPv6 host
// (`host=::1`), and the Cloud SQL socket as a URL's host, reach the dial of their address, not a
// refusal or a parse failure.
func TestAValueNoDriverPrintsIsKeptAndReachesTheServer(t *testing.T) {
	port := echoServer(t)
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGCONNECT_TIMEOUT", "5")
	at := "@"
	for name, dsn := range map[string]string{
		"a password beginning with Postgres:":          "host=127.0.0.1 port=" + port + " user=legion password=Postgres:Admin1 dbname=legion",
		"an sslpassword beginning with Postgres:":      "host=127.0.0.1 port=" + port + " user=legion sslpassword=Postgres:Admin1 dbname=legion",
		"an application_name that is a URL":            "host=127.0.0.1 port=" + port + " user=legion dbname=legion application_name=https://ci.example/run/1",
		"a URL's password beginning with Postgres:":    "postgres://legion:Postgres%3AAdmin1" + at + "127.0.0.1:" + port + "/legion",
		"a URL's sslpassword beginning with Postgres:": "postgres://legion" + at + "127.0.0.1:" + port + "/legion?sslpassword=Postgres%3AAdmin1",
		"a URL's application_name that is a URL":       "postgres://legion" + at + "127.0.0.1:" + port + "/legion?application_name=https://ci.example/run/1",
	} {
		if err := Refusal("postgres_dsn", dsn); err != nil {
			t.Errorf("Refusal of %s = %v, want it kept", name, err)
			continue
		}
		if _, err := pgconn.Connect(t.Context(), dsn); err == nil || !strings.Contains(err.Error(), `database "legion" does not exist`) {
			t.Errorf("pgconn.Connect with %s = %v, want the server's answer to an admitted client", name, err)
		}
	}
	var unparsed *pgconn.ParseConfigError
	for name, row := range map[string]struct{ dsn, address string }{
		"an IPv6 host":                {"host=::1 port=1 user=legion dbname=legion", "::1"},
		"an IPv6 host in a URL":       {"postgres://legion" + at + "[::1]:1/legion", "::1"},
		"a Cloud SQL socket in a URL": {"postgres:///legion?host=/cloudsql/acme-postgres:us-central1:db&user=sa%40acme-postgres.iam", "/cloudsql/acme-postgres:us-central1:db"},
	} {
		if err := Refusal("postgres_dsn", row.dsn); err != nil {
			t.Errorf("Refusal of %s = %v, want it kept", name, err)
			continue
		}
		if _, err := pgconn.Connect(t.Context(), row.dsn); err == nil || errors.As(err, &unparsed) || !strings.Contains(err.Error(), row.address) {
			t.Errorf("pgconn.Connect with %s = %v, want the dial of %s", name, err, row.address)
		}
	}
}

// An "@" after the user part that is no password's is refused too, by design: the rule cannot
// tell a query value holding an "@" from a password's tail. Each loads written with %40, which pgx
// decodes in a query value.
func TestRefusalRefusesAnAtInAQueryValueAndPercent40LoadsIt(t *testing.T) {
	for refused, decoded := range map[string]struct{ dsn, user, database string }{
		"postgres://db.invalid:5432?sslrootcert=/etc/ssl/ca.pem&user=sa@project.iam": {"postgres://db.invalid:5432?user=sa%40project.iam", "sa@project.iam", ""},
		"postgres://?host=/tmp&user=sa@project.iam":                                  {"postgres://?host=/tmp&user=sa%40project.iam", "sa@project.iam", ""},
		"postgres:///legion?host=/cloudsql/p:r:i&user=sa@project.iam":                {"postgres:///legion?host=/cloudsql/p:r:i&user=sa%40project.iam", "sa@project.iam", "legion"},
		"postgres://legion@db.invalid:5432/legion?user=sa@project.iam":               {"postgres://legion@db.invalid:5432/legion?user=sa%40project.iam", "sa@project.iam", "legion"},
	} {
		if err := Refusal("postgres_dsn", refused); err == nil {
			t.Errorf("Refusal(%q) = nil, want it refused", refused)
		}
		if err := Refusal("postgres_dsn", decoded.dsn); err != nil {
			t.Errorf("Refusal(%q) = %v, want the %%40 form kept", decoded.dsn, err)
		}
		config, err := pgconn.ParseConfig(decoded.dsn)
		if err != nil {
			t.Fatalf("ParseConfig(%q) = %v", decoded.dsn, err)
		}
		if decoded.user != "" && config.User != decoded.user {
			t.Errorf("ParseConfig(%q).User = %q, want %q", decoded.dsn, config.User, decoded.user)
		}
		if decoded.database != "" && config.Database != decoded.database {
			t.Errorf("ParseConfig(%q).Database = %q, want %q", decoded.dsn, config.Database, decoded.database)
		}
	}
}

// A database name holding an "@" is refused however it is written: unencoded in a URL's path (an
// "@" past the user part), as %40 in the path or in the query, and in a keyword DSN, where a user
// name and password written before a host stand. pgx decodes the %40, so each names the same
// database, which the connect error prints.
func TestADatabaseNameHoldingAnAtIsRefusedWrittenEitherWay(t *testing.T) {
	at := "@"
	for dsn, want := range map[string]string{
		"postgres://h/my" + at + "db":      `it holds an "@" after its user name and password`,
		"postgres://h/my%40db":             databaseWords,
		"postgres://h/x?dbname=my%40db":    databaseWords,
		"postgres://h/x?database=my%40db":  databaseWords,
		"host=h dbname=my" + at + "db":     databaseWords,
		"host=h database='my" + at + "db'": databaseWords,
	} {
		if err := Refusal("postgres_dsn", dsn); err == nil || !strings.HasPrefix(err.Error(), "postgres_dsn cannot be read: "+want) {
			t.Errorf("Refusal(%q) = %v, want it refused: %s", dsn, err, want)
		}
	}
	config, err := pgconn.ParseConfig("postgres://h/my%40db")
	if err != nil || config.Database != "my"+at+"db" {
		t.Fatalf("ParseConfig of the %%40 path = %+v, %v; want the database my@db, which pgx decodes", config, err)
	}
}

// pgURLCorpus is the libpq split's corpus, which the Dispatch e2e (TypeScript), the shell and the
// search-measure (Python) splits share: each case checked against libpq's own PQconninfoParse.
type pgURLCorpus struct {
	Accepted []struct {
		URL          string `json:"url"`
		LibpqRefuses string `json:"libpq_refuses"`
	} `json:"accepted"`
	Refused []struct {
		URL   string `json:"url"`
		Fault string `json:"fault"`
	} `json:"refused"`
}

// spillFaults are the corpus's faults of a URL part of whose credential stands in the address.
var spillFaults = map[string]bool{
	"a host holds a raw @":                    true,
	"an @ sits between the first / and the ?": true,
}

// queryValueAt names the corpus's accepted URLs holding an unencoded "@" in a query value, which
// Refusal refuses by design (TestRefusalRefusesAnAtInAQueryValueAndPercent40LoadsIt).
// The corpus holds none today.
var queryValueAt = map[string]bool{}

// Refusal holds to the corpus: every URL it marks spilled is refused, and every URL it accepts is
// kept, but one holding an unencoded "@" in a query value, which queryValueAt names, and one whose
// query key no keyword's shape has, which libpq itself refuses (libpq_refuses) and pgx sends the
// server as a parameter's name: it is refused by the query key rule, and named so.
func TestRefusalHoldsToTheSharedPgURLCorpus(t *testing.T) {
	body, err := os.ReadFile("../scripts/fixtures/pg-url.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus pgURLCorpus
	if err := json.Unmarshal(body, &corpus); err != nil {
		t.Fatal(err)
	}
	spills := 0
	for _, refused := range corpus.Refused {
		if !spillFaults[refused.Fault] {
			continue
		}
		spills++
		if Refusal("postgres_dsn", refused.URL) == nil {
			t.Errorf("Refusal(%q) = nil, want the corpus's spill fault %q refused", refused.URL, refused.Fault)
		}
	}
	if spills != len(spillFaults) {
		t.Fatalf("the corpus holds %d spill faults, want %d: it no longer names them as this row reads", spills, len(spillFaults))
	}
	if len(corpus.Accepted) == 0 {
		t.Fatal("the corpus accepts no URL")
	}
	for _, accepted := range corpus.Accepted {
		err := Refusal("postgres_dsn", accepted.URL)
		queryKey := err != nil && strings.HasPrefix(err.Error(), "postgres_dsn cannot be read: "+queryKeyWords)
		switch {
		case queryValueAt[accepted.URL] && err == nil:
			t.Errorf("Refusal(%q) = nil, want the unencoded @ in its query value refused", accepted.URL)
		case accepted.LibpqRefuses != "" && queryKey:
		case !queryValueAt[accepted.URL] && err != nil:
			t.Errorf("Refusal(%q) = %v, want the corpus's accepted URL kept", accepted.URL, err)
		}
	}
}

// ConnectError of a spilled DSN names "the configured Postgres" and no detail of the driver's,
// whose text names the host and the database, and still unwraps to the driver's error.
func TestConnectErrorOfASpilledDSNPrintsNoPartOfThePassword(t *testing.T) {
	for shape, dsn := range spilled {
		t.Run(shape, func(t *testing.T) {
			driver := errors.New("failed to connect to `user=legion database=zqtailtwo@db.invalid`: zqtailone:5432 (48213579): lookup madeuphead0123: dial error")
			err := ConnectError("postgres_dsn", dsn, driver)
			if !strings.HasPrefix(err.Error(), "connect to the configured Postgres: ") {
				t.Errorf("ConnectError = %v, want the configured Postgres named", err)
			}
			for _, part := range madeUp {
				if strings.Contains(err.Error(), part) {
					t.Errorf("ConnectError = %v, which holds %q of the password", err, part)
				}
			}
			if !errors.Is(err, driver) {
				t.Errorf("ConnectError = %v, which does not unwrap to the driver's error", err)
			}
		})
	}
}

// A keyword DSN pgx cannot parse is named by its setting alone, with none of pgx's text, which
// quotes the DSN: pgx masks a password only written `password=<value>`, so spaces around the "=",
// or a space after it, print. The error still unwraps to the *pgconn.ParseConfigError. A password
// of several words is refused before pgx reads it (printedShapes): its second word is a key no
// keyword has.
func TestConnectErrorOfAKeywordDSNPgxCannotParsePrintsNothingOfIt(t *testing.T) {
	for name, dsn := range map[string]string{
		"spaces around the =": "host=db.invalid user=legion password = zqtailsecret0123 dbname=legion sslmode=requir",
		"a space after the =": "host=db.invalid user=legion password= zqtailsecret0123 dbname=legion sslmode=requir",
	} {
		t.Run(name, func(t *testing.T) {
			_, parseErr := pgconn.ParseConfig(dsn)
			if parseErr == nil {
				t.Fatalf("ParseConfig(%q) = nil, want pgx's refusal", dsn)
			}
			err := ConnectError("BROKER_DATABASE_URL", dsn, parseErr)
			if want := "connect to Postgres: BROKER_DATABASE_URL cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN"; err.Error() != want {
				t.Errorf("ConnectError = %q, want %q", err, want)
			}
			var unparsed *pgconn.ParseConfigError
			if !errors.As(err, &unparsed) {
				t.Errorf("ConnectError = %v, which does not unwrap to the *pgconn.ParseConfigError", err)
			}
		})
	}
}

// The driver's own text is not ours to trust: whatever it says on a future version, the password
// the DSN carries does not reach the error, and the address it points at does.
func TestConnectErrorRedactsThePasswordOutOfWhatItWraps(t *testing.T) {
	dsn := "postgres://legion:" + "hunter2" + "@db.internal:5432/legion"
	for _, detail := range []string{
		"failed to connect to `host=db.internal password=hunter2`",
		"cannot parse postgres://legion:hunter2" + "@db.internal:5432/legion",
	} {
		err := ConnectError("postgres_dsn", dsn, errors.New(detail))
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password its driver printed: %v", err)
		}
		if !strings.Contains(err.Error(), "db.internal:5432") {
			t.Errorf("error does not name the host:port it failed to reach: %v", err)
		}
	}
}

// The ordinary devbox DSN's user, password and database name are all `legion` (urlDSN("legion")).
// Redaction that blanked every occurrence would hide the two fields an operator reads to diagnose
// the refusal.
func TestConnectErrorKeepsTheFieldsThatMerelyMatchThePassword(t *testing.T) {
	err := ConnectError("postgres_dsn", urlDSN("legion"), errors.New("failed to connect to `user=legion database=legion`: connection refused"))
	if !strings.Contains(err.Error(), "user=legion database=legion") {
		t.Errorf("error hides the user and database the daemon was connecting as: %v", err)
	}
}

// The value after a password= in a driver's text is read by libpq's rules and masked whole, with
// no DSN to name it: unquoted, a backslash taking the next character, an escaped space among them;
// two single quotes in a row inside an unquoted value; quoted, two single quotes in a row or a
// backslash taking the next character; and what follows a closing quote glued to it. The kept
// text around it stays: the server's closing quote, and the fields after a space.
func TestRedactPasswordReadsAValueByLibpqsRules(t *testing.T) {
	for detail, want := range map[string]string{
		`role "apppassword=zq` + `\ madeuphead0123" does not exist`:              `role "apppassword=xxxxx" does not exist`,
		`role "apppassword=zq'` + `'madeuphead0123" does not exist`:              `role "apppassword=xxxxx`,
		"`user=apppassword='zq'" + "'madeuphead0123' database=app`":              "`user=apppassword=xxxxx database=app`",
		"`user=apppassword='zq\\'" + "madeuphead0123' database=app`":             "`user=apppassword=xxxxx database=app`",
		"`user=apppassword='zq'" + "madeuphead0123 database=app`: server closed": "`user=apppassword=xxxxx",
		`role "apppassword=madeuphead0123" does not exist`:                       `role "apppassword=xxxxx" does not exist`,
	} {
		got := redactPassword(detail, nil)
		if leaked(got) || !strings.HasPrefix(got, want) {
			t.Errorf("redactPassword(%q) = %q, want it to begin %q, with none of the password", detail, got, want)
		}
	}
}
