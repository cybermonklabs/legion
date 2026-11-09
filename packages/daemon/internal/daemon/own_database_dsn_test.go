package daemon

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The one rewrite of LEGION_TEST_PG_DSN the live proofs make, and the check that the rewrite
// still names the server, the port and the user the DSN named.
//
// takeOwnDatabase (live_agent_proof_test.go) creates a database for the run on the server
// LEGION_TEST_PG_DSN names, and then points that variable at the new database. The rewrite is
// Go's net/url. net/url ends the user name at the last `@` and starts the query at the first
// `?`. libpq ends the user name at the first `@` and starts no query inside it. The two read
// another host from some DSNs, and the shell guard admits the DSN by libpq's reading
// (scripts/e2e/lib/loopback-dsn.sh, which lib/live-daemon-proof.sh calls in `prerequisites`).
// So the rewrite is read back here with the parser the driver itself connects through,
// pgconn.ParseConfig of the pinned pgx. This file adds no third parser.
//
// The check rests on pgx reading these DSNs as libpq does. pgx v5.11.0's own pgconn/parse_url.go
// is written to match libpq's URI parser and says so in its first comment. That comment also
// lists the differences pgx makes on purpose, and the cases below meet one: pgx takes a query
// parameter libpq does not know, where libpq refuses the whole string. That version is the
// pin in packages/daemon/go.mod:13 and packages/envoy/go.mod:31. Nothing automated reads the
// pin: .github/dependabot.yml covers the github-actions ecosystem alone, and no CI job runs `go
// mod tidy`, `go mod verify` or any other check of which pgx version is in use. Whoever raises
// pgx must drive the cases below again, as scripts/e2e/README.md asks of the shell guard's own
// cases. dispatch://INFRA-2338 tracks that watching gap.
//
// This file carries no build tag, so `go test ./internal/daemon/` drives the cases below, while
// the e2e-tagged proof calls the same function.

// ownDatabaseDSN is the DSN named rewritten to name database, which is the database a run made
// for itself on the server named names. It rewrites with net/url, and then reads both strings
// with pgconn.ParseConfig. It refuses a rewrite pgx reads another host, port or user from, and
// one pgx reads another database from than database: a run that connected with such a rewrite
// would write a server the shell guard never admitted. The error says what pgx read of each
// string, and prints no password.
func ownDatabaseDSN(named, database string) (string, error) {
	own, err := url.Parse(named)
	if err != nil {
		return "", fmt.Errorf("read LEGION_TEST_PG_DSN as a URL: %s", parseReason(err))
	}
	own.Path = "/" + database
	rewritten := own.String()
	before, beforeErr := pgconn.ParseConfig(named)
	after, afterErr := pgconn.ParseConfig(rewritten)
	if beforeErr != nil || afterErr != nil ||
		before.Host != after.Host || before.Port != after.Port || before.User != after.User ||
		after.Database != database {
		return "", fmt.Errorf("LEGION_TEST_PG_DSN rewritten to database %s is not the same server, port and user with that database:\n"+
			"\tpgx reads LEGION_TEST_PG_DSN as %s\n"+
			"\tpgx reads the rewrite as %s",
			database, dsnReading(before, beforeErr), dsnReading(after, afterErr))
	}
	return rewritten, nil
}

// dsnReading is what pgx read from one DSN, in one line: the four fields ownDatabaseDSN compares,
// or the refusal that stands in for them. It prints no password, since a refusal names the
// connection string it refused and the string holds one.
func dsnReading(cfg *pgconn.Config, err error) string {
	if err != nil {
		return "nothing, because pgx refuses it: " + parseReason(err)
	}
	return fmt.Sprintf("host %s, port %d, user %s, database %s", cfg.Host, cfg.Port, cfg.User, cfg.Database)
}

// parseReason is why a parser refused a DSN, with no connection string in it. net/url names the
// URL it refused (url.Error) and pgx quotes the connection string back ahead of its own reason
// (pgconn.ParseConfigError, "cannot parse `<the DSN>`: <the reason>"), and either text holds the
// password. pgx redacts that password itself, and says in pgconn/errors.go that the redaction is
// best effort, so this cuts the string out rather than trust it.
func parseReason(err error) string {
	var badURL *url.Error
	if errors.As(err, &badURL) && badURL.Err != nil {
		return badURL.Err.Error()
	}
	var refused *pgconn.ParseConfigError
	if errors.As(err, &refused) {
		if quoted := strings.LastIndex(err.Error(), "`: "); quoted >= 0 {
			return err.Error()[quoted+len("`: "):]
		}
	}
	return err.Error()
}

// The shapes the two parsers read differently are the point of the check, so each is driven here:
// the one measured in this pod, and the ones the shell guard's own cases
// (scripts/e2e/lib/loopback-dsn.test.ts) found. A refusal must name what pgx read of both
// strings, so a reader of a failed proof sees which string moved the connection.
func TestOwnDatabaseDSNRefusesARewriteThatMovesTheConnection(t *testing.T) {
	const database = "legion_live_proof_1"
	for _, tc := range []struct {
		name   string
		dsn    string
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			own, err := ownDatabaseDSN(tc.dsn, database)
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
