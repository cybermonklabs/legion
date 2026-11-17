package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The record of a step names its agent's session and the model that answered it, and the daemon
// reads both off the claim's machine where the engine writes the record (workflow.Config.StepAgent):
// the session the machine published, and the model that answered last in that session. A fact the
// boot applies before attach reads nothing, and so does a claim this daemon does not supervise.
func TestTheDaemonReadsAStepsAgentFromItsClaimsMachine(t *testing.T) {
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	w := &workflowRuntime{log: quietLogger()}
	tester := mustClaimToken(t, "LEGION-208", claim.RoleTester)
	if got := w.stepAgent(tester); got != (workflow.StepAgent{}) {
		t.Fatalf("the agent read before attach = %+v, want nothing", got)
	}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: tester, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, State: supervise.StateWorking, Session: "ses_tester"}, "")
	if err != nil {
		t.Fatalf("create the tester's claim: %v", err)
	}
	if got := w.stepAgent(tester); got != (workflow.StepAgent{Session: "ses_tester"}) {
		t.Fatalf("the agent read before any answer = %+v, want its session and no model", got)
	}
	if err := machine.Handle(ctx, supervise.StreamAnswered{Claim: tester, Model: "second/second-model"}); err != nil {
		t.Fatalf("the tester's answer: %v", err)
	}
	if got := w.stepAgent(tester); got != (workflow.StepAgent{Session: "ses_tester", Model: "second/second-model"}) {
		t.Fatalf("the agent read after its answer = %+v, want its session and the model that answered", got)
	}
	if got := w.stepAgent(mustClaimToken(t, "LEGION-208", claim.RoleReviewer)); got != (workflow.StepAgent{}) {
		t.Fatalf("the agent read of a claim this daemon does not supervise = %+v, want nothing", got)
	}
}
