package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldRetroStart is the start of the retro of task b, `implementer:k reviewer`, the task that
// merges, which LEGION-208's hold before its retro held for its child LEGION-209 (StartHeldRetro),
// and the report of b's reviewer that start's task is told (handoffReport): the reviewer's phases
// row holds b's handoff, and b's approved review passes its last step while LEGION-209 is in the
// workflow beneath LEGION-208, so b is held at its retro; LEGION-209 then leaves done. Task a,
// `implementer:k`, which b needs, holds k at its step where busy, until a passes; else a is done.
func heldRetroStart(t *testing.T, busy bool) (record.SuperviseRequest, string) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	a := planTask{id: "a", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskDone, place: atStep(1)}
	if busy {
		a.state = record.TaskRunning
	}
	seedTasks(t, pool, root.Key, root.Generation, a,
		planTask{id: "b", steps: []string{"implementer:k", "reviewer"}, agents: []claim.Agent{"k", "reviewer"}, needs: []string{"a"},
			state: record.TaskRunning, place: atStep(2)})
	last := record.PhaseRow{Issue: root.Key, Role: claim.RoleReviewer, Claim: agentClaim(t, root.Key, "reviewer"),
		HandoffRecord: "review-b", HandoffVersion: 1, HandoffCommit: "head-b", StepTask: "b", StepPlace: 2}
	seedPhase(t, pool, last)
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	retro := record.TaskPlace{Kind: record.PlaceRetro}

	taskMoves(t, pool, engine, root.Key, "b", TriggerReviewApproved, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskHeld, retro)
	childLeavesDone(t, pool, engine)
	if busy {
		// a still runs, so LEGION-208 stands where a does, held by nothing (issuePhaseOfTasks), and
		// no release of a hold before a retro reaches it: b stays held, and no retro starts.
		assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskHeld, retro)
		if issue := issueOf(t, pool, engine, root.Key); record.HeldForChildren(issue) {
			t.Fatalf("%s while a runs stands in %s with hold %+v, want no hold for its children: a running task decides where it stands", root.Key, issue.Phase, issue.Hold)
		}
		if starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(starts) != 0 {
			t.Fatalf("the retro starts of %s while a runs = %+v, want none", root.Key, starts)
		}
		taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
		joinsEnd(t, pool, engine, root.Key)
	}
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskRunning, retro)
	if wait := tasks["b"].StepWait; wait != record.StepWaitNone {
		t.Fatalf("b's retro waits for %q, want nothing", wait)
	}
	starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro)
	if len(starts) != 1 {
		t.Fatalf("the retro starts of %s = %+v, want one", root.Key, starts)
	}
	assertStartOf(t, starts[0], root.Key, "b", "k")
	return starts[0], handoffReport(root, last)
}

// The release of a parent held before its retro never starts a retro that waits for its agent:
// the retro's agent is the agent of the task's last implementation step, and another task of the
// issue that holds that agent is a running task, which decides where the issue stands, so the
// issue is held for its children by no task while it runs, and nothing releases the hold; the task
// that ran ends, the issue stands held, and the release starts the retro with its agent free. Task
// b, `implementer:k reviewer`, the task that merges, is held at its retro for LEGION-209; a,
// `implementer:k`, does its step for k when LEGION-209 leaves done, and b stays held while a runs;
// a passes, and b's retro starts at once for k, told the reviewer's report (Handoff:, Handoff
// commit:) word for word as where a was done before LEGION-209 left.
//
// It holds that StartHeldRetro's start needs nothing kept through a wait for its agent (its
// control: the same release where a is done, whose retro starts at once).
func TestAHeldRetroStartsOnlyWithItsAgentFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy bool
	}{
		{"its agent free (the control)", false},
		{"its agent busy with another task's step", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, report := heldRetroStart(t, tc.busy)
			if report == "" || !strings.Contains(start.Task, report) {
				t.Fatalf("b's retro is told %q,\nwant the reviewer's report %q", start.Task, report)
			}
		})
	}
	at, _ := heldRetroStart(t, false)
	waited, _ := heldRetroStart(t, true)
	if waited.Task != at.Task {
		t.Fatalf("the retro started once a passed is told %q,\nwant what the retro started at once is told: %q", waited.Task, at.Task)
	}
}
