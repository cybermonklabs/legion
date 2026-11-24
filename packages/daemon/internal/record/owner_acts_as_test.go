package record

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// ownersPlanOf is the plan of issue at generation written as steps (`role` or `role:owner`), its one
// task a running at place, each step of its built-in role's type (plan.BuiltinRoles).
func ownersPlanOf(issue string, generation uint64, steps string, place TaskPlace) (PlanVersion, []PlanTask, []PlanStep) {
	v := PlanVersion{Issue: issue, Generation: generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
	tasks := []PlanTask{{Issue: issue, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: TaskRunning, Place: place}}
	types := plan.BuiltinRoles()
	var rows []PlanStep
	for i, step := range strings.Fields(steps) {
		role, key, _ := strings.Cut(step, ":")
		rows = append(rows, PlanStep{Issue: issue, Generation: generation, Task: "a", Position: i + 1, Role: role, Type: types[role].Type, AgentKey: key})
	}
	return v, tasks, rows
}

// With no merge forward asked of it, the owner's slot acts as the role of the step it does, and
// only then: the issues are read back from the store, so the role the slot acts as is decided on
// the issue's own plan rows. `implementer:owner reviewer` at step 1 acts as the implementer;
// `implementer reviewer:owner` at step 2 as the reviewer, and at step 1, a step of another agent,
// as nothing; `implementer acceptance:owner` at accepting as acceptance. The owner's retro is done
// as the role of the task's last step of the type implementation where that step is the owner's: so
// `implementer:owner reviewer` acts as the implementer at its retro, the App that opened its pull
// request, and the same retro held acts as nothing. Where the task has no such step the owner does
// the retro as itself, so `tester` and `acceptance:owner` act as nothing there; where another agent
// does that step, `implementer reviewer`, the retro is that agent's and the owner acts as nothing.
// An issue with no plan rows acts as nothing in accepting; and the owner's implementer step while
// the issue is held from it acts as nothing, since the owner does no step then.
func TestTheOwnersSlotActsAsTheStepItDoes(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	step := func(position int) TaskPlace { return TaskPlace{Kind: PlaceStep, Position: position} }
	type planned struct {
		steps string
		place TaskPlace
	}
	cases := []struct {
		key    string
		phase  phase.Phase
		hold   *Hold
		plan   *planned
		want   claim.Role
		acting bool
	}{
		{"LEGION-301", phase.Implementing, nil, &planned{"implementer:owner reviewer", step(1)}, claim.RoleImplementer, true},
		{"LEGION-302", phase.Reviewing, nil, &planned{"implementer reviewer:owner", step(2)}, claim.RoleReviewer, true},
		{"LEGION-303", phase.Implementing, nil, &planned{"implementer reviewer:owner", step(1)}, "", false},
		{"LEGION-304", phase.Retro, nil, &planned{"tester", TaskPlace{Kind: PlaceRetro}}, "", false},
		{"LEGION-308", phase.Retro, nil, &planned{"implementer:owner reviewer", TaskPlace{Kind: PlaceRetro}}, claim.RoleImplementer, true},
		{"LEGION-309", phase.Retro, nil, &planned{"acceptance:owner", TaskPlace{Kind: PlaceRetro}}, "", false},
		{"LEGION-310", phase.Retro, nil, &planned{"implementer reviewer", TaskPlace{Kind: PlaceRetro}}, "", false},
		{"LEGION-311", phase.Held, &Hold{From: phase.Retro, Reason: HoldEscalated}, &planned{"implementer:owner reviewer", TaskPlace{Kind: PlaceRetro}}, "", false},
		{"LEGION-305", phase.Accepting, nil, &planned{"implementer acceptance:owner", step(2)}, claim.RoleAcceptance, true},
		{"LEGION-306", phase.Accepting, nil, nil, "", false},
		{"LEGION-307", phase.Held, &Hold{From: phase.Implementing, Reason: HoldEscalated}, &planned{"implementer:owner reviewer", step(1)}, "", false},
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range cases {
			issue := issueFixture(tc.key)
			issue.Phase, issue.Hold = tc.phase, tc.hold
			must(t, records.PutIssue(ctx, tx, issue))
			if tc.plan != nil {
				v, tasks, steps := ownersPlanOf(issue.Key, issue.Generation, tc.plan.steps, tc.plan.place)
				putPlan(t, tx, v, tasks, steps)
			}
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range cases {
			issue, err := records.Issue(ctx, tx, tc.key)
			if err != nil || issue == nil {
				t.Fatalf("read %s: %+v, %v", tc.key, issue, err)
			}
			var steps []PlanStep
			if p, err := records.Plan(ctx, tx, tc.key, issue.Generation); err != nil {
				t.Fatalf("read the plan of %s: %v", tc.key, err)
			} else if p != nil {
				steps = p.Steps["a"]
			}
			if role, acting := OwnerActsAs(*issue, steps); role != tc.want || acting != tc.acting {
				where := "no plan rows"
				if tc.plan != nil {
					where = tc.plan.steps
				}
				t.Errorf("OwnerActsAs(%s on %s at %+v in %s) = (%q, %v), want (%q, %v)", tc.key, where, issue.StepRole, tc.phase, role, acting, tc.want, tc.acting)
			}
		}
	})
}
