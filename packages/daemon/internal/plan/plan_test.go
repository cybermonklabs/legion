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

// The default steps walk today's line: the five steps' phases in order, then Legion's own retro
// and merge.
func TestLineOfTheDefaultStepsIsTodaysOrder(t *testing.T) {
	if got := Line(defaultSteps(t), Deployment{Roles: BuiltinRoles()}); !reflect.DeepEqual(got, phase.Order) {
		t.Fatalf("Line(default steps) = %v, want %v", got, phase.Order)
	}
	steps, _ := ParseSteps("implementer reviewer")
	want := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}
	if got := Line(steps, Deployment{Roles: BuiltinRoles()}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer reviewer) = %v, want %v", got, want)
	}
}

// Each role of a step in this release is one of Legion's five, with the type and the phase of
// today's line that it runs as.
func TestBuiltinRolesAreLegionsFiveStepRoles(t *testing.T) {
	want := map[string]RoleDef{
		"planner":     {Name: "planner", Type: TypePlanning, Phase: phase.Planning},
		"implementer": {Name: "implementer", Type: TypeImplementation, Phase: phase.Implementing},
		"tester":      {Name: "tester", Type: TypeTesting, Phase: phase.Testing},
		"reviewer":    {Name: "reviewer", Type: TypeReview, Phase: phase.Reviewing},
		"acceptance":  {Name: "acceptance", Type: TypeReview, Phase: phase.Accepting},
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
		p, err := FromBlocks("plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: attributes, Text: "Do the change."}})
		if err != nil {
			t.Fatalf("FromBlocks(%v): %v", attributes, err)
		}
		if len(p.Tasks) != 1 || p.Tasks[0].NamesSteps() || p.Tasks[0].Steps != nil {
			t.Fatalf("FromBlocks(%v) = %+v, want one task with no steps", attributes, p.Tasks)
		}
		if r := Supported(p); len(r) != 0 {
			t.Fatalf("Supported(%v) = %+v, want none", attributes, r)
		}
		if r := Validate(p, Deployment{Roles: BuiltinRoles()}); len(r) != 0 {
			t.Fatalf("Validate(%v) = %+v, want none", attributes, r)
		}
		if got := StepsOf(p.Tasks[0], defaults); !reflect.DeepEqual(got, defaults) {
			t.Fatalf("StepsOf(FromBlocks(%v)) = %+v, want the defaults", attributes, got)
		}
	}
}

// FromBlocks reads each `plan_task` block as a task, in the document's order, with what each
// attribute says; each `needs` and `part_of` value is kept whole, as the block writes it. Each
// `plan_role` block is a role the plan defines, named by its id. Every other block is the
// document's prose and no part of the plan.
func TestFromBlocksReadsTaskAndRoleBlocks(t *testing.T) {
	p, err := FromBlocks("plan-md", 3, []Block{
		{ID: "intro", Type: "paragraph", Text: "Why the issue is split so."},
		{ID: "d", Type: "plan_task", Attributes: map[string]string{
			"needs":  "dispatch://ACME-1/artifact/plan-md#b-a dispatch://ACME-1/artifact/plan-md#b-c@v2",
			"serves": "dispatch://ACME-1/spec#b-r1",
			"steps":  "implementer:cd reviewer:r",
		}, Text: "What task D does."},
		{ID: "f", Type: "plan_task", Attributes: map[string]string{"owner": "architect"}, Text: "What this part must reach."},
		{ID: "f1", Type: "plan_task", Attributes: map[string]string{
			"part_of": "dispatch://ACME-1/artifact/plan-md#b-f", "steps": "implementer reviewer", "merges": "true",
		}, Text: "What task F1 does."},
		{ID: "prober", Type: RoleBlockType, Attributes: map[string]string{"type": "testing"}, Text: "You probe."},
	})
	if err != nil {
		t.Fatalf("FromBlocks: %v", err)
	}
	want := Plan{ArtifactID: "plan-md", Version: 3, RoleBlocks: []string{"prober"}, Tasks: []Task{
		{ID: "d", Needs: []string{"dispatch://ACME-1/artifact/plan-md#b-a", "dispatch://ACME-1/artifact/plan-md#b-c@v2"},
			Steps: []Step{{Role: "implementer", Key: "cd"}, {Role: "reviewer", Key: "r"}}, Text: "What task D does."},
		{ID: "f", Owner: "architect", Text: "What this part must reach."},
		{ID: "f1", PartOf: "dispatch://ACME-1/artifact/plan-md#b-f", Merges: true,
			Steps: []Step{{Role: "implementer"}, {Role: "reviewer"}}, Text: "What task F1 does."},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("FromBlocks =\n%+v\nwant\n%+v", p, want)
	}

	// `merges` is read by its presence: only the word false says the task does not merge, so a
	// bare attribute is never read as a task that keeps its work back.
	for value, want := range map[string]bool{"false": false, "": true, "true": true} {
		p, err := FromBlocks("plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: map[string]string{"merges": value, "steps": "implementer"}}})
		if err != nil || p.Tasks[0].Merges != want {
			t.Fatalf("FromBlocks(merges=%q) = %+v, %v, want Merges %v", value, p.Tasks, err, want)
		}
	}

	// The blocks FromBlocks cannot read at all: steps outside the grammar, a task or a role with
	// no id, and one id on two blocks of the plan.
	if _, err := FromBlocks("plan-md", 1, []Block{{ID: "a", Type: "plan_task", Attributes: map[string]string{"steps": "implementer Reviewer"}}}); err == nil ||
		!strings.Contains(err.Error(), "task a") || !strings.Contains(err.Error(), `"Reviewer"`) {
		t.Fatalf("FromBlocks(steps outside the grammar) error = %v, want one naming task a and the word", err)
	}
	for name, blocks := range map[string][]Block{
		"a task with no id":         {{Type: "plan_task", Text: "Do it."}},
		"a role with no id":         {{ID: "a", Type: "plan_task"}, {Type: RoleBlockType}},
		"two tasks, one id":         {{ID: "a", Type: "plan_task"}, {ID: "a", Type: "plan_task"}},
		"a task and a role, one id": {{ID: "a", Type: "plan_task"}, {ID: "a", Type: RoleBlockType}},
	} {
		if p, err := FromBlocks("plan-md", 1, blocks); err == nil {
			t.Errorf("FromBlocks(%s) = %+v, want an error", name, p)
		}
	}
	// A document with no task block is read, as a plan with no task, which Validate refuses.
	if p, err := FromBlocks("plan-md", 1, []Block{{ID: "intro", Type: "paragraph", Text: "No task here."}}); err != nil || len(p.Tasks) != 0 {
		t.Fatalf("FromBlocks(no task block) = %+v, %v, want a plan with no task and no error", p, err)
	}
}

// CanMoveBack holds every pair of two different workflow phases but the 14 no line of a task
// Supported takes makes as a move back, written out here rather than read from the checks: each
// step phase to retro or to merging, and retro to merging, since the retro and the merge end every
// line, and implementing to testing, reviewing or accepting, since the steps that read the
// implementer's pull request come after it. Among the 28: a move back of another order than
// today's (testing-reviewing, on implementer reviewer tester), a planner's move back to the
// implementer (planning-implementing, on implementer planner), and the merge's move back to the
// retro.
func TestCanMoveBackHoldsTheMovesBackOfEveryLineSupportedTakes(t *testing.T) {
	never := []string{
		"planning-retro", "implementing-retro", "testing-retro", "reviewing-retro", "accepting-retro",
		"planning-merging", "implementing-merging", "testing-merging", "reviewing-merging", "accepting-merging",
		"retro-merging", "implementing-testing", "implementing-reviewing", "implementing-accepting",
	}
	for _, pair := range phase.MovePairs() {
		from, to, _ := phase.ParsePair(pair)
		fires := !slices.Contains(never, pair)
		if can := CanMoveBack(from, to); can != fires {
			t.Errorf("CanMoveBack(%s, %s) = %t, want %t", from, to, can, fires)
		}
	}
	for _, move := range [][2]phase.Phase{{phase.Testing, phase.Reviewing}, {phase.Planning, phase.Implementing}, {phase.Merging, phase.Retro}} {
		if !CanMoveBack(move[0], move[1]) {
			t.Errorf("CanMoveBack(%s, %s) = false, want a move back", move[0], move[1])
		}
	}
}
