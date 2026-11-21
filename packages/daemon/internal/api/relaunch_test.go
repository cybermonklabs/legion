package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// relaunchHarness is the API with the daemon's relaunch answering what a case sets, recording the
// claim each call reached (Options.Relaunch, internal/daemon's supervision.relaunch).
func relaunchHarness(t *testing.T, answer error) (*harness, *[]claim.Token) {
	t.Helper()
	h := newHarness(t)
	reached := &[]claim.Token{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{&phaseRestarter{}}, RoleFor: phase.RoleFor,
		Relaunch: func(_ context.Context, m *supervise.Machine) error {
			*reached = append(*reached, m.Claim().Token)
			return answer
		},
	}).Handler
	return h, reached
}

// The controller's relaunch names its claim by the token the daemon's notices carry, under the
// controller's grant, and answers {} once the claim was relaunched.
func TestTheControllersRelaunchReachesTheClaimItsTokenNames(t *testing.T) {
	h, reached := relaunchHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/relaunch",
		RelaunchRequest{GrantID: controllerGrant(t, h).GrantID, Claim: string(token)}, nil)

	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("relaunch = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	if len(*reached) != 1 || (*reached)[0] != token {
		t.Fatalf("the daemon's relaunch reached %v, want %s", *reached, token)
	}
}

// Each refusal is answered with the code the controller acts on, and the five a person must
// handle say so in the sentence. The tree lifecycle's refusal reaches the route as the machine
// refuses it, a refusal of the claim in its state, and is answered with its own code rather than
// the one of a claim between states. So is a tree check the store did not answer, with a 503 of
// its own that says nothing was stopped and the relaunch may be asked for again, rather than the
// tree's refusal, which sends the controller away from the claim.
func TestTheControllersRelaunchNamesEachRefusal(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		person bool
	}{
		{supervise.ErrClaimReachable, "CLAIM_REACHABLE", false},
		{fmt.Errorf("%w: its open call is bash", supervise.ErrRelaunchGuard), "RELAUNCH_GUARD", false},
		{&supervise.RefusedError{State: supervise.StateLaunching, Request: "relaunch", Reason: "no settled agent"}, "CLAIM_UNSETTLED", false},
		{supervise.ErrRelaunchedRecently, "RELAUNCHED_RECENTLY", true},
		{supervise.ErrPodUnreachable, "POD_UNREACHABLE", true},
		{fmt.Errorf("relaunch refused: %w", runtime.ErrLauncherNotConnected), "LAUNCHER_NOT_CONNECTED", true},
		{fmt.Errorf("relaunch refused: %w", runtime.ErrRelaunchReplacesPod), "RELAUNCH_REPLACES_POD", true},
		{fmt.Errorf("relaunch refused: %w", runtime.ErrProcessOutlivedStop), "PROCESS_OUTLIVED_STOP", true},
		{&supervise.RefusedError{State: supervise.StateWorking, Request: "relaunch", Reason: "tree cleanup is reserved",
			Err: fmt.Errorf("%w: %w", supervise.ErrTreeRefusesLaunch, treelifecycle.ErrCleanupReserved)}, "TREE_REFUSES_LAUNCH", false},
		{&supervise.RefusedError{State: supervise.StateWorking, Request: "relaunch", Reason: "the store did not answer",
			Err: fmt.Errorf("%w: %w", supervise.ErrTreeCheckFailed, errors.New("the store did not answer"))}, "TREE_CHECK_FAILED", false},
		{errors.New("the store did not answer"), "RELAUNCH_FAILED", false},
		{fmt.Errorf("%w; %w: the store did not answer", fmt.Errorf("%w: its open call is bash", supervise.ErrRelaunchGuard),
			supervise.ErrReachUnwritten), "RELAUNCH_FAILED", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h, _ := relaunchHarness(t, tc.err)
			token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
			recorder := h.request(http.MethodPost, "/legion/v1/claims/relaunch",
				RelaunchRequest{GrantID: controllerGrant(t, h).GrantID, Claim: string(token)}, nil)
			status := http.StatusConflict
			switch tc.code {
			case "RELAUNCH_FAILED":
				status = http.StatusInternalServerError
			case "TREE_CHECK_FAILED":
				status = http.StatusServiceUnavailable
			}
			body := recorder.Body.String()
			refused(t, recorder, status, tc.code)
			if says := strings.Contains(body, "case for a person"); says != tc.person {
				t.Fatalf("the refusal %s says a case for a person: %t, want %t", body, says, tc.person)
			}
		})
	}
}

// The relaunch is the controller's alone, refuses the controller's own claim, which the daemon
// relaunches itself, and names a claim the daemon does not supervise with a 404.
func TestTheControllersRelaunchNeedsAControllerGrantAndAnotherClaim(t *testing.T) {
	h, reached := relaunchHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	registered := h.registered(h.bootToken(token), "ses_implementer")
	var worker GrantResponse
	decodeInto(t, h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: "ses_implementer", Secret: registered.Secret, Tree: "LEGION-208", Issue: "LEGION-209",
	}, nil), &worker)

	refused(t, h.request(http.MethodPost, "/legion/v1/claims/relaunch",
		RelaunchRequest{GrantID: worker.GrantID, Claim: string(token)}, nil), http.StatusForbidden, "CONTROLLER_REQUIRED")
	grant := controllerGrant(t, h).GrantID
	refused(t, h.request(http.MethodPost, "/legion/v1/claims/relaunch",
		RelaunchRequest{GrantID: grant, Claim: string(claim.ControllerToken(testProject))}, nil), http.StatusConflict, "CLAIM_IS_CONTROLLER")
	refused(t, h.request(http.MethodPost, "/legion/v1/claims/relaunch",
		RelaunchRequest{GrantID: controllerGrant(t, h).GrantID, Claim: "legion-legion-legion-777-tester"}, nil), http.StatusNotFound, "NO_CLAIM")
	if len(*reached) != 0 {
		t.Fatalf("a refused request reached the daemon's relaunch for %v", *reached)
	}
}
