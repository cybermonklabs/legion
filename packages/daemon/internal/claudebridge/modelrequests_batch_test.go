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

// expectChunks reads the reports of one send of ids, more of them than one report holds: the send
// puts every chunk of at most claim.MaxModelRequests on its way at once (modelRequests' doc), so the
// chunks reach the daemon in either order. It fails unless the next
// ⌈len(ids)/claim.MaxModelRequests⌉ reports each hold at most claim.MaxModelRequests requests and
// together hold ids, each once.
func (r *modelRequestRig) expectChunks(t *testing.T, ids []string) {
	t.Helper()
	var got []string
	for range (len(ids) + claim.MaxModelRequests - 1) / claim.MaxModelRequests {
		batch := testwait.Receive(t, "a chunk of the model requests", r.posts)
		if len(batch) > claim.MaxModelRequests {
			t.Fatalf("a report has %d IDs, want at most %d", len(batch), claim.MaxModelRequests)
		}
		for _, request := range batch {
			got = append(got, request.ID)
		}
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(ids)); !slices.Equal(got, want) {
		t.Fatalf("the reports have %d IDs, want %d; got %q, want %q", len(got), len(want), got, want)
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
// every open request and flushes (flushLocked), and an early one only arms again. A
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
// reporter has acted on it (advance): a window that is due closes every open request and flushes
// (flushLocked), and an early one only arms again. It counts that window's firing alone: the
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
		name string
		// failOrder is the order the stand-in fails the two reports in: 0 is the earlier, 1 the
		// later.
		failOrder [2]int
	}{
		{"earlier report fails first", [2]int{0, 1}},
		{"later report fails first", [2]int{1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			releaseFirst := make(chan struct{})
			releaseSecond := make(chan struct{})
			var onceFirst, onceSecond sync.Once
			freeFirst := func() { onceFirst.Do(func() { close(releaseFirst) }) }
			freeSecond := func() { onceSecond.Do(func() { close(releaseSecond) }) }
			free := [2]func(){freeFirst, freeSecond}
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
			free[tc.failOrder[0]]()
			testwait.Eventually(t, "the first failure to be processed", func() bool {
				r.reporter.mu.Lock()
				defer r.reporter.mu.Unlock()
				return r.reporter.posting == 1
			})
			free[tc.failOrder[1]]()
			r.awaitAnswer(t)
			r.result()
			r.expectEnds(t, reportedEnd{"tool-call", capturedSession, last})
		})
	}
}

// A daemon slow to answer one report must not delay a later request's report past requestWindow
// after its end: the report a window sends while another is on its way goes at once
// (modelRequests' doc), carrying only the request no report on its way carries. The stand-in holds
// the first report unanswered until the second has come, across a 6-second move of the rig's
// window clock in which a request read late falls due, so a second report that waited for the
// first's answer never comes and the row fails on the test binary's deadline.
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
	r.expectEnds(t, reportedEnd{"late", capturedSubagent, at(lateEnd)})

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

// A daemon that fails a send of more requests than one report holds fails every chunk of it in
// one window, and the requests, put back (pendingRequest), go again at the window requestWindow
// after those calls ended, never sooner: no answer sends, and their own deadlines, past already, do
// not make that window due. That window carries one chunk of them, the oldest ends first, and the
// rest wait requestLeftDueAfter after it (modelRequests' doc).
func TestModelRequestsBacksOffOnDaemonFailureWithOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"server error", "unavailable", http.StatusInternalServerError},
		{"unreadable answer", "not JSON", http.StatusOK},
		{"nothing kept", `{"kept":[]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
				// The first window's two chunks fail.
				if attempt <= 2 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
			})
			// Each request ends a millisecond after the one before, so the oldest are the first.
			start := r.clock.Now()
			ids := make([]string, claim.MaxModelRequests+5)
			for i := range ids {
				ids[i] = fmt.Sprintf("req-%03d", i)
				r.assistant(ids[i], start.Add(time.Duration(i)*time.Millisecond))
			}
			if fired := r.clock.fire(); fired != 1 {
				t.Fatalf("fired %d initial windows, want 1", fired)
			}
			r.expectChunks(t, ids)
			r.awaitAnswer(t)
			select {
			case extra := <-r.posts:
				t.Fatalf("sent %d IDs without waiting for a retry window", len(extra))
			default:
			}
			// The retry window comes requestWindow after the failed calls ended, and carries the
			// oldest chunk.
			retry := r.clock.Now().Add(requestWindow)
			r.moveTo(t, retry.Add(-time.Millisecond))
			r.fireWindow(t, retry)
			r.expectBatch(t, ids[:claim.MaxModelRequests])
			r.awaitAnswer(t)
			// The rest wait requestLeftDueAfter after the retry's flush.
			next := r.clock.Now().Add(requestLeftDueAfter)
			r.moveTo(t, next.Add(-time.Millisecond))
			r.fireWindow(t, next)
			r.expectBatch(t, ids[claim.MaxModelRequests:])
		})
	}
}

// A send of more requests no report carried than one report holds puts every chunk of them on its
// way at once, none waiting for another's answer (modelRequests' doc). The stand-in holds the first
// call it takes until the second has come, so a chunk that waited for an answer never comes and
// the row fails on the test binary's deadline.
func TestModelRequestsSendsEveryChunkAtOnce(t *testing.T) {
	t.Parallel()
	second := make(chan struct{})
	var once sync.Once
	arrived := func() { once.Do(func() { close(second) }) }
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		switch attempt {
		case 1:
			<-second
		case 2:
			arrived()
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	t.Cleanup(arrived)
	ids := r.enqueueBatch(claim.MaxModelRequests+5, "req")
	r.result()
	r.expectChunks(t, ids)
	r.awaitAnswer(t)
}

// timeoutClock is the rig's window clock whose After fires as its window does, once the row moves
// the clock to its due: a report's call ends at its timeout (requestReportTimeout) on the rig's
// clock.
type timeoutClock struct{ *windowClock }

func (c timeoutClock) After(d time.Duration) <-chan time.Time { return c.window(d) }

// The deep review's long stall, on the bridge: a daemon that takes each report's connection and
// never answers, so each call ends at its timeout (requestReportTimeout) on the rig's clock, while
// the role's session ends a request every tick of 200 ms and a turn's result comes every 30 s, for
// 3 minutes, so the put-back requests (pendingRequest) pile up past several chunks. A window or a
// result flushes: it sends every fresh request, and at most one chunk of put-back ones, none while
// another is on its way, and the put-back requests it leaves wait requestLeftDueAfter
// (modelRequests' doc). So the most calls open at once in the stall's last minute is no more than
// in its first, and every fresh request goes at its window or result, within requestWindow of its
// end. On a bridge whose flush sends a chunk of put-back requests while another is on its way, the
// open calls grow with the backlog. A call's timeout and a window never fall in one tick, so the
// calls a flush makes are the reports on their way it adds, and the calls open at once are the
// reports on their way just after it.
func TestModelRequestsOpenCallsStayBoundedThroughALongStall(t *testing.T) {
	t.Parallel()
	stalled := make(chan struct{})
	r := newModelRequestRig(t, func(http.ResponseWriter, []string, int) { <-stalled })
	t.Cleanup(func() { close(stalled) })
	r.reporter.clock = timeoutClock{r.clock}
	const tick, resultEvery, stall, span = 200 * time.Millisecond, 30 * time.Second, 3 * time.Minute, time.Minute
	start := r.clock.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }
	posting := func() int {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return r.reporter.posting
	}
	// Each flush that sent: when, and how many calls it made.
	type flush struct {
		at    time.Time
		calls int
	}
	var flushes []flush
	ended, sent := map[string]time.Time{}, map[string]time.Time{}
	// backlog is how many requests had gone once by the stall's end: each is taken, pending or on
	// its way again, since the daemon keeps none.
	mostEarly, mostLate, backlog := 0, 0, -1
	// flushed reads the made reports a flush put on its way while before others were on theirs, and
	// counts the calls open at once after it.
	flushed := func(before, made int) {
		now := r.clock.Now()
		flushes = append(flushes, flush{now, made})
		for range made {
			for _, request := range testwait.Receive(t, "a report of the flush", r.posts) {
				if _, ok := sent[request.ID]; !ok {
					sent[request.ID] = now
				}
			}
		}
		switch open := before + made; {
		case now.Before(at(span)):
			mostEarly = max(mostEarly, open)
		case !now.Before(at(stall-span)) && now.Before(at(stall)):
			mostLate = max(mostLate, open)
		}
	}
	for n := 0; r.clock.Now().Before(at(stall + requestWindow)); n++ {
		now := r.clock.Now()
		if now.Before(at(stall)) {
			id := fmt.Sprintf("req-%05d", n)
			r.assistant(id, now)
			ended[id] = now
			if now.Sub(start)%resultEvery == 0 {
				before := posting()
				r.result()
				if made := posting() - before; made != 0 {
					flushed(before, made)
				}
			}
		} else if backlog < 0 {
			backlog = len(sent)
		}
		before, ending := posting(), 0
		for _, f := range flushes {
			if timeout := f.at.Add(requestReportTimeout); timeout.After(now) && !timeout.After(now.Add(tick)) {
				ending += f.calls
			}
		}
		acted := r.advance(t, tick)
		if ending != 0 {
			if acted {
				t.Fatalf("test setup: a window at +%s fell in the tick of a call's timeout", r.clock.Now().Sub(start))
			}
			testwait.Eventually(t, "the calls whose timeout passed to end", func() bool { return posting() == before-ending })
			continue
		}
		if made := posting() - before; acted && made != 0 {
			flushed(before, made)
		}
	}
	t.Logf("most calls open at once: %d in the stall's first %s, %d in its last; %d requests taken by its end", mostEarly, span, mostLate, backlog)
	if backlog <= 2*claim.MaxModelRequests {
		t.Fatalf("by the stall's end %d requests had gone once, want more than %d: the taken backlog never passed several chunks", backlog, 2*claim.MaxModelRequests)
	}
	if mostLate > mostEarly {
		t.Errorf("%d calls were open at once in the stall's last %s, more than the %d in its first: the open calls grow with the backlog", mostLate, span, mostEarly)
	}
	for id, end := range ended {
		if went, ok := sent[id]; !ok || went.Sub(end) > requestWindow {
			t.Errorf("%s, ended at +%s, first went at +%s (%t): not at its window or result, within requestWindow of its end", id, end.Sub(start), went.Sub(start), ok)
		}
	}
}

// A chunk of put-back requests does not leave while another chunk of them is on its way, and the
// put-back requests that wait for it make the window due for nothing meanwhile; once that chunk's
// call ends, a chunk of put-back requests whose window fell due meanwhile goes at once, sooner than
// the window armed for a later request (modelRequests' doc). A chunk's call ends by its timeout as
// a rule, before the window is due for the others: here the rig's clock fires no timeout, and the
// stand-in holds the answer to the first chunk until the row lets it go, 29 s after it left, past
// the window of the request whose report failed beside it.
func TestModelRequestsTakenChunkWaitsForTheOneOnItsWay(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	var mu sync.Mutex
	seen := map[string]bool{}
	// A request's first report fails; a report of a request that went before waits for release.
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		mu.Lock()
		again := seen[ids[0]]
		for _, id := range ids {
			seen[id] = true
		}
		mu.Unlock()
		if !again {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-release
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	t.Cleanup(free)
	start := r.clock.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }
	state := func() (posting int, windows uint64) {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return r.reporter.posting, r.reporter.windows
	}
	reports := func(n int) [][]string {
		var got [][]string
		for range n {
			var ids []string
			for _, request := range testwait.Receive(t, "a report", r.posts) {
				ids = append(ids, request.ID)
			}
			got = append(got, ids)
		}
		slices.SortFunc(got, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
		return got
	}

	// a's report fails at the result at 0 s, and a is put back.
	r.assistant("a", start)
	r.result()
	r.expectBatch(t, []string{"a"})
	r.awaitAnswer(t)
	// At 1 s a result sends b in a report of its own, which fails, and a again, in a chunk of
	// put-back requests the stand-in holds: b, put back, is due at +16 s, and waits for that chunk.
	r.moveTo(t, at(time.Second))
	r.assistant("b", at(time.Second))
	r.result()
	if got, want := reports(2), [][]string{{"a"}, {"b"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("the result at +1s sent %q, want %q", got, want)
	}
	testwait.Eventually(t, "b's report to fail while a's chunk is held", func() bool {
		posting, _ := state()
		return posting == 1
	})
	_, windowsBefore := state()
	// c, read at 25 s, arms the window for its own deadline, +39.75 s. Through 30 s no window comes
	// and nothing goes: the window c armed is the only one armed in the wait.
	r.moveTo(t, at(25*time.Second))
	r.assistant("c", at(25*time.Second))
	r.moveTo(t, at(30*time.Second))
	r.noBatch(t)
	if _, windows := state(); windows-windowsBefore != 1 {
		t.Fatalf("in the wait the bridge armed %d windows, want the 1 c armed", windows-windowsBefore)
	}
	// The daemon answers a's chunk at 30 s: the window is armed again at once, for b, and b's chunk
	// goes then, beside c's report.
	free()
	r.awaitAnswer(t)
	r.fireWindow(t, at(30*time.Second))
	if got, want := reports(2), [][]string{{"b"}, {"c"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("the window at +30s sent %q, want %q", got, want)
	}
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

// A chunk aborted at its timeout (requestReportTimeout) is put back due requestWindow after its
// call ended, not after the flush that sent it (modelRequests' doc). The stand-in takes the first
// report's connection and never answers, and the rig's clock ends that call at its timeout, 10 s
// after the result sent it: no window comes before that end plus requestWindow, 25 s after the
// result, and the request goes again then.
func TestModelRequestsAChunkAbortedAtItsTimeoutIsDueAWindowAfterItsCallEnded(t *testing.T) {
	t.Parallel()
	stalled := make(chan struct{})
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			<-stalled
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	t.Cleanup(func() { close(stalled) })
	r.reporter.clock = timeoutClock{r.clock}
	start := r.clock.Now()
	r.assistant("a", start)
	r.result()
	r.expectBatch(t, []string{"a"})
	ended := start.Add(requestReportTimeout)
	r.moveTo(t, ended)
	r.awaitAnswer(t)
	due := ended.Add(requestWindow)
	r.moveTo(t, due.Add(-time.Millisecond))
	r.noBatch(t)
	r.fireWindow(t, due)
	r.expectBatch(t, []string{"a"})
	r.awaitAnswer(t)
}

// A report answered with a 408 or a 429 failed, from the daemon or a proxy and whatever its body,
// and its request is put back, due requestWindow after the call; any other 4xx, the daemon's
// refusal or a proxy's page, drops it with a line (modelRequests' doc).
func TestModelRequestsPutsBackA408OrA429AndDropsAnyOther4xx(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		status int
		body   string
		again  bool
	}{
		{"the daemon's 408", http.StatusRequestTimeout, `{"error":"the daemon gave up on the body"}`, true},
		{"a proxy's 408", http.StatusRequestTimeout, "<html><body><h1>408 Request Timeout</h1></body></html>", true},
		{"the daemon's 429", http.StatusTooManyRequests, `{"error":"too many reports"}`, true},
		{"a proxy's 429", http.StatusTooManyRequests, "<html><body><h1>429 Too Many Requests</h1></body></html>", true},
		{"the daemon's 409", http.StatusConflict, `{"error":"the claim's launch generation 4 is not its current one"}`, false},
		{"a proxy's 403", http.StatusForbidden, "<html><body><h1>403 Forbidden</h1></body></html>", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
				if attempt == 1 {
					w.WriteHeader(row.status)
					_, _ = io.WriteString(w, row.body)
					return
				}
				_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
			})
			r.assistant("a", r.clock.Now())
			r.result()
			r.expectBatch(t, []string{"a"})
			r.awaitAnswer(t)
			if row.again {
				r.fireWindow(t, r.clock.Now().Add(requestWindow))
				r.expectBatch(t, []string{"a"})
				return
			}
			r.result()
			r.noBatch(t)
			if !strings.Contains(r.logged.String(), "they go unreported") {
				t.Fatalf("the bridge logged %q, want the report dropped with a line", r.logged)
			}
		})
	}
}

// A call that fails before it leaves failed, and its request is put back (modelRequests' doc):
// the bridge cannot build the call, as a daemon URL Go's parser refuses makes it, or cannot encode
// the report, as an end past the year 9999 makes it. The request is pending again, put back, due
// requestWindow after the call; once the bridge can build the call again, it goes then.
func TestModelRequestsPutsBackACallThatFailsBeforeItLeaves(t *testing.T) {
	t.Parallel()
	putBack := func(t *testing.T, r *modelRequestRig, due time.Time) {
		t.Helper()
		r.reporter.mu.Lock()
		pending, armed, at := slices.Clone(r.reporter.pending), r.reporter.window != 0, r.reporter.windowDue
		r.reporter.mu.Unlock()
		if len(pending) != 1 || pending[0].ID != "a" || !pending[0].taken || !armed || !at.Equal(due) {
			t.Fatalf("the bridge holds %+v, its window armed (%t) for %s, want a put back and the window armed for %s",
				pending, armed, at.Format(time.RFC3339Nano), due.Format(time.RFC3339Nano))
		}
		if !strings.Contains(r.logged.String(), "they go again") {
			t.Fatalf("the bridge logged %q, want the report put back with a line", r.logged)
		}
	}
	t.Run("the call cannot be built", func(t *testing.T) {
		t.Parallel()
		r := newModelRequestRig(t, nil)
		url := r.reporter.url
		r.reporter.url = url + "\x7f"
		start := r.clock.Now()
		r.assistant("a", start)
		r.result()
		r.awaitAnswer(t)
		r.noBatch(t)
		due := start.Add(requestWindow)
		putBack(t, r, due)
		r.reporter.url = url
		r.moveTo(t, due.Add(-time.Millisecond))
		r.fireWindow(t, due)
		r.expectBatch(t, []string{"a"})
	})
	t.Run("the report cannot be encoded", func(t *testing.T) {
		t.Parallel()
		r := newModelRequestRig(t, nil)
		far := time.Date(10_000, time.January, 1, 0, 0, 0, 0, time.UTC)
		r.clock.set(far)
		r.assistant("a", far)
		r.result()
		r.awaitAnswer(t)
		r.noBatch(t)
		putBack(t, r, far.Add(requestWindow))
	})
}

// A request the daemon would refuse (claim.ModelRequestProblem) is dropped where the bridge reads
// it, with a line naming its id and the reason, so it never shares a report (modelRequests' doc):
// the three the tester measured on the real bridge, a model of spaces, a model of U+0085 and an id
// of spaces, and an empty model, each a request of the role's own session beside a valid one. The
// valid one goes alone at its window; sent with them, the daemon's 400 would drop it too.
func TestModelRequestsDropsARequestTheDaemonRefusesWhereItReadsIt(t *testing.T) {
	t.Parallel()
	r := newModelRequestRig(t, nil)
	start := r.clock.Now()
	r.assistant("valid", start)
	for _, refused := range []struct{ id, model, reason string }{
		{"model-of-spaces", "  ", "model is required"},
		{"model-of-nel", "\u0085", "model is required"},
		{"   ", capturedModel, "id is required"},
		{"model-empty", "", "model is required"},
	} {
		r.reporter.read(streamLine{
			kind: "assistant", session: capturedSession, timestamp: start.Format(time.RFC3339Nano),
			message: json.RawMessage(fmt.Sprintf(`{"id":%q,"model":%q,"usage":{"input_tokens":1}}`, refused.id, refused.model)),
		})
		if want := fmt.Sprintf("dropped model request %q: the daemon refuses it: %s", refused.id, refused.reason); !strings.Contains(r.logged.String(), want) {
			t.Errorf("the bridge logged %q, want the line %q", r.logged, want)
		}
	}
	r.fireWindow(t, start.Add(requestDueAfter))
	r.expectBatch(t, []string{"valid"})
	r.awaitAnswer(t)
}

// exit makes the reporter's send at Claude Code's end (modelRequests.exit) and answers a channel
// closed once it returns.
func (r *modelRequestRig) exit() <-chan struct{} {
	exited := make(chan struct{})
	go func() {
		r.reporter.exit()
		close(exited)
	}()
	return exited
}

// stillExiting holds that the exit has not returned within a short while, since what is open
// keeps it.
func stillExiting(t *testing.T, exited <-chan struct{}, open string) {
	t.Helper()
	select {
	case <-exited:
		t.Fatalf("the exit returned while %s", open)
	case <-time.After(300 * time.Millisecond):
	}
}

// A request whose report failed at a turn's result is put back, due 15 s after that call, and
// Claude Code ends 5 s later: the exit sends it at once, and returns only once that call has ended
// (modelRequests' doc). The stand-in fails the turn's call and holds the exit's until the row lets
// it go.
func TestModelRequestsExitSendsAPutBackRequestAndWaitsForItsCall(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-release
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	t.Cleanup(free)
	start := r.clock.Now()
	r.assistant("a", start)
	r.result()
	r.expectBatch(t, []string{"a"})
	r.awaitAnswer(t)
	r.moveTo(t, start.Add(5*time.Second))
	exited := r.exit()
	select {
	case <-exited:
		t.Fatal("the exit returned and sent no report, want a sent at once")
	case batch := <-r.posts:
		if len(batch) != 1 || batch[0].ID != "a" {
			t.Fatalf("the exit sent %+v, want a", batch)
		}
	case <-time.After(deadlineWait(t)):
		t.Fatalf("the exit sent no report, %s before the test binary's deadline", deadlineMargin)
	}
	stillExiting(t, exited, "its call was on its way")
	free()
	testwait.Receive(t, "the exit to return once its call ended", exited)
}

// A report a turn's result sent at +0 s is still on its way when Claude Code ends at +4 s with
// nothing pending, against a daemon that takes each report's connection and never answers: the
// exit sends nothing, and returns only once that call ends at its timeout (requestReportTimeout),
// at +10 s on the rig's clock, within requestReportTimeout of the exit. What that call put back is
// never sent (modelRequests' doc).
func TestModelRequestsExitWaitsForAReportOnItsWay(t *testing.T) {
	t.Parallel()
	stalled := make(chan struct{})
	r := newModelRequestRig(t, func(http.ResponseWriter, []string, int) { <-stalled })
	t.Cleanup(func() { close(stalled) })
	r.reporter.clock = timeoutClock{r.clock}
	start := r.clock.Now()
	r.assistant("a", start)
	r.result()
	r.expectBatch(t, []string{"a"})
	r.moveTo(t, start.Add(4*time.Second))
	exited := r.exit()
	stillExiting(t, exited, "the result's report was on its way")
	r.noBatch(t)
	timeout := start.Add(requestReportTimeout)
	r.moveTo(t, timeout.Add(-time.Millisecond))
	stillExiting(t, exited, "the result's report was on its way")
	r.moveTo(t, timeout)
	testwait.Receive(t, "the exit to return once the result's report ended at its timeout", exited)
	r.moveTo(t, timeout.Add(requestWindow+requestLeftDueAfter))
	r.noBatch(t)
}

// Claude Code ends at +20 s while the chunk of put-back requests a window sent at +15 s is on its
// way, with more than a chunk of put-back requests behind it and a fresh request pending, against
// a daemon that fails the turn's send and then takes each report's connection and never answers.
// The exit sends every pending request at once, whatever is on its way, the fresh one and the
// put-back ones each in chunks of at most claim.MaxModelRequests, and returns once every open call
// has ended at its timeout (requestReportTimeout) on the rig's clock, the chunk on its way at +25 s
// and the exit's own at +30 s; nothing is sent after it (modelRequests' doc). An exit whose calls
// waited for one another sends one report and fails the row on the test binary's deadline.
func TestModelRequestsExitSendsEveryPendingRequestAtOnceAndReturnsByItsTimeout(t *testing.T) {
	t.Parallel()
	stalled := make(chan struct{})
	r := newModelRequestRig(t, func(w http.ResponseWriter, _ []string, attempt int) {
		// The turn's three chunks fail; no later report is answered.
		if attempt <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-stalled
	})
	t.Cleanup(func() { close(stalled) })
	r.reporter.clock = timeoutClock{r.clock}
	start := r.clock.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }
	// Each request ends a millisecond after the one before, so the oldest are the first.
	ids := make([]string, 2*claim.MaxModelRequests+50)
	for i := range ids {
		ids[i] = fmt.Sprintf("req-%03d", i)
		r.assistant(ids[i], at(time.Duration(i)*time.Millisecond))
	}
	r.result()
	r.expectChunks(t, ids)
	r.awaitAnswer(t)
	// The window 15 s after those calls ended sends their oldest chunk, which the daemon holds.
	r.fireWindow(t, at(requestWindow))
	r.expectBatch(t, ids[:claim.MaxModelRequests])
	end := at(20 * time.Second)
	r.moveTo(t, end)
	r.assistant("fresh", end)
	exited := r.exit()
	// The fresh request's report, and two of the 250 put-back requests behind the chunk on its way.
	const reports = 3
	var got []string
	for n := range reports {
		select {
		case <-exited:
			t.Fatalf("the exit returned after %d reports, want it to send %d and wait for their calls", n, reports)
		case batch := <-r.posts:
			if len(batch) > claim.MaxModelRequests {
				t.Fatalf("a report has %d IDs, want at most %d", len(batch), claim.MaxModelRequests)
			}
			if len(batch) > 1 && slices.ContainsFunc(batch, func(request claim.ModelRequest) bool { return request.ID == "fresh" }) {
				t.Fatalf("a report has the fresh request among %d put-back ones, want it in a report of its own", len(batch)-1)
			}
			for _, request := range batch {
				got = append(got, request.ID)
			}
		case <-time.After(deadlineWait(t)):
			t.Fatalf("the exit sent %d of %d reports, %s before the test binary's deadline", n, reports, deadlineMargin)
		}
	}
	want := append([]string{"fresh"}, ids[claim.MaxModelRequests:]...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the exit's reports have %d IDs, want %d; got %q, want %q", len(got), len(want), got, want)
	}
	// The chunk on its way ends at +25 s and the exit's calls at their timeout, 10 s after it: the
	// exit returns then and no sooner.
	r.moveTo(t, end.Add(requestReportTimeout-time.Millisecond))
	stillExiting(t, exited, "its calls were on their way")
	r.moveTo(t, end.Add(requestReportTimeout))
	testwait.Receive(t, "the exit to return once its calls ended at their timeout", exited)
	// What those calls put back is never sent: no window comes after the exit.
	r.moveTo(t, end.Add(requestReportTimeout+requestLeftDueAfter))
	r.noBatch(t)
}
