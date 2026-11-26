package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"

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
