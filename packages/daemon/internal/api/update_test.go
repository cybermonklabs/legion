package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// testUpdateRound is a pod round's identity, as the daemon's sweep writes it and as the update
// task names it to the worker.
const testUpdateRound = "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22"

// readyForUpdate posts the worker's answer as its plugin does.
func (h *harness) readyForUpdate(body claim.ReadyForUpdateRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/ready-for-update", body, nil)
}

// The answer to the update task travels the whole route: a worker in an open round answers that
// round and is acknowledged, the same answer naming any other round is refused 409 with the
// sentence the agent reads, and an answer carrying no round at all is refused 400 before any
// claim is touched. The refusal is the one the machine makes (supervise.ErrForeignUpdateRound),
// rendered by claimFailure as every claim refusal is.
func TestReadyForUpdateTakesTheOpenRoundsAnswerAndRefusesAnother(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	registration := h.registered(boot, session)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatal("no machine for the implementer")
	}
	// The agent says it is ready through its machine rather than the route, so that the secret
	// its one registration issued is the secret the answer below carries.
	if err := machine.Handle(h.ctx, supervise.RequestReady{
		Claim: token, Generation: registration.Generation, Session: session,
	}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	round := supervise.PodUpdateOpen{
		Claim: token, Round: testUpdateRound, Reasons: "image",
		Task: supervise.UpdateTask(testUpdateRound, "image", time.Now().Add(30*time.Minute)),
	}
	if err := machine.Handle(h.ctx, round); err != nil {
		t.Fatalf("open the update round: %v", err)
	}
	answer := claim.ReadyForUpdateRequest{
		ClaimToken: token, SessionID: session, Secret: registration.Secret,
		Generation: registration.Generation, Round: testUpdateRound,
		Head: "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35",
	}

	noRound := answer
	noRound.Round = ""
	wantRefusal(t, h.readyForUpdate(noRound), http.StatusBadRequest, "round is required")
	if c := machine.Claim(); c.UpdateSignalledAt != nil {
		t.Fatalf("claim after the answer with no round = %+v, want nothing recorded", c)
	}

	foreign := answer
	foreign.Round = supervise.PluginRoundPrefix + strconv.Itoa(DaemonAPIVersion)
	wantRefusal(t, h.readyForUpdate(foreign), http.StatusConflict,
		"ready_for_update refused: the answer names an update round this claim is not in. "+
			"It answers \""+foreign.Round+"\", and this claim is in \""+testUpdateRound+"\". "+
			"Do not answer that round in this turn. End your turn. The daemon then sends you the task of that round. Answer it after you read that task")
	if c := machine.Claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Fatalf("claim after the answer for another round = %+v, want nothing of it recorded", c)
	}

	if recorder := h.readyForUpdate(answer); recorder.Code != http.StatusNoContent {
		t.Fatalf("the answer for the open round = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
	c := machine.Claim()
	if c.UpdateSignalledAt == nil || c.UpdateHead != answer.Head {
		t.Fatalf("claim after the open round's own answer = %+v, want the answer and the head recorded", c)
	}
}

// A worker that answers a plugin update round after this release's boot has taken its claim out
// of it is refused, and nothing of the answer is recorded (dispatch://INFRA-2712 R5). The boot
// ends every plugin round an earlier daemon left, over every claim, because nothing of this
// release carries such a round and one left standing holds the claim's next task behind it for
// ever (internal/daemon's endPluginRounds). A turn that was in flight across the restart still
// holds the round's task, and its `ready_for_update` arrives at a claim in no round at all.
//
// Nothing new refuses it: podUpdateSignalled already refuses an answer for a round its claim is
// not in, and this is that case with the round removed rather than replaced. The worker reads a
// 409 that says the daemon asks it for nothing now, and its claim is neither failed nor retired
// for it — the answer is a request the claim's state did not allow, as every other refused
// request of it is, and the next task the daemon sends is what the worker works.
func TestReadyForUpdateForAPluginRoundThisBootEndedIsRefusedAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	registration := h.registered(boot, session)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatal("no machine for the implementer")
	}
	if err := machine.Handle(h.ctx, supervise.RequestReady{
		Claim: token, Generation: registration.Generation, Session: session,
	}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	// The round an earlier daemon opened over this claim, and the end this boot posts for it.
	round := supervise.PluginRoundPrefix + strconv.Itoa(DaemonAPIVersion)
	if err := machine.Handle(h.ctx, supervise.PodUpdateOpen{
		Claim: token, Round: round, Reasons: "plugin", Task: supervise.UpdateTask(round, "plugin", time.Now().Add(30*time.Minute)),
	}); err != nil {
		t.Fatalf("open the plugin round: %v", err)
	}
	if err := machine.Handle(h.ctx, supervise.PodUpdateEnded{Claim: token, Round: round}); err != nil {
		t.Fatalf("end the plugin round: %v", err)
	}
	machine.Wait()
	if c := machine.Claim(); c.UpdateRound != "" {
		t.Fatalf("the claim after the boot's end is in %q, want no round: the end takes it out of the round whole", c.UpdateRound)
	}
	state := machine.Claim().State

	answer := claim.ReadyForUpdateRequest{
		ClaimToken: token, SessionID: session, Secret: registration.Secret,
		Generation: registration.Generation, Round: round,
		Head: "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35",
	}
	wantRefusal(t, h.readyForUpdate(answer), http.StatusConflict,
		"ready_for_update refused: the answer names an update round this claim is not in. "+
			"It answers \""+round+"\", and this claim is in no update round. "+
			"The daemon asks this claim for no answer now")

	machine.Wait()
	c := machine.Claim()
	if c.UpdateRound != "" || c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("the claim after the refused answer = %+v, want nothing of the round or the answer recorded", c)
	}
	if c.State != state {
		t.Errorf("the claim after the refused answer is %s, want %s: a refused answer neither fails nor retires it",
			c.State, state)
	}
}
