package testdb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// plantedPart is the first part of a password that holds an unescaped '#'. url.Parse ends the
// URL's authority at the '#', reads the user, a colon and this part as host:port, and refuses the
// port, quoting this part, in the error it wraps too.
const plantedPart = "plantedfirstpart"

// unparsableDSN is a DSN whose password is plantedPart, a '#' and more, unescaped.
const unparsableDSN = "postgres://u:" + plantedPart + "#rest@db.example:5432/postgres"

// fatalRecorder is a testing.TB whose Fatal and Fatalf record what they were given and end the
// goroutine that called them, as testing.T's do; every other method is the test's own.
type fatalRecorder struct {
	testing.TB
	messages []string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatal(args ...any) {
	r.messages = append(r.messages, fmt.Sprint(args...))
	runtime.Goexit()
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// A server URL that does not parse fails DatabaseOn with no part of the URL in the message: not
// the password's first part, which even the error url.Parse wraps quotes. Control: that wrapped
// error does quote it, so a DatabaseOn that printed it would fail this test.
func TestAServerURLThatDoesNotParseIsNeverPrinted(t *testing.T) {
	t.Parallel()
	_, err := url.Parse(unparsableDSN)
	if err == nil || !strings.Contains(errors.Unwrap(err).Error(), plantedPart) {
		t.Fatalf("control: url.Parse of the planted DSN answers %v; want an error whose wrapped error quotes the password's first part, or this test proves nothing", err)
	}
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		DatabaseOn(recorder, unparsableDSN, "", "never_made")
	}()
	<-done
	if len(recorder.messages) != 1 || !strings.Contains(recorder.messages[0], "does not parse") {
		t.Fatalf("DatabaseOn of a URL that does not parse failed with %d messages, want one saying it does not parse", len(recorder.messages))
	}
	for _, part := range []string{plantedPart, "rest", "db.example"} {
		if strings.Contains(recorder.messages[0], part) {
			t.Errorf("DatabaseOn's failure prints %q of the URL it could not parse; want no part of it", part)
		}
	}
}

// testServer is the BROKER_TEST_DATABASE_URL server, skipping t when the variable is unset.
func testServer(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("BROKER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run the Postgres rows of testdb")
	}
	return admin
}

// withPairs is server with the raw query pairs added to its query.
func withPairs(server, pairs string) string {
	if strings.Contains(server, "?") {
		return server + "&" + pairs
	}
	return server + "?" + pairs
}

// DatabaseURL replaces the path and drops each dbname or database pair of the query, whatever its
// escapes, and keeps every other pair byte for byte and in order: an options value whose space is
// %20 stays %20, where re-encoding the query writes it as +, which pgx reads as a plus. An empty
// pair goes.
func TestADatabaseURLDropsOnlyTheQuerysDatabasePairs(t *testing.T) {
	t.Parallel()
	server, err := url.Parse("postgres://u@h:5432/postgres?sslmode=disable&options=-c%20statement_timeout%3D5000&dbname=postgres&&db%6Eame=x&database=other&application_name=a+b")
	if err != nil {
		t.Fatal(err)
	}
	const want = "postgres://u@h:5432/t_db?sslmode=disable&options=-c%20statement_timeout%3D5000&application_name=a+b"
	if got := DatabaseURL(*server, "t_db"); got != want {
		t.Errorf("DatabaseURL =\n  %s\nwant\n  %s", got, want)
	}
}

// A pair DatabaseURL is given replaces every pair of its key the server's query held, whatever
// that pair's escapes, and follows the pairs it keeps; the others stay byte for byte. Control:
// keep the server's pairs of a key set names, and the row's search_path appears twice.
func TestADatabaseURLsSetPairsReplaceTheServersOfTheirKey(t *testing.T) {
	t.Parallel()
	server, err := url.Parse("postgres://u@h:5432/postgres?search_path=public&options=-c%20work_mem%3D7MB&pool_max_conns=9&se%61rch_path=other")
	if err != nil {
		t.Fatal(err)
	}
	const want = "postgres://u@h:5432/t_db?options=-c%20work_mem%3D7MB&search_path=broker_test&pool_max_conns=2"
	if got := DatabaseURL(*server, "t_db", "search_path=broker_test", "pool_max_conns=2"); got != want {
		t.Errorf("DatabaseURL =\n  %s\nwant\n  %s", got, want)
	}
}

// A test's database on a server URL whose options pair holds an escaped space connects, with the
// option in force. The option is work_mem, which bounds nothing in time: the admin URL carries it
// too, so a statement_timeout there would also bound the create database, which a loaded server
// can take longer than that over (5 s did, once, in a run here). Control: re-encode the query in
// DatabaseURL (url.Values.Encode), and the connection is refused for an unrecognized
// configuration parameter "+work_mem".
func TestATestDatabaseOnAnOptionsURLConnectsWithTheOption(t *testing.T) {
	t.Parallel()
	database := DatabaseOn(t, withPairs(testServer(t), "options=-c%20work_mem%3D7MB"), "", "testdb_options_test")
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

// WithConnectDeadline replaces every connect_timeout pair with its own and keeps each other pair
// byte for byte, as DatabaseURL keeps them: an options pair's escaped space stays %20, which a
// re-encoded query (url.Values.Encode) would write as a plus that pgx reads as a plus.
func TestAConnectDeadlineKeepsTheQuerysOtherPairsByteForByte(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if _, bounded := ConnectEnd(noDeadline{t}); bounded {
		t.Fatal("ConnectEnd named a deadline for a testing.TB with no Deadline method")
	}
	got := WithConnectDeadline(noDeadline{t}, "postgres://u@h:5432/db?sslmode=disable&connect_timeout=3")
	if want := "postgres://u@h:5432/db?sslmode=disable&connect_timeout=120"; got != want {
		t.Fatalf("WithConnectDeadline for a caller with no deadline =\n  %s\nwant\n  %s", got, want)
	}
}

// A teardown whose tests began now gives up TeardownGrace past the -timeout, inside cmd/go's kill
// (TeardownGrace says why). Control: answer the -timeout alone, and this fails by TeardownGrace.
func TestATeardownGivesUpItsGracePastTheTimeout(t *testing.T) {
	t.Parallel()
	timeout, _ := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration)
	if timeout <= 0 {
		t.Skip("run under a -timeout: under -timeout 0 the teardown has no kill to stay inside")
	}
	before := time.Now()
	if got, want := TeardownDeadline(before), before.Add(timeout+TeardownGrace); !got.Equal(want) {
		t.Fatalf("a teardown whose tests began now gives up %s from now; want TeardownGrace past the -timeout %s, %s", got.Sub(before), timeout, want.Sub(before))
	}
}

// A database DatabaseOn makes runs with JIT off, set on the database itself (WithoutJIT), empty or
// a copy of a template, since a copy does not keep its template's settings: a session of it reads
// jit = off from the source 'database', which no server default, role or client gives. Control:
// take the WithoutJIT statement out of DatabaseOn, and both rows read jit from the server's
// default, 'on' on CI's image.
func TestADatabaseOnRunsWithoutJIT(t *testing.T) {
	t.Parallel()
	admin := testServer(t)
	ctx := context.Background()
	template := DatabaseOn(t, admin, "", "testdb_jit_template")
	parsed, err := url.Parse(template)
	if err != nil {
		t.Fatal("the template's URL does not parse")
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	for _, row := range []struct{ name, template string }{{"empty", ""}, {"a copy of a template", name}} {
		conn, err := pgx.Connect(ctx, DatabaseOn(t, admin, row.template, "testdb_jit_test"))
		if err != nil {
			t.Fatalf("connect to the test's %s database: %v", row.name, err)
		}
		if setting, source := jitOn(t, conn); setting != "off" || source != "database" {
			t.Errorf("DatabaseOn's %s database runs with jit = %s, from the %s; want off, set on the database (WithoutJIT)", row.name, setting, source)
		}
		conn.Close(ctx)
	}
}

// jitOn is the jit setting conn's session runs with and where it comes from (pg_settings), or two
// empty strings, with t failed, when it cannot be read.
func jitOn(t *testing.T, conn *pgx.Conn) (setting, source string) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), `select setting, source from pg_settings where name = 'jit'`).Scan(&setting, &source); err != nil {
		t.Errorf("read jit: %v", err)
	}
	return setting, source
}
