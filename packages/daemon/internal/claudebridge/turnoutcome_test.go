package claudebridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// reportingRig is a rig whose pane names a daemon: every turn the bridge ends is reported to the
// stub, which answers as the daemon's route does.
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
	}, args...)
	return r
}

// reported is the next turn the bridge reported.
func (r *reportingRig) reported() claim.TurnOutcomeRequest {
	r.t.Helper()
	select {
	case report := <-r.reports:
		return report
	case <-time.After(10 * time.Second):
		r.t.Fatal("the bridge reported no turn within 10s")
		return claim.TurnOutcomeRequest{}
	}
}

// untilTurnEnds reads the bridge's frames up to and including the turn's end, and the
// `prompt_result` behind it. A turn the daemon's prompt started ends with two frames, not one:
// `agent_end`, which names no prompt, because a turn Claude Code started by itself when a
// background task finished ends with one just like it, and then the `prompt_result` that says
// whose turn it was. Every turn of these cases is a prompt's, so leaving the second frame unread
// would hand it to the next read of the stream.
func (r *rig) untilTurnEnds() {
	r.t.Helper()
	for r.next()["type"] != "agent_end" {
	}
	r.expect("prompt_result", nil)
}

// The bridge reports every turn it ends, clean and refused alike, with its own sequence counted
// from 1 within the launch generation and the launch's boot token as its name: the daemon's run of
// refused reports is made of these reports and of nothing else. A step Claude Code ended with an
// error is the turn's refusal, carrying what it said and the status it named.
func TestTheBridgeReportsEveryTurnItEndsCleanAndRefusedAlike(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	clean := r.reported()
	want := claim.TurnOutcomeRequest{
		BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean,
	}
	if clean != want {
		t.Fatalf("the clean turn was reported %+v, want %+v", clean, want)
	}

	r.prompt("p2", "api-error-529 the provider refused")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 2 || refused.Generation != 4 {
		t.Fatalf("the refused turn was reported %+v, want the second turn of generation 4 refused", refused)
	}
	if refused.Message == "" || refused.BootToken != testBootToken || refused.HTTPStatus != 529 {
		t.Fatalf("the refused turn was reported %+v, want the provider's message, its status 529 and the launch's boot token", refused)
	}
}

// Claude Code retries a request the provider refused inside the turn, and writes an `api_retry`
// line for each attempt it retries (testdata/api-error-529.jsonl:2-3) before the one result that
// ends the turn. The bridge reports the turn at that result and at nothing else, so an attempt
// Claude Code goes on from is no turn: one turn is one report, as pi-legion reports only the ends
// Oh My Pi yields, and the daemon's run of refused turns moves by one for it.
func TestAnAttemptClaudeCodeRetriesIsNoTurnOfItsOwn(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "replay api-error-529.jsonl")
	r.open()
	r.untilTurnEnds()
	r.prompt("p2", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	var got []claim.TurnOutcomeRequest
	for len(got) == 0 || got[len(got)-1].Outcome != claim.TurnClean {
		got = append(got, r.reported())
	}
	want := []claim.TurnOutcomeRequest{
		{BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnRefused, HTTPStatus: 529},
		{BootToken: testBootToken, Generation: 4, Sequence: 2, Outcome: claim.TurnClean},
	}
	for i := range got {
		got[i].Message = "" // Claude Code's words; the case is about how many turns are reported
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a turn Claude Code retried twice inside, then a clean turn, were reported %+v, want %+v", got, want)
	}
}

// A turn the daemon's prompt did not start — one an Envoy delivery started, here — ends with no
// prompt_result, so its report is the one thing the daemon decides it by: the report carries the
// status Claude Code named for the refusal (api_error_status), and the daemon fails the claim on
// one that cannot clear. A content filter's refusal names no status, and its report carries none;
// nor does a refusal whose api_error_status is no HTTP status (outside 100 to 599), which the
// daemon's route would refuse with the whole report, leaving the turn unrecorded.
func TestARefusedTurnIsReportedWithTheStatusClaudeCodeNamedForIt(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()

	inbox.deliver("ev1", "api-error-400 the request is malformed")
	r.expect("agent_start", nil)
	for r.next()["type"] != "agent_end" {
	}
	if report := r.reported(); report.Outcome != claim.TurnRefused || report.HTTPStatus != 400 || report.Message == "" {
		t.Fatalf("the Envoy delivery's refused turn was reported %+v, want it refused with status 400 and the provider's words", report)
	}
	if answer := inbox.answer(); !answer.OK || answer.ID != "ev1" {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}

	r.prompt("p1", "refusal the content filter refused")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := r.reported(); report.Outcome != claim.TurnRefused || report.HTTPStatus != 0 || report.Sequence != 2 {
		t.Fatalf("the content filter's refusal was reported %+v, want the second turn refused with no status", report)
	}

	r.prompt("p2", "api-error-999 the provider named no HTTP status")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := r.reported(); report.Outcome != claim.TurnRefused || report.HTTPStatus != 0 || report.Sequence != 3 || report.Message == "" {
		t.Fatalf("the refusal whose api_error_status is 999 was reported %+v, want the third turn refused with the provider's words and no status", report)
	}
}

// What the daemon counts is the provider refusing a turn, keyed on the result's own terminal
// reason as the pinned Claude Code writes it (providerFailed): a step
// Claude Code ended with its own cap on the steps of one turn (error_max_turns) is the worker
// having worked, not the provider refusing, and a turn whose newest step ran is the provider
// having answered after all. Counting either would tell the project's controller of a run of
// refused turns that the provider never refused.
func TestOnlyTheStepsTheProviderRefusedAreReportedRefused(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "maxturns worked to the cap")
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
	r.prompt("p2", "fail-when go")
	r.expect("agent_start", nil)
	inbox.deliver("ev1", "say the CI is green")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("go")
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn whose last step ran was reported %+v, want it clean: the provider answered after all", report)
	}
}

// The other order, which is the rule the one above could be mistaken for: a step the provider
// refused with nothing queued behind it ends its turn, and that turn is reported refused. A
// delivery that arrives after it starts a turn of its own rather than continuing the one that
// ended, and the step that runs there is a turn of its own, reported clean — which is what clears
// the refused run the daemon counts (bridge.go's result).
func TestARefusalWithNothingQueuedEndsItsTurnAndTheNextDeliveryIsANewOne(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()

	r.prompt("p1", "fail-when now")
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
	if answer := inbox.answer(); !answer.OK || answer.ID != "ev1" {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
}

// A pane that names no daemon reports nothing and bridges as it always did: a bridge run by hand
// has no claim to report to.
func TestABridgeWithNoDaemonReportsNothingAndStillBridges(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
}
