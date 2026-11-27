package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// forwardStart is the start of task x's second step, `tester:k`, after x's implementer passed on
// LEGION-208, and the report of x's implementer that start's task is told (handoffReport): the
// implementer's phases row holds x's handoff, which the pass reads. Task y, `tester:k`, holds k at
// its step where busy, and x's tester waits for k until y passes; else y is done and x's tester
// starts at once. z, which needs x and y, is a reviewer's.
func forwardStart(t *testing.T, busy bool) (record.SuperviseRequest, string) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	y := planTask{id: "y", steps: []string{"tester:k"}, agents: []claim.Agent{"k"}, state: record.TaskDone, place: atStep(1)}
	if busy {
		y.state = record.TaskRunning
	}
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "x", steps: []string{"implementer", "tester:k"}, agents: []claim.Agent{"implementer", "k"}, state: record.TaskRunning, place: atStep(1)},
		y,
		planTask{id: "z", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"x", "y"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "x", "legion/"+root.Key+".x")
	seedTaskPR(t, pool, engine, root.Key, "x", 45)
	handoff := record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: agentClaim(t, root.Key, "implementer"),
		HandoffRecord: "implement-x", HandoffVersion: 1, HandoffCommit: "head-x", StepTask: "x", StepPlace: 1}
	seedPhase(t, pool, handoff)

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")
	if busy {
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atStep(2), record.StepWaitAgent)
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleTester); len(starts) != 0 {
			t.Fatalf("the tester's starts of %s after x's pass = %+v, want none: agent k does y's step", root.Key, starts)
		}
		taskMoves(t, pool, engine, root.Key, "y", TriggerTesterPassed, "")
		joinsEnd(t, pool, engine, root.Key)
	}
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atStep(2), record.StepWaitNone)
	start := newestStart(t, pool, root.Key, claim.RoleTester)
	assertStartOf(t, start, root.Key, "x", "k")
	return start, handoffReport(root, handoff)
}

// A move forward whose next step waits for its agent keeps the report it tells that step until the
// agent is free: task x, `implementer tester:k`, passes its implementer, whose handoff the
// implementer's phases row holds, and its tester's step waits for k, which does task y's step; y
// passes, and x's tester starts for k. That start is told word for word what the start of the same
// move is told where k was free: the implementer's report (Handoff:, Handoff commit:) in its place,
// before the pull request, which the tester reads to know which handoff and which head to test.
//
// The fault it catches is a move forward that, once it has waited for its agent, starts its step
// told nothing of the step before it (its control: the same move where k is free, whose start is
// told the report at once).
func TestAMoveForwardWhoseStepWaitedForItsAgentKeepsTheReportOfTheStepBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy bool
	}{
		{"its agent free (the control)", false},
		{"its agent busy with another task's step", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, report := forwardStart(t, tc.busy)
			if report == "" || !strings.Contains(start.Task, report+" Pull request: #45.") {
				t.Fatalf("x's tester is told %q,\nwant the implementer's report %q in its place, before the pull request", start.Task, report)
			}
		})
	}
	at, _ := forwardStart(t, false)
	waited, _ := forwardStart(t, true)
	if waited.Task != at.Task {
		t.Fatalf("the start after the wait for k is told %q,\nwant what the start where k was free is told: %q", waited.Task, at.Task)
	}
}
