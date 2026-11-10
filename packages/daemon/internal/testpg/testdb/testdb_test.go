package testdb

import (
	"strings"
	"testing"
)

// The cases for DatabaseURL, the rewrite of LEGION_TEST_PG_DSN a test binary of this module
// makes. Its own comment says what the rewrite is checked against and why, and the comment above
// require_loopback_dsn in scripts/e2e/lib/loopback-dsn.sh says what the shell reads before a live
// proof starts. Those are the two places the frames of a live proof are written.
//
// The check rests on pgx reading these DSNs as libpq does. pgx v5.11.0's own pgconn/parse_url.go
// is written to match libpq's URI parser and says so in its first comment. That comment also
// lists the differences pgx makes on purpose, and the cases below meet one: pgx takes a query
// parameter libpq does not know, where libpq refuses the whole string. Why nothing watches that
// pin, and what a raise of it moves, is written in that same shell comment, which asks whoever
// raises pgx to drive the cases below again.
//
// The shapes the two parsers read differently are the point of the check, so each is driven here:
// the one measured in this pod, the ones the shell guard's own cases
// (scripts/e2e/lib/loopback-dsn.test.ts) found, and the six a password reaches a parser's own
// text in (the last six below). A refusal is one fixed line: it names the fields that moved, or
// which string a parser refused, and it holds no value. Each case asserts that whole line, and
// each case whose DSN carries a password asserts the line holds no part of that password.
func TestDatabaseURLRefusesARewriteThatMovesTheConnection(t *testing.T) {
	// pgx reads PGPORT where a DSN names no port and PGUSER where a DSN names no user, so a
	// refusal below would turn on this machine's environment rather than on the DSN. An empty
	// value is one pgx skips, which leaves pgx's own default of 5432 for the port. A user has no
	// such default — pgx takes the account the test runs as — so PGUSER is set here, and the one
	// rewrite below that names no user reads it.
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "legion_test")
	for _, tc := range []struct {
		name     string
		dsn      string
		database string // the database the rewrite asks for
		want     string // the rewrite, where the check takes the DSN
		refuse   string // the whole refusal, where the check refuses it
		secret   string // the password the DSN carries, which no refusal may print
	}{
		{
			// The DSN a Legion worker pod runs the live proofs on rewrites to the same server,
			// the same port and the same user, with the run's own database.
			name:     "the DSN this pod's proofs run on",
			dsn:      "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
			database: "legion_live_proof_1",
			want:     "postgres://legion_test@127.0.0.1:5433/legion_live_proof_1?sslmode=disable",
		},
		{
			// The DSN of the issue, measured with Go 1.27.3. libpq reads its host as localhost, so
			// the shell guard admits it, and net/url reads evil.example there. The rewrite carries
			// the `@localhost/db` into a query, where pgx and libpq both refuse it as a parameter
			// with no `=`. The run would create its own database on loopback and then reach no
			// server at all, so the refusal belongs before the create.
			name:     "a `?` right before the `@`, which the guard admits",
			dsn:      "postgres://evil.example?@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN: pgx refuses it",
		},
		{
			// The same shape with a parameter pgx reads. libpq reads the host as localhost, since
			// `evil.example?x=` is a user name to it, and pgx reads the rewrite as host
			// evil.example: the run would migrate into a database on another machine. libpq
			// refuses the rewrite, over the unknown keyword `x`, and pgx takes unknown keywords as
			// runtime parameters, so pgx is the reading that decides where this run writes.
			name:     "a `?` inside the user name",
			dsn:      "postgres://evil.example?x=@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host and user from it",
		},
		{
			// A second `@` inside the authority is a port of `5433@10.1.2.3` for libpq, which reads
			// the host as localhost and admits the DSN. pgx refuses that port, so the refusal is
			// the line for the caller's DSN; libpq and pgx both read the rewrite as host 10.1.2.3.
			name:     "a second `@` inside the authority",
			dsn:      "postgres://legion_test@localhost:5433@10.1.2.3/db",
			database: "legion_live_proof_1",
			refuse:   "LEGION_TEST_PG_DSN: pgx refuses it",
		},
		{
			// A dbname the query sets outranks the path for libpq and for pgx, so a rewrite of the
			// path alone leaves the run migrating `other` on a server it may not own.
			name:     "a `dbname` the query sets",
			dsn:      "postgres://legion_test:s3cr3t@127.0.0.1:5433/postgres?dbname=other&sslmode=disable",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another database from it",
			secret:   "s3cr3t",
		},
		{
			// A password holding a `#` ends the authority there for net/url, which reads the rest
			// of that password as a port and names it in its own reason. The refusal is the fixed
			// line for the caller's DSN instead, and it carries no reading and no parser's text.
			name:     "a password holding a `#`",
			dsn:      "postgres://legion:s3cr3t#x@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "LEGION_TEST_PG_DSN: net/url refuses it",
			secret:   "s3cr3t",
		},
		{
			// A host that is a Unix-socket directory is a parameter rather than an authority, and
			// the rewrite leaves it where it is.
			name:     "a Unix-socket host",
			dsn:      "postgres:///postgres?host=/var/run/postgresql&user=legion_test",
			database: "legion_live_proof_1",
			want:     "postgres:///legion_live_proof_1?host=/var/run/postgresql&user=legion_test",
		},
		{
			// A password of a byte that is no UTF-8 text reaches the rewrite as the percent-encoded
			// byte it was, so the run connects with the password its caller gave.
			name:     "a password holding a percent-encoded byte",
			dsn:      "postgres://legion_test:p%FFss@127.0.0.1:5433/postgres?sslmode=disable",
			database: "legion_live_proof_1",
			want:     "postgres://legion_test:p%FFss@127.0.0.1:5433/legion_live_proof_1?sslmode=disable",
		},
		{
			// The rewrite the live proofs' binary makes before any test runs: create asks this
			// helper for the admin database `postgres`, connects to that, and creates the run's
			// own database on it (RunDatabaseMain in testdb.go).
			name:     "the admin database the run's first rewrite asks for",
			dsn:      "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
			database: "postgres",
			want:     "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
		},
		{
			// The issue's DSN at that first rewrite, which runs before the proof's own and
			// before every test. The refusal ends the binary there, with nothing connected and no
			// database made anywhere.
			name:     "the issue's DSN at the run's first rewrite",
			dsn:      "postgres://evil.example?x=@localhost/db",
			database: "postgres",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host and user from it",
		},
		{
			// The six cases below carry a password into text a parser quotes, which is where a
			// refusal printed one before the rule DatabaseURL's comment holds. Each was driven
			// through DatabaseURL by the two reviews of this change.
			//
			// A `?` in a password starts net/url's query there, so the password's head is the
			// port and its tail the query. The rewrite keeps that query, and pgx refuses it as a
			// parameter with no `=`, quoting it: that quotation was the password's tail. The
			// password is `12?SECRET`, and the assertion takes `SECRET`, since `12` is also the
			// port pgx reads from `u:12` and proves nothing there.
			name:     "a password holding a `?`",
			dsn:      "postgres://u:12?SECRET@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN: pgx refuses it",
			secret:   "SECRET",
		},
		{
			// A `/` in a password ends net/url's authority there, and `pa` is no port, so net/url
			// refuses the DSN. pgx reads the whole string and stops in the query, which is the
			// password's tail, and quoted it. The password is `pa/ss?word`, and the assertion
			// takes `word`, the part of it that reached the printed line.
			name:     "a password holding a `/` and a `?`",
			dsn:      "postgres://legion:pa/ss?word@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "LEGION_TEST_PG_DSN: net/url refuses it",
			secret:   "word",
		},
		{
			// A `#` in a password ends net/url's authority there too, and what follows is a
			// fragment rather than a port, so net/url takes this DSN: the host is `u:12`, and the
			// password's tail the fragment. The rewrite carries that fragment after the database
			// name, where pgx reads it as part of the database, and pgx reads the host and the
			// port from the password's head. All four fields moved, and the reading printed the
			// password before this rule. The password is `12#SECRET`, and the assertion takes
			// `SECRET`, since `12` is also the port pgx reads from `u:12`.
			name:     "a password holding a `#` that leaves the DSN a URL",
			dsn:      "postgres://u:12#SECRET@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host, port, user and database from it",
			secret:   "SECRET",
		},
		{
			// A percent sign that starts no byte, behind the `/` of the case above: net/url
			// refuses the DSN over the port `pa`, and pgx stops at the token it cannot decode,
			// which is the password's tail, and quoted it. The password is `pa/s%zzword`, and the
			// assertion takes `s%zzword`, the part of it that reached the printed line.
			name:     "a password holding a percent sign that starts no byte",
			dsn:      "postgres://legion:pa/s%zzword@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "LEGION_TEST_PG_DSN: net/url refuses it",
			secret:   "s%zzword",
		},
		{
			// A keyword/value connection string rather than a URL. net/url reads the whole of it
			// as a path, and pgx reads it as keyword/value: a value ends at a space, so the space
			// in the password leaves `cr3tword` a keyword with no `=`, which pgx quoted. The
			// password is `s3 cr3tword`, and the assertion takes `cr3tword`, the part of it that
			// reached the printed line.
			name:     "a password holding a space, in a keyword/value string",
			dsn:      "host=localhost user=legion password=s3 cr3tword dbname=postgres",
			database: "legion_live_proof_1",
			refuse:   "LEGION_TEST_PG_DSN: pgx refuses it",
			secret:   "cr3tword",
		},
		{
			// An `@` in a password: net/url ends the user name at the last `@` and reads the host
			// localhost, pgx ends it at the first and reads the host `ss@localhost`. So pgx reads
			// another host from the rewrite than from the DSN, and the reading printed the
			// password's tail twice before this rule. The password is `p@ss`, and the assertion
			// takes `ss`, the part of it pgx reads into the host.
			name:     "a password holding an `@`",
			dsn:      "postgres://legion:p@ss@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host from it",
			secret:   "ss",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own, err := DatabaseURL(tc.dsn, tc.database)
			if tc.want != "" {
				if err != nil {
					t.Fatalf("DatabaseURL refused %s: %v", tc.name, err)
				}
				if own != tc.want {
					t.Fatalf("the rewrite of %s is %s, want %s", tc.name, own, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("DatabaseURL took %s and answered %s, want a refusal", tc.name, own)
			}
			if err.Error() != tc.refuse {
				t.Fatalf("the refusal of %s is:\n%v\nwant:\n%s", tc.name, err, tc.refuse)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("the refusal of %s prints the password:\n%v", tc.name, err)
			}
		})
	}
}
