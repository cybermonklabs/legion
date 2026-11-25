package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// planSteps is the plan rows of task a for steps, a plan's steps, each with the type at its place,
// and each step's agent named as the daemon names it when it stores a plan (NameAgents), since a
// stored step's name is read from that column alone.
func planSteps(t *testing.T, steps string, types ...plan.Type) []PlanStep {
	t.Helper()
	parsed, err := plan.ParseSteps(steps)
	if err != nil {
		t.Fatalf("ParseSteps(%s): %v", steps, err)
	}
	if len(types) != len(parsed) {
		t.Fatalf("%s has %d steps and %d types", steps, len(parsed), len(types))
	}
	rows := make([]PlanStep, len(parsed))
	for i, step := range parsed {
		rows[i] = PlanStep{Task: "a", Position: i + 1, Role: step.Role, AgentKey: step.Key, Type: types[i]}
	}
	return NameAgents(nil, nil, nil, rows)
}

// The one rule of who works a place of a task. At a step it is the step's own role and agent. The
// retro of a task is done by the agent of its last step of the type implementation, as the role of
// that step, and where the task has no such step, or that step's key is owner, by the owner, on the
// implementer's row. The rule reads each step's type, never its role's name: a coder of the type
// implementation does its task's retro, and a task whose only step is of the type testing has the
// owner's. The merge and a step the task does not hold have no worker.
func TestOneRuleNamesWhoWorksEachPlaceOfATask(t *testing.T) {
	retro := TaskPlace{Kind: PlaceRetro}
	step := func(n int) TaskPlace { return TaskPlace{Kind: PlaceStep, Position: n} }
	owner := plan.Agent{Key: "owner"}
	for _, tc := range []struct {
		name      string
		steps     []PlanStep
		place     TaskPlace
		ok        bool
		role      claim.Role
		agent     plan.Agent
		at        int
		claimRole claim.Role
		named     claim.Agent
	}{
		{"a step with no key", planSteps(t, "implementer tester", plan.TypeImplementation, plan.TypeTesting), step(2),
			true, "tester", plan.Agent{Task: "a", Place: 2}, 2, "tester", "tester"},
		{"a step of the key owner", planSteps(t, "implementer:owner tester", plan.TypeImplementation, plan.TypeTesting), step(1),
			true, "implementer", owner, 1, claim.RoleArchitect, ""},
		{"a fresh agent of acceptance", planSteps(t, "implementer acceptance", plan.TypeImplementation, plan.TypeReview), step(2),
			true, "acceptance", plan.Agent{Task: "a", Place: 2}, 2, "acceptance", "acceptance"},
		{"the retro of the implementer", planSteps(t, "implementer tester", plan.TypeImplementation, plan.TypeTesting), retro,
			true, "implementer", plan.Agent{Task: "a", Place: 1}, 1, "implementer", "implementer"},
		{"the retro of the last implementation step", planSteps(t, "implementer:x reviewer implementer:y", plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation), retro,
			true, "implementer", plan.Agent{Key: "y"}, 3, "implementer", "implementer-2"},
		{"the retro of a task with no implementation step", planSteps(t, "tester", plan.TypeTesting), retro,
			true, "implementer", owner, 0, claim.RoleArchitect, ""},
		{"the retro of an implementer of the key owner", planSteps(t, "implementer:owner reviewer", plan.TypeImplementation, plan.TypeReview), retro,
			true, "implementer", owner, 1, claim.RoleArchitect, ""},
		{"the retro after a later step of the key owner", planSteps(t, "implementer reviewer implementer:owner", plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation), retro,
			true, "implementer", owner, 3, claim.RoleArchitect, ""},
		{"the retro of a coder of the type implementation", planSteps(t, "coder checker", plan.TypeImplementation, plan.TypeTesting), retro,
			true, "coder", plan.Agent{Task: "a", Place: 1}, 1, "coder", "coder"},
		{"the retro of a checker alone", planSteps(t, "checker", plan.TypeTesting), retro,
			true, "implementer", owner, 0, claim.RoleArchitect, ""},
		{"the merge", planSteps(t, "implementer", plan.TypeImplementation), TaskPlace{Kind: PlaceMerge}, false, "", plan.Agent{}, 0, "", ""},
		{"no act", planSteps(t, "implementer", plan.TypeImplementation), TaskPlace{}, false, "", plan.Agent{}, 0, "", ""},
		{"a step the task does not hold", planSteps(t, "implementer", plan.TypeImplementation), step(2), false, "", plan.Agent{}, 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, ok := PlaceWorker("a", tc.place, tc.steps)
			if ok != tc.ok || worker.Role != tc.role || worker.Agent != tc.agent || worker.Step != tc.at {
				t.Fatalf("PlaceWorker(%+v) = (%+v, %t), want ({Role:%s Agent:%+v Step:%d}, %t)", tc.place, worker, ok, tc.role, tc.agent, tc.at, tc.ok)
			}
			if !ok {
				return
			}
			if got := worker.ClaimRole(); got != tc.claimRole {
				t.Errorf("ClaimRole = %q, want %q", got, tc.claimRole)
			}
			if got := worker.Name(tc.steps); got != tc.named {
				t.Errorf("Name = %q, want %q", got, tc.named)
			}
			if got, want := worker.Owner(), tc.agent == owner; got != want {
				t.Errorf("Owner = %t, want %t", got, want)
			}
		})
	}
}

// A failed step's work returns to the first step before it of the type implementation, read from
// the types its rows record, and nowhere where none stands before it: a step of a role the
// deployment defines may stand before its task's implementer. A position that is no step of the
// task returns nowhere either.
func TestAFailedStepReturnsToTheFirstImplementationStepBeforeIt(t *testing.T) {
	steps := planSteps(t, "checker coder reviewer coder:x tester", plan.TypeTesting, plan.TypeImplementation, plan.TypeReview, plan.TypeImplementation, plan.TypeTesting)
	for _, tc := range []struct {
		position int
		want     int
		ok       bool
	}{
		{5, 2, true},
		{3, 2, true},
		{2, 0, false},
		{1, 0, false},
		{0, 0, false},
		{6, 0, false},
	} {
		if got, ok := ReturnTarget(steps, tc.position); got != tc.want || ok != tc.ok {
			t.Errorf("ReturnTarget(step %d) = (%d, %t), want (%d, %t)", tc.position, got, ok, tc.want, tc.ok)
		}
	}
}

// The owner's report of a step finds it running exactly where the task has plan rows, runs, stands
// at the place the report names, the issue stands, unheld, in the phase of that place, and the
// owner does it. Its refusal, OWNER_STEP_NOT_RUNNING, names who works the place where it does not
// hold.
func TestTheOwnersStepRunsOnlyWhereTheOwnerDoesThePlaceTheTaskRunsAt(t *testing.T) {
	owned := planSteps(t, "implementer:owner tester", plan.TypeImplementation, plan.TypeTesting)
	checked := planSteps(t, "checker", plan.TypeTesting)
	for _, tc := range []struct {
		name   string
		phase  phase.Phase
		stored bool
		state  TaskState
		step   int
		place  TaskPlace
		steps  []PlanStep
		runs   bool
	}{
		{"the owner's step that runs", phase.Implementing, true, TaskRunning, 1, ReportedPlace(1), owned, true},
		{"a step of another agent", phase.Testing, true, TaskRunning, 2, ReportedPlace(2), owned, false},
		{"another step than the task's", phase.Testing, true, TaskRunning, 1, ReportedPlace(2), owned, false},
		{"a held issue", phase.Held, true, TaskRunning, 1, ReportedPlace(1), owned, false},
		{"a stopped task", phase.Implementing, true, TaskStopped, 1, ReportedPlace(1), owned, false},
		{"no plan rows", phase.Implementing, false, "", 1, TaskPlace{}, nil, false},
		{"the owner's retro", phase.Retro, true, TaskRunning, 0, ReportedPlace(0), checked, true},
		{"the merge", phase.Merging, true, TaskRunning, 0, TaskPlace{Kind: PlaceMerge}, checked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: tc.phase}
			if got := OwnerStepRuns(issue, "a", tc.stored, tc.state, tc.step, tc.place, tc.steps); got != tc.runs {
				t.Fatalf("OwnerStepRuns = %t, want %t", got, tc.runs)
			}
			if tc.runs {
				return
			}
			if says := OwnerStepNotRunning(issue, "a", tc.stored, tc.state, tc.step, tc.place, tc.steps); says == "" {
				t.Errorf("OwnerStepNotRunning wrote no refusal for a step that does not run")
			}
		})
	}
}
