package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// yet ends each refusal of something a later release carries out.
const yet = " This release of Legion does not carry it out yet."

// builtin is the deployment of Legion's own roles alone.
var builtin = Deployment{Roles: BuiltinRoles()}

// validated is Validate's refusals of p under the test's context, which ends only with the test,
// so Validate's one error, its context's end, fails the test.
func validated(t *testing.T, p Plan, d Deployment) []Refusal {
	t.Helper()
	refusals, err := Validate(t.Context(), p, d)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return refusals
}

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
	r := validated(t, oneTask("a", "implementer prober"), d)
	want := Refusal{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."}
	if len(r) != 1 || r[0] != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	for _, taken := range []Plan{oneTask("a", "implementer tester"), oneTask("a", "implementer reviewer"), oneTask("a", "implementer")} {
		if r := validated(t, taken, d); len(r) != 0 {
			t.Fatalf("%v refused: %+v", taken, r)
		}
	}
	// architect, merger and controller are no roles of a step: the step roles are Legion's five.
	for _, role := range []string{"architect", "merger", "controller"} {
		r := validated(t, oneTask("a", "implementer "+role), d)
		want := Refusal{Task: "a", Reason: "step 2 of task a names the role " + role + ", which neither the deployment nor the plan defines."}
		if len(r) != 1 || r[0] != want {
			t.Fatalf("Validate(implementer %s) = %+v, want %+v", role, r, want)
		}
	}
	// A role named at two steps is one cause, refused once at its first step.
	if r := validated(t, oneTask("a", "prober implementer prober"), d); !reflect.DeepEqual(r, []Refusal{
		{Task: "a", Reason: "step 1 of task a names the role prober, which neither the deployment nor the plan defines."},
	}) {
		t.Fatalf("Validate(prober implementer prober) = %+v", r)
	}
	// A role the plan's own role block defines is defined (Supported refuses the role block).
	defined := oneTask("a", "implementer prober")
	defined.RoleBlocks = []string{"prober"}
	if r := validated(t, defined, d); len(r) != 0 {
		t.Fatalf("Validate(a role the plan defines) = %+v, want none", r)
	}
}

// A plan with no task block gives Legion no work, and Validate refuses it with the one reason no
// later release lifts, so the route answers it as each other plan Legion does not take. The
// control: a plan of one block with no `steps` is taken.
func TestValidateRefusesAPlanWithNoTask(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	empty, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "intro", Type: "paragraph", Text: "No task here."}})
	if err != nil {
		t.Fatalf("FromBlocks(no task block): %v", err)
	}
	if len(empty.Tasks) != 0 {
		t.Fatalf("FromBlocks(no task block) tasks = %+v, want none", empty.Tasks)
	}
	want := []Refusal{{Task: "", Reason: "the plan has no task block, so it gives Legion no work."}}
	if r := validated(t, empty, d); !reflect.DeepEqual(r, want) {
		t.Fatalf("Validate(no task) = %+v, want %+v", r, want)
	}
	if r := Supported(empty, d); len(r) != 0 {
		t.Fatalf("Supported(no task) = %+v, want none: the cause is Validate's alone", r)
	}
	taken, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "a", Type: "plan_task", Text: "Do the change."}})
	if err != nil {
		t.Fatalf("FromBlocks(one block): %v", err)
	}
	if r := append(Supported(taken, d), validated(t, taken, d)...); len(r) != 0 {
		t.Fatalf("a plan of one block with no steps refused: %+v", r)
	}
}

// A plan of one task whose block says whether its work merges is taken, either way: `merges`
// on the task of a one-task plan asks for what this release does, a retro and a merge of that
// task, so Supported has no cause in it. The block's word is read as taskOf reads it.
func TestAOneTaskPlanThatSaysWhetherItMergesIsTaken(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"false", false}} {
		p, err := FromBlocks("ACME-1", "plan-md", 1, []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"merges": tc.value, "steps": "implementer"}, Text: "Do it."}})
		if err != nil || len(p.Tasks) != 1 || p.Tasks[0].ID != "a" || p.Tasks[0].Merges != tc.want {
			t.Fatalf("FromBlocks(merges=%q) = %+v, %v; want task a with Merges %v", tc.value, p.Tasks, err, tc.want)
		}
		if r := Supported(p, builtin); r != nil {
			t.Fatalf("Supported(merges=%q) = %+v, want none", tc.value, r)
		}
	}
}

// Legion takes a plan of several tasks whose needs lead to one task that no task needs: the plan
// of tasks a to e, where c needs b, d needs a and c, and e needs d, and the plan of two tasks where
// b needs a, are each answered no refusal by either check, since this release runs each task of a
// plan once the tasks it needs are done.
//
// The fault it catches is a plan of several tasks, or a task that needs another, refused as not
// carried out yet (its control: the Supported of the release before, which refuses the plan of
// tasks a to e with the reason of its five tasks first).
func TestAPlanOfSeveralTasksIsTaken(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan Plan
	}{
		{"the plan of tasks a to e", planOfTasksAToE(t)},
		{"two tasks, b needing a", planOf(t, 1, taskNeeding("a"), taskNeeding("b", needWord("a")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := append(Supported(tc.plan, builtin), validated(t, tc.plan, builtin)...); len(got) != 0 {
				t.Fatalf("the checks of %s = %+v, want none", tc.name, got)
			}
		})
	}
}

// Legion takes a plan in which the work of several tasks merges, each by itself: the tasks a to e
// where b says merges, whose work merges at b and at e, and four tasks with no needs, whose work
// merges at each of them, are answered no reason of a cause this release does not carry out.
//
// The fault it catches is a plan of more than one task that merges refused as not carried out yet
// (its control: the Supported of the release before, which refuses the four tasks with the reason
// of four tasks that no task needs).
func TestAPlanWithSeveralMergesIsTaken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []Task
	}{
		{"the tasks a to e, where b says merges", tasksAToE("b")},
		{"four tasks with no needs", fourApart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supported(Plan{Tasks: tc.tasks}, builtin); got != nil {
				t.Fatalf("Supported(%s) = %+v, want none", tc.name, got)
			}
		})
	}
}

// Each cause this release does not carry out yet (Supported), with its exact text and its task.
// Each cause gets one reason, so a plan with two causes gets two. The taken plans are the subsets
// of today's line that keep the implementer, with the owner's acceptance, and with a key on a role
// but acceptance.
func TestSupportedRefusesWhatThisReleaseDoesNotCarryOut(t *testing.T) {
	implementer, _ := ParseSteps("implementer")
	withTask := func(task Task) Plan { return Plan{Tasks: []Task{task}} }
	for _, tc := range []struct {
		name string
		plan Plan
		want []Refusal
	}{
		{name: "needs that lead to one task", plan: planOf(t, 1, taskNeeding("a", "b", "dispatch://ACME-1/artifact/plan-md#b-c"), taskNeeding("b"), taskNeeding("c"))},
		{name: "a step that reads a pull request before the implementer", plan: oneTask("a", "reviewer implementer"),
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none." + yet}}},
		{name: "a task with no implementer", plan: oneTask("a", "tester reviewer"), want: []Refusal{
			{Task: "a", Reason: "task a has no step implementer." + yet},
			{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none." + yet},
			{Task: "a", Reason: "step 2 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none." + yet},
		}},
		{name: "a role two times in a task", plan: oneTask("a", "implementer reviewer reviewer"),
			want: []Refusal{{Task: "a", Reason: "task a names the role reviewer at steps 2 and 3." + yet}}},
		{name: "acceptance with no key", plan: oneTask("a", "implementer acceptance"),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names acceptance with no key, which asks for a fresh agent of the role acceptance." + yet}}},
		{name: "the key owner on a role but acceptance", plan: oneTask("a", "implementer:owner"),
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names the key owner." + yet}}},
		{name: "a key on acceptance but owner", plan: oneTask("a", "implementer acceptance:r"),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names the key r." + yet}}},
		{name: "a role block", plan: func() Plan { p := oneTask("a", "implementer"); p.RoleBlocks = []string{"prober"}; return p }(),
			want: []Refusal{{Task: "", Reason: "the plan defines the role prober." + yet}}},
		{name: "owner", plan: withTask(Task{ID: "f", Owner: "architect"}),
			want: []Refusal{{Task: "f", Reason: "task f names owner." + yet}}},
		{name: "a part of a task", plan: withTask(Task{ID: "f1", PartOf: "f", Steps: implementer}),
			want: []Refusal{{Task: "f1", Reason: "task f1 is a part of task f." + yet}}},
		{name: "two causes in one task", plan: oneTask("a", "reviewer implementer reviewer"), want: []Refusal{
			{Task: "a", Reason: "task a names the role reviewer at steps 1 and 3." + yet},
			{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none." + yet},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supported(tc.plan, builtin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Supported =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}

	for _, steps := range []string{"implementer reviewer", "planner implementer tester reviewer acceptance:owner", "implementer acceptance:owner", "implementer", "implementer:cd reviewer"} {
		if r := Supported(oneTask("a", steps), builtin); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}

	// Both checks run, so a plan with causes of each kind gets the reasons of both.
	twoTasks := oneTask("a", "implementer prober")
	twoTasks.Tasks = append(twoTasks.Tasks, Task{ID: "b", Owner: "architect"})
	got := append(Supported(twoTasks, builtin), validated(t, twoTasks, builtin)...)
	want := []Refusal{
		{Task: "b", Reason: "task b names owner." + yet},
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
		return Block{ID: id, Type: TaskBlockType, Parent: parent, Attributes: named, Text: "What block " + id + " says."}
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
			if got := validated(t, planOf(t, 1, tc.blocks...), builtin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// partBlock is the task block id under the block parent, with the attributes named in pairs, as
// TestValidateRefusesWhatAnElementCannotBe writes them.
func partBlock(id, parent string, attributes ...string) Block {
	named := map[string]string{}
	for i := 0; i+1 < len(attributes); i += 2 {
		named[attributes[i]] = attributes[i+1]
	}
	return Block{ID: id, Type: TaskBlockType, Parent: parent, Attributes: named, Text: "What block " + id + " says."}
}

// A task with a sub-owner has no steps: its work is its parts. Validate refuses a task block that
// names both, with the role of its sub-owner, and takes the same task with its sub-owner alone.
//
// The fault it catches is a task with a sub-owner that walks the steps of its block as well (its
// control: Validate with no such refusal answers nothing for the first plan).
func TestATaskWithASubOwnerNamesNoSteps(t *testing.T) {
	both := planOf(t, 1, partBlock("f", "", "owner", "architect", "steps", "planner implementer"), partBlock("f1", "f", "steps", "implementer"))
	want := []Refusal{{Task: "f", Reason: "task f names both steps and the sub-owner architect, and a task with a sub-owner has no steps: its work is its parts."}}
	if got := validated(t, both, builtin); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate(f with steps and a sub-owner) =\n%+v\nwant\n%+v", got, want)
	}
	if got := validated(t, planOf(t, 1, partBlock("f", "", "owner", "architect"), partBlock("f1", "f", "steps", "implementer")), builtin); got != nil {
		t.Fatalf("Validate(f with its sub-owner alone) = %+v, want none", got)
	}
}

// Validate refuses a plan in which a task can never start, so the task it is a part of can never be
// done: a task starts once each task it needs is done and, for a part, once its task has started,
// and a task is done only after each of its parts. Each such need is named once, at the need that
// causes the wait, and not at the needs the wait then holds back:
//   - a part that needs its own task, directly or through its tasks, with S3's reason;
//   - a part that needs a task whose needs reach its own task, with the chain to that task;
//   - a task that needs a part of its own, which starts only once the task has started;
//   - a task that needs a part of a task it stands outside, with S3's reason, named at the
//     outermost task above the part that does not hold it;
//   - a part that needs a part of a task above its own task, which is no sibling of it, named at
//     the task the part it needs is a part of: a part needs only the other parts of its own task,
//     or a whole task of the top level.
//
// It takes a part that needs its sibling, and a part that needs a task of the top level that does
// not wait for its own task.
//
// The fault it catches is a plan whose part never starts taken, or a part that needs a part of
// another task taken (its controls: needReasons with no waits answers nothing for the rows of a
// need that never ends; needReasons with its second pass alone names a's need of f in the row of
// the ids whose task of the top level sorts first, and that row fails; the need of a part read as a
// need of a task outside its own alone takes the part of a part that needs a part of the task above
// it, and that row fails).
func TestAPlanWhosePartNeverStartsIsRefused(t *testing.T) {
	f, f1 := partBlock("f", "", "owner", "architect"), partBlock("f1", "f", "steps", "implementer")
	needing := func(id, parent string, needs ...string) Block {
		words := make([]string, len(needs))
		for i, need := range needs {
			words[i] = needWord(need)
		}
		return partBlock(id, parent, "steps", "implementer", "needs", strings.Join(words, " "))
	}
	for _, tc := range []struct {
		name   string
		blocks []Block
		want   []Refusal
	}{
		{name: "a part that needs its task", blocks: []Block{f, needing("f1", "f", "f")},
			want: []Refusal{{Task: "f1", Reason: "task f1 needs f, which it is a part of, and f is done only after each of its parts."}}},
		{name: "a part of a part that needs the task above its task",
			blocks: []Block{f, partBlock("f1", "f", "owner", "architect"), needing("f1a", "f1", "f")},
			want:   []Refusal{{Task: "f1a", Reason: "task f1a needs f, which it is a part of, and f is done only after each of its parts."}}},
		{name: "a part that needs a task that needs its task", blocks: []Block{f, needing("f1", "f", "h"), needing("h", "", "f")},
			want: []Refusal{{Task: "f1", Reason: "task f1 needs h, which is done only after f1 starts: h needs f and f is done only after its part f1."}}},
		{name: "the chain held back at a sibling is named at its cause",
			blocks: []Block{f, needing("f1", "f", "h"), needing("f2", "f", "f1"), needing("h", "", "f")},
			want:   []Refusal{{Task: "f1", Reason: "task f1 needs h, which is done only after f1 starts: h needs f and f is done only after its part f1."}}},
		{name: "a part of a part that needs a task that needs the task above",
			blocks: []Block{f, partBlock("f1", "f", "owner", "architect"), needing("f1a", "f1", "h"), needing("h", "", "f")},
			want: []Refusal{{Task: "f1a", Reason: "task f1a needs h, which is done only after f1a starts: " +
				"h needs f, f is done only after its part f1 and f1 is done only after its part f1a."}}},
		{name: "the chain named at the part, whatever the order of the ids", blocks: []Block{needing("a", "", "f"), f, needing("f1", "f", "a")},
			want: []Refusal{{Task: "f1", Reason: "task f1 needs a, which is done only after f1 starts: a needs f and f is done only after its part f1."}}},
		{name: "a task that needs a part of its own", blocks: []Block{partBlock("f", "", "owner", "architect", "needs", needWord("f1")), f1},
			want: []Refusal{{Task: "f", Reason: "task f needs f1, which is done only after f starts: f1, a part of f, starts only once f has started."}}},
		{name: "a task outside a task that needs its part", blocks: []Block{f, f1, needing("h", "", "f1")},
			want: []Refusal{{Task: "h", Reason: "task h needs f1, a part of task f: a task outside f needs the whole task f, and no part of it."}}},
		{name: "a task outside a task that needs a part of its part",
			blocks: []Block{f, partBlock("f1", "f", "owner", "architect"), partBlock("f1a", "f1", "steps", "implementer"), needing("h", "", "f1a")},
			want:   []Refusal{{Task: "h", Reason: "task h needs f1a, a part of task f: a task outside f needs the whole task f, and no part of it."}}},
		{name: "a part that needs a part of its sibling",
			blocks: []Block{f, needing("f1", "f", "f2a"), partBlock("f2", "f", "owner", "architect"), partBlock("f2a", "f2", "steps", "implementer")},
			want:   []Refusal{{Task: "f1", Reason: "task f1 needs f2a, a part of task f2: a task outside f2 needs the whole task f2, and no part of it."}}},
		{name: "a part of a part that needs a part of the task above",
			blocks: []Block{f, partBlock("f1", "f", "owner", "architect"), partBlock("f2", "f", "steps", "implementer"), needing("f1a", "f1", "f2")},
			want: []Refusal{{Task: "f1a", Reason: "task f1a needs f2, a part of task f: " +
				"a part needs only the other parts of its own task, or a whole task of the top level."}}},
		{name: "a part that needs a sibling", blocks: []Block{f, f1, needing("f2", "f", "f1")}},
		{name: "a part that needs a task of the top level", blocks: []Block{partBlock("g", "", "steps", "implementer"), f, needing("f1", "f", "g")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validated(t, planOf(t, 1, tc.blocks...), builtin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A need that lies on a cycle of needs and is also a need of a part of a task its task stands
// outside gets one reason, the cycle's: h needs f1, a part of f, and f1 needs h. Validate names the
// cycle once, at f1, its first task in id order, and gives h's need no second reason. A need of a
// part that closes no cycle keeps its own reason, as TestAPlanWhosePartNeverStartsIsRefused holds.
//
// The fault it catches is one need of the need graph judged by two loops that do not agree, so it
// gets two reasons (its control: the need of a part judged before the cycles are cut, which gives
// h's need its own reason beside the cycle's, and the case fails).
func TestANeedOnACycleGetsTheCyclesReasonAlone(t *testing.T) {
	words := func(needs ...string) string {
		for i, need := range needs {
			needs[i] = needWord(need)
		}
		return strings.Join(needs, " ")
	}
	p := planOf(t, 1, partBlock("f", "", "owner", "architect"),
		partBlock("f1", "f", "steps", "implementer", "needs", words("h")),
		partBlock("h", "", "steps", "implementer", "needs", words("f1")))
	want := []Refusal{{Task: "f1", Reason: "task f1 needs itself: f1 needs h, h needs f1."}}
	if got := validated(t, p, builtin); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate =\n%+v\nwant\n%+v", got, want)
	}
}

// Two tasks with a sub-owner whose sub-owners' agents take one name (SubOwnerAgent) are refused at
// the second in id order: a short id `t-<12 hex>` and a long id whose SHA-256 starts with those 12
// hex give one name, and one claim cannot be two sub-owners. Each alone is taken.
//
// The fault it catches is the sub-owner of one task taken for the sub-owner of another (its
// control: Validate with no test of the names answers nothing for the two).
func TestTwoSubOwnersOfOneAgentNameAreRefused(t *testing.T) {
	const long = "the-whole-task-of-many-words"
	sum := sha256.Sum256([]byte(long))
	short := "t-" + hex.EncodeToString(sum[:6])
	if SubOwnerAgent(short) != SubOwnerAgent(long) {
		t.Fatalf("SubOwnerAgent(%s) = %s and SubOwnerAgent(%s) = %s, want one name", short, SubOwnerAgent(short), long, SubOwnerAgent(long))
	}
	two := planOf(t, 1, partBlock(short, "", "owner", "architect"), partBlock(long, "", "owner", "architect"))
	// `t-` sorts before `th`, so the long id is the second in id order.
	want := []Refusal{{Task: long, Reason: "task " + long + " names a sub-owner, and its agent takes the name " + string(SubOwnerAgent(long)) +
		", which the agent of the sub-owner of task " + short + " takes too: give one of the two tasks another id."}}
	if got := validated(t, two, builtin); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate(two sub-owners of one name) =\n%+v\nwant\n%+v", got, want)
	}
	for _, id := range []string{short, long} {
		if got := validated(t, planOf(t, 1, partBlock(id, "", "owner", "architect")), builtin); got != nil {
			t.Fatalf("Validate(%s alone) = %+v, want none", id, got)
		}
	}
}

// A step can name its agent by a key, and the steps of one key are one agent, so a plan whose
// steps share a key is taken. The key owner stays the owner's alone, and the owner alone does the
// accepting step: the key owner on a role but acceptance, and any other key on acceptance, are
// refused, each with the sentence of a step this release does not carry out, since Legion would
// otherwise run the step in another agent than the plan names.
func TestAKeyOtherThanOwnerIsTaken(t *testing.T) {
	if got := Supported(oneTask("t", "implementer:a tester:a reviewer"), builtin); len(got) != 0 {
		t.Fatalf("Supported = %v, want none", got)
	}
	if got := Supported(oneTask("t", "implementer:owner reviewer"), builtin); len(got) != 1 || got[0].Reason != "step 1 of task t names the key owner."+yet {
		t.Fatalf("Supported = %v, want the key owner refused", got)
	}
	if got := Supported(oneTask("t", "implementer:a acceptance:a"), builtin); len(got) != 1 || got[0].Reason != "step 2 of task t names the key a."+yet {
		t.Fatalf("Supported = %v, want the key a on acceptance refused", got)
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
		if r := append(Supported(p, builtin), validated(t, p, builtin)...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
	}
	want := Refusal{Task: "a", Reason: "task a names the role tester at steps 2 and 4. This release of Legion does not carry it out yet."}
	if r := Supported(oneTask("a", "implementer tester reviewer tester"), builtin); len(r) != 1 || r[0] != want {
		t.Fatalf("got %+v", r)
	}
}

// The tester, the reviewer and the owner's acceptance read the pull request the implementer opens
// (hasOpenOrMergedPR, the one guard of a move on), so this release refuses a step of one of those
// roles with no implementer step before it, by its role and with its word as the plan writes it.
// The planner reads no pull request, so it stands anywhere.
func TestAStepThatReadsAPullRequestNeedsAnImplementerBeforeIt(t *testing.T) {
	for _, tc := range []struct{ steps, reason string }{
		{"tester implementer", "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none."},
		{"reviewer implementer", "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none."},
		{"acceptance:owner implementer", "step 1 of task a names acceptance:owner, and no step before it names the role implementer, whose pull request acceptance:owner reads; a role of this deployment that reports pass or fail needs none."},
		{"planner reviewer implementer", "step 2 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none."},
	} {
		want := []Refusal{{Task: "a", Reason: tc.reason + yet}}
		if r := Supported(oneTask("a", tc.steps), builtin); !reflect.DeepEqual(r, want) {
			t.Fatalf("Supported(%s) =\n%+v\nwant\n%+v", tc.steps, r, want)
		}
	}
	if r := Supported(oneTask("a", "implementer planner"), builtin); len(r) != 0 {
		t.Fatalf("Supported(implementer planner) = %+v, want none", r)
	}
	// The plan `tester` alone has two causes, in the order Supported builds them, and both stand.
	want := []Refusal{
		{Task: "a", Reason: "task a has no step implementer." + yet},
		{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none." + yet},
	}
	if r := Supported(oneTask("a", "tester"), builtin); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(tester) =\n%+v\nwant\n%+v", r, want)
	}
}

// withRole is Legion's own roles and one role of the deployment, name, of type typ.
func withRole(name string, typ Type) Deployment {
	roles := BuiltinRoles()
	roles[name] = RoleDef{Name: name, Type: typ}
	return Deployment{Roles: roles}
}

// Legion's retro is done by the implementer in this release, so a task whose last step of the
// type implementation is a role of the deployment after the implementer is refused: its retro
// would go to that role's agent, which this release does not carry out. A role of the deployment
// of the type implementation before the implementer leaves the implementer the last step of that
// type, and is taken. The control: the same plan under a deployment whose coder does testing work
// is taken, so the refusal reads the type the deployment gives the role.
func TestARetroOfAnotherRoleIsNotCarriedOutYet(t *testing.T) {
	d := withRole("coder", TypeImplementation)
	want := []Refusal{{Task: "a", Reason: "task a names coder at step 2, a step of the type implementation after the step implementer at step 1, and Legion's retro is done by the implementer in this release. This release of Legion does not carry it out yet."}}
	if r := Supported(oneTask("a", "implementer coder"), d); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(implementer coder) =\n%+v\nwant\n%+v", r, want)
	}
	for _, taken := range []struct {
		steps string
		d     Deployment
	}{
		{"coder implementer", d},
		{"implementer coder", withRole("coder", TypeTesting)},
	} {
		p := oneTask("a", taken.steps)
		if r := append(Supported(p, taken.d), validated(t, p, taken.d)...); len(r) != 0 {
			t.Fatalf("Supported(%s) under coder of type %s = %+v, want none", taken.steps, taken.d.Roles["coder"].Type, r)
		}
	}
}

// The refusal of a step that reads a pull request with no implementer step before it says that a
// role of the deployment, which reports pass or fail, needs none. It reads the role and never the
// type: a role of the deployment of the type review before the implementer is taken, where
// Legion's reviewer there is refused.
func TestAStepThatReadsAPullRequestNamesARoleOfTheDeploymentThatNeedsNone(t *testing.T) {
	d := withRole("critic", TypeReview)
	want := []Refusal{{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none. This release of Legion does not carry it out yet."}}
	if r := Supported(oneTask("a", "reviewer implementer"), d); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(reviewer implementer) =\n%+v\nwant\n%+v", r, want)
	}
	p := oneTask("a", "critic implementer")
	if r := append(Supported(p, d), validated(t, p, d)...); len(r) != 0 {
		t.Fatalf("Supported(critic implementer) = %+v, want none", r)
	}
}

// A role runs at most one step of a task: a role of the deployment named at two steps is refused
// as Legion's own roles are, so neither an owner's plan (api's register_plan) nor a project's
// default steps (config's checkDefaultSteps), the two writers of a task's steps, can name one. The
// daemon reads the role of the step a task stands at as the step (record.Issue.HoldsQueued, the
// phases row of a role), which holds only while this does. The control: the same deployment's
// plan that names the role once, beside another role of the deployment, is taken.
func TestARoleOfTheDeploymentAtTwoStepsIsRefused(t *testing.T) {
	d := withRole("checker", TypeTesting)
	d.Roles["linter"] = RoleDef{Name: "linter", Type: TypeTesting}
	want := []Refusal{{Task: "a", Reason: "task a names the role checker at steps 2 and 4." + yet}}
	if r := Supported(oneTask("a", "implementer checker linter checker"), d); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(implementer checker linter checker) =\n%+v\nwant\n%+v", r, want)
	}
	p := oneTask("a", "implementer checker linter")
	if r := append(Supported(p, d), validated(t, p, d)...); len(r) != 0 {
		t.Fatalf("Supported(implementer checker linter) = %+v, want none", r)
	}
}
