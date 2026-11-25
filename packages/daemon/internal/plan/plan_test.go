package plan

import (
	"maps"
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

// A stored step names its agent by one rule, which the record and the store both read: the name
// stored with the step, and none where it holds none. A step of the key owner names none whatever
// it holds: its agent is the task's owner, the issue's own architect, and no agent the plan names.
func TestAStoredStepNamesItsAgent(t *testing.T) {
	for _, tc := range []struct {
		key    string
		stored string
		want   string
	}{
		{"", "", ""},
		{"owner", "", ""},
		{"owner", "acceptance", ""},
		{"a", "implementer", "implementer"},
		{"", "implementer-2", "implementer-2"},
	} {
		if got := StoredAgentName(tc.key, tc.stored); got != tc.want {
			t.Errorf("StoredAgentName(%q, %q) = %q, want %q", tc.key, tc.stored, got, tc.want)
		}
	}
}

// The line of the default steps is today's line: the five steps' phases in order, then Legion's
// own retro and merge.
func TestLineOfTheDefaultStepsIsTodaysOrder(t *testing.T) {
	if got := Line(defaultSteps(t)); !reflect.DeepEqual(got, phase.Order) {
		t.Fatalf("Line(default steps) = %v, want %v", got, phase.Order)
	}
	steps, _ := ParseSteps("implementer reviewer")
	want := []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}
	if got := Line(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Line(implementer reviewer) = %v, want %v", got, want)
	}
}

// Each of Legion's own roles of a step is one of five, with the type of work it does, on Oh My Pi.
func TestBuiltinRolesAreLegionsFiveStepRoles(t *testing.T) {
	want := map[string]RoleDef{
		"planner":     {Name: "planner", Type: TypePlanning, Backend: "omp"},
		"implementer": {Name: "implementer", Type: TypeImplementation, Backend: "omp"},
		"tester":      {Name: "tester", Type: TypeTesting, Backend: "omp"},
		"reviewer":    {Name: "reviewer", Type: TypeReview, Backend: "omp"},
		"acceptance":  {Name: "acceptance", Type: TypeReview, Backend: "omp"},
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
// own phase, and every other role, a role of the deployment or of the plan, runs as the phase step.
// No type is read, so a role of the deployment whose type is review still runs as step.
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
	if got := Line(steps); !reflect.DeepEqual(got, want) {
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
// `plan_role` block is a role the plan defines, read whole: its name (the block's id), its type,
// its model and its prompt (the block's text). Every other block is the document's prose and no
// part of the plan.
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
		{ID: "prober", Type: RoleBlockType, Attributes: map[string]string{"type": "testing", "model": "listed-model"}, Text: "# Prober\n\nYou probe."},
	})
	if err != nil {
		t.Fatalf("FromBlocks: %v", err)
	}
	want := Plan{ArtifactID: "plan-md", Version: 3, Roles: []PlanRole{
		{Name: "prober", Type: "testing", Model: "listed-model", Prompt: "# Prober\n\nYou probe."},
	}, Tasks: []Task{
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

// The roles a step can name are Legion's, then the plan's own, then the rest of the deployment's,
// each role of the plan a role of Oh My Pi with its type and its model. A role of the plan is read
// before a role of the deployment of its name, as a launch reads it for the plan's issue, and
// Legion's own roles before both (Validate refuses a plan that takes either name). The answer is
// a fresh map, so the deployment's roles stay as they were.
func TestRolesOfIsLegionsRolesThenThePlansThenTheDeployments(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	d.Roles["checker"] = RoleDef{Name: "checker", Type: TypeImplementation, Model: "deployment-model", Backend: "claude"}
	deployed := maps.Clone(d.Roles)
	p := oneTask("a", "implementer prober checker")
	p.Roles = []PlanRole{{Name: "prober", Type: TypeTesting, Model: "m"}, {Name: "reviewer", Type: TypeTesting}, {Name: "checker", Type: TypeTesting, Model: "plan-model"}}
	roles := RolesOf(p, d)
	if want := (RoleDef{Name: "prober", Type: "testing", Model: "m", Backend: "omp"}); roles["prober"] != want {
		t.Errorf("RolesOf[prober] = %+v, want %+v", roles["prober"], want)
	}
	if want := (RoleDef{Name: "checker", Type: "testing", Model: "plan-model", Backend: "omp"}); roles["checker"] != want {
		t.Errorf("RolesOf[checker] = %+v, want the plan's %+v, read before the deployment's", roles["checker"], want)
	}
	if got := roles["reviewer"].Type; got != TypeReview {
		t.Errorf("RolesOf[reviewer].Type = %q, want %q: Legion's reviewer wins over the plan's", got, TypeReview)
	}
	if len(roles) != 7 {
		t.Errorf("RolesOf has %d roles (%+v), want 7: Legion's five, prober and checker", len(roles), roles)
	}
	if !reflect.DeepEqual(d.Roles, deployed) {
		t.Errorf("RolesOf changed the deployment's roles: %+v", d.Roles)
	}
}

// A plan's role overrides the deployment's settings of its name exactly where SourceOf gives the
// name to the plan with the deployment holding it too: a name the plan defines that is none of
// Legion's. A name the plan does not define, and a name of Legion's whatever the plan says, keep
// the deployment's settings.
func TestAPlansRoleOverridesTheDeploymentForANameOfItsOwnAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planned bool
		want    bool
	}{
		{"prober", true, true},
		{"prober", false, false},
		{"implementer", true, false},
		{"controller", true, false},
		{"acceptance", true, false},
		{"tester", false, false},
	} {
		if got := Overrides(tc.name, tc.planned); got != tc.want {
			t.Errorf("Overrides(%s, %t) = %t, want %t", tc.name, tc.planned, got, tc.want)
		}
		var planned *PlanRole
		if tc.planned {
			planned = &PlanRole{Name: tc.name, Type: TypeTesting}
		}
		d := Deployment{Roles: map[string]RoleDef{tc.name: {Name: tc.name, Type: TypeTesting}}}
		if _, source := RoleOf(tc.name, planned, d); (source == SourcePlan) != tc.want {
			t.Errorf("RoleOf(%s) with the plan defining it %t and the deployment holding it answers %v, which disagrees with Overrides' %t",
				tc.name, tc.planned, source, tc.want)
		}
	}
}

// MovesBackPairs is every pair MovePairs holds but the 13 no line of a task Supported takes makes
// as a move back, written out here rather than read from the checks: each step phase, step among
// them, to retro or to merging, and retro to merging, since the retro and the merge end every
// line. Among the 44: a move back of another order than today's (testing-reviewing, on implementer
// reviewer tester), a move back to a tester that stands before the implementer
// (implementing-testing, on tester implementer), a planner's move back to the implementer
// (planning-implementing, on implementer planner), the merge's move back to the retro, and the
// moves of a step of a role the deployment defines, which may stand before the implementer or
// after it: step-implementing (implementer checker), implementing-step (checker implementer) and
// step-step (implementer checker linter). CanMoveBack answers each pair as the list holds it, and
// the list is a fresh slice.
func TestMovesBackPairsAreTheMovesBackOfEveryLineSupportedTakes(t *testing.T) {
	never := []string{
		"planning-retro", "implementing-retro", "testing-retro", "reviewing-retro", "accepting-retro",
		"planning-merging", "implementing-merging", "testing-merging", "reviewing-merging", "accepting-merging",
		"retro-merging", "step-retro", "step-merging",
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
	for _, pair := range []string{"testing-reviewing", "implementing-testing", "planning-implementing", "merging-retro", "step-implementing", "implementing-step", "step-step"} {
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
