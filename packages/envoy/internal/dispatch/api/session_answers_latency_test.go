package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheAnswersReadIsTimedAsTheAccessLogTimesIt reads a session's answers through the route, on
// the package's test server with a sessionanswers.Latency in its dependencies. The Latency and the
// access log read one clock, the server's (Deps.Clock, accesslog.HandlerOn): a fake that moves only
// when the test moves it, and that hands the test each instant the Latency reads, a read's begin
// and its end. A read made while the clock stands still is timed at nothing, and is no slow read. A
// read that a transaction holds, by its lock on message_deliveries, which the answers statement
// reads, while the test moves the clock 900 ms, is a slow read while it runs past 800 ms and, once
// it ended, for the 10 s after its end. Each read's time is the time its access-log line records,
// and a read of another route is not timed. No time the machine takes decides any of it.
func TestTheAnswersReadIsTimedAsTheAccessLogTimesIt(t *testing.T) {
	start := time.Date(2026, time.November, 22, 12, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	_, database, deps := newTestServer(t, testServerOptions{clock: fake})
	instants := make(chan time.Time, 8)
	latency := sessionanswers.NewLatency(func() time.Time {
		now := fake.Now()
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

	// A read made while the clock stands still: the Latency and the line both time it at nothing,
	// however long the machine takes to serve it.
	if response := dispatchRequest(t, mux, http.MethodGet, answers, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the unheld read answered %d: %s", response.Code, response.Body.String())
	}
	began, ended := nextInstant(t, instants, "an unheld read's begin"), nextInstant(t, instants, "an unheld read's end")
	requireTimedAsLogged(t, records, 0, ended.Sub(began))
	if latency.Slow(ended) {
		t.Errorf("a read timed %v, under %v, is a slow read once it ended", ended.Sub(began), sessionanswers.SlowRead)
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
	// The read waits behind the lock while the clock moves 900 ms, past SlowRead.
	held := began.Add(900 * time.Millisecond)
	fake.Set(held)
	if !latency.Slow(held) {
		t.Error("a read that has run 900 ms and not ended is no slow read")
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatalf("release message_deliveries: %v", err)
	}
	if response := awaitResponse(t, responses); response.Code != http.StatusOK {
		t.Fatalf("the held read answered %d: %s", response.Code, response.Body.String())
	}
	ended = nextInstant(t, instants, "the held read's end")
	if took := ended.Sub(began); took != 900*time.Millisecond {
		t.Fatalf("the held read was timed %v; the clock moved 900 ms while the lock held it", took)
	}
	if !latency.Slow(ended.Add(sessionanswers.SlowWithin - time.Millisecond)) {
		t.Errorf("a read that took over %v is no slow read just under %v after its end", sessionanswers.SlowRead, sessionanswers.SlowWithin)
	}
	if latency.Slow(ended.Add(sessionanswers.SlowWithin)) {
		t.Errorf("a read that took over %v is still a slow read %v after its end", sessionanswers.SlowRead, sessionanswers.SlowWithin)
	}
	requireTimedAsLogged(t, records, 1, ended.Sub(began))

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
// timed, as the line times it. The Latency times the read inside the line's own timing and on the
// same clock, the server's fake, which moves only while the test holds a read, so the two read the
// same moments and the line's time, its record's elapsed_ms as the access log logged it, is the
// Latency's to the millisecond the line keeps.
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
	if ms := timed.Milliseconds(); ms != elapsed {
		t.Errorf("answers read %d was timed %v; its access-log line records elapsed_ms=%d", n, timed, elapsed)
	}
}
