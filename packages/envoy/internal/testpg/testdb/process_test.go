package testdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// processChildEnv carries the admin URL to a test binary a row here runs again: set,
// TestProcessDatabaseChild is that child.
const processChildEnv = "TESTDB_PROCESS_CHILD_ADMIN_URL"

// processChildFailEnv, set in a child, makes it fail one of its own checks once it has made its
// database (TestAChildBinaryThatFailsStillDropsItsDatabase).
const processChildFailEnv = "TESTDB_PROCESS_CHILD_FAIL"

// processChildPrefix is the prefix of the databases the children make.
const processChildPrefix = "testdb_process_test"

// childLine is the line a child prints once it has made its database and connected to it.
var childLine = regexp.MustCompile(`(?m)^testdb-child: url_database=(\S+) current_database=(\S+)$`)

// TestProcessDatabaseChild is the child process the rows below run, and returns at once in any
// other run. In the child it does what a package's TestMain and tests do, through Run
// (processChild).
func TestProcessDatabaseChild(t *testing.T) {
	admin := os.Getenv(processChildEnv)
	if admin == "" {
		return
	}
	if code := Run(func() int { return processChild(t, admin) }); code != 0 {
		t.Fatalf("Run answered %d: one of the child's own checks failed (above), or Run's drop did (printed above)", code)
	}
}

// processChild asks ProcessDatabaseOn twice for its binary's database, checks that both answers
// are the one database, and then twice more with two different connect bounds, as
// WithConnectDeadline gives two tests of one binary: still the one database, each answer keeping
// the bound it asked with. It prints the database pgx selects from that URL and the one the server
// says the connection is in, which the parent reads. A check that fails fails t with Errorf and
// answers 1, never Fatal: Fatal would end the test's goroutine inside Run's run, before Run's
// drop, and leave a database the parent cannot name.
func processChild(t *testing.T, admin string) int {
	first := ProcessDatabaseOn(t, admin, processChildPrefix)
	if again := ProcessDatabaseOn(t, admin, processChildPrefix); again != first {
		t.Error("a second ProcessDatabaseOn of the same server and prefix answered another database")
		return 1
	}
	for _, bound := range []string{"5", "7"} {
		answer, err := url.Parse(ProcessDatabaseOn(t, withPairs(admin, "connect_timeout="+bound), processChildPrefix))
		want, werr := url.Parse(first)
		if err != nil || werr != nil || answer.Path != want.Path || answer.Query().Get("connect_timeout") != bound {
			t.Errorf("ProcessDatabaseOn with connect_timeout=%s answered another database, or lost the bound it asked with", bound)
			return 1
		}
	}
	config, err := pgx.ParseConfig(first)
	if err != nil {
		t.Error("parse the binary's database URL with pgx") // its error can quote the URL, which carries a credential
		return 1
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Errorf("connect to the binary's database: %v", err)
		return 1
	}
	defer conn.Close(ctx)
	var current string
	if err := conn.QueryRow(ctx, "select current_database()").Scan(&current); err != nil {
		t.Errorf("read current_database(): %v", err)
		return 1
	}
	fmt.Printf("testdb-child: url_database=%s current_database=%s\n", config.Database, current)
	if os.Getenv(processChildFailEnv) != "" {
		t.Error("planted: the child fails one of its checks after it made its database")
		return 1
	}
	return 0
}

// testServer is the BROKER_TEST_DATABASE_URL server, skipping t when the variable is unset, and
// the URL pgx parses it to.
func testServer(t *testing.T) (string, *pgx.ConnConfig) {
	t.Helper()
	admin := os.Getenv("BROKER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run the Postgres rows of testdb")
	}
	config, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal("parse BROKER_TEST_DATABASE_URL with pgx") // its error can quote the URL, which carries a credential
	}
	return admin, config
}

// withPairs is server with the raw query pairs added to its query.
func withPairs(server, pairs string) string {
	if strings.Contains(server, "?") {
		return server + "&" + pairs
	}
	return server + "?" + pairs
}

// childRun is one run of this test binary as TestProcessDatabaseChild.
type childRun struct {
	cmd *exec.Cmd
	out bytes.Buffer
}

// startChild starts this test binary as TestProcessDatabaseChild on admin, with env added.
func startChild(t *testing.T, admin string, env ...string) *childRun {
	t.Helper()
	c := &childRun{cmd: exec.Command(os.Args[0], "-test.run=^TestProcessDatabaseChild$", "-test.count=1", "-test.v")}
	c.cmd.Env = append(append(os.Environ(), processChildEnv+"="+admin), env...)
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatalf("start a child: %v", err)
	}
	return c
}

// databasesLeft reads which of names pg_database still holds, and drops each it holds so the
// server is left as the row found it. Every name is one a child made: the callers pass none
// without the children's prefix.
func databasesLeft(t *testing.T, admin string, config *pgx.ConnConfig, names []string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", config.Host, err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "select datname from pg_database where datname = any($1) order by 1", names)
	if err != nil {
		t.Fatalf("read pg_database: %v", err)
	}
	var left []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read pg_database: %v", err)
		}
		left = append(left, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_database: %v", err)
	}
	for _, name := range left {
		if strings.HasPrefix(name, processChildPrefix+"_") {
			_ = execOn(admin, config.Host, "drop database if exists "+name+" with (force)")
		}
	}
	return left
}

// Two test binaries on one server each get a database of their own, the one their URL names in
// every field pgx selects a database by, and Run drops each once its binary's tests end. The admin
// URL names its own database in the query as well as the path (dbname and database, which pgx
// reads over the path): kept in the binary's URL, they put both binaries in that one database, as
// a reviewer's run did. The row runs this test binary twice, at once, as TestProcessDatabaseChild,
// and reads what each says; then it reads pg_database for the two names.
//
// Controls: keep the query's dbname in databaseURL and the children read the admin's database;
// take the drop out of Run and a child's database is left on the server.
func TestTwoBinariesEachMakeADatabaseOfTheirOwnAndRunDropsIt(t *testing.T) {
	admin, config := testServer(t)
	queried := withPairs(admin, "dbname="+url.QueryEscape(config.Database)+"&database="+url.QueryEscape(config.Database))

	children := []*childRun{startChild(t, queried), startChild(t, queried)}
	names := make([]string, 0, len(children))
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v:\n%s", i, err, c.out.String())
		}
		match := childLine.FindStringSubmatch(c.out.String())
		if match == nil {
			t.Fatalf("child %d printed no testdb-child line:\n%s", i, c.out.String())
		}
		selected, current := match[1], match[2]
		if selected != current {
			t.Errorf("child %d: its URL selects %s, and the server put the connection in %s", i, selected, current)
		}
		if !strings.HasPrefix(current, processChildPrefix+"_") || current == config.Database {
			t.Errorf("child %d ran in %s, not a database of its own (%s_…): the URL's query chose the database", i, current, processChildPrefix)
			continue // a database the child did not make: read it no further, and never drop it
		}
		names = append(names, current)
	}
	if len(names) == 2 && names[0] == names[1] {
		t.Errorf("both children ran in one database, %s", names[0])
	}
	if len(names) == 0 {
		return
	}
	if left := databasesLeft(t, admin, config, names); len(left) > 0 {
		t.Errorf("Run left the children's databases on the server: %v", left)
	}
}

// A child binary one of whose own checks fails after it made its database still drops it: the
// check fails t with Errorf and answers 1 to Run, which drops the database before the binary
// exits with the failure. Control: fail the planted check with t.Fatal instead, which ends the
// test's goroutine inside Run's run, and the database is left on the server.
func TestAChildBinaryThatFailsStillDropsItsDatabase(t *testing.T) {
	admin, config := testServer(t)
	c := startChild(t, admin, processChildFailEnv+"=1")
	err := c.cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the failing child answered %v, want exit status 1:\n%s", err, c.out.String())
	}
	if !strings.Contains(c.out.String(), "planted: the child fails one of its checks") {
		t.Fatalf("the child failed, but not on its planted check:\n%s", c.out.String())
	}
	match := childLine.FindStringSubmatch(c.out.String())
	if match == nil || !strings.HasPrefix(match[2], processChildPrefix+"_") {
		t.Fatalf("the child printed no database of its own:\n%s", c.out.String())
	}
	if left := databasesLeft(t, admin, config, []string{match[2]}); len(left) > 0 {
		t.Errorf("the failing child left its database on the server: %v", left)
	}
}

// databaseURL replaces the path and drops each dbname or database pair of the query, whatever its
// escapes, and keeps every other pair byte for byte and in order: an options value whose space is
// %20 stays %20, where re-encoding the query writes it as +, which pgx reads as a plus. An empty
// pair goes.
func TestADatabaseURLDropsOnlyTheQuerysDatabasePairs(t *testing.T) {
	server, err := url.Parse("postgres://u@h:5432/postgres?sslmode=disable&options=-c%20statement_timeout%3D5000&dbname=postgres&&db%6Eame=x&database=other&application_name=a+b")
	if err != nil {
		t.Fatal(err)
	}
	const want = "postgres://u@h:5432/t_db?sslmode=disable&options=-c%20statement_timeout%3D5000&application_name=a+b"
	if got := databaseURL(*server, "t_db"); got != want {
		t.Errorf("databaseURL =\n  %s\nwant\n  %s", got, want)
	}
}

// WithConnectDeadline replaces every connect_timeout pair with its own and keeps each other pair
// byte for byte, as databaseURL keeps them: an options pair's escaped space stays %20, which a
// re-encoded query (url.Values.Encode) would write as a plus that pgx reads as a plus.
func TestAConnectDeadlineKeepsTheQuerysOtherPairsByteForByte(t *testing.T) {
	got := WithConnectDeadline(t, "postgres://u@h:5432/db?sslmode=disable&options=-c%20work_mem%3D7MB&connect_timeout=3&connect%5Ftimeout=4&application_name=a+b")
	prefix := "postgres://u@h:5432/db?sslmode=disable&options=-c%20work_mem%3D7MB&application_name=a+b&connect_timeout="
	rest, ok := strings.CutPrefix(got, prefix)
	if !ok {
		t.Fatalf("WithConnectDeadline =\n  %s\nwant the other pairs byte for byte, then the bound:\n  %s<seconds>", got, prefix)
	}
	if seconds, err := strconv.Atoi(rest); err != nil || seconds < 1 {
		t.Fatalf("WithConnectDeadline set connect_timeout=%q; want a whole number of seconds, at least 1", rest)
	}
}

// noDeadline is a testing.TB with no Deadline method, as a benchmark's *testing.B and a fuzz
// target's *testing.F are at Go 1.27.3: the interface it embeds carries no Deadline, so the
// *testing.T inside does not lend it one.
type noDeadline struct{ testing.TB }

// A caller that names no deadline keeps a real connect bound, pgxpool's two minutes, for its
// pools and its plain pgx.Connect alike, rather than the longest connect_timeout pgx holds, which
// is no bound at all. Control: connectTimeout's old answer for such a caller,
// math.MaxInt64/int64(time.Second), sets connect_timeout=9223372036 and fails this.
func TestAConnectWithNoTestDeadlineIsBoundedByTwoMinutes(t *testing.T) {
	if _, bounded := ConnectEnd(noDeadline{t}); bounded {
		t.Fatal("ConnectEnd named a deadline for a testing.TB with no Deadline method")
	}
	got := WithConnectDeadline(noDeadline{t}, "postgres://u@h:5432/db?sslmode=disable&connect_timeout=3")
	if want := "postgres://u@h:5432/db?sslmode=disable&connect_timeout=120"; got != want {
		t.Fatalf("WithConnectDeadline for a caller with no deadline =\n  %s\nwant\n  %s", got, want)
	}
}

// A test's database on a server URL whose options pair holds an escaped space connects, with the
// option in force. The option is work_mem, which bounds nothing in time: the admin URL carries it
// too, so a statement_timeout there would also bound the create database, which a loaded server
// can take longer than that over (5 s did, once, in a run here). Control: re-encode the query in
// databaseURL (url.Values.Encode), and the connection is refused for an unrecognized
// configuration parameter "+work_mem".
func TestATestDatabaseOnAnOptionsURLConnectsWithTheOption(t *testing.T) {
	admin, _ := testServer(t)
	database := DatabaseOn(t, withPairs(admin, "options=-c%20work_mem%3D7MB"), "testdb_options_test")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatalf("connect to the test's database: %v", err)
	}
	defer conn.Close(ctx)
	var workMem string
	if err := conn.QueryRow(ctx, "show work_mem").Scan(&workMem); err != nil {
		t.Fatalf("show work_mem: %v", err)
	}
	if workMem != "7MB" {
		t.Errorf("work_mem = %s, want 7MB: the options pair did not reach the connection", workMem)
	}
}
