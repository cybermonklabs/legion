package claudebridge

// The bridge's take of its claim's task (take.go), against a stand-in of the daemon's claim routes:
// each value handed over is reported seen, and taken once per revision while Claude Code is idle,
// the ready line gates the take, a finished number is owed nothing, and a task's turn is reported
// naming it. What is sent again until the daemon answers is take_resend_test.go's, and a take's
// text held through a turn the bridge did not start is take_held_test.go's.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testEpoch is the task bucket's epoch the cases' values are written at: its stream's creation
// time, RFC 3339 with nanoseconds. testTask is the number of the task the cases' claim is owed.
const (
	testEpoch = "2026-11-13T16:21:44.123456789Z"
	testTask  = 7
)

// quickCalls are the waits the cases of departure 7 send again with: an attempt with no answer after
// 200 ms, and waits from 20 ms doubling to 80 ms, so a case sees several attempts within a second.
var quickCalls = daemonCalls{answerTimeout: 200 * time.Millisecond, resendFirst: 20 * time.Millisecond, resendMax: 80 * time.Millisecond}

// taskDaemon stands in for the daemon's claim routes a bridge calls while it takes its claim's
// task (take.go): it records each seen report and take it answers, answers each as the case says,
// and records each turn report it answered 200, which a case reads by its sequence (reportOf).
// While fault is set it fails every seen report and take that reaches it with fault instead: the
// failures of departure 7 a daemon's side makes. Each turn-outcome attempt is recorded, and
// answered by the first of reportAnswers the case queued, if any, in place of the 200.
type taskDaemon struct {
	t      *testing.T
	server *httptest.Server
	// answerSeen and answerTake answer a seen report and a take that is not failed: 204, and an
	// accepted take of "say the task", unless the case sets its own before the stand-in starts.
	answerSeen func(claim.TaskSeenRequest) (int, any)
	answerTake func(claim.TakeRequest) (int, any)

	mu             sync.Mutex
	fault          func(http.ResponseWriter, *http.Request)
	seen           []claim.TaskSeenRequest
	takes          []claim.TakeRequest
	reports        []claim.TurnOutcomeRequest
	reportAttempts []claim.TurnOutcomeRequest
	reportAnswers  []func(http.ResponseWriter, *http.Request)
}

// newTaskDaemon is a stand-in not yet serving: newTakingRig starts it, or a case starts it itself.
func newTaskDaemon(t *testing.T) *taskDaemon {
	t.Helper()
	d := &taskDaemon{t: t,
		answerSeen: func(claim.TaskSeenRequest) (int, any) { return http.StatusNoContent, nil },
		answerTake: func(claim.TakeRequest) (int, any) { return accepted("say the task") },
	}
	d.server = httptest.NewUnstartedServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

// accepted is an accepted take's answer, whose turn runs text.
func accepted(text string) (int, any) {
	return http.StatusOK, claim.TakeResponse{Outcome: takeAccepted, Text: text}
}

// startAt starts the stand-in serving at addr, an address nothing listened on until now.
func (d *taskDaemon) startAt(addr string) {
	d.t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		d.t.Fatalf("listen on %s: %v", addr, err)
	}
	_ = d.server.Listener.Close()
	d.server.Listener = listener
	d.server.Start()
}

func (d *taskDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Errorf("read the bridge's %s: %v", r.URL.Path, err)
		return
	}
	switch r.URL.Path {
	case turnOutcomePath:
		var report claim.TurnOutcomeRequest
		if !d.decoded(body, &report) {
			return
		}
		d.mu.Lock()
		d.reportAttempts = append(d.reportAttempts, report)
		var answer func(http.ResponseWriter, *http.Request)
		if len(d.reportAnswers) > 0 {
			answer, d.reportAnswers = d.reportAnswers[0], d.reportAnswers[1:]
		}
		d.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		d.mu.Lock()
		d.reports = append(d.reports, report)
		d.mu.Unlock()
		writeAnswer(w, http.StatusOK, claim.TurnOutcomeResponse{})
	case taskSeenPath:
		var seen claim.TaskSeenRequest
		if d.decoded(body, &seen) && !d.failed(w, r) {
			d.mu.Lock()
			d.seen = append(d.seen, seen)
			d.mu.Unlock()
			status, out := d.answerSeen(seen)
			writeAnswer(w, status, out)
		}
	case takePath:
		var take claim.TakeRequest
		if d.decoded(body, &take) && !d.failed(w, r) {
			d.mu.Lock()
			d.takes = append(d.takes, take)
			d.mu.Unlock()
			status, out := d.answerTake(take)
			writeAnswer(w, status, out)
		}
	default:
		d.t.Errorf("the bridge posted to %s, which is no route it calls", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *taskDaemon) decoded(body []byte, into any) bool {
	if err := json.Unmarshal(body, into); err != nil {
		d.t.Errorf("decode the bridge's request %s: %v", body, err)
		return false
	}
	return true
}

// failed fails the request with the fault set, if one is, and says whether it did.
func (d *taskDaemon) failed(w http.ResponseWriter, r *http.Request) bool {
	d.mu.Lock()
	fault := d.fault
	d.mu.Unlock()
	if fault == nil {
		return false
	}
	fault(w, r)
	return true
}

// writeAnswer answers status with body as JSON, or with no body for nil.
func writeAnswer(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (d *taskDaemon) setFault(fault func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fault = fault
}

// seenReports and takesMade are the seen reports and the takes that reached the stand-in and
// were not failed, in the order they came.
func (d *taskDaemon) seenReports() []claim.TaskSeenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

func (d *taskDaemon) takesMade() []claim.TakeRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.takes)
}

// takesAt is how many takes of the revision reached the stand-in.
func (d *taskDaemon) takesAt(revision uint64) int {
	n := 0
	for _, take := range d.takesMade() {
		if take.Revision == revision {
			n++
		}
	}
	return n
}

// answerReports queues the answers of the next turn-outcome attempts, in the order they come; an
// attempt with none queued is answered 200 and handed to the case (reported).
func (d *taskDaemon) answerReports(answers ...func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reportAnswers = append(d.reportAnswers, answers...)
}

// reportsAttempted is every turn-outcome attempt that reached the stand-in, however it was
// answered, in the order they came.
func (d *taskDaemon) reportsAttempted() []claim.TurnOutcomeRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reportAttempts)
}

// hijack takes the connection of the request w answers, for a case that ends it as a daemon that
// is not serving does.
func hijack(t *testing.T, w http.ResponseWriter) *net.TCPConn {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("take the request's connection: %v", err)
		return nil
	}
	return conn.(*net.TCPConn)
}

// resetConnection answers a request with a reset connection, its answer lost.
func resetConnection(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if conn := hijack(t, w); conn != nil {
			_ = conn.SetLinger(0)
			_ = conn.Close()
		}
	}
}

// timedOutConn is a connection whose far side stopped answering: what is written to it goes
// nowhere, and every read meets the kernel's timeout of the connection (ETIMEDOUT), as net wraps
// it. The embedded end of a pipe answers the rest of net.Conn.
type timedOutConn struct{ net.Conn }

func (timedOutConn) Write(b []byte) (int, error) { return len(b), nil }

func (timedOutConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
}

// reportOf is the report of the bridge's turn sequence, which the stand-in answered 200, once it
// has come, within 10 s. A report is read by its sequence, as the daemon keys it, and not by the
// order reports arrive in: the bridge sends each from a goroutine of its own, on a connection of
// its own (turnoutcome.go), so the reports of two turns that end close together, such as a turn's
// and that of the held task's turn it starts at once, can reach the stand-in either way round.
func (d *taskDaemon) reportOf(sequence uint64) claim.TurnOutcomeRequest {
	d.t.Helper()
	var report claim.TurnOutcomeRequest
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, r := range d.reports {
			if r.Sequence == sequence {
				report = r
				return true
			}
		}
		return false
	}) {
		d.t.Fatalf("the bridge reported no turn %d within 10s", sequence)
	}
	return report
}

// seenReportWait bounds waitForSeen: 10 s, a bound of its own rather than what is left of the test
// binary's deadline (deadlineWait), so a seen report that never comes fails its own row by name and
// leaves the package's other rows their time.
const seenReportWait = 10 * time.Second

// waitForSeen waits for the seen report of the value naming number at revision, at testEpoch, the
// epoch every value the cases hand is written at (handReading), to reach d within seenReportWait,
// or what is left of the test binary's deadline less deadlineMargin where that is sooner, and fails
// the case at once, naming every seen report d recorded, when it has not come by then. It waits for
// that revision's report and not for a count of reports: the bridge sends each seen report on a
// goroutine of its own (reportSeen), the take of a value is a post apart from it, and a newer value
// stops the seen report of an older one still being sent (replace). So the take of revision 1 can
// be answered, and Claude Code start the task's turn, before revision 1's report reaches the
// daemon; revision 2 then stops that report, and only revision 2's comes. T12 of
// dispatch://INFRA-2811's plan v168 allows that order, since only the newest revision's seen report
// matters: a count of two reports never comes in it, and the report of the revision the case
// handed last does.
func (r *rig) waitForSeen(d *taskDaemon, number, revision uint64) {
	r.t.Helper()
	bound := testwait.Bounded(r.t, seenReportWait, deadlineMargin)
	if testwait.Holds(bound, 5*time.Millisecond, func() bool {
		return slices.ContainsFunc(d.seenReports(), func(seen claim.TaskSeenRequest) bool {
			return seen.Number == number && seen.Epoch == testEpoch && seen.Revision == revision
		})
	}) {
		return
	}
	var recorded []string
	for _, seen := range d.seenReports() {
		recorded = append(recorded, fmt.Sprintf("%d at %d of epoch %s", seen.Number, seen.Revision, seen.Epoch))
	}
	r.t.Fatalf("the seen report of task %d at revision %d did not reach the stand-in within %s: it recorded seen reports at %q; the bridge logged:\n%s",
		number, revision, bound, recorded, r.log)
}

// newTakingRig runs a resumed session's bridge whose pane names d at url — d's own server,
// started here, when url is "" — and which sends its seen reports and takes with calls. It is a
// plain *rig: the stand-in daemon is the d the caller already holds, so the rig keeps no second
// handle to it.
func newTakingRig(t *testing.T, d *taskDaemon, url string, calls daemonCalls) *rig {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if url == "" {
		d.server.Start()
		url = d.server.URL
	}
	r := newRigWithEnv(t, []string{
		"LEGION_DAEMON_URL=" + url,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, calls, "-p", "--resume", sessionID)
	return r
}

// handLine writes line to the inbox as claude-envoy writes its lines (src/legion-inbox.ts), down a
// connection of its own, closed once the inbox has answered, and is the inbox's answer to it.
func (r *rig) handLine(line InboxDelivery) InboxAnswer {
	r.t.Helper()
	inbox := r.dialInbox()
	defer func() { _ = inbox.conn.Close() }()
	if err := json.NewEncoder(inbox.conn).Encode(line); err != nil {
		r.t.Fatal(err)
	}
	return inbox.answer()
}

// handTask hands the bridge the claim's task value as claude-envoy writes a relayed one to the
// inbox: one task line, finished 0 since a relay frame carries no reading, which the bridge
// answers once its loop has taken the value. Number 0 is a deleted value.
func (r *rig) handTask(number, revision uint64) {
	r.t.Helper()
	r.handReading(number, revision, 0)
}

// handReading hands the bridge the value a reading of the daemon's carried — the registration's,
// or claims/task's — with the number that reading says the claim finished.
func (r *rig) handReading(number, revision, finished uint64) {
	r.t.Helper()
	id := fmt.Sprintf("task-%d-at-%d", number, revision)
	answer := r.handLine(InboxDelivery{ID: id, Kind: InboxTask, Number: number, Epoch: testEpoch, Revision: revision, Finished: &finished})
	if answer != (InboxAnswer{ID: id, OK: true}) {
		r.t.Fatalf("the inbox answered the task line %+v", answer)
	}
}

// handFinished hands the bridge a reading of the daemon's that held no value, as claude-envoy
// writes one to the inbox: one finished line naming the number the reading says the claim
// finished.
func (r *rig) handFinished(finished uint64) {
	r.t.Helper()
	id := fmt.Sprintf("finished-%d", finished)
	if answer := r.handLine(InboxDelivery{ID: id, Kind: InboxFinished, Finished: &finished}); answer != (InboxAnswer{ID: id, OK: true}) {
		r.t.Fatalf("the inbox answered the finished line %+v", answer)
	}
}

// handReady writes the ready line, as claude-envoy does once the claim's ready has answered, and
// returns once the bridge's loop has taken it.
func (r *rig) handReady() {
	r.t.Helper()
	if answer := r.handLine(InboxDelivery{ID: "ready", Kind: InboxReady}); answer != (InboxAnswer{ID: "ready", OK: true}) {
		r.t.Fatalf("the inbox answered the ready line %+v", answer)
	}
}

// userTexts is the text of every user message Claude Code read, in the order it read them.
func (r *rig) userTexts() []string {
	var texts []string
	for _, line := range strings.Split(r.recorded(), "\n") {
		body, ok := strings.CutPrefix(line, "in ")
		if !ok {
			continue
		}
		var input struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(body), &input) == nil && len(input.Message.Content) > 0 {
			texts = append(texts, input.Message.Content[0].Text)
		}
	}
	return texts
}

// pairsOf names the value each of the bridge's seen reports or takes named, as "<number> at
// <revision>", failing on one that does not name this launch's claim in the boot form or names
// another epoch.
func pairsOf[R claim.TaskSeenRequest | claim.TakeRequest](t *testing.T, requests []R) []string {
	t.Helper()
	var pairs []string
	for _, request := range requests {
		value := claim.TakeRequest(request)
		if value.BootToken != testBootToken || value.AgentCaller != (claim.AgentCaller{}) || value.Generation != 4 || value.Epoch != testEpoch {
			t.Fatalf("the bridge sent %+v, want the boot form of generation 4 at epoch %s", value, testEpoch)
		}
		pairs = append(pairs, fmt.Sprintf("%d at %d", value.Number, value.Revision))
	}
	return pairs
}

// Each value the bridge is handed is reported seen, and its task asked for once Claude Code is
// idle, once per revision: the same value relayed again — by the registration's answer and by its
// relayed frame, or after a hold refused it — asks nothing, an older revision asks nothing, and only
// the value the hold's end writes, at its next revision, is asked about again. The accepted take
// runs as a turn whose report names the task.
func TestATaskLineIsReportedSeenAndTakenOncePerRevision(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			return http.StatusOK, claim.TakeResponse{Outcome: takeRefused, Hold: "in-turn"}
		}
		return accepted("say the task")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.logged("the daemon refused the take of task 7 (hold in-turn)")
	r.handTask(testTask, 1)
	r.quiet()
	r.waitForSeen(d, testTask, 1)

	r.handTask(testTask, 2)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean || report.ProviderError != nil {
		t.Fatalf("the task's turn was reported %+v, want a clean turn of task %d", report, testTask)
	}
	r.handTask(testTask, 2)
	r.handTask(testTask, 1)
	r.quiet()

	want := []string{"7 at 1", "7 at 2"}
	if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, want) {
		t.Errorf("the bridge reported seen %q, want %q", seen, want)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, want) {
		t.Errorf("the bridge asked to take %q, want %q", takes, want)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the accepted take's text once", texts)
	}
}

// An Envoy delivery that arrives while a take of the claim's task waits for its answer starts no
// turn: it waits, and the accepted take's text starts the turn, the delivery following it as the
// turn's next step.
func TestATakenTaskRunsAheadOfAQueuedCard(t *testing.T) {
	d := newTaskDaemon(t)
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-released
		return accepted("say the task")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()
	inbox := r.dialInbox()

	r.handTask(testTask, 1)
	r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })
	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 waits for the take of the claim's task")
	r.quiet()

	release()
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task", "say the card"}) {
		t.Fatalf("Claude Code read %q, want the task's text and then the card", texts)
	}
	if report := d.reportOf(1); report.Task != testTask {
		t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
	}
}

// A value handed over during a turn is reported seen at once, and its task is asked for only once
// that turn has ended; the turn that was running is reported as no task's.
func TestNoTakeWhileARunIsInProgress(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.turn("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.waitForSeen(d, testTask, 1)
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v during a turn", takes)
	}

	r.release("r1")
	r.expect("agent_end", nil)
	if report := d.reportOf(1); report.Task != 0 {
		t.Fatalf("the delivery's turn was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(2); report.Task != testTask {
		t.Fatalf("the task's turn was reported %+v, want the turn of task %d", report, testTask)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
	}
}

// The report of a task's turn the provider failed names the task and how the provider failed it:
// the status Claude Code named, the provider, the model the turn's init line named, and its own
// words. A content filter's refusal names no status (testdata/refusal-turn.jsonl:5). A provider
// failure that ends the turn before Claude Code re-emits the task's text names the task too,
// whether or not the text reached Claude Code: its report takes the task to the daemon's
// provider-error ladder, which gives it again after its wait, rather than counting the refusal
// toward the launch-failure run (result).
func TestTheTurnOutcomeNamesTheTaskAndTheProviderError(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       claim.ProviderError
	}{
		{name: "a provider failure with a status", text: "api-error-529 WAITING on the provider",
			want: claim.ProviderError{Status: 529, Provider: claudeProvider, Model: fakeClaudeModel, Retryable: true}},
		{name: "a content filter's refusal, which names none", text: "refusal WAITING on the provider",
			want: claim.ProviderError{Provider: claudeProvider, Model: fakeClaudeModel}},
		{name: "a provider failure before Claude Code re-emitted the text", text: "unechoed-api-error-529 WAITING on the provider",
			want: claim.ProviderError{Status: 529, Provider: claudeProvider, Model: fakeClaudeModel, Retryable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(claim.TakeRequest) (int, any) { return accepted(tc.text) }
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()
			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			report := d.reportOf(1)
			if report.Task != testTask || report.Outcome != claim.TurnRefused || report.ProviderError == nil {
				t.Fatalf("the task's turn was reported %+v, want task %d refused with its provider error", report, testTask)
			}
			got := *report.ProviderError
			if got.Message != report.Message || !strings.Contains(got.Message, "API Error: 529 overloaded") {
				t.Fatalf("the provider error's message is %q beside the report's %q, want Claude Code's own words in both", got.Message, report.Message)
			}
			got.Message = ""
			if got != tc.want {
				t.Fatalf("the provider error is %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The daemon refuses a take before the claim's ready has answered, so the bridge asks for none
// before claude-envoy's ready line, and its ready gate holds every Envoy delivery until that line,
// whatever value it holds (the plan's decision 16): claude-envoy writes the reading of claims/task
// it makes once the ready has answered ahead of the ready line, and the ready decision can write a
// value the registration did not carry.
//
//   - The registration's value, handed before the ready line, is reported seen at once, and an
//     Envoy delivery waits behind it. Once the ready line comes the task is taken, and runs ahead
//     of the delivery, which is that turn's next step.
//   - The registration held no value, and a delivery — the session's own direct message — comes
//     before the reading of the value the ready decision wrote: it starts no turn, and the task the
//     reading names runs first, the delivery its next step.
//   - The read after the ready failed, so no reading line comes, only the ready line: that line
//     opens the gate all the same, and the delivery starts its turn.
func TestNothingIsTakenBeforeTheReadyLine(t *testing.T) {
	// taskRunsAheadOfTheCard takes the ready line and holds that the task is taken once and runs
	// first, the delivery ev1 the next step of its turn.
	taskRunsAheadOfTheCard := func(t *testing.T, d *taskDaemon, r *rig, inbox *inboxClient) {
		t.Helper()
		r.handReady()
		r.expect("agent_start", nil)
		if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
			t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
		}
		r.expect("agent_end", nil)
		if report := d.reportOf(1); report.Task != testTask {
			t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
		}
		if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
			t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
		}
		if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task", "say the card"}) {
			t.Fatalf("Claude Code read %q, want the task's text and then the card", texts)
		}
	}

	t.Run("the registration's value", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handReading(testTask, 1, testTask-1)
		r.waitForSeen(d, testTask, 1)
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the take of the claim's task")
		r.quiet()
		if takes := d.takesMade(); len(takes) != 0 {
			t.Fatalf("the bridge asked to take %+v before the ready line", takes)
		}
		taskRunsAheadOfTheCard(t, d, r, inbox)
	})

	t.Run("a card ahead of the reading of the value the ready decision wrote", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handFinished(testTask - 1)
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the claim's ready line")
		r.quiet()
		r.handReading(testTask, 1, testTask-1)
		r.waitForSeen(d, testTask, 1)
		r.quiet()
		taskRunsAheadOfTheCard(t, d, r, inbox)
	})

	t.Run("a failed read, with no reading line", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handFinished(testTask - 1)
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the claim's ready line")
		r.quiet()
		r.handReady()
		r.expect("agent_start", nil)
		if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
			t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
		}
		r.expect("agent_end", nil)
		if report := d.reportOf(1); report.Task != 0 {
			t.Fatalf("the card's turn was reported %+v, want a turn of no task", report)
		}
		if takes := d.takesMade(); len(takes) != 0 {
			t.Fatalf("the bridge asked to take %+v with no value held", takes)
		}
		if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card"}) {
			t.Fatalf("Claude Code read %q, want the card", texts)
		}
	})
}

// A value naming a task the daemon's reading says the claim finished is owed nothing: it is
// reported seen, as every value is, and not taken, and an Envoy delivery does not wait for it. The
// highest finished number a line carried stands: a later relayed value of the same number, which
// carries none, is not taken either, and a value naming a later task is.
func TestAValueNamingAFinishedNumberIsReportedSeenAndNotTaken(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handReading(testTask, 1, testTask)
	r.waitForSeen(d, testTask, 1)
	r.turn("ev1", "say the card")
	r.logged("Envoy delivery ev1 starts a turn")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.handTask(testTask, 2)
	r.waitForSeen(d, testTask, 2)
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v, a task the claim finished", takes)
	}

	r.handTask(testTask+1, 3)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	// The seen report and the take of a value are sent apart, each until the daemon answers, so the
	// take can land, and its turn run, before the seen report of the same value does.
	r.waitForSeen(d, testTask+1, 3)
	if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, []string{"7 at 1", "7 at 2", "8 at 3"}) {
		t.Errorf("the bridge reported seen %q, want every value", seen)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"8 at 3"}) {
		t.Errorf("the bridge asked to take %q, want task 8 alone", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card", "say the task"}) {
		t.Errorf("Claude Code read %q, want the card and then task 8's text", texts)
	}
}

// When a task's turn ends with no provider error its report finishes the task on the daemon, so
// the bridge holds the number finished from that end: a newer revision of it, written before the
// report lands — the daemon writes the value again from its record where the bucket lost it or
// holds another write, and would answer its take with the same text —, is reported seen and not
// taken. A 2xx keeps the mark, the record having finished the task; a 4xx refusal applied nothing
// and the record still owes the task, so the mark is cleared, and the newer revision is taken and
// runs once.
func TestATaskFinishedAtItsTurnsEndIsNotTakenAgainUntilItsReportIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		answer        any
		takes, texts  []string
		reportedAgain bool
	}{
		{name: "the report recorded", status: http.StatusOK, answer: claim.TurnOutcomeResponse{},
			takes: []string{"7 at 1"}, texts: []string{"say the task"}},
		{name: "the report refused with a 4xx", status: http.StatusConflict,
			answer: map[string]string{"error": "turn outcome: this launch is not the claim's current one"},
			takes:  []string{"7 at 1", "7 at 2"}, texts: []string{"say the task", "say the task"}, reportedAgain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			lands, answered := make(chan struct{}), make(chan struct{})
			land := sync.OnceFunc(func() { close(lands) })
			t.Cleanup(land)
			d.answerReports(func(w http.ResponseWriter, _ *http.Request) {
				defer close(answered)
				<-lands
				writeAnswer(w, tc.status, tc.answer)
			})
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()

			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			r.waitFor("the task's report to reach the daemon", func() bool { return len(d.reportsAttempted()) == 1 })
			r.handTask(testTask, 2)
			r.waitForSeen(d, testTask, 2)
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
				t.Fatalf("before the report landed the bridge asked to take %q, want task 7 once", takes)
			}

			land()
			<-answered
			if tc.reportedAgain {
				r.logged("the daemon refused the outcome of turn 1 (clean): 409")
				r.expect("agent_start", nil)
				r.untilTurnEnds()
				if report := d.reportOf(2); report.Task != testTask {
					t.Fatalf("the turn was reported %+v, want the second turn, of task %d", report, testTask)
				}
			}
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, tc.takes) {
				t.Errorf("the bridge asked to take %q, want %q", takes, tc.takes)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Errorf("Claude Code read %q, want %q", texts, tc.texts)
			}
			// Revision 2 stops the seen report of revision 1 where it has not reached the daemon yet
			// (replace), and the case waited for revision 2's alone: revision 1's may be missing, or,
			// stopped in flight on a connection of its own, land behind revision 2's.
			allowed := [][]string{{"7 at 1", "7 at 2"}, {"7 at 2"}, {"7 at 2", "7 at 1"}}
			if seen := pairsOf(t, d.seenReports()); !slices.ContainsFunc(allowed, func(want []string) bool { return slices.Equal(seen, want) }) {
				t.Errorf("the bridge reported seen %q, want task 7 at revision 2, and at revision 1 no more than once", seen)
			}
		})
	}
}

// A task line of the shape a claude-envoy older than this bridge wrote — no finished number, which
// that claude-envoy held itself — is refused naming why, and nothing is reported seen or taken
// from it: the bridge would otherwise weigh the value with half of what it needs.
func TestAnOlderShapeTaskLineIsRefusedNamingWhy(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	answer := r.handLine(InboxDelivery{ID: "older", Kind: InboxTask, Number: testTask, Epoch: testEpoch, Revision: 1})
	want := InboxAnswer{ID: "older", Error: "an inbox task line is one JSON object with an id, a number, an epoch, a revision and " +
		"the claim's finished number: this one names no finished number, as a claude-envoy older than this bridge writes it, so no task is taken from it"}
	if answer != want {
		t.Fatalf("the inbox answered the older task line %+v, want %+v", answer, want)
	}
	r.quiet()
	if seen, takes := len(d.seenReports()), len(d.takesMade()); seen != 0 || takes != 0 {
		t.Fatalf("the bridge sent %d seen reports and %d takes for a line it refused, want none", seen, takes)
	}
}

// A reading of the daemon's that holds no value still says which task the claim finished, and
// the bridge holds that number beside the ones its task lines carried, as Oh My Pi's take raises
// its own from such a reading: a value it already holds that names that task is owed nothing from
// then on, so the delivery waiting behind it waits only for the ready gate, and the ready line
// takes nothing and lets it go. A value naming a later task is taken. A finished line that names
// no number is refused naming why.
func TestAFinishedLineOfAReadingWithNoValueIsHeldBesideTheTaskLines(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	inbox := r.dialInbox()

	if answer, want := r.handLine(InboxDelivery{ID: "no-number", Kind: InboxFinished}),
		(InboxAnswer{ID: "no-number", Error: "an inbox finished line is one JSON object with an id and the claim's finished number"}); answer != want {
		t.Fatalf("the inbox answered a finished line with no number %+v, want %+v", answer, want)
	}
	r.handTask(testTask, 1)
	r.waitForSeen(d, testTask, 1)
	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 waits for the take of the claim's task")

	r.handFinished(testTask)
	r.quiet()
	r.handReady()
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v, a task the claim finished", takes)
	}

	r.handTask(testTask+1, 2)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"8 at 2"}) {
		t.Errorf("the bridge asked to take %q, want task 8 alone", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card", "say the task"}) {
		t.Errorf("Claude Code read %q, want the card and then task 8's text", texts)
	}
}
