package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// checkerPlan is the plan `implementer checker` of issue at generation, its one task a in state at
// place: the implementer at step 1, then checker, a role of the deployment, at step 2.
func checkerPlan(issue string, generation uint64, state TaskState, place TaskPlace) (PlanVersion, []PlanTask, []PlanStep) {
	v := PlanVersion{Issue: issue, Generation: generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
	tasks := []PlanTask{{Issue: issue, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: state, Place: place}}
	steps := []PlanStep{
		{Issue: issue, Generation: generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: issue, Generation: generation, Task: "a", Position: 2, Role: "checker", Type: "review"},
	}
	return v, tasks, steps
}

// An issue reads the role of the step its task stands at, from its plan rows, with the issue: the
// plan `implementer checker` at its second step reads checker through Issue, Issues, LiveIssues,
// SlottedIssues and TreeIssues alike, and RoleAt(step) answers it. An issue held from that step
// reads it too, so RoleAt of the phase its hold left answers checker while RoleAt(held) answers
// none. An issue with no plan rows of its run reads none, and so does one whose rows are of another
// generation. An issue at step keeps its slot and is at work.
func TestAnIssueReadsTheRoleOfItsRunningStep(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	running := issueFixture("LEGION-208")
	running.Phase = phase.Step
	held := issueFixture("LEGION-209")
	held.Phase, held.Hold = phase.Held, &Hold{From: phase.Step, Reason: HoldEscalated}
	bare := issueFixture("LEGION-210")
	earlier := issueFixture("LEGION-211")
	inTx(t, st, func(tx pgx.Tx) {
		for i, issue := range []Issue{running, held, bare, earlier} {
			must(t, records.PutIssue(ctx, tx, issue))
			must(t, records.PutSlot(ctx, tx, Slot{Issue: issue.Key, Index: i, AdmittedAt: planGivenAt}))
		}
		atStep := TaskPlace{Kind: PlaceStep, Position: 2}
		v, tasks, steps := checkerPlan(running.Key, running.Generation, TaskRunning, atStep)
		putPlan(t, tx, v, tasks, steps)
		v, tasks, steps = checkerPlan(held.Key, held.Generation, TaskStopped, atStep)
		putPlan(t, tx, v, tasks, steps)
		v, tasks, steps = checkerPlan(earlier.Key, earlier.Generation-1, TaskRunning, atStep)
		putPlan(t, tx, v, tasks, steps)
	})

	want := map[string]claim.Role{running.Key: "checker", held.Key: "checker", bare.Key: "", earlier.Key: ""}
	inTx(t, st, func(tx pgx.Tx) {
		reads := map[string]func() ([]Issue, error){
			"Issues":        func() ([]Issue, error) { return records.Issues(ctx, tx) },
			"LiveIssues":    func() ([]Issue, error) { return records.LiveIssues(ctx, tx, running.Project) },
			"SlottedIssues": func() ([]Issue, error) { return records.SlottedIssues(ctx, tx, running.Project) },
			"Issue": func() ([]Issue, error) {
				var issues []Issue
				for key := range want {
					issue, err := records.Issue(ctx, tx, key)
					if err != nil || issue == nil {
						return nil, err
					}
					issues = append(issues, *issue)
				}
				return issues, nil
			},
			"TreeIssues": func() ([]Issue, error) {
				var issues []Issue
				for key := range want {
					tree, err := records.TreeIssues(ctx, tx, key)
					if err != nil {
						return nil, err
					}
					issues = append(issues, tree...)
				}
				return issues, nil
			},
		}
		for name, read := range reads {
			issues, err := read()
			must(t, err)
			seen := map[string]bool{}
			for _, issue := range issues {
				if wanted, ok := want[issue.Key]; ok {
					seen[issue.Key] = true
					if issue.StepRole != wanted {
						t.Errorf("%s reads %s with StepRole %q, want %q", name, issue.Key, issue.StepRole, wanted)
					}
				}
			}
			if len(seen) != len(want) {
				t.Errorf("%s read %d of the %d issues, so it proves nothing of the others: %v", name, len(seen), len(want), seen)
			}
		}

		read, err := records.Issue(ctx, tx, running.Key)
		must(t, err)
		if got := read.RoleAt(phase.Step); got != "checker" {
			t.Errorf("RoleAt(step) of %s = %q, want checker", running.Key, got)
		}
		if got := read.RoleAt(phase.Implementing); got != claim.RoleImplementer {
			t.Errorf("RoleAt(implementing) of %s = %q, want the implementer", running.Key, got)
		}
		if !KeepsSlot(*read) || !AtWork(*read, TreeGate{}) {
			t.Errorf("an issue at step keeps its slot %t and is at work %t, want both", KeepsSlot(*read), AtWork(*read, TreeGate{}))
		}
		heldRead, err := records.Issue(ctx, tx, held.Key)
		must(t, err)
		if got := heldRead.RoleAt(phase.Held); got != "" {
			t.Errorf("RoleAt(held) of %s = %q, want none: no role works a hold", held.Key, got)
		}
		if got := heldRead.RoleAt(heldRead.Hold.From); got != "checker" {
			t.Errorf("RoleAt(%s) of %s, held from it, = %q, want checker", heldRead.Hold.From, held.Key, got)
		}
		if !KeepsSlot(*heldRead) {
			t.Errorf("an issue held from step keeps no slot, want it to keep the slot of step")
		}
	})
}

// DeliveryHolds is the one choice of the judge of what the supervisor asks about: at step, with the
// checker's step running, a task of the implementer's step, which the task has left, holds no
// longer, and a start that brings the implementer back and hands it nothing holds, since it is
// judged by its phase and run alone (BroughtBackHolds). Such a start of an earlier run, or of a
// phase the issue has left, holds no longer; the checker's own task holds; and a claim of no
// recorded issue holds every delivery.
func TestDeliveryHoldsJudgesAStartThatBringsARoleBackByItsPhaseAndRunAlone(t *testing.T) {
	t.Parallel()
	atChecker := &Issue{Key: "LEGION-208", Phase: phase.Step, Generation: 3, StepRole: "checker"}
	for _, tc := range []struct {
		name  string
		issue *Issue
		role  claim.Role
		d     supervise.Delivery
		want  bool
	}{
		{name: "a task of a step the task has left", issue: atChecker, role: claim.RoleImplementer,
			d: supervise.Delivery{Phase: phase.Step, Generation: 3}, want: false},
		{name: "a start that brings back the role of a step the task has left", issue: atChecker, role: claim.RoleImplementer,
			d: supervise.Delivery{Phase: phase.Step, Generation: 3, BringsBack: true}, want: true},
		{name: "a start that brings a role back on an earlier run", issue: atChecker, role: claim.RoleImplementer,
			d: supervise.Delivery{Phase: phase.Step, Generation: 2, BringsBack: true}, want: false},
		{name: "a start that brings a role back to a phase the issue has left", issue: atChecker, role: claim.RoleImplementer,
			d: supervise.Delivery{Phase: phase.Implementing, Generation: 3, BringsBack: true}, want: false},
		{name: "a task of the running step", issue: atChecker, role: "checker",
			d: supervise.Delivery{Phase: phase.Step, Generation: 3}, want: true},
		{name: "a claim of no recorded issue", issue: nil, role: claim.RoleImplementer,
			d: supervise.Delivery{Phase: phase.Step, Generation: 3}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DeliveryHolds(tc.issue, tc.role, tc.d); got != tc.want {
				t.Fatalf("DeliveryHolds(%+v, %s, %+v) = %t, want %t", tc.issue, tc.role, tc.d, got, tc.want)
			}
		})
	}
}
