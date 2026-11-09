package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// The cases for the rewrite of LEGION_TEST_PG_DSN this binary makes, which is the second of the
// two frames that hold a live proof to the server its caller named.
//
// The first frame is the shell's. lib/live-daemon-proof.sh calls require_loopback_dsn in
// `prerequisites` for each of the seven whole-daemon proofs, which reads the caller's
// LEGION_TEST_PG_DSN with libpq and ends the run where the host is not loopback
// (scripts/e2e/lib/loopback-dsn.sh). That frame reads the caller's DSN and reaches no further
// than the shell.
//
// The second frame is this binary's. Each of the seven runs the binary `go test -c -tags e2e
// ./internal/daemon` builds, and that binary rewrites the DSN it was given: TestMain makes the
// run's own database before any test starts (main_test.go, testdb.RunDatabaseMain), internal/testpg
// migrates a template and clones it for the tests that ask, and takeOwnDatabase
// (live_agent_proof_test.go) makes one more database inside the proof. Every one of those
// rewrites comes from testdb.DatabaseURL, which rewrites with Go's net/url and then reads the
// DSN and the rewrite back with pgconn.ParseConfig, the parser the driver connects through.
// net/url ends the user name at the last `@` and it starts the query at the first `?`. libpq
// ends the user name at the first `@` and it starts no query inside it. The two read another
// host from some DSNs, and the first frame admits a DSN by libpq's reading, so the second frame
// is what keeps the connection on the host the first frame read.
//
// The cases below drive that helper through ownDatabaseDSN, the call takeOwnDatabase makes. This
// file carries no build tag, so `go test ./internal/daemon/` drives them, while the e2e-tagged
// proof calls the same function.
//
// The check rests on pgx reading these DSNs as libpq does. pgx v5.11.0's own pgconn/parse_url.go
// is written to match libpq's URI parser and says so in its first comment. That comment also
// lists the differences pgx makes on purpose, and the cases below meet one: pgx takes a query
// parameter libpq does not know, where libpq refuses the whole string. Why nothing watches that
// pin, and what a raise of it moves, is written once, above require_loopback_dsn in
// scripts/e2e/lib/loopback-dsn.sh, which asks whoever raises pgx to drive the cases below again.

// ownDatabaseDSN is the DSN named rewritten to name database, which is the database a run made
// for itself on the server named names. It is testdb.DatabaseURL, the one rewrite every DSN this
// binary builds from LEGION_TEST_PG_DSN comes from, under the name takeOwnDatabase calls it by,
// so the cases below drive the call the proof makes.
func ownDatabaseDSN(named, database string) (string, error) {
	return testdb.DatabaseURL(named, database)
}

// The shapes the two parsers read differently are the point of the check, so each is driven here:
// the one measured in this pod, and the ones the shell guard's own cases
// (scripts/e2e/lib/loopback-dsn.test.ts) found. A refusal must name what pgx read of both
// strings, so a reader of a failed proof sees which string moved the connection.
func TestOwnDatabaseDSNRefusesARewriteThatMovesTheConnection(t *testing.T) {
	const database = "legion_live_proof_1"
	// pgx reads PGPORT where a DSN names no port, so a refusal below would name the port this
	// machine's environment sets rather than pgx's own default of 5432. An empty value is one
	// pgx skips. Each DSN below names its own host, user and database, so PGPORT is the one
	// default that reaches a refusal.
	t.Setenv("PGPORT", "")
	for _, tc := range []struct {
		name   string
		dsn    string
		asks   string   // the database the rewrite asks for, where it is not the run's own
		want   string   // the rewrite, where the check takes the DSN
		refuse []string // what the refusal names, where the check refuses it
		secret string   // the password the DSN carries, which no refusal may print
	}{
		{
			// The DSN a Legion worker pod runs these proofs on rewrites to the same server, the
			// same port and the same user, with the run's own database.
			name: "the DSN this pod's proofs run on",
			dsn:  "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
			want: "postgres://legion_test@127.0.0.1:5433/legion_live_proof_1?sslmode=disable",
		},
		{
			// The DSN of the issue, measured with Go 1.27.3. libpq reads its host as localhost, so
			// the shell guard admits it, and net/url reads evil.example there. The rewrite carries
			// the `@localhost/db` into a query, where pgx and libpq both refuse it as a parameter
			// with no `=`. The run would create its own database on loopback and then reach no
			// server at all, so the refusal belongs before the create.
			name:   "a `?` right before the `@`, which the guard admits",
			dsn:    "postgres://evil.example?@localhost/db",
			refuse: []string{"host localhost, port 5432, user evil.example?, database db", "pgx refuses it"},
		},
		{
			// The same shape with a parameter pgx reads. libpq reads the host as localhost, since
			// `evil.example?x=` is a user name to it, and pgx reads the rewrite as host
			// evil.example: the run would migrate into a database on another machine. libpq
			// refuses the rewrite, over the unknown keyword `x`, and pgx takes unknown keywords as
			// runtime parameters, so pgx is the reading that decides where this run writes.
			name:   "a `?` inside the user name",
			dsn:    "postgres://evil.example?x=@localhost/db",
			refuse: []string{"host localhost", "host evil.example"},
		},
		{
			// A second `@` inside the authority is a port of `5433@10.1.2.3` for libpq, which reads
			// the host as localhost and admits the DSN. pgx refuses that port, and libpq and pgx
			// both read the rewrite as host 10.1.2.3.
			name:   "a second `@` inside the authority",
			dsn:    "postgres://legion_test@localhost:5433@10.1.2.3/db",
			refuse: []string{"pgx refuses it: invalid port", "host 10.1.2.3"},
		},
		{
			// A dbname the query sets outranks the path for libpq and for pgx, so a rewrite of the
			// path alone leaves the run migrating `other` on a server it may not own.
			name:   "a `dbname` the query sets",
			dsn:    "postgres://legion_test:s3cr3t@127.0.0.1:5433/postgres?dbname=other&sslmode=disable",
			refuse: []string{"pgx reads the rewrite as host 127.0.0.1, port 5433, user legion_test, database other"},
			secret: "s3cr3t",
		},
		{
			// A host that is a Unix-socket directory is a parameter rather than an authority, and
			// the rewrite leaves it where it is.
			name: "a Unix-socket host",
			dsn:  "postgres:///postgres?host=/var/run/postgresql&user=legion_test",
			want: "postgres:///legion_live_proof_1?host=/var/run/postgresql&user=legion_test",
		},
		{
			// A password of a byte that is no UTF-8 text reaches the rewrite as the percent-encoded
			// byte it was, so the run connects with the password its caller gave.
			name: "a password holding a percent-encoded byte",
			dsn:  "postgres://legion_test:p%FFss@127.0.0.1:5433/postgres?sslmode=disable",
			want: "postgres://legion_test:p%FFss@127.0.0.1:5433/legion_live_proof_1?sslmode=disable",
		},
		{
			// The rewrite the binary's first frame makes, before any test runs: testdb.create
			// asks this helper for the admin database `postgres`, connects to that, and creates
			// the run's own database on it (main_test.go, testdb.RunDatabaseMain).
			name: "the admin database the run's first rewrite asks for",
			dsn:  "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
			asks: "postgres",
			want: "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
		},
		{
			// The issue's DSN at that first frame, which runs before the proof's own rewrite and
			// before every test. The refusal ends the binary there, with nothing connected and no
			// database made anywhere.
			name:   "the issue's DSN at the run's first rewrite",
			dsn:    "postgres://evil.example?x=@localhost/db",
			asks:   "postgres",
			refuse: []string{"host localhost", "host evil.example", "the rewrite must be host localhost"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := database
			if tc.asks != "" {
				asked = tc.asks
			}
			own, err := ownDatabaseDSN(tc.dsn, asked)
			if tc.want != "" {
				if err != nil {
					t.Fatalf("ownDatabaseDSN refused %s: %v", tc.name, err)
				}
				if own != tc.want {
					t.Fatalf("the rewrite of %s is %s, want %s", tc.name, own, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ownDatabaseDSN took %s and answered %s, want a refusal", tc.name, own)
			}
			for _, named := range tc.refuse {
				if !strings.Contains(err.Error(), named) {
					t.Fatalf("the refusal of %s does not name %q:\n%v", tc.name, named, err)
				}
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("the refusal of %s prints the password:\n%v", tc.name, err)
			}
		})
	}
}
