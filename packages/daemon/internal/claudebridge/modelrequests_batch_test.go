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
		attempt++
		n := attempt
		select {
		case r.posts <- body.Requests:
		default:
			t.Error("reporter sent more batches than the test could read")
		}
		mu.Unlock()
		if answer != nil {
			answer(w, ids, n)
		} else {
			_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
		}
	}))
	t.Cleanup(daemon.Close)
	r.reporter = startModelRequests(&daemonPoster{
		url: daemon.URL, generation: 4, bootToken: testBootToken,
		client: daemon.Client(), clock: r.clock, log: log.New(r.logged, "", 0),
	}, r.clock.window)
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
	if r.advance(t, to.Sub(r.clock.Now())) {
		t.Fatalf("moving the clock on to %s fired a window, want none due before it", to.Format(time.RFC3339Nano))
	}
}

// advance moves the clock on by d, which fires each window due by then, and, where the window
// armed was due by then, waits until the reporter has acted on it: a window that is due closes
// every open request and sends what is pending (flushLocked), and an early one only arms again. A
// firing of a replaced (armAtLocked) or a cancelled (flushLocked) window is neither waited on nor
// counted: the reporter does nothing when it fires. It answers whether the reporter acted on the
// armed window.
func (r *modelRequestRig) advance(t *testing.T, d time.Duration) bool {
	t.Helper()
	r.reporter.mu.Lock()
	armed, due := r.reporter.window, r.reporter.windowDue
	r.reporter.mu.Unlock()
	r.clock.advance(d)
	if armed == 0 || due.After(r.clock.Now()) {
		return false
	}
	testwait.Eventually(t, "the window to be acted on", func() bool {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return r.reporter.window != armed
	})
	return true
}

// fireWindow moves the clock on to to, where the one window armed fires, and waits until the
// reporter has acted on it (advance): a window that is due closes every open request and sends
// what is pending, and an early one only arms again. It counts that window's firing alone: the
// timer of a replaced or a cancelled window may fire too, and counts for nothing.
func (r *modelRequestRig) fireWindow(t *testing.T, to time.Time) {
	t.Helper()
	if !r.advance(t, to.Sub(r.clock.Now())) {
		t.Fatalf("moving the clock on to %s fired no armed window, want the one armed", to.Format(time.RFC3339Nano))
	}
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
		return r.reporter.posting == 0
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
// which only a later line of that session, the turn's result, or a due window closes. The window
// the first block armed comes while both are being written, sees the oldest end has moved on, and
// sends neither; the subagent's tool result closes the subagent's request alone; the next window
// is due requestSendMargin before requestWindow after the role's newest line and sends both, each
// once, ended at its own last block.
func TestModelRequestsKeepsOneOpenRequestPerSession(t *testing.T) {
	t.Parallel()
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
// 10:00:58, the window its first block armed at 10:01:04.75 while its tool call is still being
// written, and the tool call's block at 10:01:10. No window sends the request while its newest
// line is younger than requestDueAfter, so it goes once that much time has passed since its tool
// call, within requestWindow of its end: its tokens count in 10:01, the minute it ended in, not
// in 10:00.
func TestModelRequestsWindowWaitsForAToolCallBeingWritten(t *testing.T) {
	t.Parallel()
	r := newModelRequestRig(t, nil)
	start := time.Date(2026, time.November, 26, 10, 0, 50, 0, time.UTC)
	r.clock.set(start)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	r.assistant("tool-call", at(0))
	r.moveTo(t, at(8))
	r.assistant("tool-call", at(8))
	r.fireWindow(t, at(15).Add(-requestSendMargin))
	r.noBatch(t)
	r.moveTo(t, at(20))
	r.assistant("tool-call", at(20))
	r.fireWindow(t, at(23).Add(-requestSendMargin))
	r.noBatch(t)
	// The tool runs, and no line comes: the window sends requestSendMargin before requestWindow.
	r.moveTo(t, at(35).Add(-requestSendMargin-time.Millisecond))
	r.fireWindow(t, at(35).Add(-requestSendMargin))
	r.expectEnds(t, reportedEnd{"tool-call", capturedSession, at(20)})
	r.awaitAnswer(t)
	r.toolResult("")
	r.assistant("after-tool", at(40))
	r.result()
	r.expectEnds(t, reportedEnd{"after-tool", capturedSession, at(40)})
}

// The role's own requests and a subagent's end a second or more apart, and the bridge reads each
// line a quarter of a second after Claude Code wrote it. One window serves both sessions, so each
// window that sends sends one report with every request the bridge holds, never a report for each
// session a second apart, and each request goes within requestWindow of its end on the bridge's
// clock, not requestWindow after the bridge read its line. The row moves the clock a tick at a
// time, so a report's time is the window's to within a tick.
func TestModelRequestsStaggeredSessionsShareOneWindow(t *testing.T) {
	t.Parallel()
	r := newModelRequestRig(t, nil)
	start := r.clock.Now()
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	const read, tick = 250 * time.Millisecond, 50 * time.Millisecond
	lines := []struct {
		id, agent string
		end       time.Time
	}{
		{"role-1", "", at(0)}, {"sub-1", capturedSubagent, at(1)},
		{"role-2", "", at(20)}, {"sub-2", capturedSubagent, at(21)},
		{"role-2", "", at(22)}, {"sub-2", capturedSubagent, at(27)},
		{"role-3", "", at(40)}, {"sub-3", capturedSubagent, at(41)},
	}
	type report struct {
		at       time.Time
		requests []claim.ModelRequest
	}
	var reports []report
	for r.clock.Now().Before(at(70)) {
		for len(lines) != 0 && !r.clock.Now().Before(lines[0].end.Add(read)) {
			r.block(lines[0].id, lines[0].agent, lines[0].end)
			lines = lines[1:]
		}
		if !r.advance(t, tick) {
			continue
		}
		r.awaitAnswer(t)
		for drained := false; !drained; {
			select {
			case batch := <-r.posts:
				reports = append(reports, report{at: r.clock.Now(), requests: batch})
			default:
				drained = true
			}
		}
	}
	sent := map[string]reportedEnd{}
	for i, report := range reports {
		if i > 0 && report.at.Sub(reports[i-1].at) < requestDueAfter {
			t.Errorf("the bridge sent a report at +%s, %s after the one before: more than one report in a window",
				report.at.Sub(start), report.at.Sub(reports[i-1].at))
		}
		for _, request := range report.requests {
			if report.at.After(request.EndedAt.Add(requestWindow)) {
				t.Errorf("%s, ended at +%s, went at +%s: later than requestWindow after its end",
					request.ID, request.EndedAt.Sub(start), report.at.Sub(start))
			}
			sent[request.ID] = reportedEnd{request.ID, request.Session, request.EndedAt}
		}
	}
	for _, want := range []reportedEnd{
		{"role-1", capturedSession, at(0)}, {"sub-1", capturedSubagent, at(1)},
		{"role-2", capturedSession, at(22)}, {"sub-2", capturedSubagent, at(27)},
		{"role-3", capturedSession, at(40)}, {"sub-3", capturedSubagent, at(41)},
	} {
		if got := sent[want.id]; got.session != want.session || !got.end.Equal(want.end) {
			t.Errorf("the bridge last reported %s as %v, want %v", want.id, got, want)
		}
	}
}

// A block Claude Code wrote before a window sent, which the bridge reads only after that window,
// goes by its own end + requestWindow on the bridge's clock, not requestWindow after that window:
// whether the bridge holds nothing else, or a window armed for a later request comes after that
// moment. A block the bridge reads only after that moment goes at once. The row moves the clock a
// tick at a time, so a report's time is the window's to within a tick.
func TestModelRequestsSendsABlockReadAfterAWindowByItsOwnEnd(t *testing.T) {
	t.Parallel()
	const ms, tick = time.Millisecond, 50 * time.Millisecond
	for _, row := range []struct {
		name string
		// next is whether a request of the role's own, read as Claude Code writes it after the
		// window, arms the next window before the bridge reads the late block.
		next bool
		// end is when Claude Code wrote the late block, read when the bridge read it, and by the
		// latest its report may go, each after the row's start.
		end, read, by time.Duration
	}{
		{"read after a window", false, 14_350 * ms, 14_850 * ms, 14_350*ms + requestWindow},
		{"read after a window, with the next armed", true, 14_350 * ms, 14_850 * ms, 14_350*ms + requestWindow},
		{"read after its own end + requestWindow", false, 5_000 * ms, 20_000 * ms, 20_000 * ms},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newModelRequestRig(t, nil)
			start := r.clock.Now()
			at := func(d time.Duration) time.Time { return start.Add(d) }
			r.block("first", "", start)
			r.fireWindow(t, at(requestDueAfter))
			r.expectEnds(t, reportedEnd{"first", capturedSession, start})
			r.awaitAnswer(t)
			want := []reportedEnd{{"late", capturedSubagent, at(row.end)}}
			if row.next {
				next := at(14_800 * ms)
				r.moveTo(t, next)
				r.block("next", "", next)
				want = append(want, reportedEnd{"next", capturedSession, next})
			}
			r.moveTo(t, at(row.read))
			r.block("late", capturedSubagent, at(row.end))
			// The first step moves the clock on by nothing: a window due at once fires there.
			for step := time.Duration(0); len(r.posts) == 0; step = tick {
				if r.clock.Now().After(at(time.Minute)) {
					t.Fatalf("the bridge sent nothing by +%s", time.Minute)
				}
				if r.advance(t, step) {
					r.awaitAnswer(t)
				}
			}
			sent := r.clock.Now().Sub(start)
			r.expectEnds(t, want...)
			if sent > row.by {
				t.Errorf("the late block, ended at +%s and read at +%s, went at +%s, want by +%s", row.end, row.read, sent, row.by)
			}
		})
	}
}

// A block of a request whose report is on its way opens the request again. Both reports can be on
// their way at once, and if both fail (in either order) the request goes again once, ended at the
// later block: never twice in one batch, which the daemon refuses whole (an id names one request
// of a report), and never at its earlier end alone.
func TestModelRequestsSendsALateBlockOfAFailedReportOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		firstFirst bool
	}{
		{"earlier report fails first", true},
		{"later report fails first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			releaseFirst := make(chan struct{})
			releaseSecond := make(chan struct{})
			var onceFirst, onceSecond sync.Once
			freeFirst := func() { onceFirst.Do(func() { close(releaseFirst) }) }
			freeSecond := func() { onceSecond.Do(func() { close(releaseSecond) }) }
			r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
				switch attempt {
				case 1:
					<-releaseFirst
					w.WriteHeader(http.StatusInternalServerError)
				case 2:
					<-releaseSecond
					w.WriteHeader(http.StatusInternalServerError)
				default:
					_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
				}
			})
			t.Cleanup(func() { freeFirst(); freeSecond() })
			first := r.clock.Now()
			r.assistant("tool-call", first)
			r.result()
			r.expectEnds(t, reportedEnd{"tool-call", capturedSession, first})
			last := first.Add(20 * time.Second)
			r.assistant("tool-call", last)
			r.result()
			r.expectEnds(t, reportedEnd{"tool-call", capturedSession, last})
			if tc.firstFirst {
				freeFirst()
				testwait.Eventually(t, "the first failure to be processed", func() bool {
					r.reporter.mu.Lock()
					defer r.reporter.mu.Unlock()
					return r.reporter.posting == 1
				})
				freeSecond()
			} else {
				freeSecond()
				testwait.Eventually(t, "the second failure to be processed", func() bool {
					r.reporter.mu.Lock()
					defer r.reporter.mu.Unlock()
					return r.reporter.posting == 1
				})
				freeFirst()
			}
			r.awaitAnswer(t)
			r.result()
			r.expectEnds(t, reportedEnd{"tool-call", capturedSession, last})
		})
	}
}

// A slow daemon that holds one report unanswered for 6 seconds must not delay a later request's
// report past requestWindow after its end: a report that falls due while another is on its way
// goes at once, in a call of its own, carrying only requests no report on its way carries. The
// stand-in daemon holds the first report unanswered across a 6-second move of the rig's fake
// window clock while a late-read request falls due and must arrive before the first is answered.
func TestModelRequestsSendsWhilePreviousReportIsSlow(t *testing.T) {
	t.Parallel()
	const ms = time.Millisecond
	const slowAnswer = 6 * time.Second
	releaseFirst := make(chan struct{})
	var once sync.Once
	freeFirst := func() { once.Do(func() { close(releaseFirst) }) }
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			<-releaseFirst
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	t.Cleanup(freeFirst)
	start := r.clock.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }

	// Report 1 fires at requestDueAfter and the daemon holds its answer for slowAnswer (6 s).
	r.block("first", "", start)
	r.fireWindow(t, at(requestDueAfter))
	r.expectEnds(t, reportedEnd{"first", capturedSession, start})

	// A block that ended at +5 s is read at +15 s, just after report 1's flush; its own deadline
	// (+19.75 s) falls inside report 1's 6 s hold (+14.75 s .. +20.75 s), and waiting for report 1
	// would send it at +20.75 s — 15.75 s after its end, past requestWindow.
	lateEnd := 5_000 * ms
	lateRead := 15_000 * ms
	r.moveTo(t, at(lateRead))
	r.block("late", capturedSubagent, at(lateEnd))
	lateDue := lateEnd + requestDueAfter
	if lateDue >= requestDueAfter+slowAnswer {
		t.Fatalf("test setup: late due +%s must fall within report 1's +%s..+%s hold",
			lateDue, requestDueAfter, requestDueAfter+slowAnswer)
	}

	r.fireWindow(t, at(lateDue))
	// Report 2 must arrive while report 1 is still held unanswered, within requestWindow of late's end.
	batch := testwait.ReceiveWithin(t, 2*time.Second, "the second report while the first is unanswered",
		"a due report waited for the report already on its way instead of leaving in a call of its own", r.posts)
	sent := r.clock.Now().Sub(start)
	var got []reportedEnd
	for _, req := range batch {
		got = append(got, reportedEnd{id: req.ID, session: req.Session, end: req.EndedAt})
	}
	want := []reportedEnd{{"late", capturedSubagent, at(lateEnd)}}
	if !slices.EqualFunc(got, want, func(a, b reportedEnd) bool { return a.id == b.id && a.session == b.session && a.end.Equal(b.end) }) {
		t.Fatalf("the second report carried %v, want %v (carrying only requests not on their way)", got, want)
	}
	if sent > lateEnd+requestWindow {
		t.Errorf("the late request, ended at +%s, went at +%s: later than requestWindow (%s) after its end",
			lateEnd, sent, requestWindow)
	}

	// Move past the 6 s hold and let the first report's answer finish cleanly.
	r.moveTo(t, at(requestDueAfter+slowAnswer))
	freeFirst()
	r.awaitAnswer(t)
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
			// The failed requests are taken (pendingRequest), so their own deadlines, past already, do
			// not make the retry window due: it comes no sooner than requestDueAfter after the failed
			// report's flush.
			retry := r.clock.Now().Add(requestDueAfter)
			r.moveTo(t, retry.Add(-time.Millisecond))
			r.fireWindow(t, retry)
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
