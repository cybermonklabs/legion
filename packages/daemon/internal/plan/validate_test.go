package plan

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
	// A role the plan's own role block defines is defined, and Supported takes the role block.
	defined := oneTask("a", "implementer prober")
	defined.Roles = []PlanRole{{Name: "prober", Type: TypeTesting}}
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
		for _, name := range blocks {
			p.Roles = append(p.Roles, PlanRole{Name: name})
		}
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
	if r := Supported(empty); len(r) != 0 {
		t.Fatalf("Supported(no task) = %+v, want none: the cause is Validate's alone", r)
	}
	taken, err := FromBlocks("plan-md", 1, []Block{{ID: "a", Type: "plan_task", Text: "Do the change."}})
	if err != nil {
		t.Fatalf("FromBlocks(one block): %v", err)
	}
	if r := append(Supported(taken), Validate(taken, d)...); len(r) != 0 {
		t.Fatalf("a plan of one block with no steps refused: %+v", r)
	}
}

// Each cause this release does not carry out yet (Supported), with its exact text and its task.
// Each cause gets one reason, so a plan with two causes gets two. The taken plans are subsets of
// today's line, with the owner's acceptance, a fresh agent's, and a key on any role.
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

		{name: "merges", plan: withTask(Task{ID: "a", Merges: true, Steps: implementer}),
			want: []Refusal{{Task: "a", Reason: "task a names merges." + yet}}},
		{name: "owner", plan: withTask(Task{ID: "f", Owner: "architect"}),
			want: []Refusal{{Task: "f", Reason: "task f names owner." + yet}}},
		{name: "part_of", plan: withTask(Task{ID: "f1", PartOf: "f", Steps: implementer}),
			want: []Refusal{{Task: "f1", Reason: "task f1 names part_of." + yet}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supported(tc.plan); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Supported =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
	// Each cause of one task gets one reason of that task.
	if got := Supported(withTask(Task{ID: "a", Needs: []string{"b"}, Merges: true, Steps: implementer})); len(got) != 2 || got[0].Task != "a" || got[1].Task != "a" {
		t.Fatalf("Supported of a task with two causes = %+v, want two reasons of task a", got)
	}

	for _, steps := range []string{"implementer reviewer", "planner implementer tester reviewer acceptance:owner", "implementer acceptance:owner", "implementer", "implementer:cd reviewer",
		"implementer acceptance", "tester reviewer", "reviewer implementer", "implementer reviewer reviewer"} {
		if r := Supported(oneTask("a", steps)); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}

	// Both checks run, so a plan with a cause of each gets two reasons, one from each.
	twoTasks := oneTask("a", "implementer prober")
	twoTasks.Tasks = append(twoTasks.Tasks, Task{ID: "b", Steps: implementer})
	got := append(Supported(twoTasks), Validate(twoTasks, builtin)...)
	want := []Refusal{
		{Task: "", Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue." + yet},
		{Task: "a", Reason: "step 2 of task a names the role prober, which neither the deployment nor the plan defines."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two tasks and prober =\n%+v\nwant\n%+v", got, want)
	}
}

// A step can name its agent by a key on any role, and the steps of one key are one agent, so each
// plan below is taken: the agent r does the accepting step alone, one agent r does both steps,
// the owner does a step of the implementer, and one agent a does two roles.
func TestAKeyOnAnyStepIsTaken(t *testing.T) {
	for _, steps := range []string{"implementer acceptance:r", "implementer:r acceptance:r", "implementer:owner reviewer", "implementer:a tester:a reviewer"} {
		p := oneTask("t", steps)
		if got := append(Supported(p), Validate(p, builtin)...); len(got) != 0 {
			t.Errorf("%s refused: %+v, want it taken", steps, got)
		}
	}
}

// A task with no step implementer is taken, whatever steps it names: the owner does its retro
// (record.PlaceWorker), and a step that reads its task's pull request reads what its task text names
// where none exists. So is acceptance with no key, a fresh agent of the role acceptance, and a
// step of the tester or the reviewer before the implementer.
func TestAPlanWithNoImplementerIsTaken(t *testing.T) {
	for _, steps := range []string{"tester", "reviewer", "acceptance:owner", "acceptance", "planner tester", "implementer:owner reviewer",
		"tester:owner", "implementer acceptance", "reviewer implementer"} {
		p := oneTask("a", steps)
		if got := Supported(p); got != nil {
			t.Errorf("Supported(%s) = %+v, want nil", steps, got)
		}
		if got := Validate(p, builtin); got != nil {
			t.Errorf("Validate(%s) = %+v, want nil", steps, got)
		}
	}
}

// Validate refuses each role of a plan that Legion cannot run, each with its own reason: a name
// Dispatch's step record cannot hold, a name the deployment or Legion holds already, a type the
// step record cannot hold, and a model the deployment does not list, at the first step of each task
// that names the role, or for the whole plan where no step names it. A name nothing holds (agent,
// owner), a type no list holds, a role with no type or no model, and a prompt of 900,000 bytes are
// taken: the launch's own bound decides a prompt.
func TestValidateRefusesARoleThePlanCannotRun(t *testing.T) {
	listing := Deployment{Roles: BuiltinRoles(), Models: []string{"m-one", "m-two"}}
	withRoles := func(steps string, roles ...PlanRole) Plan {
		p := oneTask("a", steps)
		p.Roles = roles
		return p
	}
	const longName = "a-role-name-of-thirty-three-chars"
	const longType = "a-type-name-of-thirty-three-chars"
	const form = " is not of the form [a-z][a-z0-9-]* with at most 32 characters: Dispatch's step record takes no other."
	collides := func(name string) []Refusal {
		return []Refusal{{Task: "", Reason: "the plan defines the role " + name + ", which the deployment or Legion defines already; a role of a plan takes a name of its own."}}
	}
	for _, tc := range []struct {
		name string
		plan Plan
		d    Deployment
		want []Refusal
	}{
		{name: "a name of 33 characters", plan: withRoles("implementer "+longName, PlanRole{Name: longName, Type: TypeTesting}),
			want: []Refusal{{Task: "", Reason: "the plan defines the role " + longName + ", whose name" + form}}},
		{name: "the name tester", plan: withRoles("implementer tester", PlanRole{Name: "tester", Type: TypeTesting}), want: collides("tester")},
		{name: "the name merger", plan: withRoles("implementer merger", PlanRole{Name: "merger", Type: TypeTesting}), want: collides("merger")},
		{name: "the name controller", plan: withRoles("implementer controller", PlanRole{Name: "controller", Type: TypeTesting}), want: collides("controller")},
		{name: "the name agent", plan: withRoles("implementer agent", PlanRole{Name: "agent", Type: TypeTesting})},
		{name: "the name owner", plan: withRoles("implementer owner", PlanRole{Name: "owner", Type: TypeTesting})},
		{name: "the role owner done by the owner", plan: withRoles("implementer owner:owner", PlanRole{Name: "owner", Type: TypeTesting})},
		{name: "a type with an upper-case letter", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: "Fuzzing"}),
			want: []Refusal{{Task: "", Reason: "the plan defines the role prober with the type Fuzzing, which" + form}}},
		{name: "a type of 33 characters", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: longType}),
			want: []Refusal{{Task: "", Reason: "the plan defines the role prober with the type " + longType + ", which" + form}}},
		{name: "a type no list holds", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: "fuzzing"})},
		{name: "no type", plan: withRoles("implementer prober", PlanRole{Name: "prober"})},
		{name: "a model the deployment does not list", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: TypeTesting, Model: "m-three"}),
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names the role prober, whose model m-three the deployment does not list; it lists m-one, m-two."}}},
		{name: "a model of a deployment that lists none", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: TypeTesting, Model: "m-three"}),
			d:    Deployment{Roles: BuiltinRoles()},
			want: []Refusal{{Task: "a", Reason: "step 2 of task a names the role prober, whose model m-three the deployment does not list; it lists none."}}},
		{name: "a listed model", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: TypeTesting, Model: "m-two"})},
		{name: "no model", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: TypeTesting})},
		{name: "a model the deployment does not list, of a role no step names", plan: withRoles("implementer", PlanRole{Name: "spare", Type: TypeTesting, Model: "m-three"}),
			want: []Refusal{{Task: "", Reason: "the plan defines the role spare with the model m-three, which the deployment does not list; it lists m-one, m-two."}}},
		{name: "a prompt of 900,000 bytes", plan: withRoles("implementer prober", PlanRole{Name: "prober", Type: TypeTesting, Prompt: strings.Repeat("p", 900_000)})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.d
			if d.Roles == nil {
				d = listing
			}
			if got := Validate(tc.plan, d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}

	// Legion's names are read off claim's lists and BuiltinRoles, so a role either adds is one: each
	// is refused as a name of a plan's role, and `owner` and a role of the plan's own are not.
	var legion []string
	for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
		legion = append(legion, string(role))
	}
	for name := range BuiltinRoles() {
		legion = append(legion, name)
	}
	for _, name := range legion {
		if !IsLegionName(name) || !slices.Contains(LegionNames(), name) {
			t.Errorf("IsLegionName(%s) = %t, LegionNames = %v; want %s among Legion's names", name, IsLegionName(name), LegionNames(), name)
		}
		refused := Validate(withRoles("implementer", PlanRole{Name: name, Type: TypeTesting}), listing)
		want := "the plan defines the role " + name + ", which the deployment or Legion defines already; a role of a plan takes a name of its own."
		if !slices.ContainsFunc(refused, func(r Refusal) bool { return r.Reason == want }) {
			t.Errorf("Validate of a plan role named %s = %+v, want the refusal %q", name, refused, want)
		}
	}
	for _, name := range []string{"owner", "prober"} {
		if IsLegionName(name) {
			t.Errorf("IsLegionName(%s) = true, want false", name)
		}
	}
}

// One agent runs on one model and one harness, so Validate refuses a key whose steps name roles of
// two models, or of two harnesses, at the first step that differs from the key's first, once for
// each key. Two roles of no model run on one model, the deployment's default. The steps of no key
// are each an agent of their own. The owner does a step of the key owner in its own session, so
// only its model is compared with the role's, and a role on another harness is taken there.
func TestValidateRefusesOneAgentOnRolesOfTwoModels(t *testing.T) {
	deployment := func(implementerBackend string) Deployment {
		roles := BuiltinRoles()
		for name, role := range roles {
			role.Backend = "omp"
			roles[name] = role
		}
		implementer := roles["implementer"]
		implementer.Backend = implementerBackend
		roles["implementer"] = implementer
		return Deployment{Roles: roles, Models: []string{"m-one", "m-two"}}
	}
	withRoles := func(steps string, roles ...PlanRole) Plan {
		p := oneTask("a", steps)
		p.Roles = roles
		return p
	}
	prober := func(model string) PlanRole { return PlanRole{Name: "prober", Type: TypeTesting, Model: model} }
	for _, tc := range []struct {
		name    string
		plan    Plan
		backend string
		want    []Refusal
	}{
		{name: "a key on roles of two models", plan: withRoles("implementer:a prober:a", prober("m-two")), backend: "omp",
			want: []Refusal{{Task: "a", Reason: "the key a names one agent for implementer at step 1 of task a and prober at step 2 of task a, whose roles run on two models (the default model, m-two); one agent runs on one model."}}},
		{name: "a key on roles of two harnesses", plan: withRoles("implementer:a prober:a", prober("")), backend: "claude",
			want: []Refusal{{Task: "a", Reason: "the key a names one agent for implementer at step 1 of task a and prober at step 2 of task a, whose roles run on two harnesses (claude, omp); one agent runs on one harness."}}},
		{name: "a key on two roles of one model and one harness", plan: withRoles("implementer:a reviewer:a"), backend: "omp"},
		{name: "two steps of no key", plan: withRoles("implementer prober", prober("m-two")), backend: "omp"},
		{name: "the owner on a role of another model", plan: withRoles("prober:owner", prober("m-two")), backend: "omp",
			want: []Refusal{{Task: "a", Reason: "step 1 of task a names prober:owner, whose model m-two is not the owner's (the default model); the owner runs on its own model."}}},
		{name: "a key on three roles, two of another model", plan: withRoles("implementer:a prober:a critic:a", prober("m-two"),
			PlanRole{Name: "critic", Type: TypeTesting, Model: "m-two"}), backend: "omp",
			want: []Refusal{{Task: "a", Reason: "the key a names one agent for implementer at step 1 of task a and prober at step 2 of task a, whose roles run on two models (the default model, m-two); one agent runs on one model."}}},
		{name: "the owner on a role of another harness", plan: withRoles("implementer:owner"), backend: "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(tc.plan, deployment(tc.backend)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A role can stand at two steps of a task: the steps of one key are one agent, which takes the role
// of each step it does, so `implementer:a reviewer implementer:a` is taken; and two agents can
// start with one role, each with a name of its own (`implementer-2`, record.NameAgents), so
// `implementer reviewer implementer` is taken too. The fault: a second agent of one role refused.
func TestARoleCanStandAtTwoSteps(t *testing.T) {
	for _, steps := range []string{"implementer:a reviewer implementer:a", "implementer:a reviewer:b implementer:b",
		"implementer:a tester:a reviewer implementer:a tester:a", "implementer acceptance:owner reviewer acceptance:owner",
		"implementer reviewer implementer", "implementer:a implementer:b", "implementer reviewer:a tester:a reviewer",
		"implementer reviewer implementer implementer"} {
		p := oneTask("t", steps)
		if r := append(Supported(p), Validate(p, builtin)...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
	}
}

// A task names its steps in any order: the planner, the tester, the reviewer and the owner's
// acceptance each stand at any place, and a second agent of one role is taken as well. The twin:
// today's line, the steps an issue with no plan rows runs, is taken.
func TestSupportedTakesStepsInAnyOrder(t *testing.T) {
	for _, steps := range []string{"implementer reviewer tester", "implementer tester reviewer", "implementer acceptance:owner reviewer",
		"implementer reviewer acceptance:owner tester", "implementer planner", "planner implementer reviewer", "implementer reviewer planner tester",
		"planner implementer tester reviewer acceptance:owner", "implementer tester reviewer tester"} {
		p := oneTask("a", steps)
		if r := append(Supported(p), Validate(p, builtin)...); len(r) != 0 {
			t.Fatalf("%s refused: %+v", steps, r)
		}
	}
}

// withRole is Legion's own roles and one role of the deployment, name, of type typ.
func withRole(name string, typ Type) Deployment {
	roles := BuiltinRoles()
	roles[name] = RoleDef{Name: name, Type: typ}
	return Deployment{Roles: roles}
}

// A step of any role stands at any place: a step that reads its task's pull request before any
// step of the implementer reads what its task text names where none exists, and a role of the
// deployment of the type implementation after the implementer does its task's retro
// (record.PlaceWorker). Each plan below is taken, under a deployment whose coder does implementation work
// and whose critic does review work.
func TestAStepOfAnyRoleStandsAnywhere(t *testing.T) {
	d := withRole("coder", TypeImplementation)
	d.Roles["critic"] = RoleDef{Name: "critic", Type: TypeReview}
	for _, steps := range []string{"tester implementer", "reviewer implementer", "acceptance:owner implementer", "planner reviewer implementer",
		"implementer coder", "coder implementer", "critic implementer", "reviewer coder"} {
		p := oneTask("a", steps)
		if r := append(Supported(p), Validate(p, d)...); len(r) != 0 {
			t.Errorf("%s refused: %+v, want it taken", steps, r)
		}
	}
}

// An owner can define a role in its plan: a role block is no cause of Supported, and a task whose
// step names that role, on a model the deployment lists, is taken by both checks.
func TestSupportedTakesARoleBlock(t *testing.T) {
	p := oneTask("a", "implementer prober")
	p.Roles = []PlanRole{{Name: "prober", Type: TypeTesting, Model: "m-one", Prompt: "# Prober\n\nProbe the change."}}
	d := Deployment{Roles: BuiltinRoles(), Models: []string{"m-one"}}
	if r := Supported(p); len(r) != 0 {
		t.Errorf("Supported(implementer prober) with the role block prober = %+v, want none", r)
	}
	if r := Validate(p, d); len(r) != 0 {
		t.Errorf("Validate(implementer prober) with prober on the listed model m-one = %+v, want none", r)
	}
}

// A role the plan defines of the type implementation stands at any place, as one of the deployment
// does: its row records the type it has as the plan's steps name it (RolesOf), and after the
// implementer its agent does its task's retro (record.PlaceWorker), the retro of the last step of the type
// implementation. The same role of the type testing stands at any place too, and does no retro.
func TestARoleOfAPlanOfTheTypeImplementationStandsAnywhere(t *testing.T) {
	withCoder := func(steps string, typ Type) Plan {
		p := oneTask("a", steps)
		p.Roles = []PlanRole{{Name: "coder", Type: typ}}
		return p
	}
	for _, p := range []Plan{withCoder("implementer coder", TypeImplementation), withCoder("coder implementer", TypeImplementation), withCoder("implementer coder", TypeTesting)} {
		if r := append(Supported(p), Validate(p, builtin)...); len(r) != 0 {
			t.Fatalf("Supported(%s) with the plan's coder of the type %s = %+v, want none", p.Tasks[0].Steps, p.Roles[0].Type, r)
		}
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
		if r := append(Supported(p), Validate(p, d)...); len(r) != 0 {
			t.Fatalf("Supported(%s) = %+v, want none", steps, r)
		}
	}
}
