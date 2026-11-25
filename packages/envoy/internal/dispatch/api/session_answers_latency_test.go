package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheAnswersReadIsTimedAsTheAccessLogTimesIt reads a session's answers through the route, on
// the package's test server with a sessionanswers.Latency in its dependencies whose clock hands
// the test each instant it reads: a read's begin and its end. A fast read is timed, and is no slow
// read. A read that a transaction holds for 900 ms, by its lock on message_deliveries, which the
// answers statement reads, is a slow read while it runs past 800 ms and, once it ended, for the
// 10 s after its end. Each read's time is the time its access-log line records, but for the line's
// truncation to the millisecond, and a read of another route is not timed.
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
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	const answers = "/api/v1/agents/" + answersSession + "/answers"

	// An unheld read is fast unless the machine keeps it past SlowRead, and a read the machine
	// kept is a slow read by the Latency's own measure, which shows nothing of the fast case. So
	// the read is made again until the Latency times one at or under SlowRead, each read held to
	// its access-log line as it goes; lastSlowEnd is the end of the latest read the machine kept.
	var began, ended, lastSlowEnd time.Time
	reads := 0
	testwait.UntilPolling(t, 20*time.Millisecond, "an unheld answers read the Latency times at or under SlowRead (the time the last one took)", func() (time.Duration, bool) {
		if response := dispatchRequest(t, mux, http.MethodGet, answers, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("the unheld read answered %d: %s", response.Code, response.Body.String())
		}
		began, ended = nextInstant(t, instants, "an unheld read's begin"), nextInstant(t, instants, "an unheld read's end")
		took := ended.Sub(began)
		requireTimedAsLogged(t, records, reads, took)
		reads++
		if took > sessionanswers.SlowRead {
			t.Logf("unheld answers read %d took %v, over %v, so it shows no fast read: the read is made again", reads, took, sessionanswers.SlowRead)
			lastSlowEnd = ended
			return took, false
		}
		return took, true
	})
	// A read the machine kept makes Slow answer yes for its own sake until SlowWithin after its
	// end, so a question about another read is asked no earlier than that.
	pastKept := func(at time.Time) time.Time {
		if !lastSlowEnd.IsZero() && at.Before(lastSlowEnd.Add(sessionanswers.SlowWithin)) {
			return lastSlowEnd.Add(sessionanswers.SlowWithin)
		}
		return at
	}
	if latency.Slow(pastKept(ended)) {
		t.Error("a fast read that ended is a slow read")
	}

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
	if !latency.Slow(pastKept(time.Now())) {
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
	requireTimedAsLogged(t, records, reads, ended.Sub(began))

	if response := dispatchRequest(t, mux, http.MethodGet, "/api/v1/agents/"+answersSession+"/messages", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the session's messages answered %d: %s", response.Code, response.Body.String())
	}
	select {
	case instant := <-instants:
		t.Errorf("a read of the session's messages was timed (at %v)", instant)
	default:
	}
}

// nextInstant is the next instant the Latency's clock read. Its one bound is the test binary's
// deadline less testwait's margin.
func nextInstant(t *testing.T, instants <-chan time.Time, what string) time.Time {
	t.Helper()
	return testwait.Receive(t, "the Latency to read "+what, instants)
}

// answersRoute is the answers read's route as its access-log line names it.
const answersRoute = "/api/v1/agents/{session_id}/answers"

// requireTimedAsLogged fails unless the answers read the access log logged n-th (from 0) took
// timed, as the line times it: the Latency times the read inside the line's own timing, so it is
// at most the line's, and short of it only by the layers between the two, which take
// microseconds; timedAsLoggedSlack allows for a scheduler that parks the goroutine between them
// on a loaded runner, and is still far below the 900 ms read the test holds. The line's time is
// its record's elapsed_ms, as the access log logged it.
func requireTimedAsLogged(t *testing.T, records *logtest.Recorder, n int, timed time.Duration) {
	t.Helper()
	var reads []int64
	for _, record := range records.Records() {
		if record.Message != accesslog.Message {
			continue
		}
		route, elapsed := "", int64(-1)
		record.Attrs(func(attr slog.Attr) bool {
			switch {
			case attr.Key == "route":
				route = attr.Value.String()
			case attr.Key == "elapsed_ms" && attr.Value.Kind() == slog.KindInt64:
				elapsed = attr.Value.Int64()
			}
			return true
		})
		if route == answersRoute {
			reads = append(reads, elapsed)
		}
	}
	if len(reads) <= n {
		t.Fatalf("the access log holds %d answers reads (elapsed_ms %v); want a line for read %d", len(reads), reads, n)
	}
	elapsed := reads[n]
	if elapsed < 0 {
		t.Fatalf("the access-log line of answers read %d carries no elapsed_ms", n)
	}
	if ms := timed.Milliseconds(); ms > elapsed || elapsed > ms+timedAsLoggedSlack.Milliseconds() {
		t.Errorf("answers read %d was timed %v; its access-log line records elapsed_ms=%d", n, timed, elapsed)
	}
}

// timedAsLoggedSlack is how far the access log's time of an answers read may pass the Latency's.
const timedAsLoggedSlack = 50 * time.Millisecond
