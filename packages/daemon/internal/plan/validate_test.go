package plan

import (
	"reflect"
	"testing"
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

// A role the plan defines is never named as a second or later agent of another role
// (claim.NumberedAgentName), Legion's, the deployment's or the plan's own, and its own such agent
// is never named as a role of the deployment: an agent of each role would hold one claim. Each
// cause is refused once, naming the role the plan defines. The control is a plan whose roles only
// look numbered or hold another role's name, which Validate takes.
func TestValidateRefusesARoleNamedAsAnotherRolesNumberedAgent(t *testing.T) {
	withBlocks := func(blocks ...string) Plan {
		p := oneTask("a", "implementer")
		p.RoleBlocks = blocks
		return p
	}
	held := ", so an agent of each role would hold one claim."
	for _, tc := range []struct {
		name string
		plan Plan
		d    Deployment
		want string
	}{
		{"a second agent of Legion's planner", withBlocks("planner-2"), builtin,
			"the plan defines the role planner-2, the name Legion gives a second or later agent of the role planner" + held},
		{"a second agent of the deployment's role", withBlocks("checker-2"), withRole("checker", TypeTesting),
			"the plan defines the role checker-2, the name Legion gives a second or later agent of the role checker" + held},
		{"a third agent of a role the plan defines after it", withBlocks("prober-3", "prober"), builtin,
			"the plan defines the role prober-3, the name Legion gives a second or later agent of the role prober" + held},
		{"a role of the deployment named as a second agent of the plan's role", withBlocks("prober"), withRole("prober-2", TypeTesting),
			"the plan defines the role prober, whose second or later agent Legion would name prober-2, the deployment's role" + held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r, want := Validate(tc.plan, tc.d), []Refusal{{Reason: tc.want}}; !reflect.DeepEqual(r, want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", r, want)
			}
		})
	}
	control := withBlocks("security-reviewer", "planner2", "code-checker", "checker-1", "planner-02")
	if r := Validate(control, withRole("checker", TypeTesting)); len(r) != 0 {
		t.Fatalf("Validate(roles no agent of another role takes) = %+v, want none", r)
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
// type implementation before the implementer, and the other steps at any place, with the owner's
// acceptance, and with a key on a role but acceptance.
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
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none." + yet}}},
		{name: "a task with no implementer", plan: oneTask("a", "tester reviewer"), want: []Refusal{
			{Task: "a", Reason: "task a has no step implementer." + yet},
			{Task: "a", Reason: "step 1 of task a names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none." + yet},
			{Task: "a", Reason: "step 2 of task a names reviewer, and no step before it names the role implementer, whose pull request reviewer reads; a role of this deployment that reports pass or fail needs none." + yet},
		}},
		{name: "acceptance with no key", plan: oneTask("a", "implementer acceptance"),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names acceptance with no key, which asks for a fresh agent of the role acceptance." + yet}}},
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
		{name: "two causes in one task", plan: oneTask("a", "reviewer:owner implementer"), want: []Refusal{
			{Task: "a", Reason: "step 1 of task a names the key owner." + yet},
			{Task: "a", Reason: "step 1 of task a names reviewer:owner, and no step before it names the role implementer, whose pull request reviewer:owner reads; a role of this deployment that reports pass or fail needs none." + yet},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supported(tc.plan, builtin); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Supported =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}

	for _, steps := range []string{"implementer reviewer", "planner implementer tester reviewer acceptance:owner", "implementer acceptance:owner", "implementer", "implementer:cd reviewer", "implementer reviewer reviewer"} {
		if r := Supported(oneTask("a", steps), builtin); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}

	// Both checks run, so a plan with a cause of each gets two reasons, one from each.
	twoTasks := oneTask("a", "implementer prober")
	twoTasks.Tasks = append(twoTasks.Tasks, Task{ID: "b", Steps: implementer})
	got := append(Supported(twoTasks, builtin), Validate(twoTasks, builtin)...)
	want := []Refusal{
		{Task: "", Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue." + yet},
		{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two tasks and prober =\n%+v\nwant\n%+v", got, want)
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

// A role can stand at two steps of a task: the steps of one key are one agent, which takes the role
// of each step it does, so `implementer:a reviewer implementer:a` is taken; and two agents can
// start with one role, each with a name of its own (`implementer-2`, record.NameAgents), so
// `implementer reviewer implementer` is taken too. The owner has no name, so `implementer
// acceptance:owner acceptance` is refused for its keyless acceptance alone. The fault: a second
// agent of one role refused.
func TestARoleCanStandAtTwoSteps(t *testing.T) {
	for _, steps := range []string{"implementer:a reviewer implementer:a", "implementer:a reviewer:b implementer:b",
		"implementer:a tester:a reviewer implementer:a tester:a", "implementer acceptance:owner reviewer acceptance:owner",
		"implementer reviewer implementer", "implementer:a implementer:b", "implementer reviewer:a tester:a reviewer",
		"implementer reviewer implementer implementer"} {
		p := oneTask("t", steps)
		if r := append(Supported(p, builtin), Validate(p, builtin)...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
	}
	want := []Refusal{{Task: "t", Reason: "step 3 of task t names acceptance with no key, which asks for a fresh agent of the role acceptance." + yet}}
	if r := Supported(oneTask("t", "implementer acceptance:owner acceptance"), builtin); !reflect.DeepEqual(r, want) {
		t.Fatalf("Supported(implementer acceptance:owner acceptance) =\n%+v\nwant\n%+v", r, want)
	}
}

// A task names its steps in any order. The planner stands at any place, and the tester, the
// reviewer and the owner's acceptance each come after the implementer, in any order among them. A
// second agent of one role is taken as well. The twin:
// today's line, the steps an issue with no plan rows runs, is taken.
func TestSupportedTakesStepsInAnyOrder(t *testing.T) {
	for _, steps := range []string{"implementer reviewer tester", "implementer tester reviewer", "implementer acceptance:owner reviewer",
		"implementer reviewer acceptance:owner tester", "implementer planner", "planner implementer reviewer", "implementer reviewer planner tester",
		"planner implementer tester reviewer acceptance:owner", "implementer tester reviewer tester"} {
		p := oneTask("a", steps)
		if r := append(Supported(p, builtin), Validate(p, builtin)...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
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
		if r := append(Supported(p, taken.d), Validate(p, taken.d)...); len(r) != 0 {
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
	if r := append(Supported(p, d), Validate(p, d)...); len(r) != 0 {
		t.Fatalf("Supported(critic implementer) = %+v, want none", r)
	}
}

// A role of the deployment stands at two steps as Legion's own roles do, where one agent does both
// and where a second agent starts with it, which gets a name of its own, so an owner's plan (api's
// register_plan) and a project's default steps (config's checkDefaultSteps), the two writers of a
// task's steps, can each name one.
func TestARoleOfTheDeploymentStandsAtTwoStepsAsLegionsOwnDo(t *testing.T) {
	d := withRole("checker", TypeTesting)
	d.Roles["linter"] = RoleDef{Name: "linter", Type: TypeTesting}
	for _, steps := range []string{"implementer checker linter", "implementer checker:c linter checker:c", "implementer checker linter checker"} {
		p := oneTask("a", steps)
		if r := append(Supported(p, d), Validate(p, d)...); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}
}
