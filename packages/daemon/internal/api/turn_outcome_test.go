package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// turnOutcome posts one report and answers what the daemon made of it.
func (h *harness) turnOutcome(req claim.TurnOutcomeRequest) (*httptest.ResponseRecorder, claim.TurnOutcomeResponse) {
	h.t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/claims/turn-outcome", req, nil)
	var answer claim.TurnOutcomeResponse
	if recorder.Code == http.StatusOK {
		decodeInto(h.t, recorder, &answer)
	}
	return recorder, answer
}

// A session that holds its registration's secret reports each turn it ends on the claim's own
// route, and the answer says what the daemon derived: the run of refused turns the reports now
// end with, counted up to the launch failure limit. A run of refusals fails no claim, however long
// it is (dispatch://INFRA-4508), so the answer's `failed` is false at the limit and past it, and
// the claim keeps the provider's words for the newest refusal.
func TestTheClaimsSecretReportsEveryTurnAndARunOfRefusalsFailsNoClaim(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")
	report := func(sequence uint64, outcome, message string) (*httptest.ResponseRecorder, claim.TurnOutcomeResponse) {
		return h.turnOutcome(claim.TurnOutcomeRequest{
			ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
			Generation: 1, Sequence: sequence, Outcome: outcome, Message: message,
		})
	}

	if _, answer := report(1, claim.TurnClean, ""); answer != (claim.TurnOutcomeResponse{}) {
		t.Fatalf("a clean turn answered %+v, want no run and no failure", answer)
	}
	if _, answer := report(2, claim.TurnRefused, "stopReason: error"); answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the first refusal answered %+v, want a run of 1", answer)
	}
	if _, answer := report(3, claim.TurnRefused, "stopReason: error"); answer != (claim.TurnOutcomeResponse{RefusedRun: 2}) {
		t.Fatalf("the second refusal answered %+v, want a run of 2", answer)
	}

	for sequence := uint64(4); sequence <= 6; sequence++ {
		recorder, answer := report(sequence, claim.TurnRefused, "the provider refused the request")
		if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 3}) {
			t.Fatalf("refusal %d = %d %+v, want 200 with the run at the limit of 3 and no failure", sequence-1, recorder.Code, answer)
		}
	}
	stored := h.stored(token)
	if stored.State == supervise.StateFailed || stored.Failure != "" {
		t.Fatalf("the claim is %s with failure %q, want it live: a run of refused turns fails no claim", stored.State, stored.Failure)
	}
	if got := stored.LastRefusal(); got != "the provider refused the request" {
		t.Fatalf("the claim keeps %q as the newest refusal, want the provider's words", got)
	}
}

// The Claude bridge holds no registration secret — its pane's registration is claude-envoy's —
// so it reports with the launch's own boot token, which names the same claim.
func TestTheLaunchsBootTokenReportsTheSameClaimsTurns(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-209", claim.RoleImplementer)

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		BootToken: boot, Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "Claude Code: error_during_execution",
	})

	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the bridge's report = %d %+v, want 200 with a run of 1", recorder.Code, answer)
	}
	stored := h.stored(token)
	if len(stored.TurnOutcomes) != 1 || stored.LastRefusal() != "Claude Code: error_during_execution" {
		t.Fatalf("the claim keeps %+v, want the bridge's refusal recorded", stored.TurnOutcomes)
	}
}

// A pane left running from a launch the claim has replaced still holds that launch's boot token,
// and its provider's refusals are not the current pane's: the route refuses the token a relaunch
// left behind, exactly as the shim's hello on that token is refused, and refuses a report for any
// generation but the one its launch was minted for. Either would otherwise walk a working claim
// to the launch failure limit and abandon the session it is working in.
func TestATurnOutcomeFromAReplacedLaunchIsRefusedAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	token, first := h.launch("LEGION-209", claim.RoleImplementer)
	current := h.relaunch(token)

	stale, _ := h.turnOutcome(claim.TurnOutcomeRequest{
		BootToken: first, Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "the replaced pane's provider",
	})
	wantRefusal(t, stale, claim.InvalidBootToken.Status, claim.InvalidBootToken.Message)

	otherGeneration, _ := h.turnOutcome(claim.TurnOutcomeRequest{
		BootToken: current, Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "a turn of a generation this launch never ran",
	})
	wantRefusal(t, otherGeneration, claim.StaleGeneration.Status, claim.StaleGeneration.Message)

	if kept := h.stored(token).TurnOutcomes; len(kept) != 0 {
		t.Fatalf("the claim keeps %+v, want neither refused report recorded", kept)
	}
	if _, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		BootToken: current, Generation: 2, Sequence: 1, Outcome: claim.TurnRefused, Message: "the live pane's provider",
	}); answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the live pane's report answered %+v, want a run of 1", answer)
	}
}

// Every body the route cannot act on is refused with the sentence that says what to send, and
// nothing is recorded: a caller that names no identity or both, a sequence of 0 (how a body that
// named none decodes, which would collide every turn on one key), a generation of 0, and an
// outcome that is neither word.
func TestATurnOutcomeTheRouteCannotActOnIsRefusedAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")
	good := claim.TurnOutcomeRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
		Generation: 1, Sequence: 1, Outcome: claim.TurnClean,
	}
	for name, testCase := range map[string]struct {
		body    func(claim.TurnOutcomeRequest) claim.TurnOutcomeRequest
		status  int
		message string
	}{
		"naming no caller": {
			body: func(claim.TurnOutcomeRequest) claim.TurnOutcomeRequest {
				return claim.TurnOutcomeRequest{Generation: 1, Sequence: 1, Outcome: claim.TurnClean}
			},
			status:  http.StatusBadRequest,
			message: "a turn outcome names its caller with claimToken, sessionId and secret, or with bootToken; give one",
		},
		"naming both callers": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.BootToken = boot; return req },
			status:  http.StatusBadRequest,
			message: "a turn outcome names its caller with claimToken, sessionId and secret, or with bootToken; give one",
		},
		"with no sequence": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.Sequence = 0; return req },
			status:  http.StatusBadRequest,
			message: "sequence is required, counted from 1 within the generation",
		},
		"with no generation": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.Generation = 0; return req },
			status:  http.StatusBadRequest,
			message: "generation is required",
		},
		"with an outcome that is neither word": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.Outcome = "refused-ish"; return req },
			status:  http.StatusBadRequest,
			message: `outcome "refused-ish" is neither "clean" nor "refused"`,
		},
		"with the wrong secret": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.Secret = "not the secret"; return req },
			status:  claim.InvalidSecret.Status,
			message: claim.InvalidSecret.Message,
		},
		"with a boot token no launch minted": {
			body: func(claim.TurnOutcomeRequest) claim.TurnOutcomeRequest {
				return claim.TurnOutcomeRequest{BootToken: "not a boot token", Generation: 1, Sequence: 1, Outcome: claim.TurnClean}
			},
			status:  claim.InvalidBootToken.Status,
			message: claim.InvalidBootToken.Message,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder, _ := h.turnOutcome(testCase.body(good))

			wantRefusal(t, recorder, testCase.status, testCase.message)
			if kept := h.stored(token).TurnOutcomes; len(kept) != 0 {
				t.Fatalf("the refused report left %+v on the claim, want nothing recorded", kept)
			}
		})
	}
}

// A status outside the HTTP range is no status a provider answered, and the rollout of the field
// is warn-first: the report is taken, as one whose status cannot be read, and the daemon warns
// naming the number rather than refusing it. So a turn that ran no prompt of the daemon's, refused
// with 1400, counts in the claim's run of provider-error turns and fails nothing, as a refusal
// with no status does, where a 400 would fail the claim.
func TestATurnOutcomeWithAStatusOutsideTheHTTPRangeIsTakenAsNoStatusWithAWarning(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
		Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "the provider refused the reminder's turn",
		HTTPStatus: 1400, Unprompted: true,
	})

	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the report = %d %+v, want 200 with a run of 1 and the claim not failed", recorder.Code, answer)
	}
	if stored := h.stored(token); stored.State == supervise.StateFailed || stored.ProviderErrorRun != 1 {
		t.Fatalf("the claim is %s with a run of %d provider errors, want it live with 1: a refusal with no status",
			stored.State, stored.ProviderErrorRun)
	}
	if lines := h.serverLog.lines("level=WARN", "taken as no status", "httpStatus=1400"); len(lines) != 1 {
		t.Fatalf("the route warned %q, want one line naming the status it took as none", h.serverLog.lines("httpStatus"))
	}
}

// A report of a generation the claim has left is not stale: it is a turn of that generation, and
// the run is derived over the reports in generation order, so it counts where it belongs.
func TestAReportOfAnEarlierGenerationIsRecordedInItsOwnPlace(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
		Generation: 1, Sequence: 9, Outcome: claim.TurnRefused, Message: "refused at the older generation",
	})

	if recorder.Code != http.StatusOK || answer.RefusedRun != 1 {
		t.Fatalf("the report = %d %+v, want 200 with a run of 1", recorder.Code, answer)
	}
	kept := h.stored(token).TurnOutcomes
	if len(kept) != 1 || kept[0].Generation != 1 || kept[0].Sequence != 9 {
		t.Fatalf("the claim keeps %+v, want the report under its own (generation, sequence)", kept)
	}
}

// The report of a turn that ran no prompt of the daemon's is that turn's one record, so the daemon
// judges the turn from it (supervise's turnOutcome): refused with a status that can clear, it
// fails nothing, and refused with one that cannot, a 400, it fails the claim, and the answer's
// `failed` says so (claim.TurnOutcomeResponse).
func TestAnUnpromptedTurnsReportIsJudgedAndAnswersWhetherItFailedTheClaim(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")
	report := func(sequence uint64, status int) (*httptest.ResponseRecorder, claim.TurnOutcomeResponse) {
		return h.turnOutcome(claim.TurnOutcomeRequest{
			ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
			Generation: 1, Sequence: sequence, Outcome: claim.TurnRefused, Message: "the provider refused the reminder's turn",
			HTTPStatus: status, Unprompted: true,
		})
	}

	if recorder, answer := report(1, 529); recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the 529 = %d %+v, want 200 with a run of 1 and no failure", recorder.Code, answer)
	}
	if stored := h.stored(token); stored.State == supervise.StateFailed || stored.ProviderErrorRun != 1 {
		t.Fatalf("after the 529 the claim is %s with a run of %d provider errors, want it live with 1", stored.State, stored.ProviderErrorRun)
	}

	recorder, answer := report(2, 400)

	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 2, Failed: true}) {
		t.Fatalf("the 400 = %d %+v, want 200 with a run of 2 and the claim failed", recorder.Code, answer)
	}
	if stored := h.stored(token); stored.State != supervise.StateFailed || stored.FailureCause != supervise.CauseProviderError {
		t.Fatalf("after the 400 the claim is %s (%q), want it failed by a provider error", stored.State, stored.FailureCause)
	}
}

// A report of a turn that ran no prompt of the daemon's can carry the class pi-legion gave the
// refusal with Oh My Pi's own classifier: `code: "cyber_policy"`, OpenAI's refusal of the
// session's content. It fails the claim under cause cyber_policy whatever the status says — here
// none, and a gateway's 503 — and the answer's `failed` says so: without it, the daemon reads a
// refusal with no status or a 503 as one that can clear and fails nothing, and the claim's next
// prompt sends the refused content to OpenAI again.
func TestAnUnpromptedTurnsCyberPolicyCodeFailsTheClaimUnderItsOwnCause(t *testing.T) {
	for _, status := range []int{0, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			h := newHarness(t)
			token, boot := h.launch("LEGION-208", claim.RoleArchitect)
			registered := h.registered(boot, "ses_architect")

			recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
				ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
				Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "cyber_policy: synthetic refusal text",
				HTTPStatus: status, Unprompted: true, Code: claim.TurnCodeCyberPolicy,
			})

			if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1, Failed: true}) {
				t.Fatalf("the report = %d %+v, want 200 with a run of 1 and the claim failed", recorder.Code, answer)
			}
			if stored := h.stored(token); stored.State != supervise.StateFailed || stored.FailureCause != supervise.CauseCyberPolicy ||
				!strings.Contains(stored.Failure, "cyber_policy") {
				t.Fatalf("the claim is %s (%q, %q), want it failed under cause cyber_policy", stored.State, stored.FailureCause, stored.Failure)
			}
		})
	}
}

// The daemon reads one class off a report, `cyber_policy`; a report naming another is taken as one
// that names none, as a status outside the HTTP range is, and the route warns naming it rather
// than refusing the report, so the turn is still counted.
func TestATurnOutcomeWithAnUnknownCodeIsTakenAsNoCodeWithAWarning(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	registered := h.registered(boot, "ses_architect")

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
		Generation: 1, Sequence: 1, Outcome: claim.TurnRefused, Message: "the provider refused the reminder's turn",
		Unprompted: true, Code: "cost_cap",
	})

	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1}) {
		t.Fatalf("the report = %d %+v, want 200 with a run of 1 and no failure", recorder.Code, answer)
	}
	if stored := h.stored(token); stored.State == supervise.StateFailed {
		t.Fatalf("the claim failed (%q) on a code the daemon reads none of", stored.FailureCause)
	}
	if lines := h.serverLog.lines("level=WARN", "taken as no code", "code=cost_cap"); len(lines) != 1 {
		t.Fatalf("the route warned %q, want one line naming the code it took as none", h.serverLog.lines("code="))
	}
}
