package plan

import (
	"reflect"
	"testing"
)

// yet ends each refusal of something a later release carries out.
const yet = " This release of Legion does not carry it out yet."

// builtin is the deployment of Legion's own roles alone.
var builtin = Deployment{Roles: BuiltinRoles()}

// planDocument is the plan document of these tests, ACME-1's plan.md, as a reference to one of its
// blocks names it (FromBlocks, ParentOf).
const planDocument = "dispatch://ACME-1/artifact/plan-md"

// needWord is a need written as Dispatch writes a reference to the block id of the plan document.
func needWord(id string) string { return planDocument + "#b-" + id }

// What no release carries out, as this release checks it: a step that names a role neither the
// deployment nor the plan defines is refused, naming the step, its task and the role. Validate
// refuses no plan for a step that it lacks, so each plan of Legion's own roles passes it whatever
// steps it leaves out; a task with no implementer step, and a tester, reviewer or acceptance step
// with no implementer before it, are what this release does not carry out yet, and Supported
// refuses them. The control is in the second half: a validator that also required a reviewer
// step would refuse `implementer tester`, and one that required a tester would refuse
// `implementer reviewer`.
func TestValidateRefusesARoleThatNobodyDefines(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	r := Validate(oneTask("a", "implementer prober"), d)
	want := Refusal{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."}
	if len(r) != 1 || r[0] != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	for _, taken := range []Plan{oneTask("a", "implementer tester"), oneTask("a", "implementer reviewer"), oneTask("a", "implementer")} {
		if r := Validate(taken, d); len(r) != 0 {
			t.Fatalf("%v refused: %+v", taken, r)
		}
	}
	// architect, merger and controller are no roles of a step: the step roles are Legion's five.
	for _, role := range []string{"architect", "merger", "controller"} {
		r := Validate(oneTask("a", "implementer "+role), d)
		want := Refusal{Task: "a", Reason: "step 2 of task a names the role " + role + ", which neither the deployment nor the plan defines."}
		if len(r) != 1 || r[0] != want {
			t.Fatalf("Validate(implementer %s) = %+v, want %+v", role, r, want)
		}
	}
	// A role named at two steps is one cause, refused once at its first step.
	if r := Validate(oneTask("a", "prober implementer prober"), d); !reflect.DeepEqual(r, []Refusal{
		{Task: "a", Reason: "step 1 of task a names the role prober, which neither the deployment nor the plan defines."},
	}) {
		t.Fatalf("Validate(prober implementer prober) = %+v", r)
	}
	// A role the plan's own role block defines is defined (Supported refuses the role block).
	defined := oneTask("a", "implementer prober")
	defined.RoleBlocks = []string{"prober"}
	if r := Validate(defined, d); len(r) != 0 {
		t.Fatalf("Validate(a role the plan defines) = %+v, want none", r)
	}
}

// A plan with no task block gives Legion no work, and Validate refuses it with the one reason no
// later release lifts, so the route answers it as each other plan Legion does not take. The
// control: a plan of one block with no `steps` is taken.
func TestValidateRefusesAPlanWithNoTask(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	empty, err := FromBlocks("plan-md", planDocument, 1, []Block{{ID: "intro", Type: "paragraph", Text: "No task here."}})
	if err != nil {
		t.Fatalf("FromBlocks(no task block): %v", err)
	}
	if len(empty.Tasks) != 0 {
		t.Fatalf("FromBlocks(no task block) tasks = %+v, want none", empty.Tasks)
	}
	want := []Refusal{{Task: "", Reason: "the plan has no task block, so it gives Legion no work."}}
	if r := Validate(empty, d); !reflect.DeepEqual(r, want) {
		t.Fatalf("Validate(no task) = %+v, want %+v", r, want)
	}
	if r := Supported(empty); len(r) != 0 {
		t.Fatalf("Supported(no task) = %+v, want none: the cause is Validate's alone", r)
	}
	taken, err := FromBlocks("plan-md", planDocument, 1, []Block{{ID: "a", Type: "plan_task", Text: "Do the change."}})
	if err != nil {
		t.Fatalf("FromBlocks(one block): %v", err)
	}
	if r := append(Supported(taken), Validate(taken, d)...); len(r) != 0 {
		t.Fatalf("a plan of one block with no steps refused: %+v", r)
	}
}

// Each cause this release does not carry out yet (Supported), with its exact text and its task.
// Each cause gets one reason, so a plan with two causes gets two. The taken plans are the subsets
// of today's line that keep the implementer, with the owner's acceptance.
func TestSupportedRefusesWhatThisReleaseDoesNotCarryOut(t *testing.T) {
	implementer, _ := ParseSteps("implementer")
	withTask := func(task Task) Plan { return Plan{Tasks: []Task{task}} }
	for _, tc := range []struct {
		name string
		plan Plan
		want []Refusal
	}{
		{name: "more than one task", plan: Plan{Tasks: []Task{
			{ID: "a", Steps: implementer}, {ID: "b", Steps: implementer}, {ID: "c"},
		}}, want: []Refusal{{Task: "", Reason: "the plan has 3 tasks (a, b, c), and this release runs one task for each issue." + yet}}},
		{name: "needs", plan: withTask(Task{ID: "a", Needs: []string{"b", "dispatch://ACME-2/artifact/plan-md#b-c"}, Steps: implementer}),
			want: []Refusal{{Task: "a", Reason: "task a needs b, dispatch://ACME-2/artifact/plan-md#b-c." + yet}}},
		{name: "a step that reads a pull request before the implementer", plan: oneTask("a", "reviewer implementer"),
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads." + yet}}},
		{name: "a task with no implementer", plan: oneTask("a", "tester reviewer"), want: []Refusal{
			{Task: "a", Reason: "task a has no step implementer." + yet},
			{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads." + yet},
			{Task: "a", Reason: "step 2 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads." + yet},
		}},
		{name: "a role two times in a task", plan: oneTask("a", "implementer reviewer reviewer"),
			want: []Refusal{{Task: "a", Reason: "task a names the role reviewer at steps 2 and 3." + yet}}},
		{name: "acceptance with no key", plan: oneTask("a", "implementer acceptance"),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names acceptance with no key, which asks for a fresh agent of the role acceptance." + yet}}},
		{name: "a key", plan: oneTask("a", "implementer:cd reviewer"),
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names the key cd." + yet}}},
		{name: "the key owner on a role but acceptance", plan: oneTask("a", "implementer:owner"),
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names the key owner." + yet}}},
		{name: "a key on acceptance but owner", plan: oneTask("a", "implementer acceptance:r"),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names the key r." + yet}}},
		{name: "a role block", plan: func() Plan { p := oneTask("a", "implementer"); p.RoleBlocks = []string{"prober"}; return p }(),
			want: []Refusal{{Task: "", Reason: "the plan defines the role prober." + yet}}},
		{name: "merges", plan: withTask(Task{ID: "a", Merges: true, Steps: implementer}),
			want: []Refusal{{Task: "a", Reason: "task a names merges." + yet}}},
		{name: "owner", plan: withTask(Task{ID: "f", Owner: "architect"}),
			want: []Refusal{{Task: "f", Reason: "task f names owner." + yet}}},
		{name: "a part of a task", plan: withTask(Task{ID: "f1", PartOf: "f", Steps: implementer}),
			want: []Refusal{{Task: "f1", Reason: "task f1 is a part of task f." + yet}}},
		{name: "two causes in one task", plan: oneTask("a", "reviewer implementer reviewer"), want: []Refusal{
			{Task: "a", Reason: "task a names the role reviewer at steps 1 and 3." + yet},
			{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads." + yet},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supported(tc.plan); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Supported =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}

	for _, steps := range []string{"implementer reviewer", "planner implementer tester reviewer acceptance:owner", "implementer acceptance:owner", "implementer"} {
		if r := Supported(oneTask("a", steps)); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}

	// Both checks run, so a plan with a cause of each gets two reasons, one from each.
	twoTasks := oneTask("a", "implementer prober")
	twoTasks.Tasks = append(twoTasks.Tasks, Task{ID: "b", Steps: implementer})
	got := append(Supported(twoTasks), Validate(twoTasks, Deployment{Roles: BuiltinRoles()})...)
	want := []Refusal{
		{Task: "", Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue." + yet},
		{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two tasks and prober =\n%+v\nwant\n%+v", got, want)
	}
}

// Legion does not carry out an element, so Validate refuses an element that names what only a task
// carries out, `steps`, `owner` or `merges`, and a task that needs an element. It refuses a task
// block whose Parent names an id that no typed block of the version holds. Each reason names the
// block and the cause, and none says "yet": no release carries any of them out. An element that
// names none of the three is taken, and so is a part that needs a part of its own task, a sibling.
//
// The faults are an element with steps taken, whose steps Legion never runs though the block
// names them, and a task that waits for an element, which never ends. The controls: Places with
// no answer of an element, where a block under a task with no sub-owner stands at the top, makes
// the element with steps a task of the plan, and its row fails; and a FromBlocks that leaves
// MissingParent empty fails the row of that parent.
func TestValidateRefusesWhatAnElementCannotBe(t *testing.T) {
	// block is a task block id under the block parent, with the attributes named in pairs.
	block := func(id, parent string, attributes ...string) Block {
		named := map[string]string{}
		for i := 0; i+1 < len(attributes); i += 2 {
			named[attributes[i]] = attributes[i+1]
		}
		return withParent(Block{ID: id, Type: TaskBlockType, Attributes: named, Text: "What block " + id + " says."}, parent)
	}
	a := block("a", "", "steps", "implementer")
	element := func(id, names string) string {
		return "task block " + id + " is an element of task a: it stands under a, which names no sub-owner. " +
			"Legion does not carry out an element, so an element names no steps, owner or merges, and " + id + " names " + names + "."
	}
	for _, tc := range []struct {
		name   string
		blocks []Block
		want   []Refusal
	}{
		{name: "an element that names steps", blocks: []Block{a, block("e1", "a", "steps", "implementer")},
			want: []Refusal{{Task: "e1", Reason: element("e1", "steps")}}},
		{name: "an element under an element that names owner", blocks: []Block{a, block("e1", "a"), block("e2", "e1", "owner", "architect")},
			want: []Refusal{{Task: "e2", Reason: "task block e2 is an element of task a: it stands under e1, an element of a. " +
				"Legion does not carry out an element, so an element names no steps, owner or merges, and e2 names owner."}}},
		{name: "an element that names merges", blocks: []Block{a, block("e1", "a", "merges", "false")},
			want: []Refusal{{Task: "e1", Reason: element("e1", "merges")}}},
		{name: "an element that names all three", blocks: []Block{a, block("e1", "a", "steps", "tester", "owner", "architect", "merges", "true")},
			want: []Refusal{{Task: "e1", Reason: element("e1", "steps, owner and merges")}}},
		{name: "a task that needs an element", blocks: []Block{a, block("e1", "a"), block("h", "", "steps", "implementer", "needs", needWord("a")+" "+needWord("e1"))},
			want: []Refusal{{Task: "h", Reason: "task h needs " + needWord("e1") + ", which is an element of task a. Legion does not carry out an element, so a task cannot need one."}}},
		{name: "a parent that names an id the version does not hold", blocks: []Block{a, block("b", "gone", "steps", "implementer", "needs", needWord("a"))},
			want: []Refusal{{Task: "b", Reason: "task block b names gone as its parent, which no typed block of version 1 of the plan holds. " +
				"Name a typed block of this version in parent, or leave parent out."}}},
		{name: "an element that names none of the three", blocks: []Block{a, block("e1", "a", "needs", needWord("a"))}},
		{name: "a part that needs a sibling part", blocks: []Block{block("f", "", "owner", "architect"), block("f1", "f", "steps", "implementer"),
			block("f2", "f", "steps", "implementer", "needs", needWord("f1"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(planOf(t, 1, tc.blocks...), builtin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A task names its steps in any order. The planner stands at any place, and the tester, the
// reviewer and the owner's acceptance each come after the implementer, in any order among them. A
// role named at two steps stays refused in this release. The twin: today's line, the steps an
// issue with no plan rows runs, is taken.
func TestSupportedTakesStepsInAnyOrder(t *testing.T) {
	for _, steps := range []string{"implementer reviewer tester", "implementer tester reviewer", "implementer acceptance:owner reviewer",
		"implementer reviewer acceptance:owner tester", "implementer planner", "planner implementer reviewer", "implementer reviewer planner tester",
		"planner implementer tester reviewer acceptance:owner"} {
		p := oneTask("a", steps)
		if r := append(Supported(p), Validate(p, Deployment{Roles: BuiltinRoles()})...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
	}
	want := Refusal{Task: "a", Reason: "task a names the role tester at steps 2 and 4. This release of Legion does not carry it out yet."}
	if r := Supported(oneTask("a", "implementer tester reviewer tester")); len(r) != 1 || r[0] != want {
		t.Fatalf("got %+v", r)
	}
}

// The tester, the reviewer and the owner's acceptance read the pull request the implementer opens
// (hasOpenOrMergedPR, the one guard of a move on), so this release refuses a step of one of those
// roles with no implementer step before it, by its role and with its word as the plan writes it.
// The planner reads no pull request, so it stands anywhere.
func TestAStepThatReadsAPullRequestNeedsAnImplementerBeforeIt(t *testing.T) {
	for _, tc := range []struct{ steps, reason string }{
		{"tester implementer", "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads."},
		{"reviewer implementer", "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads."},
		{"acceptance:owner implementer", "step 1 of task a names acceptance:owner, and no step before it names the role implementer, whose pull request acceptance:owner reads."},
		{"planner reviewer implementer", "step 2 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads."},
	} {
		want := []Refusal{{Task: "a", Reason: tc.reason + yet}}
		if r := Supported(oneTask("a", tc.steps)); !reflect.DeepEqual(r, want) {
			t.Fatalf("Supported(%s) =\n%+v\nwant\n%+v", tc.steps, r, want)
		}
	}
	if r := Supported(oneTask("a", "implementer planner")); len(r) != 0 {
		t.Fatalf("Supported(implementer planner) = %+v, want none", r)
	}
	// The plan `tester` alone has two causes, in the order Supported builds them, and both stand.
	want := []Refusal{
		{Task: "a", Reason: "task a has no step implementer." + yet},
		{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads." + yet},
	}
	if r := Supported(oneTask("a", "tester")); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(tester) =\n%+v\nwant\n%+v", r, want)
	}
}
