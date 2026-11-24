package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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

// An issue reads the running step its task stands at, from its plan rows, with the issue: the
// plan `implementer checker` at its second step reads checker at step through Issue, Issues,
// LiveIssues, SlottedIssues and TreeIssues alike, and RunningAt(step) answers it. An issue held from
// that step, its task running under the issue's hold, reads it too, held, so RunningAt of the phase
// its hold left answers checker while RunningAt(held) answers none. An issue with no plan rows of
// its run reads none at step, and so does one whose rows are of another generation. An issue at
// step keeps its slot and is at work.
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
		v, tasks, steps = checkerPlan(held.Key, held.Generation, TaskRunning, atStep)
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
					if got := roleAtStep(issue); got != wanted {
						t.Errorf("%s reads %s with the role %q at step, want %q", name, issue.Key, got, wanted)
					}
				}
			}
			if len(seen) != len(want) {
				t.Errorf("%s read %d of the %d issues, so it proves nothing of the others: %v", name, len(seen), len(want), seen)
			}
		}

		read, err := records.Issue(ctx, tx, running.Key)
		must(t, err)
		if got := roleAtStep(*read); got != "checker" {
			t.Errorf("RunningAt(step) of %s names %q, want checker", running.Key, got)
		}
		if got := read.RunningAt(phase.Implementing); len(got) != 0 {
			t.Errorf("RunningAt(implementing) of %s = %+v, want none: its task stands at step 2", running.Key, got)
		}
		if !KeepsSlot(*read) || !AtWork(*read, TreeGate{}) {
			t.Errorf("an issue at step keeps its slot %t and is at work %t, want both", KeepsSlot(*read), AtWork(*read, TreeGate{}))
		}
		heldRead, err := records.Issue(ctx, tx, held.Key)
		must(t, err)
		if got := heldRead.RunningAt(phase.Held); len(got) != 0 {
			t.Errorf("RunningAt(held) of %s = %+v, want none: no step runs at a hold", held.Key, got)
		}
		if got := heldRead.RunningAt(heldRead.Hold.From); len(got) != 1 || got[0].Role != "checker" || !got[0].Held {
			t.Errorf("RunningAt(%s) of %s, held from it, = %+v, want the checker's step, held", heldRead.Hold.From, held.Key, got)
		}
		if !KeepsSlot(*heldRead) {
			t.Errorf("an issue held from step keeps no slot, want it to keep the slot of step")
		}
	})
}

// roleAtStep is the role of issue's first running step at step, "" where none runs there.
func roleAtStep(issue Issue) claim.Role {
	if at := issue.RunningAt(phase.Step); len(at) > 0 {
		return at[0].Role
	}
	return ""
}
