package claudebridge

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// instantWait is a clock a report's wait runs on whose pauses take no time: each moves the clock on
// by the pause and is recorded, after onPause, when set, has run.
type instantWait struct {
	at      time.Time
	pauses  []time.Duration
	onPause func()
}

func (w *instantWait) clock(limit time.Duration) waitClock {
	return waitClock{
		now: func() time.Time { return w.at },
		sleep: func(pause time.Duration) {
			if w.onPause != nil {
				w.onPause()
			}
			w.pauses = append(w.pauses, pause)
			w.at = w.at.Add(pause)
		},
		attemptLimit: limit,
	}
}

// reportsTo is a reporter posting to the daemon at base on clock, and the log it writes.
func reportsTo(base string, clock waitClock) (*turnReports, *lockedBuffer) {
	logged := &lockedBuffer{}
	return &turnReports{
		url: base + "/legion/v1/claims/turn-outcome", bootToken: testBootToken, generation: 4,
		client: &http.Client{}, log: log.New(logged, "", 0), wait: clock,
	}, logged
}

// The wait's whole table of answers, each the first post of a report gets, with the daemon taking
// the next: a post that got no answer from the daemon is sent again after the first pause — the
// connection refused, reset or closed before an answer, a post the daemon took and did not answer
// within the attempt's limit, and a 502, 503 or 504 with no code of the daemon's — and the report
// lands. An answer the daemon gave goes back at once, with no pause and no second post: a 4xx, an
// uncoded 500, and the daemon's own coded 500 and 502, each of which says itself what comes next.
func TestAReportWaitsOutADaemonThatIsNotServingAndNoAnswerTheDaemonGave(t *testing.T) {
	const limit = 200 * time.Millisecond
	answer := func(status int, body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	hangUp := func(reset bool) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack the post's connection: %v", err)
				return
			}
			if reset {
				_ = conn.(*net.TCPConn).SetLinger(0)
			}
			_ = conn.Close()
		}
	}
	unanswered := make(chan struct{}, 1)
	for _, tc := range []struct {
		name string
		// down is a daemon that refuses the first post's connection, listening from the pause on.
		down bool
		// first is how the daemon meets the first post, when it is up.
		first  func(http.ResponseWriter, *http.Request)
		waited bool
		// unanswered is a first post the daemon holds unanswered, which the wait must end.
		unanswered bool
	}{
		{name: "the connection refused, as a restarting daemon's is", down: true, waited: true},
		{name: "the connection reset before an answer", first: hangUp(true), waited: true},
		{name: "the connection closed before an answer", first: hangUp(false), waited: true},
		{name: "a post the daemon took and did not answer within the attempt's limit", waited: true, unanswered: true,
			first: func(_ http.ResponseWriter, request *http.Request) {
				// The server watches the connection for its end only once the body is read.
				_, _ = io.Copy(io.Discard, request.Body)
				select {
				case <-request.Context().Done():
					unanswered <- struct{}{}
				case <-time.After(10 * time.Second):
					t.Error("the bridge held a post the daemon did not answer open for 10 s, past the attempt's limit")
				}
			}},
		{name: "an uncoded 502", first: answer(http.StatusBadGateway, `{"error":"bad gateway"}`), waited: true},
		{name: "an uncoded 503, as the daemon answers while it stops", first: answer(http.StatusServiceUnavailable, `{"error":"the daemon is stopping"}`), waited: true},
		{name: "an uncoded 504 whose body is not the daemon's refusal shape", first: answer(http.StatusGatewayTimeout, "upstream request timeout"), waited: true},
		{name: "an uncoded 500", first: answer(http.StatusInternalServerError, `{"error":"the report could not be stored"}`)},
		{name: "the daemon's coded 500, DISPATCH_UNAVAILABLE", first: answer(http.StatusInternalServerError, `{"code":"DISPATCH_UNAVAILABLE","error":"Dispatch is unavailable"}`)},
		{name: "the daemon's coded 502, DISPATCH_FAILED", first: answer(http.StatusBadGateway, `{"code":"DISPATCH_FAILED","error":"Dispatch read failed"}`)},
		{name: "a 4xx refusal", first: answer(http.StatusBadRequest, `{"error":"generation is required"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var posts []claim.TurnOutcomeRequest
			taken := func() []claim.TurnOutcomeRequest {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(posts)
			}
			daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				mu.Lock()
				first := len(posts) == 0 && tc.first != nil
				if first {
					posts = append(posts, claim.TurnOutcomeRequest{})
				}
				mu.Unlock()
				if first {
					tc.first(w, request)
					return
				}
				var report claim.TurnOutcomeRequest
				if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
					t.Errorf("decode the bridge's report: %v", err)
				}
				mu.Lock()
				posts = append(posts, report)
				mu.Unlock()
				_, _ = io.WriteString(w, `{"refusedRun":0,"failed":false}`)
			}))
			address := daemon.Listener.Addr().String()
			wait := &instantWait{at: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
			if tc.down {
				if err := daemon.Listener.Close(); err != nil {
					t.Fatal(err)
				}
				var listen func() net.Listener
				address, listen = refusingPort(t)
				wait.onPause = func() {
					if len(wait.pauses) > 0 {
						return
					}
					daemon.Listener = listen()
					daemon.Start()
				}
			} else {
				daemon.Start()
			}
			t.Cleanup(daemon.Close)
			reports, logged := reportsTo("http://"+address, wait.clock(limit))
			request := claim.TurnOutcomeRequest{BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean}

			reports.post(request)

			took := taken()
			if !tc.waited {
				if len(wait.pauses) != 0 || len(took) != 1 {
					t.Fatalf("the report paused %v and was posted %d times, want it posted once and back at once; the bridge logged:\n%s", wait.pauses, len(took), logged)
				}
				if want := "the daemon refused the outcome of turn 1 (clean)"; !strings.Contains(logged.String(), want) {
					t.Fatalf("the bridge logged:\n%s\nwant it to say %q", logged, want)
				}
				return
			}
			if !slices.Equal(wait.pauses, []time.Duration{firstPause}) {
				t.Fatalf("the report paused %v, want one pause of %s before it was sent again; the bridge logged:\n%s", wait.pauses, firstPause, logged)
			}
			if len(took) == 0 || took[len(took)-1] != request {
				t.Fatalf("the daemon took %+v, want the report %+v last; the bridge logged:\n%s", took, request, logged)
			}
			if want := "the Legion daemon is unreachable, most likely restarting"; !strings.Contains(logged.String(), want) {
				t.Fatalf("the bridge logged:\n%s\nwant it to say %q", logged, want)
			}
			if tc.unanswered {
				select {
				case <-unanswered:
				case <-time.After(5 * time.Second):
					t.Fatal("the post the daemon did not answer was never ended: its connection stayed open")
				}
			}
		})
	}
}

// A daemon that never serves again is waited for an hour, the wait an Oh My Pi worker's call makes
// (config.StallDaemonWait, pi-shared's DAEMON_RESTART_BUDGET_MS), its pauses from half a second
// doubling to 30 s and the last one ending at the hour, and then the report is given up on and
// said to be. One post the daemon takes is given 5 minutes to be answered.
func TestAReportWaitsAnHourForADaemonThatNeverServesAndThenGivesUp(t *testing.T) {
	if config.StallDaemonWait != time.Hour || realWait.attemptLimit != 5*time.Minute {
		t.Fatalf("the wait is %s with an attempt limit of %s, want an hour and 5 minutes", config.StallDaemonWait, realWait.attemptLimit)
	}
	address, _ := refusingPort(t)
	wait := &instantWait{at: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	reports, logged := reportsTo("http://"+address, wait.clock(time.Second))

	reports.post(claim.TurnOutcomeRequest{BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnRefused, Message: "overloaded"})

	var waited time.Duration
	for i, want := 0, firstPause; i < len(wait.pauses); i, want = i+1, min(2*want, longestPause) {
		if pause := wait.pauses[i]; pause != want && i != len(wait.pauses)-1 {
			t.Fatalf("pause %d was %s, want %s; every pause: %v", i, pause, want, wait.pauses)
		}
		waited += wait.pauses[i]
	}
	if waited != time.Hour {
		t.Fatalf("the report waited %s over %d pauses, want an hour", waited, len(wait.pauses))
	}
	if want := "could not report the outcome of turn 1 (refused): the Legion daemon was unreachable"; !strings.Contains(logged.String(), want) {
		t.Fatalf("the bridge logged:\n%s\nwant it to say %q", logged, want)
	}
	if want := "and stayed so for 1h0m0s"; !strings.Contains(logged.String(), want) {
		t.Fatalf("the bridge logged:\n%s\nwant it to say %q", logged, want)
	}
}
