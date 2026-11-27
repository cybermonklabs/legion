package claudebridge

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// modelRequestRig drives stream lines into the reporter and reads the wire requests at its daemon.
type modelRequestRig struct {
	reporter *modelRequests
	clock    *windowClock
	logged   *lockedBuffer
	posts    chan []claim.ModelRequest
}

func newModelRequestRig(t *testing.T, answer func(http.ResponseWriter, []string, int)) *modelRequestRig {
	t.Helper()
	r := &modelRequestRig{clock: &windowClock{at: time.Now().UTC()}, logged: &lockedBuffer{}, posts: make(chan []claim.ModelRequest, 8)}
	var mu sync.Mutex
	attempt := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body claim.ModelRequestsRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("read model request batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ids := make([]string, 0, len(body.Requests))
		for _, entry := range body.Requests {
			ids = append(ids, entry.ID)
		}
		mu.Lock()
		defer mu.Unlock()
		attempt++
		select {
		case r.posts <- body.Requests:
		default:
			t.Error("reporter sent more batches than the test could read")
		}
		if answer != nil {
			answer(w, ids, attempt)
		} else {
			_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
		}
	}))
	t.Cleanup(daemon.Close)
	r.reporter = startModelRequests(&daemonPoster{
		url: daemon.URL, generation: 4, bootToken: testBootToken,
		client: daemon.Client(), clock: r.clock, log: log.New(r.logged, "", 0),
	})
	return r
}

func (r *modelRequestRig) assistant(id string, at time.Time) { r.block(id, "", at) }

// block reads the assistant line of one block of request id, written at at: of the role's own
// session where agent is "", else of the subagent agent, which serves a Task call.
func (r *modelRequestRig) block(id, agent string, at time.Time) {
	line := streamLine{
		kind: "assistant", session: capturedSession, timestamp: at.Format(time.RFC3339Nano),
		message: json.RawMessage(fmt.Sprintf(`{"id":%q,"model":%q,"usage":{"input_tokens":1}}`, id, capturedModel)),
	}
	if agent != "" {
		line.parent, line.agent = "toolu_task_"+agent, agent
	}
	r.reporter.read(line)
}

// toolResult reads the user line of a tool's result: of the role's own session where agent is
// "", else of the subagent agent.
func (r *modelRequestRig) toolResult(agent string) {
	line := streamLine{kind: "user", session: capturedSession}
	if agent != "" {
		line.parent, line.agent = "toolu_task_"+agent, agent
	}
	r.reporter.read(line)
}

func (r *modelRequestRig) result() { r.reporter.read(streamLine{kind: "result"}) }

// expectBatch detects both loss and re-sends of a kept ID.
func (r *modelRequestRig) expectBatch(t *testing.T, want []string) {
	t.Helper()
	batch := testwait.Receive(t, "the model request batch", r.posts)
	got := make([]string, 0, len(batch))
	for _, request := range batch {
		got = append(got, request.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("batch has %d IDs, want %d; got %q, want %q", len(got), len(want), got, want)
	}
}

// reportedEnd is what a row asserts of one request of a batch: its id, its session and its end.
type reportedEnd struct {
	id, session string
	end         time.Time
}

func (e reportedEnd) String() string {
	return fmt.Sprintf("%s of %s ended %s", e.id, e.session, e.end.UTC().Format(time.RFC3339Nano))
}

// expectEnds reads the next batch and fails unless it holds want, in order: each request once,
// under its session, ended where want says.
func (r *modelRequestRig) expectEnds(t *testing.T, want ...reportedEnd) {
	t.Helper()
	var got []reportedEnd
	for _, request := range testwait.Receive(t, "the model request batch", r.posts) {
		got = append(got, reportedEnd{id: request.ID, session: request.Session, end: request.EndedAt})
	}
	if !slices.EqualFunc(got, want, func(a, b reportedEnd) bool { return a.id == b.id && a.session == b.session && a.end.Equal(b.end) }) {
		t.Fatalf("the batch reported %v, want %v", got, want)
	}
}

// noBatch holds that the reporter sends no batch within a short while.
func (r *modelRequestRig) noBatch(t *testing.T) {
	t.Helper()
	select {
	case batch := <-r.posts:
		t.Fatalf("the reporter sent %+v, want no batch", batch)
	case <-time.After(300 * time.Millisecond):
	}
}

// moveTo moves the clock on to to, where no window is due.
func (r *modelRequestRig) moveTo(t *testing.T, to time.Time) {
	t.Helper()
	if fired := r.clock.advance(to.Sub(r.clock.Now())); fired != 0 {
		t.Fatalf("moving the clock on to %s fired %d windows, want none due before it", to.Format(time.RFC3339Nano), fired)
	}
}

// fireWindow moves the clock on to to, where the one window armed is due, and waits until the
// reporter has acted on it: closed each open request that is due, sent what is pending and armed
// the next window.
func (r *modelRequestRig) fireWindow(t *testing.T, to time.Time) {
	t.Helper()
	r.reporter.mu.Lock()
	armed := r.reporter.window
	r.reporter.mu.Unlock()
	if fired := r.clock.advance(to.Sub(r.clock.Now())); fired != 1 {
		t.Fatalf("moving the clock on to %s fired %d windows, want the one armed", to.Format(time.RFC3339Nano), fired)
	}
	testwait.Eventually(t, "the window to be acted on", func() bool {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return r.reporter.window != armed
	})
}

func (r *modelRequestRig) enqueueBatch(n int, prefix string) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%03d", prefix, i)
		r.assistant(ids[i], r.clock.Now())
	}
	return ids
}

func (r *modelRequestRig) awaitAnswer(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the daemon's answer to be processed", func() bool {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return !r.reporter.sending
	})
}

// A request whose tool runs longer than the window is reported at the window, ended at its newest
// block. A block of it Claude Code writes after that report, as it writes a tool call that takes
// longer than the window to write, opens the request again: it goes once more under its id, ended
// at that block, and the daemon moves it there (record.Store.KeepModelRequests), so no block of a
// request leaves its tokens in an earlier minute.
func TestModelRequestsWindowFlushesOpenRequestDuringLongToolCall(t *testing.T) {
	r := newModelRequestRig(t, nil)
	first := r.clock.Now()
	r.assistant("tool-call", first)
	if fired := r.clock.fire(); fired != 1 {
		t.Fatalf("fired %d windows while the tool ran, want 1", fired)
	}
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, first})
	r.awaitAnswer(t)
	last := r.clock.Now()
	r.assistant("tool-call", last)
	r.assistant("after-tool", last)
	r.result()
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, last}, reportedEnd{"after-tool", capturedSession, last})
}

// The role's own request and a subagent's interleave: each session has its own open request,
// which only a later line of that session, the turn's result, or requestWindow with no line of it
// closes. The window the first block armed comes while both are being written and sends neither;
// the subagent's tool result closes the subagent's request alone; the next window comes when the
// role's newest line is requestWindow old and sends both, each once, ended at its own last block.
func TestModelRequestsKeepsOneOpenRequestPerSession(t *testing.T) {
	r := newModelRequestRig(t, nil)
	start := r.clock.Now()
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	r.block("role-1", "", at(0))
	r.moveTo(t, at(4))
	r.block("sub-1", capturedSubagent, at(4))
	r.moveTo(t, at(9))
	r.block("role-1", "", at(9))
	r.moveTo(t, at(12))
	r.block("sub-1", capturedSubagent, at(12))
	// role-1's newest line is 6 seconds old at the window, and sub-1's 3.
	r.fireWindow(t, at(15))
	r.noBatch(t)
	r.moveTo(t, at(16))
	r.toolResult(capturedSubagent)
	r.fireWindow(t, at(24))
	r.expectEnds(t, reportedEnd{"sub-1", capturedSubagent, at(12)}, reportedEnd{"role-1", capturedSession, at(9)})
	r.awaitAnswer(t)
	r.result()
	r.noBatch(t)
}

// A tool call written across the window: the request's first block at 10:00:50, its next at
// 10:00:58, the window its first block armed at 10:01:05 while its tool call is still being
// written, and the tool call's block at 10:01:10. No window sends the request while its newest
// line is younger than requestWindow, so it goes once the tool has run for requestWindow, ended at
// its tool call: its tokens count in 10:01, the minute it ended in, not in 10:00.
func TestModelRequestsWindowWaitsForAToolCallBeingWritten(t *testing.T) {
	r := newModelRequestRig(t, nil)
	start := time.Date(2026, time.November, 26, 10, 0, 50, 0, time.UTC)
	r.clock.mu.Lock()
	r.clock.at = start
	r.clock.mu.Unlock()
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	r.assistant("tool-call", at(0))
	r.moveTo(t, at(8))
	r.assistant("tool-call", at(8))
	r.fireWindow(t, at(15))
	r.noBatch(t)
	r.moveTo(t, at(20))
	r.assistant("tool-call", at(20))
	r.fireWindow(t, at(23))
	r.noBatch(t)
	// The tool runs, and no line comes for requestWindow.
	r.fireWindow(t, at(35))
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, at(20)})
	r.awaitAnswer(t)
	r.toolResult("")
	r.assistant("after-tool", at(40))
	r.result()
	r.expectEnds(t, reportedEnd{"after-tool", capturedSession, at(40)})
}

// A block of a request whose report is on its way opens the request again. That report fails, and
// the request goes again once, ended at the later block: never twice in one batch, which the
// daemon refuses whole (an id names one request of a report), and never at its earlier end alone.
func TestModelRequestsSendsALateBlockOfAFailedReportOnce(t *testing.T) {
	release := make(chan struct{})
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			<-release
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	first := r.clock.Now()
	r.assistant("tool-call", first)
	r.result()
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, first})
	last := first.Add(20 * time.Second)
	r.assistant("tool-call", last)
	r.result()
	free()
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, last})
}

func TestModelRequestsReadsFullMaxBatchKeptAnswer(t *testing.T) {
	r := newModelRequestRig(t, nil)
	ids := r.enqueueBatch(claim.MaxModelRequests, "refused-"+capturedSession+"-0000000000000000")
	r.result()
	r.expectBatch(t, ids)
	r.awaitAnswer(t)
	r.assistant("next", r.clock.Now())
	r.result()
	r.expectBatch(t, []string{"next"})
}

func TestModelRequestsBacksOffOnDaemonFailureWithOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"server error", "unavailable", http.StatusInternalServerError},
		{"unreadable answer", "not JSON", http.StatusOK},
		{"no progress", `{"kept":[]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
				if attempt == 1 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
			})
			ids := r.enqueueBatch(claim.MaxModelRequests+5, "req")
			if fired := r.clock.fire(); fired != 1 {
				t.Fatalf("fired %d initial windows, want 1", fired)
			}
			r.expectBatch(t, ids[:claim.MaxModelRequests])
			r.awaitAnswer(t)
			select {
			case extra := <-r.posts:
				t.Fatalf("sent %d IDs without waiting for a retry window", len(extra))
			default:
			}
			if fired := r.clock.fire(); fired != 1 {
				t.Fatalf("fired %d retry windows, want 1", fired)
			}
			r.expectBatch(t, ids[:claim.MaxModelRequests])
			r.expectBatch(t, ids[claim.MaxModelRequests:])
		})
	}
}

func TestModelRequestsDrainsOverflowImmediatelyAfterKeptAnswer(t *testing.T) {
	r := newModelRequestRig(t, nil)
	ids := r.enqueueBatch(claim.MaxModelRequests+5, "req")
	r.result()
	r.expectBatch(t, ids[:claim.MaxModelRequests])
	r.expectBatch(t, ids[claim.MaxModelRequests:])
}

func TestModelRequestsDropsRequestsPastKeepWindow(t *testing.T) {
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			ids = nil
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	r.assistant("old", r.clock.Now().Add(-time.Hour+time.Second))
	r.result()
	r.expectBatch(t, []string{"old"})
	// The request expires while waiting after the first answer did not keep it.
	r.awaitAnswer(t)
	r.assistant("fresh", r.clock.Now())
	r.clock.fire()
	r.expectBatch(t, []string{"fresh"})
	if n := strings.Count(r.logged.String(), `dropped model request "old"`); n != 1 {
		t.Fatalf("logged %d drops, want one: %s", n, r.logged)
	}
}
