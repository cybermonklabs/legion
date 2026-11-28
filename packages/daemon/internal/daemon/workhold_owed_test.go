package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The end-of-turn reminder under `gates.hold: work`. Both plugins' reminder renders the daemon's
// answer to `POST /legion/v1/claims/owed` (api's claimOwed and owedBy;
// packages/pi-shared/src/phase-reminder.ts), and the stall watch reads it. A phase worker whose
// stop the hold holds for the turn its close message started is told to end that turn, so the
// answer at that turn's end names no work: a reminder to go on with its phase would contradict
// the close message and R2 (no phase runs while the tree's standing design gate is closed).

// owedSecret is the secret the implementer's registration was issued in these cases.
const owedSecret = "secret-of-the-implementer"

// workingWithASecret is working, with role's registration issued secret: the claim records the
// hash the daemon's register route stores of it (sha256, api's capabilityHash), so the owed
// route takes the claim's session with that secret as the one that holds the claim.
func (r *workHoldRig) workingWithASecret(role claim.Role, secret string) {
	r.t.Helper()
	machine := r.create(role, supervise.Claim{State: supervise.StateQueued})
	token, session := r.token(role), "ses-"+string(role)
	r.handle(role, supervise.RequestSpawn{Claim: token})
	generation := machine.Claim().Generation
	capability := sha256.Sum256([]byte(secret))
	r.handle(role, supervise.StreamHello{Claim: token, Generation: generation})
	r.handle(role, supervise.RequestRegister{Claim: token, Generation: generation, Session: session,
		SessionFile: "/tmp/" + string(role) + ".jsonl", CapabilityHash: capability[:]})
	r.handle(role, supervise.RequestReady{Claim: token, Generation: generation, Session: session})
	r.handle(role, supervise.StreamTurnStart{Claim: token})
}

// owed is the answer of the daemon's owed route, as its API serves it over the rig's supervisor,
// store and workflow, to role's session asking with secret what role's claim owes now.
func (r *workHoldRig) owed(role claim.Role, secret string) api.OwedResponse {
	r.t.Helper()
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: r.sup, Project: r.tokenProject, DispatchProject: r.tree.project, Pool: r.pool, Record: r.records,
		RoleFor: workflow.RoleFor, Log: quietLogger(),
	}).Handler
	body, err := json.Marshal(api.OwedRequest{ClaimToken: r.token(role), SessionID: "ses-" + string(role), Secret: secret})
	if err != nil {
		r.t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(r.ctx, http.MethodPost, "/legion/v1/claims/owed", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the owed route for the %s = %d, want 200; body %s", role, recorder.Code, recorder.Body)
	}
	var answer api.OwedResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		r.t.Fatalf("decode the owed answer for the %s: %v; body %s", role, err, recorder.Body)
	}
	return answer
}

// The root's implementer is at work in its implementing phase, with the root's architect live.
// Before the close it owes that phase, and the route names it and the architect that owns it.
// The close under `work` publishes its close message and holds its stop for the turn that
// message reached (supervise.Claim.WorkHeld): asked at that turn's end, the route answers that it
// owes nothing, names no work, and still names the owning architect.
//
// A claim the hold has suspended is no case here: its process is stopped and it holds no
// capability (supervise.NormalizeClaim), so no reminder can run in it and nothing asks the route
// for it.
func TestWorkHoldOwedTellsAHeldWorkerItOwesNothing(t *testing.T) {
	t.Parallel()
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	r.live(claim.RoleArchitect)
	r.workingWithASecret(claim.RoleImplementer, owedSecret)
	architect := r.token(claim.RoleArchitect)

	want := api.OwedResponse{Owes: true, Work: "the implementing phase of LEGION-208", Architect: architect}
	if got := r.owed(claim.RoleImplementer, owedSecret); got != want {
		t.Fatalf("before the close the implementer is told %+v, want %+v", got, want)
	}

	r.close(4)
	r.runOnce()
	r.runOnce()
	if c := r.machine(claim.RoleImplementer).Claim(); !c.WorkHeld() || c.State != supervise.StateWorking {
		t.Fatalf("after the close the implementer is %s, held %v; want its stop held for its turn: the case's own premise", c.State, c.WorkHeld())
	}
	if got := r.published(claim.RoleImplementer); len(got) != 1 || got[0] != r.workerClose(4) {
		t.Fatalf("after the close the implementer was told %q, want the close message under `work`", got)
	}
	if got, want := r.owed(claim.RoleImplementer, owedSecret), (api.OwedResponse{Architect: architect}); got != want {
		t.Fatalf("with its stop held for the close message's turn the implementer is told %+v, want %+v: nothing owed, no work named", got, want)
	}
}
