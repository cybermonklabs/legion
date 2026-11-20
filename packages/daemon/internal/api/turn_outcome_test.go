package api

import (
	"net/http"
	"net/http/httptest"
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
// route. The answer gives the run of refused turns the reports end with. A refusal whose report
// names no status, or one that can clear, fails no claim.
func TestTheClaimsSecretReportsEveryTurnWithoutFailingTheClaim(t *testing.T) {
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

	recorder, answer := report(4, claim.TurnRefused, "the provider refused the request")
	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 3}) {
		t.Fatalf("the third refusal = %d %+v, want 200 with a run of 3 and no failure", recorder.Code, answer)
	}
	overloaded, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret,
		Generation: 1, Sequence: 5, Outcome: claim.TurnRefused, Message: "overloaded_error", HTTPStatus: 529,
	})
	if overloaded.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 4}) {
		t.Fatalf("a refusal with status 529 = %d %+v, want 200 with a run of 4 and no failure", overloaded.Code, answer)
	}
	stored := h.stored(token)
	if stored.State == supervise.StateFailed || stored.Failure != "" {
		t.Fatalf("the claim is %s with failure %q, want it not failed", stored.State, stored.Failure)
	}
}

// A refusal whose report carries a status that cannot clear fails the claim at once, through the
// route, as a prompt result with that status does: the report is the one decision a turn no
// daemon prompt ran gets. The answer says the claim is failed, and so does the store.
func TestARefusalReportedWithAStatusThatCannotClearFailsTheClaim(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-209", claim.RoleImplementer)

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		BootToken: boot, Generation: 1, Sequence: 1, Outcome: claim.TurnRefused,
		Message: "API Error: 400 invalid_request_error", HTTPStatus: http.StatusBadRequest,
	})

	if recorder.Code != http.StatusOK || answer != (claim.TurnOutcomeResponse{RefusedRun: 1, Failed: true}) {
		t.Fatalf("the bridge's report of a 400 = %d %+v, want 200 with a run of 1 and the claim failed", recorder.Code, answer)
	}
	stored := h.stored(token)
	if want := "a provider error that cannot clear ended this claim's turn: a provider error (HTTP 400)"; stored.State != supervise.StateFailed ||
		stored.FailureCause != supervise.CauseProviderError || stored.Failure != want {
		t.Fatalf("the claim is %s (%q, %q), want it failed with %q (%q)", stored.State, stored.FailureCause, stored.Failure, want, supervise.CauseProviderError)
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
// generation but the one its launch was minted for. Either would otherwise count the replaced
// pane's refusals in the run the controller is told of, and fail the claim on a refusal of a turn
// its own agent never ran.
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
// named none decodes, which would collide every turn on one key), a generation of 0, an outcome
// that is neither word, and a status on a clean turn or outside HTTP's.
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
		"with a status on a clean turn": {
			body:    func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest { req.HTTPStatus = 400; return req },
			status:  http.StatusBadRequest,
			message: `httpStatus 400 is given only with outcome "refused", from 100 to 599`,
		},
		"with a status that is no HTTP status": {
			body: func(req claim.TurnOutcomeRequest) claim.TurnOutcomeRequest {
				req.Outcome, req.HTTPStatus = claim.TurnRefused, 4000
				return req
			},
			status:  http.StatusBadRequest,
			message: `httpStatus 4000 is given only with outcome "refused", from 100 to 599`,
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
