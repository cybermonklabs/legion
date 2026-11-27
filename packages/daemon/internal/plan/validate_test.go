package plan

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// yet ends each refusal of something a later release carries out.
const yet = " This release of Legion does not carry it out yet."

// builtin is the deployment of Legion's own roles alone.
var builtin = Deployment{Roles: BuiltinRoles()}

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
	// architect, merger and controller are no roles of a step: Legion's step roles are five, and a deployment adds its own.
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
	empty, err := FromBlocks("plan-md", 1, []Block{{ID: "intro", Type: "paragraph", Text: "No task here."}})
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
	if r := Supported(empty, d); len(r) != 0 {
		t.Fatalf("Supported(no task) = %+v, want none: the cause is Validate's alone", r)
	}
	taken, err := FromBlocks("plan-md", 1, []Block{{ID: "a", Type: "plan_task", Text: "Do the change."}})
	if err != nil {
		t.Fatalf("FromBlocks(one block): %v", err)
	}
	if r := append(Supported(taken, d), Validate(taken, d)...); len(r) != 0 {
		t.Fatalf("a plan of one block with no steps refused: %+v", r)
	}
}

// Each cause this release does not carry out yet (Supported), with its exact text and its task.
// Each cause gets one reason, so a plan with two causes gets two. The taken plans keep the
// implementer before each tester, reviewer and acceptance step, a role of the deployment of the
// type implementation before the implementer, and the other steps at any place.
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
		}}, want: []Refusal{
			{Task: "", Reason: "the plan has 3 tasks (a, b, c), and this release runs one task for each issue." + yet},
			{Task: "", Reason: "the plan has 3 tasks that no task needs (a, b, c), and the work of each would merge by itself." + yet},
		}},
		{name: "needs", plan: planOf(t, 1, taskNeeding("a", "b", "dispatch://ACME-2/artifact/plan-md#b-c"), taskNeeding("b"), taskNeeding("c")),
			want: []Refusal{
				{Task: "", Reason: "the plan has 3 tasks (a, b, c), and this release runs one task for each issue." + yet},
				{Task: "a", Reason: "task a needs b, c." + yet},
			}},
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
		{name: "part_of", plan: withTask(Task{ID: "f1", PartOf: "f", Steps: implementer}),
			want: []Refusal{{Task: "f1", Reason: "task f1 names part_of." + yet}}},
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

	for _, steps := range []string{"implementer reviewer", "planner implementer tester reviewer acceptance:owner", "implementer acceptance:owner", "implementer"} {
		if r := Supported(oneTask("a", steps), builtin); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}

	// Both checks run, so a plan with causes of each kind gets the reasons of both, as one refusal.
	twoTasks := oneTask("a", "implementer prober")
	twoTasks.Tasks = append(twoTasks.Tasks, Task{ID: "b", Steps: implementer})
	got := append(Supported(twoTasks, builtin), Validate(twoTasks, builtin)...)
	want := []Refusal{
		{Task: "", Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue." + yet},
		{Task: "", Reason: "the plan has 2 tasks that no task needs (a, b), and the work of each would merge by itself." + yet},
		{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two tasks and prober =\n%+v\nwant\n%+v", got, want)
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
		if r := Refusals(p, builtin); len(r) != 0 {
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
// is taken, so the refusal reads the type the deployment gives the role. Each such step is a cause
// of its own kind, which the refusal bounds as it bounds each kind (reasons): the first
// MostReasons named, then one reason of the whole plan that counts the rest.
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
		if r := Refusals(p, taken.d); len(r) != 0 {
			t.Fatalf("Supported(%s) under coder of type %s = %+v, want none", taken.steps, taken.d.Roles["coder"].Type, r)
		}
	}
	coders := numbered("coder", MostReasons+3)
	many := Deployment{Roles: BuiltinRoles()}
	for _, coder := range coders {
		many.Roles[coder] = RoleDef{Name: coder, Type: TypeImplementation}
	}
	r := Supported(oneTask("a", "implementer "+strings.Join(coders, " ")), many)
	if len(r) != MostReasons+1 || !strings.Contains(r[MostReasons-1].Reason, "names "+coders[MostReasons-1]+" at step") ||
		r[MostReasons].Task != "" || !strings.Contains(r[MostReasons].Reason, " 3 ") {
		t.Fatalf("Supported(implementer and %d coders) = %.3000v, want the first %d named and the 3 left counted", len(coders), r, MostReasons)
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
	if r := Refusals(p, d); len(r) != 0 {
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
	if r := Refusals(p, d); len(r) != 0 {
		t.Fatalf("Supported(implementer checker linter) = %+v, want none", r)
	}
}

// numbered is count words, prefix then a number of five digits from 1, so no word holds another.
func numbered(prefix string, count int) []string {
	words := make([]string, count)
	for i := range words {
		words[i] = fmt.Sprintf("%s%05d", prefix, i+1)
	}
	return words
}

// shown is a reason a refusal holds: one of task that names item, or, where count is above 0, one
// of the whole plan that counts the rest of a kind of cause.
type shown struct {
	task, item string
	count      int
}

// naming is one reason of task for each item, each naming it, in order.
func naming(task string, items ...string) []shown {
	reasons := make([]shown, len(items))
	for i, item := range items {
		reasons[i] = shown{task: task, item: item}
	}
	return reasons
}

// counting is the reason that counts count more causes of a kind.
func counting(count int) []shown { return []shown{{count: count}} }

// One refusal of a plan (Refusals: Supported's reasons, then Validate's, as every caller reads
// them) names the first MostReasons causes of each kind in the order the checks find them, and
// right after the last of them one more reason, of the whole plan, counts the rest of that kind,
// so the number of its reasons does not grow with the plan and no kind pushes another out. Each
// check that finds a cause for each item of the plan is held to it: a `needs` word that names no
// task, a role nobody defines, a step's key, and a step that reads a pull request with no
// implementer before it. A kind with MostReasons causes or fewer is named whole. Each kind has its
// own bound, so a cycle is named whatever else the plan has, and the reasons named keep the order
// the checks find them in, the dropped ones left out. Each case holds the task and the word or
// step each reason names, and each count, in order; none holds a sentence. The fault is a refusal
// that names every cause, or one bound for the whole refusal, under which the plan of a ring of 22
// tasks names 19 of its needs and counts its cycle. The control is reasons.add with its bound
// taken out, under which each case past the bound names every cause.
func TestARefusalNamesTheFirstReasonsOfEachKindAndCountsTheRest(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	steps := func(words ...[]string) Plan { return oneTask("a", strings.Join(slices.Concat(words...), " ")) }
	keyed := func(role string, keys []string) []string {
		words := make([]string, len(keys))
		for i, key := range keys {
			words[i] = role + ":" + key
		}
		return words
	}
	testers := func(count int) []string { return slices.Repeat([]string{"tester"}, count) }
	// stepAt names the steps from 1 to count, as a reason of a step names it.
	stepAt := func(count int) []string {
		words := make([]string, count)
		for i := range words {
			words[i] = fmt.Sprintf("step %d of", i+1)
		}
		return words
	}
	implementer := []string{"implementer"}
	// ringOf is count tasks t01 to t<count>, each needing the next and the last needing t01, with
	// the reasons of the first MostReasons needs, which this release does not carry out yet; the
	// one set of tasks that need each other follows them.
	ringOf := func(count int) (ring []Block, needs []shown) {
		for i := 1; i <= count; i++ {
			id, next := fmt.Sprintf("t%02d", i), fmt.Sprintf("t%02d", i%count+1)
			ring = append(ring, taskNeeding(id, next))
			if i <= MostReasons {
				needs = append(needs, shown{task: id, item: next})
			}
		}
		return ring, needs
	}
	ring, ringNeeds := ringOf(22)
	ring21, ring21Needs := ringOf(21)
	// selves is 21 tasks s01 to s21, each needing itself: 21 sets of one task. Of its first 20 tasks,
	// the reason of the need holds `task s01 needs s01.` and the reason of the set `needs itself:
	// s01 needs s01`, which the other does not hold, so a swap of the two groups fails.
	var selves []Block
	var selfNeeds, selfSets []shown
	for i := 1; i <= 21; i++ {
		id := fmt.Sprintf("s%02d", i)
		selves = append(selves, taskNeeding(id, id))
		if i <= MostReasons {
			selfNeeds = append(selfNeeds, shown{task: id, item: "task " + id + " needs " + id + "."})
			selfSets = append(selfSets, shown{task: id, item: "needs itself: " + id + " needs " + id})
		}
	}
	// mixed is task a with 25 words that name no task and two roles nobody defines, then task b
	// with 3 such words and one such role. The checks find them in that order (Validate's loop over
	// the tasks), so the 21st to 28th words are dropped, and b's role comes after a's.
	mixed := planOf(t, 1,
		Block{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer r00001 r00002", "needs": strings.Join(numbered("x", 25), " ")}},
		Block{ID: "b", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer r00003", "needs": strings.Join(numbered("y", 3), " ")}})
	const past = MostReasons + 7
	for _, tc := range []struct {
		name string
		plan Plan
		want []shown
	}{
		{name: "needs words that name no task, past the bound", plan: planOf(t, 1, taskNeeding("a", numbered("x", past)...)),
			want: slices.Concat(naming("a", numbered("x", MostReasons)...), counting(7))},
		{name: "needs words that name no task, at the bound", plan: planOf(t, 1, taskNeeding("a", numbered("x", MostReasons)...)),
			want: naming("a", numbered("x", MostReasons)...)},
		{name: "roles nobody defines, past the bound", plan: steps(implementer, numbered("r", past)),
			want: slices.Concat(naming("a", numbered("r", MostReasons)...), counting(7))},
		{name: "roles nobody defines, at the bound", plan: steps(implementer, numbered("r", MostReasons)),
			want: naming("a", numbered("r", MostReasons)...)},
		// Each step reviewer:kNNNNN names a key, and then the role reviewer is named at two steps.
		{name: "keys, past the bound", plan: steps(implementer, keyed("reviewer", numbered("k", past))),
			want: slices.Concat(naming("a", numbered("k", MostReasons)...), counting(7), naming("a", "reviewer at steps 2 and 3"))},
		{name: "keys, at the bound", plan: steps(implementer, keyed("reviewer", numbered("k", MostReasons))),
			want: slices.Concat(naming("a", numbered("k", MostReasons)...), naming("a", "reviewer at steps 2 and 3"))},
		// The role tester is named at two steps, and then each tester step has no implementer before it.
		{name: "steps with no implementer before them, past the bound", plan: steps(testers(past), implementer),
			want: slices.Concat(naming("a", "tester at steps 1 and 2"), naming("a", stepAt(MostReasons)...), counting(7))},
		{name: "steps with no implementer before them, at the bound", plan: steps(testers(MostReasons), implementer),
			want: slices.Concat(naming("a", "tester at steps 1 and 2"), naming("a", stepAt(MostReasons)...))},
		{name: "Supported's kinds and Validate's each have their own bound", plan: steps(implementer, keyed("reviewer", numbered("k", 25)), numbered("r", 25)),
			want: slices.Concat(naming("a", numbered("k", MostReasons)...), counting(5), naming("a", "reviewer at steps 2 and 3"),
				naming("a", numbered("r", MostReasons)...), counting(5))},
		// The ring's chain of 22 needs is named by its first MostReasons - 1, a count of the two
		// left out, and the need that closes it.
		{name: "a cycle is named after more than MostReasons reasons of another kind", plan: planOf(t, 1, ring...),
			want: slices.Concat(naming("", "(t01, t02, "), ringNeeds, counting(2), naming("t01", "t19 needs t20, 2 more needs, t22 needs t01."))},
		// A chain that leaves one need out counts it as one need, not "1 more needs": 21 tasks give
		// 19 named needs, the one left out, and the need that closes the ring.
		{name: "a cycle of 21 tasks counts the one need it leaves out", plan: planOf(t, 1, ring21...),
			want: slices.Concat(naming("", "(t01, t02, "), ring21Needs, counting(1), naming("t01", "t19 needs t20, 1 more need, t21 needs t01."))},
		{name: "21 sets of tasks that need each other: 20 named, 1 counted", plan: planOf(t, 1, selves...),
			want: slices.Concat(naming("", "(s01, s02, "), selfNeeds, counting(1), selfSets, counting(1))},
		{name: "the reasons named keep the checks' order, the dropped ones left out", plan: mixed,
			want: slices.Concat(naming("", "(a, b)", "(a, b)"), naming("a", numbered("x", MostReasons)...), counting(8),
				naming("a", "r00001", "r00002"), naming("b", "r00003"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Refusals(tc.plan, d)
			if len(got) != len(tc.want) {
				t.Fatalf("the refusal holds %d reasons, want %d: %.3000v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				reason := got[i]
				switch {
				case want.count > 0:
					if reason.Task != "" || !strings.Contains(reason.Reason, fmt.Sprintf(" %d ", want.count)) {
						t.Errorf("reason %d is %+v, want one of the whole plan counting %d more of a kind", i+1, reason, want.count)
					}
				case reason.Task != want.task || !strings.Contains(reason.Reason, want.item):
					t.Errorf("reason %d is %+v, want one of task %q naming %q", i+1, reason, want.task, want.item)
				}
			}
		})
	}
}

// Every place that quotes a word of the plan quotes it to at most MostQuotedBytes, cut at the
// start of a character and marked with the length of the word, and a word at the bound whole:
// a need, a role nobody defines, a role block's id, a key, a role at two steps and a step that
// reads a pull request with no implementer before it, each a reason whose task the case holds;
// one id on two blocks and a step outside the grammar, each an error of the read. And FromBlocks
// takes a task id at steprecord.TaskIDMax and refuses one outside Dispatch's task form with one
// reason, which states the form and quotes the id the same way. The fault is a reason that quotes
// a word whole; the control is quoted with its cut taken out, under which each long word is
// quoted whole and the 900 KB id's refusal grows with it.
func TestAReasonQuotesAWordOfThePlanWithinABound(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	atBound := strings.Repeat("x", MostQuotedBytes)
	long := strings.Repeat("x", 1<<20)
	// 201 bytes whose byte at the bound falls inside a two-byte é.
	wide := "a" + strings.Repeat("é", 100)
	role := "r" + strings.Repeat("o", 300)
	cut := func(word string) string {
		return fmt.Sprintf("%s… (cut from %d bytes)", word[:MostQuotedBytes], len(word))
	}
	for _, tc := range []struct {
		name   string
		got    []Refusal
		at     int    // the reason that quotes the word, among the reasons the plan gets
		task   string // that reason's task
		quote  string // the word as that reason quotes it
		quotes int    // how many times the reason names that quote
	}{
		{"a need of 1 MiB", Validate(planOf(t, 1, taskNeeding("a", long)), d), 0, "a", cut(long) + ",", 1},
		{"a need at the bound", Validate(planOf(t, 1, taskNeeding("a", atBound)), d), 0, "a", "needs " + atBound + ",", 1},
		{"a need cut at the start of a character", Validate(planOf(t, 1, taskNeeding("a", wide)), d), 0, "a", wide[:127] + "… (cut from 201 bytes),", 1},
		{"a role of 301 bytes", Validate(oneTask("a", "implementer "+role), d), 0, "a", cut(role) + ",", 1},
		{"a key of 301 bytes", Supported(oneTask("a", "implementer reviewer:"+role), d), 0, "a", cut(role) + ".", 1},
		{"a role block's id of 301 bytes", Supported(Plan{RoleBlocks: []string{role}, Tasks: []Task{{ID: "a", Steps: []Step{{Role: "implementer"}}}}}, d), 0, "", cut(role) + ".", 1},
		{"a role of 301 bytes at two steps", Supported(oneTask("a", "implementer "+role+" "+role), d), 0, "a", cut(role) + " at steps 2 and 3.", 1},
		// The key's reason comes first, then the step's. stepReasons names the whole word
		// `tester:<key>` twice, so its size includes two quotes.
		{"a step of 308 bytes read before the implementer", Supported(oneTask("a", "tester:"+role+" implementer"), d), 1, "a", cut("tester:"+role) + ",", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) <= tc.at {
				t.Fatalf("the reasons %.400q hold no reason %d", tc.got, tc.at+1)
			}
			reason := tc.got[tc.at]
			if reason.Task != tc.task || !strings.Contains(reason.Reason, tc.quote) || len(reason.Reason) > 300+tc.quotes*len(tc.quote) {
				t.Fatalf("reason %d is %.400q of task %q, want one of task %q quoting %.200q within 300 bytes of it", tc.at+1, reason.Reason, reason.Task, tc.task, tc.quote)
			}
			if count := strings.Count(reason.Reason, strings.TrimSuffix(tc.quote, ",")); count != tc.quotes {
				t.Errorf("reason %d names the quote %d times, want %d: %.600q", tc.at+1, count, tc.quotes, reason.Reason)
			}
		})
	}
	// This table is the whole boundary, rather than examples someone picked. For each total byte
	// length from eight below through eight above the bound, each character width 1 to 4 starts at
	// every byte that can end at or straddle it. `wholePrefixAtMost` is the independent invariant:
	// after URL redaction, quoted keeps the longest whole-character prefix at most the bound. A
	// malformed-byte row holds the loop to its bounded look-back, and a URL row holds it to decide
	// the length after redaction, so it never indexes past a redacted string.
	for _, edge := range quoteBoundaryCases() {
		t.Run("a need "+edge.name, func(t *testing.T) {
			redacted := redact.URLCredentials(edge.word)
			kept, cut := wholePrefixAtMost(redacted, MostQuotedBytes)
			want := "needs " + kept + ","
			if cut {
				want = fmt.Sprintf("%s… (cut from %d bytes),", kept, len(redacted))
			}
			got := Validate(planOf(t, 1, taskNeeding("a", edge.word)), d)
			if len(got) == 0 || got[0].Task != "a" || !strings.Contains(got[0].Reason, want) {
				t.Fatalf("Validate of %s, %d bytes before redaction and %d after, = %.400q, want a reason of task a quoting %q",
					edge.name, len(edge.word), len(redacted), got, want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		blocks []Block
		quote  string
	}{
		{"one id of 301 bytes on two blocks", []Block{{ID: role, Type: RoleBlockType}, taskNeeding(role)}, cut(role)},
		{"a step of 301 bytes outside the grammar", []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"steps": "implementer " + strings.ToUpper(role)}}}, cut(strings.ToUpper(role))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromBlocks("plan-md", 1, tc.blocks)
			if err == nil || !strings.Contains(err.Error(), tc.quote) || len(err.Error()) > 300+len(tc.quote) {
				t.Fatalf("FromBlocks = %.600v, want one error within 300 bytes of the quote %.200q", err, tc.quote)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		id    string
		taken bool
	}{
		{"an id of TaskIDMax", "a" + strings.Repeat("0", steprecord.TaskIDMax-1), true},
		{"an id one byte past it", "a" + strings.Repeat("0", steprecord.TaskIDMax), false},
		{"an id with a dot", "a.b", false},
		{"an id with a capital", "A", false},
		{"an id of 900 KB", strings.Repeat("a", 900<<10), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromBlocks("plan-md", 1, []Block{taskNeeding(tc.id)})
			if tc.taken != (err == nil) || err != nil && (len(err.Error()) > 500 ||
				!strings.Contains(err.Error(), "the task "+quoted(tc.id)+" has an id") || !strings.Contains(err.Error(), "lowercase letters, digits and hyphens")) {
				t.Fatalf("FromBlocks(an id of %d bytes) = %.600v, want taken %v, or one reason within 500 bytes stating the form", len(tc.id), err, tc.taken)
			}
		})
	}
}

type quoteBoundaryCase struct {
	name string
	word string
}

// quoteBoundaryCases generates each valid word at the quote's boundary: every total byte length
// from eight below through eight above it, every character width, and each position where that
// character ends at or crosses it. The malformed row proves Bytes looks back only as far as it
// promises. The URL row is longer than the bound before redaction and shorter after it.
func quoteBoundaryCases() []quoteBoundaryCase {
	const boundarySpan = 8
	cases := []quoteBoundaryCase{}
	for total := MostQuotedBytes - boundarySpan; total <= MostQuotedBytes+boundarySpan; total++ {
		for _, char := range []string{"x", "é", "€", "𝄞"} {
			if total < MostQuotedBytes && total >= len(char) {
				start := total - len(char)
				cases = append(cases, quoteBoundaryCase{
					name: fmt.Sprintf("%d bytes, a %d-byte character ending at byte %d", total, len(char), total),
					word: strings.Repeat("a", start) + char,
				})
			}
			for start := max(0, MostQuotedBytes-len(char)); start <= MostQuotedBytes; start++ {
				suffix := total - start - len(char)
				if suffix < 0 {
					continue
				}
				cases = append(cases, quoteBoundaryCase{
					name: fmt.Sprintf("%d bytes, a %d-byte character at byte %d", total, len(char), start),
					word: strings.Repeat("a", start) + char + strings.Repeat("z", suffix),
				})
			}
		}
	}
	cases = append(cases,
		quoteBoundaryCase{
			name: "a malformed-byte run at the bound",
			word: strings.Repeat("a", MostQuotedBytes-boundarySpan) + strings.Repeat("\x80", boundarySpan*2),
		},
		quoteBoundaryCase{
			name: "a URL redacted under the bound",
			word: "https://" + strings.Repeat("u", MostQuotedBytes+boundarySpan*2) + "@h.example/p",
		},
	)
	return cases
}

// wholePrefixAtMost is the longest whole-character prefix of text whose bytes fit limit. It uses
// Go's range positions, not redact.Bytes, so quoteBoundaryCases holds the result to the invariant
// rather than restating Bytes' look-back loop. A malformed byte is one range item, so its prefix
// stays as long as the limit lets it.
func wholePrefixAtMost(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	end := 0
	for at := range text {
		if at > limit {
			break
		}
		end = at
	}
	return text[:end], true
}

// The daemon holds a task block's id to Dispatch's own form, and to no form of its own: the one
// generated from packages/contracts, which Dispatch's step write and plan write hold a task to and
// which the daemon reads through steprecord (steprecord.TaskIDHolds). Over the ids below,
// FromBlocks takes the block if and only if steprecord.TaskIDHolds takes its id, and the reason of
// an id it refuses states the rule's words as Dispatch's refusal states them
// (steprecord.TaskIDRule). The fault is a second form in the daemon, which a change of Dispatch's
// leaves behind with no test failing; the control is this comparison, which fails for any id the
// two read apart.
func TestFromBlocksHoldsATaskIDToDispatchsForm(t *testing.T) {
	for _, id := range []string{"a", "0", "default", "a-b", "01a1edce-23dd-726e-b804-626dd5bb3957",
		strings.Repeat("a", steprecord.TaskIDMax), strings.Repeat("a", steprecord.TaskIDMax+1),
		"a.b", "A", "-a", "a b", "a_b", ""} {
		t.Run(fmt.Sprintf("%.32q of %d bytes", id, len(id)), func(t *testing.T) {
			_, err := FromBlocks("plan-md", 1, []Block{taskNeeding(id)})
			if want := steprecord.TaskIDHolds(id); want != (err == nil) {
				t.Fatalf("FromBlocks(%.64q) = %.300v, and steprecord.TaskIDHolds says %t", id, err, want)
			}
			if err != nil && id != "" && !strings.Contains(err.Error(), steprecord.TaskIDRule) {
				t.Errorf("the reason %.300q does not state the rule %q", err.Error(), steprecord.TaskIDRule)
			}
		})
	}
}

// plantedUser is the user name a planted URL carries as its userinfo, 300 bytes, written as the
// redact package's tests write theirs: a user name with no password, which a secret scanner takes
// for no credential, and which redact.URLCredentials removes as it removes a password or a token.
var plantedUser = strings.Repeat("planted-reader-", 20)

// plantedURL is a URL whose userinfo is plantedUser, and redactedURL is what the URL rule
// (redact.URLCredentials) leaves of it, read from the rule as quoteBoundaryCases' rows read it,
// never written out here: a rule that removes the userinfo and one that writes a mark in place of
// the whole authority both leave this test true. Its path is long enough that what the rule
// leaves is still past MostQuotedBytes, so a reason quotes it cut.
var (
	plantedURL  = "https://" + plantedUser + "@h.example/" + strings.Repeat("p", 200)
	redactedURL = redact.URLCredentials(plantedURL)
)

// credentialIn is the first run of six bytes of plantedUser that text holds, and "" where it holds
// none.
func credentialIn(text string) string {
	for at := 0; at+6 <= len(plantedUser); at++ {
		if part := plantedUser[at : at+6]; strings.Contains(text, part) {
			return part
		}
	}
	return ""
}

// No byte of a URL's credential that a word of the plan carries reaches a reason or an error that
// quotes the word, however long the word is: the word is read whole through the URL rule
// (redact.URLCredentials) before it is cut to MostQuotedBytes, so the cut, and the length its mark
// gives, are of what the rule left. Each place that quotes a word is given plantedURL: a need, a
// role nobody defines, a role block's id, a key, a role at two steps and a step read before the
// implementer, each a reason of its task or of the whole plan, and one id on two blocks, a task id
// outside the form and a step outside the grammar, each an error of the read, which the route
// answers as a reason of the whole plan. Each case holds the reason's task, the quote of
// redactedURL cut and marked, and no run of six bytes of plantedUser, and each error of the read
// names no length of plantedURL, which would give away the length of its credential, where the
// rule leaves neither a part nor the length of one (redact.URLCredentials). The fault is a cut
// made before the rule, which can end inside the userinfo, before its "@", where the rule then
// finds no userinfo; the control is quoted cutting first, under which each quote keeps 120 bytes
// of plantedUser. The fault of the length is an error that prints the raw word's length beside
// its quote.
func TestAReasonQuotesNoByteOfAURLsCredential(t *testing.T) {
	d := Deployment{Roles: BuiltinRoles()}
	implementer := Step{Role: "implementer"}
	withSteps := func(steps ...Step) Plan { return Plan{Tasks: []Task{{ID: "a", Steps: steps}}} }
	cut := func(word string) string {
		return fmt.Sprintf("%s… (cut from %d bytes)", word[:MostQuotedBytes], len(word))
	}
	quote := cut(redactedURL)
	for _, tc := range []struct {
		name  string
		got   []Refusal
		task  string // the task of the reason that quotes the word
		quote string
	}{
		{"a need", Validate(planOf(t, 1, taskNeeding("a", plantedURL)), d), "a", quote},
		{"a role nobody defines", Validate(withSteps(implementer, Step{Role: plantedURL}), d), "a", quote},
		{"a role block's id", Supported(Plan{RoleBlocks: []string{plantedURL}, Tasks: []Task{{ID: "a", Steps: []Step{implementer}}}}, d), "", quote},
		{"a key", Supported(withSteps(implementer, Step{Role: "reviewer", Key: plantedURL}), d), "a", quote},
		{"a role at two steps", Supported(withSteps(implementer, Step{Role: plantedURL}, Step{Role: plantedURL}), d), "a", quote},
		{"a step read before the implementer", Supported(withSteps(Step{Role: "tester", Key: plantedURL}, implementer), d), "a", cut("tester:" + redactedURL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) == 0 || !slices.ContainsFunc(tc.got, func(r Refusal) bool { return r.Task == tc.task && strings.Contains(r.Reason, tc.quote) }) {
				t.Fatalf("the reasons %.1000q, want one of task %q quoting %.300q", tc.got, tc.task, tc.quote)
			}
			for _, reason := range tc.got {
				if part := credentialIn(reason.Reason); part != "" {
					t.Errorf("the reason %.1000q holds %q of the URL's credential", reason.Reason, part)
				}
			}
		})
	}
	for _, tc := range []struct {
		name   string
		blocks []Block
	}{
		{"one id on two blocks", []Block{{ID: plantedURL, Type: RoleBlockType}, {ID: plantedURL, Type: TaskBlockType}}},
		{"a task id outside the form", []Block{taskNeeding(plantedURL)}},
		{"a step outside the grammar", []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"steps": plantedURL}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromBlocks("plan-md", 1, tc.blocks)
			if err == nil || !strings.Contains(err.Error(), quote) {
				t.Fatalf("FromBlocks = %.1000v, want an error quoting %.300q", err, quote)
			}
			if part := credentialIn(err.Error()); part != "" {
				t.Errorf("the error %.1000q holds %q of the URL's credential", err.Error(), part)
			}
			if raw := strconv.Itoa(len(plantedURL)); strings.Contains(err.Error(), raw) {
				t.Errorf("the error %.1000q names %s, the length of the word with its credential", err.Error(), raw)
			}
		})
	}
}
