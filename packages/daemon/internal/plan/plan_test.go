package plan

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// oneTask is a plan of one task, id, whose block names steps in the grammar of ParseSteps; the
// empty steps are a block with no `steps` at all.
func oneTask(id, steps string) Plan {
	parsed, err := ParseSteps(steps)
	if err != nil {
		panic("oneTask(" + id + ", " + steps + "): " + err.Error())
	}
	return Plan{Tasks: []Task{{ID: id, Steps: parsed}}}
}

// defaultSteps is the project's default steps as the configuration ships them.
func defaultSteps(t *testing.T) []Step {
	t.Helper()
	steps, err := ParseSteps("planner implementer tester reviewer acceptance:owner")
	if err != nil {
		t.Fatalf("ParseSteps of the default steps: %v", err)
	}
	return steps
}

// A step is a role or a role and a key, each matching [a-z][a-z0-9-]*, with one space between
// two steps. A word with an upper-case letter, a second colon or an empty key is refused, so a
// plan that Legion takes names each role and each key in one spelling.
func TestParseSteps(t *testing.T) {
	got, err := ParseSteps("implementer:cd reviewer:r")
	if err != nil {
		t.Fatalf("ParseSteps: %v", err)
	}
	want := []Step{{Role: "implementer", Key: "cd"}, {Role: "reviewer", Key: "r"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSteps = %+v, want %+v", got, want)
	}
	if got, err := ParseSteps("planner tester"); err != nil || !reflect.DeepEqual(got, []Step{{Role: "planner"}, {Role: "tester"}}) {
		t.Fatalf("ParseSteps(planner tester) = %+v, %v", got, err)
	}
	if got, err := ParseSteps(""); err != nil || len(got) != 0 {
		t.Fatalf("ParseSteps of no word = %+v, %v, want no step", got, err)
	}
	for _, refused := range []string{"Implementer", "a::b", "implementer:", ":cd", "implementer 9tester", "implementer:cd:e"} {
		if got, err := ParseSteps(refused); err == nil {
			t.Errorf("ParseSteps(%q) = %+v, want a refusal", refused, got)
		} else if !strings.Contains(err.Error(), "[a-z][a-z0-9-]*") {
			t.Errorf("ParseSteps(%q) refusal %q names no grammar", refused, err)
		}
	}
}

// Each step has one agent. The steps of one key are one agent, in any task; a step with no key is
// an agent of that step alone, the same at each return to it; and the key owner is the owner of
// the task. The fault: a keyed step given a fresh agent, so implementer:a and tester:a run in two
// sessions.
func TestStepsOfOneKeyHaveOneAgent(t *testing.T) {
	steps, err := ParseSteps("implementer:a tester:a reviewer acceptance:owner")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]Agent, len(steps))
	for i, s := range steps {
		got[i] = AgentOf("t", i+1, s)
	}
	if got[0] != got[1] {
		t.Errorf("implementer:a and tester:a: agents %+v and %+v, want one", got[0], got[1])
	}
	if other := AgentOf("u", 2, Step{Role: "reviewer", Key: "a"}); other != got[0] {
		t.Errorf("reviewer:a of task u: agent %+v, want %+v, the agent of the key a in task t", other, got[0])
	}
	if got[2] == got[0] || got[2] != (Agent{Task: "t", Place: 3}) {
		t.Errorf("reviewer: agent %+v, want a fresh agent of step 3", got[2])
	}
	if again := AgentOf("t", 3, steps[2]); again != got[2] {
		t.Errorf("reviewer again: agent %+v, want %+v, the agent it had", again, got[2])
	}
	if !got[3].Owner() || got[0].Owner() || got[2].Owner() {
		t.Errorf("owner: %v, %v and %v, want only acceptance:owner", got[3].Owner(), got[0].Owner(), got[2].Owner())
	}
}

// The default steps walk today's line: the five steps' phases in order, then Legion's own retro
// and merge.
func TestLineOfTheDefaultStepsIsTodaysOrder(t *testing.T) {
	if got := Line(defaultSteps(t), true); !reflect.DeepEqual(got, phase.Order) {
		t.Fatalf("Line(default steps) = %v, want %v", got, phase.Order)
	}
	steps, _ := ParseSteps("implementer reviewer")
	want := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}
	if got := Line(steps, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer reviewer) = %v, want %v", got, want)
	}
}

// Only a task whose work merges by itself walks Legion's retro and merge after its steps: any
// other task ends at its last step. The fault it catches is a task that does not merge running a
// retro and a merge of its own (its control: Line appending them whatever merges says).
func TestALineOfATaskThatDoesNotMergeEndsAtItsLastStep(t *testing.T) {
	steps, _ := ParseSteps("implementer reviewer")
	if got, want := Line(steps, false), []phase.Phase{phase.Implementing, phase.Reviewing}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer reviewer, does not merge) = %v, want %v", got, want)
	}
	if got, want := Line(steps, true), []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer reviewer, merges) = %v, want %v", got, want)
	}
}

// Each of Legion's own roles of a step is one of five, with the type of work it does.
func TestBuiltinRolesAreLegionsFiveStepRoles(t *testing.T) {
	want := map[string]RoleDef{
		"planner":     {Name: "planner", Type: TypePlanning},
		"implementer": {Name: "implementer", Type: TypeImplementation},
		"tester":      {Name: "tester", Type: TypeTesting},
		"reviewer":    {Name: "reviewer", Type: TypeReview},
		"acceptance":  {Name: "acceptance", Type: TypeReview},
	}
	roles := BuiltinRoles()
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("BuiltinRoles = %+v, want %+v", roles, want)
	}
	delete(roles, "planner")
	if _, ok := BuiltinRoles()["planner"]; !ok {
		t.Fatal("a caller that changed one answer of BuiltinRoles changed the next")
	}
}

// How a step runs is decided by its role alone: each of Legion's five roles of a step runs as its
// own phase, and every other role, a role of the deployment, runs as the phase step. No type is
// read, so a role of the deployment whose type is review still runs as step.
func TestHowAStepRunsIsItsRolesAndNeverItsType(t *testing.T) {
	for role, want := range map[string]phase.Phase{
		"planner": phase.Planning, "implementer": phase.Implementing, "tester": phase.Testing,
		"reviewer": phase.Reviewing, "acceptance": phase.Accepting,
		"checker": phase.Step, "linter": phase.Step, "owner": phase.Step,
	} {
		if got := RunsAs(role); got != want {
			t.Errorf("RunsAs(%q) = %q, want %q", role, got, want)
		}
	}
	steps, _ := ParseSteps("implementer checker linter")
	want := []phase.Phase{phase.Implementing, phase.Step, phase.Step, phase.Retro, phase.Merging}
	if got := Line(steps, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer checker linter) = %v, want %v", got, want)
	}
}

// A task with neither steps nor a sub-owner runs the project's default steps, and a block
// whose `steps` is empty is the same as a block with none. Neither check refuses such a plan.
func TestATaskWithNoStepsRunsTheDefaultSteps(t *testing.T) {
	defaults := defaultSteps(t)
	if got := StepsOf(Task{ID: "a"}, defaults); !reflect.DeepEqual(got, defaults) {
		t.Fatalf("StepsOf(a task with no steps) = %+v, want the defaults %+v", got, defaults)
	}
	own, _ := ParseSteps("implementer reviewer")
	if got := StepsOf(Task{ID: "a", Steps: own}, defaults); !reflect.DeepEqual(got, own) {
		t.Fatalf("StepsOf(a task with steps) = %+v, want its own %+v", got, own)
	}
	if got := StepsOf(Task{ID: "f", Owner: "architect"}, defaults); got != nil {
		t.Fatalf("StepsOf(a task of a sub-owner) = %+v, want no step", got)
	}
	for _, attributes := range []map[string]string{nil, {"steps": ""}, {"steps": "  "}} {
		p, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: attributes, Text: "Do the change."}})
		if err != nil {
			t.Fatalf("FromBlocks(%v): %v", attributes, err)
		}
		if len(p.Tasks) != 1 || p.Tasks[0].NamesSteps() || p.Tasks[0].Steps != nil {
			t.Fatalf("FromBlocks(%v) = %+v, want one task with no steps", attributes, p.Tasks)
		}
		if r := Supported(p, Deployment{Roles: BuiltinRoles()}); len(r) != 0 {
			t.Fatalf("Supported(%v) = %+v, want none", attributes, r)
		}
		if r := validated(t, p, Deployment{Roles: BuiltinRoles()}); len(r) != 0 {
			t.Fatalf("Validate(%v) = %+v, want none", attributes, r)
		}
		if got := StepsOf(p.Tasks[0], defaults); !reflect.DeepEqual(got, defaults) {
			t.Fatalf("StepsOf(FromBlocks(%v)) = %+v, want the defaults", attributes, got)
		}
	}
}

// FromBlocks reads each `plan_task` block as a task, in the document's order, with what each
// attribute says: each `needs` word as the id of the task it names, and a word that names no task
// of the plan kept whole in Unresolved. A block whose Parent is a task with a sub-owner is a part
// of that task, by its id, and a block whose `parent` is a requirement of another document is a
// task of the top level. Each `plan_role` block is a role the plan defines, named by its id. Every
// other block is the document's prose and no part of the plan.
func TestFromBlocksReadsTaskAndRoleBlocks(t *testing.T) {
	p, err := FromBlocks("ACME-1", "plan-md", 3, []Block{
		{ID: "intro", Type: "paragraph", Text: "Why the issue is split so."},
		{ID: "d", Type: "plan_task", Attributes: map[string]string{
			"needs":  "dispatch://ACME-1/artifact/plan-md#b-f1 dispatch://ACME-1/artifact/plan-md#b-c@v2",
			"serves": "dispatch://ACME-1/spec#b-r1",
			"steps":  "implementer:cd reviewer:r",
			"parent": "dispatch://ACME-1/spec#b-r1",
		}, Text: "What task D does."},
		{ID: "f", Type: "plan_task", Attributes: map[string]string{"owner": "architect"}, Text: "What this part must reach."},
		{ID: "f1", Type: "plan_task", Parent: "f", Attributes: map[string]string{
			"parent": "dispatch://ACME-1/artifact/plan-md#b-f", "steps": "implementer reviewer", "merges": "true",
		}, Text: "What task F1 does."},
		{ID: "prober", Type: RoleBlockType, Attributes: map[string]string{"type": "testing"}, Text: "You probe."},
	})
	if err != nil {
		t.Fatalf("FromBlocks: %v", err)
	}
	want := Plan{ArtifactID: "plan-md", Version: 3, RoleBlocks: []string{"prober"}, Tasks: []Task{
		{ID: "d", Needs: []string{"f1"}, Unresolved: []string{"dispatch://ACME-1/artifact/plan-md#b-c@v2"},
			Steps: []Step{{Role: "implementer", Key: "cd"}, {Role: "reviewer", Key: "r"}}, Text: "What task D does."},
		{ID: "f", Owner: "architect", Text: "What this part must reach."},
		{ID: "f1", PartOf: "f", Merges: true,
			Steps: []Step{{Role: "implementer"}, {Role: "reviewer"}}, Text: "What task F1 does."},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("FromBlocks =\n%+v\nwant\n%+v", p, want)
	}

	// `merges` is read by its presence: only the word false says the task does not merge, so a
	// bare attribute is never read as a task that keeps its work back.
	for value, want := range map[string]bool{"false": false, "": true, "true": true} {
		p, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: map[string]string{"merges": value, "steps": "implementer"}}})
		if err != nil || p.Tasks[0].Merges != want {
			t.Fatalf("FromBlocks(merges=%q) = %+v, %v, want Merges %v", value, p.Tasks, err, want)
		}
	}

	// The blocks FromBlocks cannot read at all: steps outside the grammar, a task or a role with
	// no id, and one id on two blocks of the plan.
	if _, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: map[string]string{"steps": "implementer Reviewer"}}}); err == nil ||
		!strings.Contains(err.Error(), "task a") || !strings.Contains(err.Error(), `"Reviewer"`) {
		t.Fatalf("FromBlocks(steps outside the grammar) error = %v, want one naming task a and the word", err)
	}
	for name, blocks := range map[string][]Block{
		"a task with no id":         {{Type: "plan_task", Text: "Do it."}},
		"a role with no id":         {{ID: "a", Type: "plan_task"}, {Type: RoleBlockType}},
		"two tasks, one id":         {{ID: "a", Type: "plan_task"}, {ID: "a", Type: "plan_task"}},
		"a task and a role, one id": {{ID: "a", Type: "plan_task"}, {ID: "a", Type: RoleBlockType}},
	} {
		if p, err := FromBlocks("ACME-1", "plan-md", 1, blocks); err == nil {
			t.Errorf("FromBlocks(%s) = %+v, want an error", name, p)
		}
	}
	// A document with no task block is read, as a plan with no task, which Validate refuses.
	if p, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "intro", Type: "paragraph", Text: "No task here."}}); err != nil || len(p.Tasks) != 0 {
		t.Fatalf("FromBlocks(no task block) = %+v, %v, want a plan with no task and no error", p, err)
	}
}

// MovesBackPairs is every pair phase.MovePairs holds but the 16 no line of a task Supported takes
// makes as a move back, written out here rather than read from the checks: each step phase, step
// among them, to retro or to merging, and retro to merging, since the retro and the merge end every
// line, and implementing to testing, reviewing or accepting, since the steps that read the
// implementer's pull request come after it. Among the 41: a move back of another order than
// today's (testing-reviewing, on implementer reviewer tester), a planner's move back to the
// implementer (planning-implementing, on implementer planner), the merge's move back to the retro,
// and the moves back that leave or reach the step of a role the deployment defines, step-step (two
// such roles) among them. CanMoveBack answers each pair as the list holds it, and the list is a
// fresh slice.
func TestMovesBackPairsAreTheMovesBackOfEveryLineSupportedTakes(t *testing.T) {
	never := []string{
		"planning-retro", "implementing-retro", "testing-retro", "reviewing-retro", "accepting-retro", "step-retro",
		"planning-merging", "implementing-merging", "testing-merging", "reviewing-merging", "accepting-merging", "step-merging",
		"retro-merging", "implementing-testing", "implementing-reviewing", "implementing-accepting",
	}
	var want []string
	for _, pair := range phase.MovePairs() {
		if !slices.Contains(never, pair) {
			want = append(want, pair)
		}
	}
	got := MovesBackPairs()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MovesBackPairs() = %q (%d), want %q (%d)", got, len(got), want, len(want))
	}
	for _, pair := range []string{"testing-reviewing", "planning-implementing", "merging-retro", "step-implementing", "implementing-step", "step-step", "retro-step"} {
		if !slices.Contains(got, pair) {
			t.Errorf("MovesBackPairs() leaves out %s", pair)
		}
	}
	for _, pair := range phase.MovePairs() {
		from, to, _ := phase.ParsePair(pair)
		fires := !slices.Contains(never, pair)
		if can := CanMoveBack(from, to); can != fires {
			t.Errorf("CanMoveBack(%s, %s) = %t, want %t", from, to, can, fires)
		}
	}
	got[0] = "rubbish"
	if MovesBackPairs()[0] == "rubbish" {
		t.Fatal("MovesBackPairs returned the stored slice, which a caller can overwrite")
	}
}
