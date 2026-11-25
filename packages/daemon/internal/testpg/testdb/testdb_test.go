package testdb

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	// such default — pgx takes the account the test runs as — so PGUSER is set here, and the
	// three rewrites below that name no user read it.
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
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host, user and parameters from it",
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
			// The rewrite create makes before it makes a database: it asks this helper for the
			// admin database `postgres`, connects to that, and creates the test's own database
			// on it (create in testdb.go).
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
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host, user and parameters from it",
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
			// name, where pgx reads it as part of the database, and pgx reads the user name as
			// the host and the password's head as the port. All four fields moved, and the
			// reading printed the password before this rule. The password is `12#SECRET`, and
			// the assertion takes `SECRET`, since `12` is also the port pgx reads from `u:12`.
			name:     "a password holding a `#` that leaves the DSN a URL",
			dsn:      "postgres://u:12#SECRET@localhost/db",
			database: "legion_live_proof_1",
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host, port, user, database and credential from it",
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
			refuse:   "the rewrite of LEGION_TEST_PG_DSN is not the connection it was asked for: pgx reads another host and credential from it",
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

// PoolBounded gives a pool on the URL the pool keys LEGION_TEST_PG_DSN sets (server) in place of
// any the URL holds, and pool_max_conns=PoolConns where server sets no size: a size the DSN sets,
// as internal/daemon's daemonDSN sets one connection, is kept, whether the URL is server itself or
// a URL DatabaseURL made without it. It keeps every byte of the query it does not change: pgx
// reads the query with its own libpq-style parser, which decodes only %XX and keeps a `+`, so a
// query rebuilt by Go's encoder (`%20` written as `+`) would send the server another value.
func TestPoolBoundedHoldsThePoolUnlessTheDSNSizesItItself(t *testing.T) {
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "legion_test")
	for _, tc := range []struct {
		name, server, dsn, want string
		conns                   int32
	}{
		{"a DSN that sets no size", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable",
			"postgres://legion_test@127.0.0.1:5433/run?sslmode=disable&pool_max_conns=4", PoolConns},
		{"a DSN with no query", "postgres://legion_test@127.0.0.1:5433/run", "postgres://legion_test@127.0.0.1:5433/run",
			"postgres://legion_test@127.0.0.1:5433/run?pool_max_conns=4", PoolConns},
		{"a DSN that sets its own size", "postgres://legion_test@127.0.0.1:5433/run?pool_max_conns=1&sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?pool_max_conns=1&sslmode=disable",
			"postgres://legion_test@127.0.0.1:5433/run?sslmode=disable&pool_max_conns=1", 1},
		{"a database URL made from a DSN that sets its own size", "postgres://legion_test@127.0.0.1:5433/legion?pool_max_conns=2&pool_min_conns=1&sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable",
			"postgres://legion_test@127.0.0.1:5433/run?sslmode=disable&pool_max_conns=2&pool_min_conns=1", 2},
		{"a DSN whose options hold an encoded space", "postgres://legion_test@127.0.0.1:5433/run?options=-c%20search_path%3Dx&sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?options=-c%20search_path%3Dx&sslmode=disable",
			"postgres://legion_test@127.0.0.1:5433/run?options=-c%20search_path%3Dx&sslmode=disable&pool_max_conns=4", PoolConns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PoolBounded(tc.server, tc.dsn)
			if err != nil {
				t.Fatalf("PoolBounded refused %s: %v", tc.dsn, err)
			}
			if got != tc.want {
				t.Fatalf("PoolBounded(%s, %s) = %s, want %s", tc.server, tc.dsn, got, tc.want)
			}
			config, err := pgxpool.ParseConfig(got)
			if err != nil {
				t.Fatalf("pgxpool refused %s: %v", got, err)
			}
			if config.MaxConns != tc.conns {
				t.Fatalf("a pool on %s holds %d connections, want %d", got, config.MaxConns, tc.conns)
			}
			before, err := pgxpool.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatalf("pgxpool refused %s: %v", tc.dsn, err)
			}
			if !maps.Equal(config.ConnConfig.RuntimeParams, before.ConnConfig.RuntimeParams) {
				t.Fatalf("pgx reads the parameters %v from %s, want %v as from %s", config.ConnConfig.RuntimeParams, got, before.ConnConfig.RuntimeParams, tc.dsn)
			}
		})
	}
}

// DatabaseURL takes every key pgxpool reads out of the URL it makes, whatever LEGION_TEST_PG_DSN
// holds, and keeps every other byte of the query: its URL goes to readers that are no pool (an
// admin connection, psql, a scratch Dispatch), and pgx.Connect sends a pool key to the server as a
// setting the server refuses, where psql refuses the URL itself and Dispatch refuses one that sets
// pool_max_conns.
func TestDatabaseURLCarriesNoPoolKey(t *testing.T) {
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "legion_test")
	const dsn = "postgres://legion_test@127.0.0.1:5433/legion?pool_max_conns=2&options=-c%20search_path%3Dx&pool_min_conns=1&sslmode=disable"
	const want = "postgres://legion_test@127.0.0.1:5433/run?options=-c%20search_path%3Dx&sslmode=disable"
	got, err := DatabaseURL(dsn, "run")
	if err != nil {
		t.Fatalf("DatabaseURL refused %s: %v", dsn, err)
	}
	if got != want {
		t.Fatalf("DatabaseURL(%s) = %s, want %s", dsn, got, want)
	}
	config, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatalf("pgx refused %s: %v", got, err)
	}
	for key := range config.RuntimeParams {
		if strings.HasPrefix(key, "pool_") {
			t.Errorf("pgx reads %s from %s, which pgx.Connect sends to the server", key, got)
		}
	}
}

// A LEGION_TEST_PG_DSN that sets one of pgxpool's keys, as a developer sizes a run's pools,
// reaches every reader on the server it names: a plain URL carries no pool key, so pgx.Connect
// connects with it, and a pool URL keeps the size the DSN set.
func TestADSNThatSetsAPoolSizeReachesEveryReader(t *testing.T) {
	dsn := DSN(t)
	sized, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("LEGION_TEST_PG_DSN: net/url refuses it")
	}
	if sized.RawQuery != "" {
		sized.RawQuery += "&"
	}
	sized.RawQuery += "pool_max_conns=2"
	t.Setenv("LEGION_TEST_PG_DSN", sized.String())

	plain := PlainDatabase(t, "legion_testdb_pool_key")
	conn, err := pgx.Connect(t.Context(), plain)
	if err != nil {
		t.Fatalf("connect to the plain database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var one int
	if err := conn.QueryRow(t.Context(), "select 1").Scan(&one); err != nil {
		t.Fatalf("read on the plain database: %v", err)
	}

	config, err := pgxpool.ParseConfig(Database(t, "legion_testdb_pool_key"))
	if err != nil {
		t.Fatalf("pgxpool refuses the pool URL: %v", err)
	}
	if config.MaxConns != 2 {
		t.Fatalf("a pool on the pool URL holds %d connections, want the 2 LEGION_TEST_PG_DSN sets", config.MaxConns)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open a pool on the pool URL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping the pool: %v", err)
	}
}

// rewriteDSN refuses a rewrite from which pgx reads any value other than the one asked for, not
// only another host, port, user or database: a run-time parameter, such as options, and a password
// (named the credential) are read from the URL too, and each is named in the refusal with no value.
func TestARewriteThatChangesAnyValuePgxReadsIsRefused(t *testing.T) {
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "legion_test")
	// The password case's URLs are built, as internal/daemon's secretDSN builds its own: a test's
	// stand-in password is no credential, and a URL literal holding one reads as one.
	withPassword := func(password string) string {
		return (&url.URL{Scheme: "postgres", User: url.UserPassword("legion_test", password), Host: "127.0.0.1:5433", Path: "/run"}).String()
	}
	for _, tc := range []struct{ name, from, to, moved string }{
		{"an option", "postgres://legion_test@127.0.0.1:5433/run?options=-c%20search_path%3Dx", "postgres://legion_test@127.0.0.1:5433/run?options=-c+search_path%3Dx", "parameters"},
		{"a password", withPassword("first"), withPassword("second"), "credential"},
		{"a connection setting", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?sslmode=require", "connection settings"},
		{"nothing", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable", "postgres://legion_test@127.0.0.1:5433/run?sslmode=disable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, err := pgconn.ParseConfig(tc.from)
			if err != nil {
				t.Fatal(err)
			}
			to, err := pgconn.ParseConfig(tc.to)
			if err != nil {
				t.Fatal(err)
			}
			if got := readingOf(to).moved(readingOf(from)); got != tc.moved {
				t.Fatalf("the fields that moved from %s to %s = %q, want %q", tc.from, tc.to, got, tc.moved)
			}
		})
	}
}

// prereqRecorder is a testing.TB that keeps what DSNFor's testprereq.Require asked of it instead
// of ending the test, as packages/envoy/testprereq's own test keeps it, so the test reads the
// failure or the skip a real test would have been given.
type prereqRecorder struct {
	testing.TB
	failed, skipped string
}

func (r *prereqRecorder) Helper() {}

func (r *prereqRecorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func (r *prereqRecorder) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

// Without LEGION_TEST_PG_DSN, DSNFor fails the test on GitHub Actions, naming the variable and the
// ci.yaml job it was given, and skips it elsewhere with the command that starts a Postgres
// (skipMessage). With the variable set it does neither, on GitHub Actions or off it, and answers
// the variable's value.
//
// Each variable is set to "" here rather than taken out of the environment. DSNFor reads
// LEGION_TEST_PG_DSN with os.Getenv and compares it with "", and testprereq.Enforced reads
// GITHUB_ACTIONS with os.Getenv and compares it with "true", so to both an empty value is an unset
// one. The module's fixtureenv.Unset would take them out, but it imports other packages of this
// module, and this package imports none but internal/testwait (its package comment says why).
func TestDSNForFailsOnGitHubActionsSkipsElsewhereAndAnswersTheServer(t *testing.T) {
	const job = "the-job-that-runs-it"
	t.Run("unset, on GitHub Actions", func(t *testing.T) {
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv(dsnName, "")
		got := &prereqRecorder{TB: t}
		DSNFor(got, job)
		if !strings.Contains(got.failed, dsnName) || !strings.Contains(got.failed, "ci.yaml's "+job+" job") || got.skipped != "" {
			t.Errorf("DSNFor with %s unset under GITHUB_ACTIONS=true failed with %q and skipped with %q, want a failure naming %s and the %s job, and no skip",
				dsnName, got.failed, got.skipped, dsnName, job)
		}
	})
	t.Run("unset, off GitHub Actions", func(t *testing.T) {
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv(dsnName, "")
		got := &prereqRecorder{TB: t}
		DSNFor(got, job)
		if got.skipped != skipMessage || got.failed != "" {
			t.Errorf("DSNFor with %s and GITHUB_ACTIONS unset skipped with %q and failed with %q, want the skip that starts a Postgres (skipMessage) and no failure",
				dsnName, got.skipped, got.failed)
		}
	})
	const server = "postgres://legion_test@127.0.0.1:5433/legion?sslmode=disable"
	for _, actions := range []string{"true", ""} {
		t.Run("set, GITHUB_ACTIONS="+actions, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", actions)
			t.Setenv(dsnName, server)
			got := &prereqRecorder{TB: t}
			if answer := DSNFor(got, job); answer != server || got.failed != "" || got.skipped != "" {
				t.Errorf("DSNFor with %s set answered %q, failed with %q and skipped with %q, want %q and neither",
					dsnName, answer, got.failed, got.skipped, server)
			}
		})
	}
}
