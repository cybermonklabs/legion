package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
)

// TestTheAnswersReadIsTimedAsTheAccessLogTimesIt reads a session's answers through the route, on
// the package's test server with a sessionanswers.Latency in its dependencies whose clock hands
// the test each instant it reads: a read's begin and its end. A fast read is timed, and is no slow
// read. A read that a transaction holds for 900 ms, by its lock on message_deliveries, which the
// answers statement reads, is a slow read while it runs past 800 ms and, once it ended, for the
// 10 s after its end. Each read's time is the time its access-log line records, but for the
// line's truncation to the millisecond, and a read of another route is not timed.
func TestTheAnswersReadIsTimedAsTheAccessLogTimesIt(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	instants := make(chan time.Time, 8)
	latency := sessionanswers.NewLatency(func() time.Time {
		now := time.Now()
		instants <- now
		return now
	})
	deps.AnswersLatency = latency
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	logged := captureLog(t)
	const answers = "/api/v1/agents/" + answersSession + "/answers"

	if response := dispatchRequest(t, mux, http.MethodGet, answers, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the fast read answered %d: %s", response.Code, response.Body.String())
	}
	began, ended := nextInstant(t, instants, "the fast read's begin"), nextInstant(t, instants, "the fast read's end")
	if took := ended.Sub(began); took > sessionanswers.SlowRead {
		t.Fatalf("the fast read took %v, over %v with nothing holding it", took, sessionanswers.SlowRead)
	}
	if latency.Slow(ended) {
		t.Error("a fast read that ended is a slow read")
	}
	requireTimedAsLogged(t, logged, 0, ended.Sub(began))

	holder, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that holds the read: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `lock table message_deliveries in access exclusive mode`); err != nil {
		t.Fatalf("lock message_deliveries: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() { responses <- dispatchRequest(t, mux, http.MethodGet, answers, nil, "alice") }()
	began = nextInstant(t, instants, "the held read's begin")
	waitForDatabaseLocks(t, holder, 1)
	time.Sleep(time.Until(began.Add(900 * time.Millisecond)))
	if !latency.Slow(time.Now()) {
		t.Error("a read that has run 900 ms and not ended is no slow read")
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatalf("release message_deliveries: %v", err)
	}
	if response := awaitResponse(t, responses); response.Code != http.StatusOK {
		t.Fatalf("the held read answered %d: %s", response.Code, response.Body.String())
	}
	ended = nextInstant(t, instants, "the held read's end")
	if took := ended.Sub(began); took < 900*time.Millisecond {
		t.Fatalf("the held read was timed %v; the lock held it 900 ms", took)
	}
	if !latency.Slow(ended.Add(sessionanswers.SlowWithin - time.Millisecond)) {
		t.Errorf("a read that took over %v is no slow read just under %v after its end", sessionanswers.SlowRead, sessionanswers.SlowWithin)
	}
	if latency.Slow(ended.Add(sessionanswers.SlowWithin)) {
		t.Errorf("a read that took over %v is still a slow read %v after its end", sessionanswers.SlowRead, sessionanswers.SlowWithin)
	}
	requireTimedAsLogged(t, logged, 1, ended.Sub(began))

	if response := dispatchRequest(t, mux, http.MethodGet, "/api/v1/agents/"+answersSession+"/messages", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the session's messages answered %d: %s", response.Code, response.Body.String())
	}
	select {
	case instant := <-instants:
		t.Errorf("a read of the session's messages was timed (at %v)", instant)
	default:
	}
}

// nextInstant is the next instant the Latency's clock read, within 2 s.
func nextInstant(t *testing.T, instants <-chan time.Time, what string) time.Time {
	t.Helper()
	select {
	case instant := <-instants:
		return instant
	case <-time.After(2 * time.Second):
		t.Fatalf("the Latency never read %s", what)
		return time.Time{}
	}
}

// elapsedField reads an access-log line's elapsed_ms.
var elapsedField = regexp.MustCompile(`\belapsed_ms=(\d+)\b`)

// requireTimedAsLogged fails unless the answers read the access log logged n-th (from 0) took
// timed, as the line times it: the Latency times the read inside the line's own timing, so it is
// at most the line's, and short of it only by the layers between the two, which take
// microseconds; timedAsLoggedSlack allows for a scheduler that parks the goroutine between them
// on a loaded runner, and is still far below the 900 ms read the test holds.
func requireTimedAsLogged(t *testing.T, logged *logBuffer, n int, timed time.Duration) {
	t.Helper()
	var reads []string
	for _, line := range accessLines(t, logged) {
		if strings.Contains(line, "route=/api/v1/agents/{session_id}/answers ") {
			reads = append(reads, line)
		}
	}
	if len(reads) <= n {
		t.Fatalf("the access log holds %d answers reads; want a line for read %d:\n%s", len(reads), n, strings.Join(reads, "\n"))
	}
	match := elapsedField.FindStringSubmatch(reads[n])
	if match == nil {
		t.Fatalf("the access-log line %q carries no elapsed_ms", reads[n])
	}
	elapsed, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatalf("read elapsed_ms of %q: %v", reads[n], err)
	}
	if ms := timed.Milliseconds(); ms > elapsed || elapsed > ms+timedAsLoggedSlack.Milliseconds() {
		t.Errorf("answers read %d was timed %v; its access-log line records elapsed_ms=%d", n, timed, elapsed)
	}
}

// timedAsLoggedSlack is how far the access log's time of an answers read may pass the Latency's.
const timedAsLoggedSlack = 50 * time.Millisecond
