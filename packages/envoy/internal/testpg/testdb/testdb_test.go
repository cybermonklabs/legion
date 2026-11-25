package testdb

import (
	"math"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// deadlineTB is a testing.TB whose deadline the test chooses.
type deadlineTB struct {
	testing.TB
	at      time.Time
	bounded bool
}

func (d deadlineTB) Deadline() (time.Time, bool) { return d.at, d.bounded }

// noDeadlineTB is a testing.TB that names no deadline, as a benchmark's does not.
type noDeadlineTB struct{ testing.TB }

// connectTimeoutOf is the connect_timeout databaseURL names, failing t when it names none or a
// value that is no whole number of seconds.
func connectTimeoutOf(t *testing.T, databaseURL string) int64 {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the URL: %v", err)
	}
	seconds, err := strconv.ParseInt(parsed.Query().Get("connect_timeout"), 10, 64)
	if err != nil {
		t.Fatalf("connect_timeout of %s: %v", databaseURL, err)
	}
	return seconds
}

// WithConnectDeadline gives a URL the seconds left until the binary's deadline, rounded up, in
// place of the connect_timeout the URL named, and keeps the rest of the URL. Half a second past
// a whole second is left, so a bound that rounds down lands below the time left. The time left is
// counted from a moment the test names, so the reading is exact.
func TestWithConnectDeadlineReplacesTheURLsConnectTimeoutWithTheSecondsLeftRoundedUp(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	at := now.Add(90*time.Second + 500*time.Millisecond)
	got := withConnectDeadlineAt(deadlineTB{TB: t, at: at, bounded: true}, "postgres://tester@db.test:5432/app?connect_timeout=5&sslmode=disable", now)

	if seconds := connectTimeoutOf(t, got); seconds != 91 {
		t.Fatalf("connect_timeout = %d, want 91: the 90.5 seconds left until the deadline, rounded up", seconds)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse the URL: %v", err)
	}
	if parsed.User.String() != "tester" || parsed.Host != "db.test:5432" || parsed.Path != "/app" || parsed.Query().Get("sslmode") != "disable" {
		t.Fatalf("WithConnectDeadline = %s, want the URL it was given with only its connect_timeout changed", got)
	}
}

// A deadline already passed still gives a connect a bound pgx reads as one, 1 s, and not 0, which
// pgx reads as none and a pool as its own 2 minutes.
func TestWithConnectDeadlineGivesAPassedDeadlineOneSecond(t *testing.T) {
	got := WithConnectDeadline(deadlineTB{TB: t, at: time.Now().Add(-time.Minute), bounded: true}, "postgres://tester@db.test:5432/app")
	if seconds := connectTimeoutOf(t, got); seconds != 1 {
		t.Fatalf("connect_timeout with the deadline passed = %d, want 1", seconds)
	}
}

// Under -timeout 0, and for a t that names no deadline, the connect has no bound of the test's
// own: connect_timeout is the longest pgx holds, which pgxpool keeps in place of its 2 minutes.
func TestWithConnectDeadlineWithNoDeadlineIsTheLongestBoundPgxHolds(t *testing.T) {
	const longest = math.MaxInt64 / int64(time.Second)
	for name, tb := range map[string]testing.TB{
		"under -timeout 0":     deadlineTB{TB: t},
		"a t with no Deadline": noDeadlineTB{TB: t},
	} {
		got := WithConnectDeadline(tb, "postgres://tester@db.test:5432/app?connect_timeout=5")
		if seconds := connectTimeoutOf(t, got); seconds != longest {
			t.Errorf("%s: connect_timeout = %d, want %d", name, seconds, longest)
		}
	}
}

// Server sets the bound on the URL its variable names, so every database, clone and pool a
// fixture makes from that URL carries it; the variable's own connect_timeout does not survive. The
// time left is counted from a moment the test names, so the reading is exact.
func TestAServersURLCarriesTheBinarysDeadlineAsItsConnectBound(t *testing.T) {
	const variable = "TESTDB_CONNECT_DEADLINE_SERVER"
	t.Setenv(variable, "postgres://tester@db.test:5432/lane?connect_timeout=5&sslmode=disable")
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	at := now.Add(10*time.Minute + 500*time.Millisecond)
	server := Server(variable).urlAt(deadlineTB{TB: t, at: at, bounded: true}, now)

	clone := databaseURL(server, "testdb_test_clone")
	if seconds := connectTimeoutOf(t, clone); seconds != 601 {
		t.Fatalf("a clone's connect_timeout = %d, want 601: the 600.5 seconds left until the deadline, rounded up", seconds)
	}
	if server.Query().Get("sslmode") != "disable" {
		t.Fatalf("the server's URL = %s, want the variable's sslmode kept", server.String())
	}
}
