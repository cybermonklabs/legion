package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// agentsOfTask is the name of the agent of each stored step of task, in place order.
func agentsOfTask(stored *record.StoredPlan, task string) []claim.Agent {
	var names []claim.Agent
	for _, step := range stored.Steps[task] {
		names = append(names, step.Agent)
	}
	return names
}

// A name a key held is that key's alone: a later version in which a step with no key, or another
// key, would take it would run that agent in the name's claim and resume the key's session. Such a
// version is refused as a plan, 409 PLAN_REFUSED with R14's ending, naming the key that holds the
// name, before anything is written; the fact stays unspent, and version 1's rows, with their names,
// stand.
func TestALaterVersionThatWouldGiveAKeysNameToAnotherAgentIsRefusedAsAPlan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     []string
		firstWant []claim.Agent
		later     []string
		place     int
		takes     string
		holder    string
	}{
		{name: "a step with no key", first: []string{"implementer:a", "tester:a", "reviewer"}, firstWant: []claim.Agent{"implementer", "implementer", "reviewer"},
			later: []string{"implementer", "tester:a", "reviewer"}, place: 1, takes: "implementer", holder: "a"},
		{name: "the key dropped and its step given to no key", first: []string{"implementer", "tester:k", "reviewer:k"}, firstWant: []claim.Agent{"implementer", "tester", "tester"},
			later: []string{"implementer", "tester", "reviewer"}, place: 2, takes: "tester", holder: "k"},
		{name: "the key dropped and its step given to another key", first: []string{"implementer", "tester:k", "reviewer:k"}, firstWant: []claim.Agent{"implementer", "tester", "tester"},
			later: []string{"implementer", "tester:m", "reviewer"}, place: 2, takes: "tester", holder: "k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", tc.first...))))
			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			before := planStandingOf(t, pool, engine, key, gen)
			if got := agentsOfTask(before.plan, "a"); !slices.Equal(got, tc.firstWant) {
				t.Fatalf("the agents of version 1 = %q, want %q", got, tc.firstWant)
			}

			id := planID(key, gen, 2)
			reason := intake.AgentUnnamed("a", tc.place, tc.takes, tc.holder)
			if !strings.Contains(reason.Reason, "key "+tc.holder) || !strings.Contains(reason.Reason, " "+tc.takes+",") {
				t.Fatalf("the refusal's reason %q names neither key %s nor the name %s", reason.Reason, tc.holder, tc.takes)
			}
			want := &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason.Reason,
				Reasons: []plan.Refusal{reason}}
			assertPlanRefusal(t, pool, engine, key, gen, id,
				apply(id, ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", tc.later...))), want, before)
		})
	}
}

// A version that gives a task again names its keys from the rows read before it replaces the
// task's steps, so a key keeps the name it had, even where its first step is now of another role.
func TestARegivenTaskKeepsTheNamesOfItsKeys(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 1 = %q, want implementer, key a named tester, and reviewer", got)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget, and its manual.", "implementer:a", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"tester", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want key a still named tester at steps 1 and 2, and reviewer", got)
	}
}

// The project's default steps are named from their own steps alone when the first step of a run
// with no owner's plan writes them: each agent has a name, the steps of a key share it.
func TestTheDefaultStepsAreNamedWhenTheyAreWritten(t *testing.T) {
	engine := returnEngine()
	engine.cfg.DefaultSteps = []plan.Step{{Role: "implementer"}, {Role: "tester", Key: "r"}, {Role: "reviewer", Key: "r"}}
	_, _, steps, handed := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 1}, nil, nil)
	if len(handed) != 0 {
		t.Fatalf("the default steps with no earlier rows handed on %+v, want no name", handed)
	}
	var names []claim.Agent
	for _, step := range steps {
		names = append(names, step.Agent)
	}
	if want := []claim.Agent{"implementer", "tester", "tester"}; !slices.Equal(names, want) {
		t.Fatalf("the default steps are named %q, want %q", names, want)
	}
}

// A row with no key that shows a key's name says the default steps handed that name on before
// only where it is no older than the key's hold of the name. Generation 1 ran the default steps,
// whose tester took the name tester; generation 2's owner gave key k that name, and those rows went
// when generation 3's first step passed over that owner's plan, so only the lasting record keeps
// k's hold, at generation 2. Generation 3's default tester takes the name from key k for the first
// time: it is handed on, not handed before, so its claim leaves k's session and the issue is told.
// The control: key k held the name at generation 1 and the default steps took it at generation 2,
// so generation 3's tester was handed it before.
func TestADefaultStepIsHandedAKeysNameAnEarlierDefaultRunShowedBeforeTheKeyHeldIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		earlier []record.PlanStep
		held    record.KeyName
		before  bool
	}{
		{"the default rows are older than the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 2}, false},
		{"the control: the default rows came after the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: "a", Position: 1, Role: "tester", AgentKey: "k", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester", LeaveSession: "ses-key-k"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := returnEngine()
			engine.cfg.DefaultSteps = []plan.Step{{Role: "tester"}, {Role: "reviewer"}}
			_, _, steps, handed := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 3}, tc.earlier, []record.KeyName{tc.held})
			if len(handed) != 1 || handed[0].Name != "tester" || handed[0].Holder != "k" || handed[0].before != tc.before {
				t.Fatalf("the default steps handed on %+v, want key k's name tester handed on with before %t", handed, tc.before)
			}
			if steps[0].Agent != "tester" {
				t.Fatalf("the default tester is named %q, want tester", steps[0].Agent)
			}
		})
	}
}

// A name a key held stays that key's after a later version drops the key: version 1 names key k
// tester, version 2 drops k and its step, which writes over the rows that showed k's name, and
// version 3, whose tester step has no key, would take the name tester and so run in k's claim and
// resume k's session. Version 3 is refused as a plan, 409 PLAN_REFUSED naming key k, before
// anything is written: the name is read from the lasting record of the names keys held
// (record.KeyNames, plan_key_names), since no plan_steps row shows it any more.
func TestANameAKeyHeldStaysItsAfterAVersionDropsTheKey(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
	move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "tester"}) {
		t.Fatalf("the agents of version 1 = %q, want key k named tester", got)
	}
	if result := apply("stop-1", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's first stop was refused: %+v", result.Refusal)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
	if result := apply("stop-2", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's second stop was refused: %+v", result.Refusal)
	}
	before := planStandingOf(t, pool, engine, key, gen)
	if got := agentsOfTask(before.plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want implementer and reviewer, and no row showing key k's name", got)
	}

	id := planID(key, gen, 3)
	reason := intake.AgentUnnamed("a", 2, "tester", "k")
	want := &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason.Reason,
		Reasons: []plan.Refusal{reason}}
	assertPlanRefusal(t, pool, engine, key, gen, id,
		apply(id, ownersPlan(t, key, gen, 3, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))), want, before)
}

// The project's default steps after a change of owner are named from the lasting record of the
// names keys held, but they cannot be refused as an owner's version is. LEGION-209's first owner,
// the architect of LEGION-208, gave `implementer tester:k reviewer:k`, key k named tester, whose
// claim records key k's session; LEGION-209 is then admitted as a root of its own, its first step
// passes over the earlier owner's plan and deletes its rows, and it runs the default steps, whose
// tester step takes the name tester all the same. So the step is written with that claim's session
// to leave (record.PlanStep.LeaveSession), its start carries it (record.SuperviseRequest's
// LeaveSession), and the issue and the controller are told once that the agent starts in a session
// of its own, which was key k's. The reviewer's step, whose name no key held, carries none.
//
// The issue's next run takes the default steps again under the same owner, and nobody is told a
// second time. Where the claim still records key k's session, since no start of the tester ran to
// leave it, the tester's step leaves it again; where the claim records a session of its own, the
// tester's step leaves nothing.
func TestTheDefaultStepsAfterAChangeOfOwnerLeaveTheSessionOfAKeysName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		leaves  string
	}{
		{"the claim still records key k's session at the next run", "ses-key-k", "ses-key-k"},
		{"the claim records a session of its own at the next run", "ses-own", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			engine.cfg.DefaultSteps = []plan.Step{{Role: "tester"}, {Role: "reviewer"}}
			apply := applyFacts(t, pool, engine)
			const key = "LEGION-209"
			recordedAt(t, pool, key, "LEGION-208")
			mustTake(t, "the first owner's version 1", apply(planID(key, 1, 1), ownersPlan(t, key, 1, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
			named := claim.Token(token(t, key, claim.RoleTester))
			if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
				values ($1, 'legion', $2, $2, 'reviewer', 3, 'ses-key-k', '/sessions/ses-key-k.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
				string(named), key); err != nil {
				t.Fatalf("seed key k's claim: %v", err)
			}
			recordedAt(t, pool, key, key)

			firstStep(t, pool, engine, key)

			steps := planStandingOf(t, pool, engine, key, 1).plan.Steps[defaultTask]
			if len(steps) != 2 || steps[0].Role != "tester" || steps[0].Agent != "tester" || steps[0].LeaveSession != "ses-key-k" || steps[1].LeaveSession != "" {
				t.Fatalf("the default steps after the change of owner are %+v, want the tester's step named tester leaving key k's session ses-key-k, and the reviewer's leaving none", steps)
			}
			var tester []record.SuperviseRequest
			for _, start := range superviseRowsOf(t, pool, key, "start") {
				if start.Role == claim.RoleTester {
					tester = append(tester, start)
				}
			}
			if len(tester) != 1 || tester[0].LeaveSession != "ses-key-k" {
				t.Fatalf("the tester's starts are %+v, want one carrying the session to leave, ses-key-k", tester)
			}
			toldOnce := func(when string) {
				t.Helper()
				var told []string
				for _, body := range messageBodies(t, pool) {
					if strings.Contains(body, "key k") {
						told = append(told, body)
					}
				}
				if len(told) != 1 || !strings.Contains(told[0], `"ses-key-k"`) {
					t.Fatalf("%s the issue was told %q, want once, naming key k and its session ses-key-k", when, told)
				}
				if got := noticesOf(t, pool, key, "session-lost"); len(got) != 1 {
					t.Fatalf("%s the architect was told %+v, want one session-lost notice", when, got)
				}
				var controller int
				if err := pool.QueryRow(t.Context(), `select count(*) from outbox where issue = $1 and kind = 'controller_notice' and payload->>'kind' = 'session-lost'`,
					key).Scan(&controller); err != nil || controller != 1 {
					t.Fatalf("%s the controller was told %d session-lost notices (%v), want one", when, controller, err)
				}
			}
			toldOnce("after the first run's default steps,")

			if _, err := pool.Exec(t.Context(), `update claims set session = $2 where token = $1`, string(named), tc.session); err != nil {
				t.Fatalf("set the claim's session: %v", err)
			}
			next := issueOf(t, pool, engine, key)
			next.Generation, next.Phase, next.Status = 2, phase.Admitted, "in_progress"
			seedIssue(t, pool, next)

			firstStep(t, pool, engine, key)

			steps = planStandingOf(t, pool, engine, key, 2).plan.Steps[defaultTask]
			if len(steps) != 2 || steps[0].Agent != "tester" || steps[0].LeaveSession != tc.leaves {
				t.Fatalf("the next run's default steps are %+v, want the tester's step named tester leaving %q", steps, tc.leaves)
			}
			toldOnce("after the next run's default steps,")
		})
	}
}
