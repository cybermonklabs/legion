package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The record of a step names its agent's session and the model that answered it, and the daemon
// reads both off the claim's machine where the engine writes the record (workflow.Config.StepAgent):
// the session the machine published, and the model that answered last in that session. Two claims
// of one issue each read as their own, so a record of one never names the other's session or
// model. A fact the boot applies before attach reads nothing, and so does a claim this daemon does
// not supervise.
func TestTheDaemonReadsAStepsAgentFromItsClaimsMachine(t *testing.T) {
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	w := &workflowRuntime{log: quietLogger()}
	tester := mustClaimToken(t, "LEGION-208", claim.RoleTester)
	reviewer := mustClaimToken(t, "LEGION-208", claim.RoleReviewer)
	if got := w.stepAgent(tester); got != (workflow.StepAgent{}) {
		t.Fatalf("the agent read before attach = %+v, want nothing", got)
	}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	// Each claim works at a launch of its own, as a working claim always does, and the stream
	// stamps each answer with the generation of the launch that read it (stream.Answered).
	supervised := func(token claim.Token, role claim.Role, session string, generation uint64) *supervise.Machine {
		t.Helper()
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: role, State: supervise.StateWorking, Session: session, Generation: generation}, "")
		if err != nil {
			t.Fatalf("create the %s's claim: %v", role, err)
		}
		if got := machine.Claim().Generation; got != generation {
			t.Fatalf("the %s's claim is at generation %d, want %d", role, got, generation)
		}
		return machine
	}
	answered := func(machine *supervise.Machine, token claim.Token, model string, generation uint64) {
		t.Helper()
		if err := machine.Handle(ctx, supervise.StreamAnswered{Claim: token, Model: model, Generation: generation}); err != nil {
			t.Fatalf("the answer of %s: %v", token, err)
		}
	}
	testerMachine := supervised(tester, claim.RoleTester, "ses_tester", 3)
	if got := w.stepAgent(tester); got != (workflow.StepAgent{Session: "ses_tester"}) {
		t.Fatalf("the agent read before any answer = %+v, want its session and no model", got)
	}
	reviewerMachine := supervised(reviewer, claim.RoleReviewer, "ses_reviewer", 5)
	answered(testerMachine, tester, "second/tester-model", 3)
	answered(reviewerMachine, reviewer, "second/reviewer-model", 5)
	// An answer read at a launch the tester's claim has not reached (the reviewer's generation) is
	// not the tester's model.
	answered(testerMachine, tester, "second/not-the-testers-launch", 5)
	for token, want := range map[claim.Token]workflow.StepAgent{
		tester:   {Session: "ses_tester", Model: "second/tester-model"},
		reviewer: {Session: "ses_reviewer", Model: "second/reviewer-model"},
	} {
		if got := w.stepAgent(token); got != want {
			t.Errorf("the agent read of %s after both answered = %+v, want its own session and model %+v", token, got, want)
		}
	}
	if got := w.stepAgent(mustClaimToken(t, "LEGION-208", claim.RoleImplementer)); got != (workflow.StepAgent{}) {
		t.Fatalf("the agent read of a claim this daemon does not supervise = %+v, want nothing", got)
	}
}

// A claim recording a session another claim recorded first, as a claim the boot could not suspend
// does, has no session named on its step's record (stepAgent, plan E's t-e-54): the record shows
// Dispatch who works the step, and that session is not this claim's, as the role-holder
// publication leaves it out. A claim holding its own session is named as before.
//
// The control (the pull request records the run): stepAgent with no hold check names ses_taken.
func TestAStepsAgentNamesNoSessionItsClaimDoesNotHold(t *testing.T) {
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	holder := mustClaimToken(t, "LEGION-943", claim.RoleImplementer)
	if _, err := sup.deps.Store.HoldSessions(ctx, "legion", holder, supervise.RecordedHolds("ses_taken", "")); err != nil {
		t.Fatalf("hold ses_taken for %s, the claim that recorded it first: %v", holder, err)
	}
	w := &workflowRuntime{log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	for _, c := range []struct {
		role    claim.Role
		session string
		want    workflow.StepAgent
	}{
		{claim.RoleTester, "ses_taken", workflow.StepAgent{}},
		{claim.RoleReviewer, "ses_own", workflow.StepAgent{Session: "ses_own"}},
	} {
		token := mustClaimToken(t, "LEGION-208", c.role)
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: c.role, State: supervise.StateWorking, Session: c.session, Generation: 1}, ""); err != nil {
			t.Fatalf("create the %s's claim: %v", c.role, err)
		}
		if got := w.stepAgent(token); got != c.want {
			t.Fatalf("the step agent of the %s recording %s = %+v, want %+v", c.role, c.session, got, c.want)
		}
	}
}

// An answer whose model no store can keep reaches the daemon as an Answered that names no model
// (stream.Conn's answeredBy), and the daemon reads it as the claim's last answer like any other: the
// record of the step names the session and no model, never the model that answered before it.
func TestAnAnswerWithNoModelLeavesTheStepWithNone(t *testing.T) {
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	w := &workflowRuntime{log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	planner := mustClaimToken(t, "LEGION-208", claim.RolePlanner)
	const generation = 3
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RolePlanner, State: supervise.StateWorking, Session: "ses_planner", Generation: generation}, "")
	if err != nil {
		t.Fatalf("create the planner's claim: %v", err)
	}
	answered := func(ev stream.Answered) {
		t.Helper()
		mapped, err := superviseEvent(ev)
		if err != nil {
			t.Fatalf("map %#v: %v", ev, err)
		}
		if err := machine.Handle(ctx, mapped); err != nil {
			t.Fatalf("the answer %#v: %v", ev, err)
		}
	}
	answered(stream.Answered{Claim: planner, Provider: "first", Model: "first-model", Generation: generation})
	if got := w.stepAgent(planner); got != (workflow.StepAgent{Session: "ses_planner", Model: "first/first-model"}) {
		t.Fatalf("the agent read after the first answer = %+v, want its session and first/first-model", got)
	}
	answered(stream.Answered{Claim: planner, Generation: generation})
	if got := w.stepAgent(planner); got != (workflow.StepAgent{Session: "ses_planner"}) {
		t.Fatalf("the agent read after an answer naming no model = %+v, want its session and no model: first/first-model did not answer last", got)
	}
}

// The model and the session reach the daemon from the agent's own frames, and the record of a step
// takes neither unless it is in the form Dispatch holds a step's agent text to
// (steprecord.AgentTextHolds): printable ASCII with no space, at most 128 characters. One outside it
// is left out where the daemon reads it, so the end names no model, or no session, and is still
// written whole: it never shifts the columns of `dispatch steps`, forges a line of it, or has
// Dispatch refuse the record. A model and a session inside the form are kept, which is the control.
func TestAStepsAgentLeavesOutTextOutsideTheForm(t *testing.T) {
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	w := &workflowRuntime{log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	const generation = 3
	for i, tc := range []struct {
		name, session, model string
		want                 workflow.StepAgent
	}{
		// Each row's claim records a session of its own: a session another claim recorded first is
		// held to that claim, and its record would name none (TestAStepsAgentNamesNoSessionItsClaimDoesNotHold).
		{"both inside the form", "ses_planner_1", "first/first-model", workflow.StepAgent{Session: "ses_planner_1", Model: "first/first-model"}},
		{"a model with a newline", "ses_planner_2", "first/model\ndefault 9 step forged", workflow.StepAgent{Session: "ses_planner_2"}},
		{"a model with spaces", "ses_planner_3", "first/model with spaces", workflow.StepAgent{Session: "ses_planner_3"}},
		{"a model over the bound", "ses_planner_4", "first/" + strings.Repeat("m", 123), workflow.StepAgent{Session: "ses_planner_4"}},
		{"a session with a space", "ses planner", "first/first-model", workflow.StepAgent{Model: "first/first-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := fmt.Sprintf("LEGION-%d", 300+i)
			planner := mustClaimToken(t, issue, claim.RolePlanner)
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue, Issue: issue,
				Role: claim.RolePlanner, State: supervise.StateWorking, Session: tc.session, Generation: generation}, "")
			if err != nil {
				t.Fatalf("create the planner's claim: %v", err)
			}
			if err := machine.Handle(ctx, supervise.StreamAnswered{Claim: planner, Model: tc.model, Generation: generation}); err != nil {
				t.Fatalf("the answer of %s: %v", planner, err)
			}
			if got := w.stepAgent(planner); got != tc.want {
				t.Fatalf("the agent read of session %q and model %q = %+v, want %+v", tc.session, tc.model, got, tc.want)
			}
		})
	}
}
