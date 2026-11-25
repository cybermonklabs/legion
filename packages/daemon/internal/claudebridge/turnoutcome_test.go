package claudebridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// reportClock is a Config.Clock on which only the bound of an attempt of a request to the daemon
// passes (taskAnswerTimeout, daemonLink.attempt), and only once the row releases it: every other
// timer of the bridge's, its stop's and the wait before an attempt is sent again, never fires on
// it.
type reportClock struct {
	release chan struct{}
}

func (c reportClock) After(d time.Duration) <-chan time.Time {
	fires := make(chan time.Time, 1)
	if d == taskAnswerTimeout {
		go func() {
			<-c.release
			fires <- time.Unix(0, 0).Add(d)
		}()
	}
	return fires
}

// A turn report the daemon takes and never answers is cut at the attempt's bound,
// taskAnswerTimeout, which passes on the bridge's clock once the daemon has the report, and the
// pane's line says that bound, not the cancel that carried it: the attempt's context ends with
// errNoAnswerInTime as its cause, and the report is sent again until it is answered
// (daemonLink.untilAnswered).
func TestATurnReportCutByItsBoundNamesTheBound(t *testing.T) {
	taken, hang := make(chan struct{}, 1), make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		taken <- struct{}{}
		select {
		case <-hang:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(daemon.Close)
	t.Cleanup(func() { close(hang) }) // runs before daemon.Close, which waits for the handler
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := reportClock{release: make(chan struct{})}
	r := startRig(t, []string{"LEGION_DAEMON_URL=" + daemon.URL, "LEGION_BOOT_TOKEN_FILE=" + tokenFile, "LEGION_GENERATION=4"},
		daemonCalls{}, clock, "-p", "--resume", sessionID)
	r.handReady()
	r.turn("p1", "say hello")
	r.untilTurnEnds()
	testwait.Receive(t, "the daemon to take the turn's report", taken)
	close(clock.release)
	want := fmt.Sprintf("claims/turn-outcome of turn 1 got no answer from the daemon (no answer within %v); it is sent again until it is answered", taskAnswerTimeout)
	r.waitFor("the bridge to log the report cut at its bound: "+want, func() bool { return strings.Contains(r.log.String(), want) })
}

// reportingRig is a rig whose pane names a daemon: every turn the bridge ends is reported to the
// stub, which answers as the daemon's route does. The pane's claim is ready, as claude-envoy says
// once its ready and the read of its value after it have answered (the ready line): the bridge
// holds every Envoy delivery until that line.
type reportingRig struct {
	*rig
	reports chan claim.TurnOutcomeRequest
	answer  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse
}

const testBootToken = "boot-token-of-this-launch"

func newReportingRig(t *testing.T, args ...string) *reportingRig {
	t.Helper()
	r := &reportingRig{
		reports: make(chan claim.TurnOutcomeRequest, 16),
		answer:  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse { return claim.TurnOutcomeResponse{} },
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/legion/v1/claims/turn-outcome" {
			t.Errorf("the bridge posted to %s, want the turn-outcome route", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read the bridge's report: %v", err)
			return
		}
		var report claim.TurnOutcomeRequest
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("decode the bridge's report %s: %v", body, err)
			return
		}
		r.reports <- report
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.answer(report))
	}))
	t.Cleanup(daemon.Close)
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.rig = newRigWithEnv(t, []string{
		"LEGION_DAEMON_URL=" + daemon.URL,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, daemonCalls{}, args...)
	r.handReady()
	return r
}

// reported is the next turn the bridge reported, waited for until deadlineMargin before the test
// binary's deadline (deadlineWait).
func (r *reportingRig) reported() claim.TurnOutcomeRequest {
	r.t.Helper()
	select {
	case report := <-r.reports:
		return report
	case <-time.After(deadlineWait(r.t)):
		r.t.Fatalf("the bridge reported no turn, %s before the test binary's deadline; the bridge logged:\n%s", deadlineMargin, r.log)
		return claim.TurnOutcomeRequest{}
	}
}

// untilTurnEnds reads the bridge's frames up to and including the turn's end.
func (r *rig) untilTurnEnds() {
	r.t.Helper()
	for r.next()["type"] != "agent_end" {
	}
}

// The bridge reports every turn it ends, clean and refused alike, with its own sequence counted
// from 1 within the launch generation and the launch's boot token as its name: the daemon's count
// of refused turns is made of these reports and of nothing else. A step Claude Code ended with an
// error is the turn's refusal, carrying what it said and, in its provider error, the status Claude
// Code named in api_error_status. Both turns are an inbox delivery's, no take's, so neither report
// names a task, which is how this bridge says what the unprompted mark says: it sends neither that
// mark nor a status of its own beside the provider error's (turnReports).
func TestTheBridgeReportsEveryTurnItEndsCleanAndRefusedAlike(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.turn("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	clean := r.reported()
	want := claim.TurnOutcomeRequest{
		BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean,
	}
	if clean != want {
		t.Fatalf("the clean turn was reported %+v, want %+v", clean, want)
	}

	r.turn("p2", "api-error-529 the provider refused")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 2 || refused.Generation != 4 {
		t.Fatalf("the refused turn was reported %+v, want the second turn of generation 4 refused", refused)
	}
	if refused.Message == "" || refused.BootToken != testBootToken {
		t.Fatalf("the refused turn was reported %+v, want the provider's message and the launch's boot token", refused)
	}
	if refused.ProviderError == nil || refused.ProviderError.Status != 529 || refused.HTTPStatus != 0 || refused.Unprompted || refused.Task != 0 {
		t.Fatalf("the refused turn was reported %+v, want the 529 Claude Code named in its provider error, no task, and neither httpStatus nor the unprompted mark", refused)
	}
}

// What the daemon counts is the provider refusing a turn, keyed on the result's own terminal
// reason as the pinned Claude Code writes it (providerFailed): a step
// Claude Code ended with its own cap on the steps of one turn (error_max_turns) is the worker
// having worked, not the provider refusing, and a turn whose newest step ran is the provider
// having answered after all. Counting either would walk a working claim to the launch failure
// limit, fail it, and abandon the session it is working in (dispatch://INFRA-714).
func TestOnlyTheStepsTheProviderRefusedAreReportedRefused(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.turn("p1", "maxturns worked to the cap")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn Claude Code's own turn cap ended was reported %+v, want it clean", report)
	}

	// The second turn's step is refused with a delivery already queued behind it, so the bridge
	// sends that delivery as the next step of the same turn and the turn ends on a step that ran.
	// The order is the test's to fix, not a race to be hoped for: the delivery goes in first, the
	// bridge's own log says it is queued, and only then is the refusal let go.
	inbox := r.dialInbox()
	r.turn("p2", "fail-when go")
	r.expect("agent_start", nil)
	inbox.deliver("ev1", "say the CI is green")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("go")
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn whose last step ran was reported %+v, want it clean: the provider answered after all", report)
	}
}

// Only a turn the provider failed is reported refused, with Claude Code's own words as its
// message: a provider failure with a status, and a content filter's refusal, which names none
// (testdata/refusal-turn.jsonl:5). A turn a tool failed in and one Claude Code's own limit ended
// are the agent having worked, reported clean
// (dispatch://INFRA-956/comment/4fa88c00-3313-4008-8eea-d7f95a04e24d).
func TestOnlyAProviderFailureIsReportedRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		refused bool
	}{
		{name: "a provider failure with a status", script: "api-error-529 WAITING on the provider", refused: true},
		{name: "a content filter's refusal, which names none", script: "refusal WAITING on the provider", refused: true},
		{name: "a turn a tool failed in", script: "fail WAITING on the provider"},
		{name: "a turn Claude Code's own limit ended", script: "max-turns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReportingRig(t, "-p", "--resume", sessionID)
			r.turn("p1", tc.script)
			r.expect("agent_start", nil)
			r.expect("message_end", nil)
			r.untilTurnEnds()
			report := r.reported()
			if !tc.refused {
				if report.Outcome != claim.TurnClean || report.Message != "" {
					t.Fatalf("the turn was reported %+v, want it clean: the agent worked", report)
				}
				return
			}
			if report.Outcome != claim.TurnRefused || !strings.Contains(report.Message, "API Error: 529 overloaded") {
				t.Fatalf("the turn was reported %+v, want it refused with Claude Code's own words", report)
			}
		})
	}
}

// The other order, which is the rule the one above could be mistaken for: a step the provider
// refused with nothing queued behind it ends its turn, and that turn is reported refused. A
// delivery that arrives after it starts a turn of its own rather than continuing the one that
// ended, and the step that runs there is a turn of its own, reported clean — which is what clears
// the refused run the daemon counts (bridge.go's result). Neither turn was a take's, so neither
// report names a task, which is what has the daemon judge each from its report, and neither is
// marked unprompted: naming no task is this bridge's one way of saying it (turnReports).
func TestARefusalWithNothingQueuedEndsItsTurnAndTheNextDeliveryIsANewOne(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()

	r.turn("p1", "fail-when now")
	r.expect("agent_start", nil)
	r.release("now")
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 1 || refused.Message == "" {
		t.Fatalf("the refused turn was reported %+v, want the run's first turn refused with the provider's words", refused)
	}

	inbox.deliver("ev1", "say the CI is green")
	r.expect("agent_start", nil)
	for r.next()["type"] != "agent_end" {
	}

	clean := r.reported()
	if clean.Outcome != claim.TurnClean || clean.Sequence != 2 || clean.Message != "" {
		t.Fatalf("the delivery's turn was reported %+v, want the run's second turn clean: it is a turn of its own, and it clears the refusal", clean)
	}
	if clean.Unprompted || refused.Unprompted || clean.Task != 0 || refused.Task != 0 {
		t.Fatalf("the first turn was reported %+v and the delivery's %+v, want neither naming a task nor marked unprompted", refused, clean)
	}
	if answer := inbox.answer(); !answer.OK || answer.ID != "ev1" {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
}

// A pane that names no daemon reports nothing and bridges as it always did: a bridge run by hand
// has no claim to report to.
func TestABridgeWithNoDaemonReportsNothingAndStillBridges(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)

	r.turn("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
}
