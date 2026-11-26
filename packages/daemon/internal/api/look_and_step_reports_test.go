package api

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Each report of a look and of a step of one agent is recorded as what it is, in whichever order
// the agent took the two tasks. A merge's production check goes to its task's agent of the checking
// role (workflow's checkClaim), which can be the agent of a step of another task of the issue, so
// one claim holds a look's task and a step's task, one turn at a time. LEGION-208 is in testing,
// its tester's claim is that agent, and its production check, of an earlier merge of the run,
// stands open at look 2. In each order the first task's turn runs, the second task is given to the
// claim while it runs, the first task's report is made in that turn, and the second task's report
// in the second task's own turn after it:
//
//   - a step's task given while the look's turn runs: the look's report is a check report of look 2,
//     then the step's completion is the tester's phase completion;
//   - a look's task given while the step's turn runs: the step's completion is the tester's phase
//     completion, then the look's report is a check report of look 2.
//
// The look's report names its check (checkOpenedAt), so the route takes it as a check report
// whatever the claim serves when it arrives: the claim stops serving the look it retired as soon as
// a task of a phase is queued, and a route that read that state routed the look's report made in
// its own turn as a phase completion and refused it.
func TestEachReportOfALookAndAStepOfOneAgentIsRecordedAsWhatItIs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lookFirst bool
	}{
		{"a step's task given while the look's turn runs", true},
		{"a look's task given while the step's turn runs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			machine, ok := h.supervisor.Machine(tester.token)
			if !ok {
				t.Fatal("no machine for the tester")
			}
			ctx := context.Background()
			generation := issueGeneration(t, h, "LEGION-208")
			look := supervise.RequestDeliver{Claim: tester.token, ID: "outbox:look", Task: "Production check of LEGION-208.", Generation: generation,
				CheckLook: supervise.CheckLook{Look: 2, OpenedAt: checkOpenedAt}}
			step := supervise.RequestDeliver{Claim: tester.token, ID: "outbox:step", Task: "Continue Workflow. Issue: LEGION-208. Phase: testing.",
				Generation: generation, Phase: phase.Testing}
			give := func(task supervise.RequestDeliver) {
				t.Helper()
				if err := machine.Handle(ctx, task); err != nil {
					t.Fatalf("give the tester %s: %v", task.ID, err)
				}
			}
			turnOf := func(id string) {
				t.Helper()
				if machine.Claim().State == supervise.StateWorking {
					if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
						t.Fatalf("end the tester's turn: %v", err)
					}
				}
				if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
					t.Fatalf("start the turn of %s: %v", id, err)
				}
			}
			reportsTheLook := func() {
				t.Helper()
				completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "the deploy has not carried the merge",
					Verdict: "pending", WakeAt: &checkWakeAt, CheckOpenedAt: &checkOpenedAt}))
				want := intake.CheckReport{Issue: "LEGION-208", Claim: tester.token, Look: 2, Verdict: "pending",
					Summary: "the deploy has not carried the merge", WakeAt: &checkWakeAt, OpenedAt: checkOpenedAt}
				if got := lastCheckReport(t, facts); !sameCheckReport(got, want) {
					t.Fatalf("the look's report was recorded as %+v, want %+v", got, want)
				}
			}
			completesTheStep := func() {
				t.Helper()
				completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
					HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
				if got := lastCompletion(t, facts); got.Role != claim.RoleTester || got.Issue != "LEGION-208" {
					t.Fatalf("the step's completion was recorded as %+v, want the tester's phase completion of LEGION-208", got)
				}
			}

			if tc.lookFirst {
				give(look)
				turnOf(look.ID)
				give(step)
				reportsTheLook()
				turnOf(step.ID)
				completesTheStep()
				return
			}
			give(step)
			turnOf(step.ID)
			give(look)
			completesTheStep()
			turnOf(look.ID)
			reportsTheLook()
		})
	}
}
