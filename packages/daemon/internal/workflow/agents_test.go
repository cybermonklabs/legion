package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// A later version in which a step with no key would need the name its role gives while a key holds
// that name from an earlier version is taken: the step's agent gets a name of its own,
// `implementer-2`, and key a keeps its name, so neither takes the other's claim or session. The
// fault: the fresh step takes key a's name, or the version is refused.
func TestALaterVersionGivesAFreshAgentANameOfItsOwnBesideAKeysName(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer:a", "tester:a", "reviewer"))))
	move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "implementer", "reviewer"}) {
		t.Fatalf("the agents of version 1 = %q, want key a named implementer at steps 1 and 2, and reviewer", got)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer-2", "implementer", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want implementer-2 at step 1, key a still named implementer, and reviewer", got)
	}
}

// A name a key held stays that key's when a later version drops the key: version 1, `implementer
// tester:k reviewer:k`, names key k tester, and version 2 gives key k's first step to a step with
// no key or to another key, m. That step's agent would run in the claim tester and resume key k's
// session; it takes a name of its own, tester-2, and the version is taken. A step with no key
// beside a key that keeps its name is TestALaterVersionGivesAFreshAgentANameOfItsOwnBesideAKeysName.
// The fault: the agent at key k's step takes tester, or the version is refused.
func TestALaterVersionThatDropsAKeyGivesItsNameToNoOtherAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		later []string
	}{
		{name: "the key dropped and its step given to no key", later: []string{"implementer", "tester", "reviewer"}},
		{name: "the key dropped and its step given to another key", later: []string{"implementer", "tester:m", "reviewer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "tester"}) {
				t.Fatalf("the agents of version 1 = %q, want implementer, and key k named tester at steps 2 and 3", got)
			}
			mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", tc.later...))))
			if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester-2", "reviewer"}) {
				t.Fatalf("the agents of version 2 = %q, want implementer, tester-2 at step 2, a name of its own beside key k's tester, and reviewer", got)
			}
		})
	}
}

// Every start of a step names the agent of that step in one string (record.SuperviseRequest.Agent,
// plan.Agent.ID), the agent of its key or, for a step with no key, the agent of its task and place,
// and the claim the start reaches records the agent whose session it holds: the agent of the start
// that began that session (supervise.Claim.SessionAgent). The runner starts a claim in a new session
// exactly where the two differ. So a later version, given after the owner stopped the task that ran
// first, names its step's agent by the plan it is, and never by the plan in force at the rows that
// ran before it.
//
// Each case runs version 1 to the step whose agent last ran on the claim the later step takes,
// stops the task, gives version 2, and reads the agent version 2's first step names. The step's
// agent takes the name, and so the claim, the earlier agent ran on: the name implementer in each
// case, so each case is one claim and the question is whose session it resumes. The one exception
// is the step whose key was taken off: the name a key held is that key's (record.NameAgents), so
// that step takes implementer-2, a claim of its own, and key k's claim waits for key k.
//   - A keyed agent keeps its own session across a version that moves its second step: v1
//     `implementer:a tester:a reviewer`, v2 `implementer:a reviewer tester:a`. Before, the entry
//     judged the claim's last row, place 2, by the plan in force, where place 2 is now the reviewer,
//     and asked for a fresh session for key a.
//   - A new key in a new task, and a key added at the place an agent with no key ran, are agents
//     of their own. Before, a keyed step was taken for the same agent as any row of a task the plan
//     no longer held, and a re-keyed place for the agent of the place.
//   - A step whose key was taken off is a fresh agent, on a name of its own. Before, the place's
//     agent with no key was taken for the same agent as the key's, and then it took the key's name.
//   - A key's agent across two tasks is one agent.
//
// The fault in each: the agent the later step names is not the agent whose session it should
// resume or leave behind.
func TestAStartNamesTheAgentOfItsStepWhateverThePlanInForceHeldBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		// first and later are the task id and the steps of version 1 and version 2.
		firstTask string
		first     []string
		laterTask string
		later     []string
		// ranStep2 is whether step 1 completes, so step 2 of version 1 runs on the claim last.
		ranStep2 bool
		// ran is the agent of the last step of version 1 that ran on the claim, and agent the agent
		// of version 2's step 1.
		ran, agent plan.AgentID
		// named is the name version 2's step 1 takes, where it is not implementer.
		named claim.Agent
	}{
		{name: "a keyed agent keeps its session across a version that moves its step", firstTask: "a", first: []string{"implementer:a", "tester:a", "reviewer"},
			laterTask: "a", later: []string{"implementer:a", "reviewer", "tester:a"}, ranStep2: true, ran: "key:a", agent: "key:a"},
		{name: "a new key in a new task is an agent of its own", firstTask: "a", first: []string{"implementer", "reviewer"},
			laterTask: "p", later: []string{"implementer:b", "reviewer"}, ran: "step:1:a", agent: "key:b"},
		{name: "a step with no key in a new task is an agent of its own", firstTask: "a", first: []string{"implementer", "reviewer"},
			laterTask: "p", later: []string{"implementer", "reviewer"}, ran: "step:1:a", agent: "step:1:p"},
		{name: "a key added at the place of an agent with no key is an agent of its own", firstTask: "a", first: []string{"implementer", "reviewer"},
			laterTask: "a", later: []string{"implementer:k", "reviewer"}, ran: "step:1:a", agent: "key:k"},
		{name: "a step whose key was taken off is an agent of its own", firstTask: "a", first: []string{"implementer:k", "reviewer"},
			laterTask: "a", later: []string{"implementer", "reviewer"}, ran: "key:k", agent: "step:1:a", named: "implementer-2"},
		{name: "a key's agent in two tasks is one agent", firstTask: "a", first: []string{"implementer:k", "reviewer"},
			laterTask: "b", later: []string{"implementer:k", "reviewer"}, ran: "key:k", agent: "key:k"},
		{name: "control: an agent with no key that returns to its place is one agent", firstTask: "a", first: []string{"implementer", "reviewer"},
			laterTask: "a", later: []string{"implementer", "reviewer"}, ran: "step:1:a", agent: "step:1:a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			root := stepRoot(t, pool, phase.Admitted)
			key, gen := root.Key, root.Generation
			first := givenTask(t, "Build the widget.", tc.first...)
			first.ID = tc.firstTask
			mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, first)))
			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			ran := lastStartOf(t, pool, key, claim.RoleImplementer)
			if tc.ranStep2 {
				completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 1, "head")
				ran = lastStartOf(t, pool, key, claim.RoleTester)
			}
			if ran.Agent != tc.ran {
				t.Fatalf("the last start of version 1 on the claim implementer names the agent %q, want %q", ran.Agent, tc.ran)
			}
			if result := apply("stop", ownersStop(t, root, tc.firstTask)); result.Refusal != nil {
				t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
			}
			later := givenTask(t, "Build the manual.", tc.later...)
			later.ID = tc.laterTask
			mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, later)))
			named := tc.named
			if named == "" {
				named = "implementer"
			}
			if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, tc.laterTask); len(got) == 0 || got[0] != named {
				t.Fatalf("the agents of version 2's task %s = %q, want step 1's named %s", tc.laterTask, got, named)
			}
			start := lastStartOf(t, pool, key, claim.RoleImplementer)
			if start.Agent != tc.agent {
				t.Fatalf("the start of version 2's step 1 = %+v, want it to name the agent %q", start, tc.agent)
			}
			if start.FreshSession {
				t.Errorf("the start of version 2's step 1 asks for a fresh session of its own, want the claim's record of its session's agent to decide: %+v", start)
			}
		})
	}
}

// A key that one version leaves out finds its name, and so its claim and the session that claim
// holds, when a later version gives it again. Version 1, `implementer:k reviewer`, runs step 1 on
// the claim implementer, and the owner stops the task. Version 2, `implementer reviewer`, runs its
// step 1, which has no key, on implementer-2, a claim of its own, and the owner stops it again.
// Version 3, `implementer:k reviewer`, starts key k on implementer and names key k, the agent of
// every start that claim was given, so the session it holds is key k's own to resume. The fault:
// version 2's step with no key took implementer, the runner left key k's session behind as another
// agent's, and key k's return found that claim holding the other agent's session.
func TestAKeyLeftOutOfOneVersionFindsItsClaimWhenALaterVersionGivesItAgain(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	for i, v := range []struct {
		steps []string
		// name is the name of the claim the version's step 1 starts on, agent the agent it names.
		name  claim.Agent
		agent plan.AgentID
	}{
		{steps: []string{"implementer:k", "reviewer"}, name: "implementer", agent: "key:k"},
		{steps: []string{"implementer", "reviewer"}, name: "implementer-2", agent: "step:1:a"},
		{steps: []string{"implementer:k", "reviewer"}, name: "implementer", agent: "key:k"},
	} {
		version := i + 1
		mustTake(t, fmt.Sprintf("version %d", version), apply(planID(key, gen, version), ownersPlan(t, key, gen, version, givenTask(t, "Build the widget.", v.steps...))))
		if version == 1 {
			move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		}
		start := lastStartOf(t, pool, key, claim.RoleImplementer)
		if start.Agent != v.agent || start.FreshSession {
			t.Fatalf("version %d: step 1's start = %+v, want it to name the agent %q and ask for no fresh session", version, start, v.agent)
		}
		if row := roleRowNow(t, pool, engine, key, claim.RoleImplementer); row.Claim != agentToken(t, v.name) {
			t.Fatalf("version %d: step 1 runs on the claim %q, want %q", version, row.Claim, agentToken(t, v.name))
		}
		if version < 3 {
			if result := apply(fmt.Sprintf("stop %d", version), ownersStop(t, root, "a")); result.Refusal != nil {
				t.Fatalf("version %d: the owner's stop was refused: %+v", version, result.Refusal)
			}
		}
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

// The project's default steps are named from their own steps when the first step of a run with no
// owner's plan writes them: each agent has a name, the steps of a key share it. A name whose claim
// holds the session of a key an earlier plan gave (record.NameClaims) is that key's, and so is a
// name an earlier row of a key held, so the default steps' implementer takes implementer-2 beside
// either; a name an earlier row with no key held is free.
func TestTheDefaultStepsAreNamedWhenTheyAreWritten(t *testing.T) {
	engine := returnEngine()
	engine.cfg.DefaultSteps = []plan.Step{{Role: "implementer"}, {Role: "tester", Key: "r"}, {Role: "reviewer", Key: "r"}}
	for _, tc := range []struct {
		earlier []record.PlanStep
		claims  record.NameClaims
		want    []claim.Agent
	}{
		{want: []claim.Agent{"implementer", "tester", "tester"}},
		{claims: record.NameClaims{"implementer": {Role: "implementer", Key: "k"}}, want: []claim.Agent{"implementer-2", "tester", "tester"}},
		{earlier: []record.PlanStep{{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "k", Agent: "implementer"}},
			want: []claim.Agent{"implementer-2", "tester", "tester"}},
		{earlier: []record.PlanStep{{Generation: 1, Task: "a", Position: 1, Role: "implementer", Agent: "implementer"}},
			want: []claim.Agent{"implementer", "tester", "tester"}},
	} {
		_, _, steps := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 2}, tc.earlier, nil, tc.claims)
		var names []claim.Agent
		for _, step := range steps {
			names = append(names, step.Agent)
		}
		if !slices.Equal(names, tc.want) {
			t.Fatalf("the default steps beside the rows %+v and the claims by name %v are named %q, want %q", tc.earlier, tc.claims, names, tc.want)
		}
	}
}

// A run with no owner's plan names its default steps from every earlier row of the issue as well
// as from the claims' sessions, as a given version is named, so rows a release before the claim
// recorded its session's agent wrote keep a key's name for it: generation 1 ran default steps
// `implementer:k reviewer`, its rows naming key k implementer, and no claim records key k's
// session (that release records none). Generation 2's first step writes the default steps
// `implementer reviewer`, whose implementer has no key, so it takes implementer-2: key k's claim,
// and the session it holds, wait for key k. The control: generation 1's rows name no key, and the
// implementer takes implementer, the name only a step with no key held. The fault: the default
// steps were named from no earlier row, so their implementer took implementer, and with it the
// session key k's claim holds unrecorded, which the claim gives the first start that reaches it.
func TestTheDefaultStepsKeepANameAKeyHeldInRowsOfAnEarlierGeneration(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want claim.Agent
	}{
		{name: "generation 1's rows name key k implementer", key: "k", want: "implementer-2"},
		{name: "control: generation 1's rows name no key", want: "implementer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := engineRunningByDefault(t, "implementer", "reviewer")
			root := stepRoot(t, pool, phase.Admitted)
			roles := plan.BuiltinRoles()
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().PutPlan(t.Context(), tx,
					record.PlanVersion{Issue: root.Key, Generation: 1, Source: record.PlanSourceDefault, GivenAt: stepClock},
					[]record.PlanTask{{Issue: root.Key, Generation: 1, Task: plan.DefaultTask, BlockText: "The project's default steps.",
						StepsByDefault: true, State: record.TaskWaiting}},
					[]record.PlanStep{
						{Issue: root.Key, Generation: 1, Task: plan.DefaultTask, Position: 1, Role: "implementer", AgentKey: tc.key,
							Type: roles["implementer"].Type, Agent: "implementer"},
						{Issue: root.Key, Generation: 1, Task: plan.DefaultTask, Position: 2, Role: "reviewer",
							Type: roles["reviewer"].Type, Agent: "reviewer"},
					})
			})
			root.Generation = 2
			seedIssue(t, pool, root)

			firstStep(t, pool, engine, root.Key)

			stored := planStandingOf(t, pool, engine, root.Key, 2).plan
			assertDefaultPlan(t, stored, root.Key, 2, []string{"implementer/implementation", "reviewer/review"})
			if got := agentsOfTask(stored, plan.DefaultTask); !slices.Equal(got, []claim.Agent{tc.want, "reviewer"}) {
				t.Fatalf("the agents of generation 2's default steps = %q, want %s and reviewer", got, tc.want)
			}
		})
	}
}

// rolesRows is every phases row of key's role, by place.
func rolesRows(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) map[int]record.PhaseRow {
	t.Helper()
	rows := map[int]record.PhaseRow{}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		all, err := engine.store.Phases(ctx, tx, key)
		for _, row := range all {
			if row.Role == role {
				rows[row.StepPlace] = row
			}
		}
		return err
	})
	return rows
}

// roleRowNow is role's row of key now, the one with the highest entry (record.PhaseRowOf), read
// with no walk: where an entry rose since, it is the row that rose.
func roleRowNow(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := engine.store.Phases(ctx, tx, key)
		row = record.PhaseRowOf(rows, role)
		return err
	})
	return row
}

// A role's row of no step, the row a new generation cleared, is the row its first step's entry
// raises: the role keeps one row, which carries the record it last named across the generation as
// it did, and no row of no step stands beside the step's. Before, the entry wrote a second row at
// the step and the cleared row stood on beside it, read by any reader of the role's first row.
func TestARowOfNoStepBecomesTheRowOfTheRolesStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: agentToken(t, "implementer"), Entry: 4,
		LastHandoff: "implement-of-the-generation-before", EntryRaisedAt: stepClock})
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
	if row, ok := rows[1]; len(rows) != 1 || !ok || row.StepTask != "a" || row.Entry != 5 || row.LastHandoff != "implement-of-the-generation-before" {
		t.Fatalf("the implementer's rows = %+v, want one, at step 1 of task a, raised to entry 5, keeping the record it last named", rows)
	}
}

// agentToken is the claim token of agent on LEGION-208.
func agentToken(t *testing.T, agent claim.Agent) claim.Token {
	t.Helper()
	token, err := claim.AgentToken("legion", "LEGION-208", agent)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A role stands at two steps of `implementer:a reviewer implementer:a`, and each step's run keeps a
// row of its own, so the task of step 3 names the report of step 1 and of step 2, each from the row
// of its place, and step 3 runs in step 1's claim. A fourth step reads the report of each of the
// implementer's two steps once, under its own place. Before, the implementer's one row was step
// 1's until step 3's start cleared its report, so step 3 named no report of step 1, and a match of
// the rows by role names each implementer's report under both of its places.
func TestTheThirdStepsTaskNamesTheReportsOfStepsOneAndTwo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{"implementer:a reviewer implementer:a", []string{"implementer:a", "reviewer", "implementer:a"}},
		{"implementer:a reviewer implementer:a tester", []string{"implementer:a", "reviewer", "implementer:a", "tester"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, tc.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 1, "head")
			completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
			move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))

			third := lastStart(t, pool, root.Key, claim.RoleImplementer)
			for _, says := range []string{" Step 3 of " + string(rune('0'+len(tc.steps))) + ": implementer:a.",
				" Step 1 (implementer:a) reported: implement version 1.", " Step 2 (reviewer) reported: review version 1."} {
				if !strings.Contains(third, says) {
					t.Fatalf("step 3's task = %q, want it to say %q", third, says)
				}
			}
			implementer := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
			if first, again := implementer[1], implementer[3]; first.Claim != agentToken(t, "implementer") || again.Claim != first.Claim {
				t.Fatalf("the implementer's rows name the claims %q at step 1 and %q at step 3, want both %q: one key is one agent",
					first.Claim, again.Claim, agentToken(t, "implementer"))
			}
			if len(tc.steps) < 4 {
				return
			}
			completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", 2, "head")
			fourth := lastStart(t, pool, root.Key, claim.RoleTester)
			want := " Step 1 (implementer:a) reported: implement version 1. Step 2 (reviewer) reported: review version 1." +
				" Step 3 (implementer:a) reported: implement version 2."
			if !strings.Contains(fourth, want) || strings.Count(fourth, "reported: implement") != 2 {
				t.Fatalf("step 4's task = %q, want each step's report once: %q", fourth, want)
			}
		})
	}
}

// While step 2 of `implementer:a reviewer implementer:a` runs, the engine's row of the implementer
// is the row of step 1, the implementer's latest step before it, which holds the report step 2
// reads; while step 3 runs it is the row of step 3. The reviewer's row read at step 3 is step 2's.
func TestAReviewerReadsTheLatestImplementersRowBeforeIt(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.StepPlace != 1 || row.HandoffRecord != "implement-1" {
		t.Fatalf("the implementer's row while step 2 runs = %+v, want step 1's, with its report", row)
	}
	completes(t, pool, engine, claim.RoleReviewer, "review", 1)
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	first := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1]
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer); row.StepPlace != 3 || row.HandoffRecord != "" || row.Entry <= first.Entry {
		t.Fatalf("the implementer's row while step 3 runs = %+v, want step 3's, raised above step 1's entry %d with no report yet", row, first.Entry)
	}
	if first.HandoffRecord != "implement-1" {
		t.Fatalf("step 1's row after step 3 started = %+v, want its report kept", first)
	}
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.StepPlace != 2 || row.HandoffRecord != "review-1" {
		t.Fatalf("the reviewer's row while step 3 runs = %+v, want step 2's", row)
	}
}

// A role written before its first step is written on the row of that step, as the one row of a
// role was: a reviewer's row read and written back while the task stands at the implementer's step
// is the row of the reviewer's step, which the reviewer's start then raises, so the role keeps one
// row and what was written stands on it.
func TestAWriteBeforeARolesFirstStepLandsOnThatStepsRow(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		row, err := engine.phaseRow(ctx, tx, root, claim.RoleReviewer)
		if err != nil {
			return err
		}
		row.ReviewedHead = "head-read-before"
		return engine.store.PutPhase(ctx, tx, row)
	})
	completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
	if row, ok := rows[2]; len(rows) != 1 || !ok || row.ReviewedHead != "head-read-before" || row.Entry == 0 {
		t.Fatalf("the reviewer's rows = %+v, want one, at step 2, raised, with the commit written before its step", rows)
	}
}

// A role that stands at two steps runs its phase at two places, and a move back to that phase
// reaches the nearest of them before the task's place, while a failed step's work returns to the
// first step of the type implementation before it (Walk.returnTarget): in
// `implementer:a reviewer implementer:a tester`, the tester's move back to implementing reaches
// step 3, the implementer's step the tester tested, and a fail of the tester that names no step
// returns to step 1. Before, a phase that ran at two earlier places served no backward row, and the
// move was refused as one no row serves.
func TestAMoveBackToAPhaseThatRunsTwiceReachesTheNearest(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a", "tester")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 2, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(4))
	if result := applyFacts(t, pool, engine)("tester-back", intake.BackwardMove{Issue: root.Key, Requester: claim.RoleTester, To: phase.Implementing,
		Reason: "the harness needs a fixture"}); result.Refusal != nil {
		t.Fatalf("the tester's move back to implementing was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
}

// The claim of a role's worker read with no walk (record.WorkerClaim) is the claim of the row the
// engine reads where the task stands (record.PhaseRowAt): in `implementer:a tester:b implementer:b
// reviewer`, where key a is named implementer and key b tester, the implementer's worker is key a
// at places 1 and 2, key b at places 3 and 4, and key a again once a failed review returns the task
// to step 1. Before, the first row of the role answered, key a at every place.
func TestWorkerClaimPicksTheRowThatPhaseRowAtGives(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:b", "implementer:b", "reviewer")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "tester")
	worker := func(in phase.Phase, at record.TaskPlace, want claim.Token) {
		t.Helper()
		assertWalksAt(t, pool, engine, root.Key, in, at)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := engine.store.Phases(ctx, tx, root.Key)
			if err != nil {
				return err
			}
			row, err := engine.phaseRow(ctx, tx, root, claim.RoleImplementer)
			if err != nil {
				return err
			}
			got, err := record.WorkerClaim("legion", root.Key, claim.RoleImplementer, rows)
			if err != nil {
				return err
			}
			if got != want || row.Claim != want {
				t.Errorf("at %+v the implementer's worker = %q and the engine's row names %q, want %q", at, got, row.Claim, want)
			}
			return nil
		})
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	worker(phase.Implementing, stepPlace(1), keyA)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	worker(phase.Testing, stepPlace(2), keyA)
	completesAs(t, pool, engine, claim.RoleTester, keyB, "test", 1, "head")
	worker(phase.Implementing, stepPlace(3), keyB)
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	worker(phase.Reviewing, stepPlace(4), keyB)
	move(t, pool, engine, root.Key, TriggerReviewRejected, stepPlace(1), record.PhaseRow{})
	worker(phase.Implementing, stepPlace(1), keyA)
}

// Two agents of one role keep a row of it each, and the entry a step's start raises is above every
// row of its role: in `implementer:a tester:b implementer:b reviewer`, the failed review returns
// the task to step 1, whose row rises above step 3's, so key b's completion at the entry it was
// handed for step 3 is behind step 1's, and step 1's row keeps key a and no report. Step 1 is key
// a's work, so the refusal is HANDOFF_NOT_CURRENT_PHASE naming key a, and it tells key b that no
// task of that step comes to it, unspent. Before, the entry rose only above the rows of its own
// agent, so step 1's row and step 3's stood on one entry, and key b's completion passed the fence
// and was recorded as step 1's, its claim and its record on that row; and then it was refused
// HANDOFF_STALE_ENTRY, whose words told key b that step 1's task was owed to it and came by itself,
// though that task goes to key a. A turn of the same agent's other step is still refused
// HANDOFF_STALE_ENTRY (TestATurnOfARolesEarlierStepDoesNotCompleteItsLaterStep).
func TestAStepsEntryRisesAboveEveryRowOfItsRole(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:b", "implementer:b", "reviewer")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleTester, keyB, "test", 1, "head")
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	served := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[3].Entry
	move(t, pool, engine, root.Key, TriggerReviewRejected, stepPlace(1), record.PhaseRow{})
	result := applyFacts(t, pool, engine)("implementer-late-key-b", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
		Claim: keyB, Entry: served, Summary: "done", Commit: "head", HandoffRecord: "implement-3", HandoffVersion: 3})
	rows := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)
	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" || !result.Unspent {
		t.Fatalf("key b's completion at entry %d after the return to step 1 = %+v (unspent %t), want HANDOFF_NOT_CURRENT_PHASE, unspent; step 1's row names %q with the record %q at entry %d",
			served, result.Refusal, result.Unspent, rows[1].Claim, rows[1].HandoffRecord, rows[1].Entry)
	}
	for _, says := range []string{"is the work of another agent's claim, " + string(keyA), "no task of that step comes to this claim", "do not wait for one"} {
		if !strings.Contains(result.Refusal.Message, says) {
			t.Fatalf("the refusal = %q, want it to say %q", result.Refusal.Message, says)
		}
	}
	if strings.Contains(result.Refusal.Message, intake.StaleEntryInstruction) {
		t.Fatalf("the refusal = %q, want no promise of step 1's task to key b", result.Refusal.Message)
	}
	if first, third := rows[1], rows[3]; first.Claim != keyA || first.HandoffRecord != "" || first.Entry <= third.Entry {
		t.Fatalf("step 1's row after the refusal names %q with the record %q at entry %d, want %q with none, above step 3's entry %d",
			first.Claim, first.HandoffRecord, first.Entry, keyA, third.Entry)
	}
}

// A completion counts only for the step its turn was given. In `implementer:a reviewer
// implementer:a`, the reviewer's approval enters step 3 by a forward move at the head step 1 left,
// and key a's turn of step 1, still open on step 1's entry, completes at the pull request's head
// with a record of its own. Its entry is behind step 3's and is step 1's, another step of the role,
// so it is refused HANDOFF_STALE_ENTRY, step 3's row records nothing and the task stays at step 3.
// Before, a completion behind its row at the head the record holds was taken unless a move back
// raised the row's entry, so the turn of step 1 was recorded as step 3's and moved the task on to
// its retro before step 3's task reached the agent.
func TestATurnOfARolesEarlierStepDoesNotCompleteItsLaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	served := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1].Entry
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
	result := applyFacts(t, pool, engine)("implementer-step-1-turn", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
		Claim: implementer, Entry: served, Summary: "done", Commit: "head", HandoffRecord: "implement-9", HandoffVersion: 9})
	third := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[3]
	if result.Refusal == nil || result.Refusal.Code != intake.StaleEntryCode {
		t.Fatalf("the turn of step 1 completing at entry %d and the head after step 3 was entered = %+v, want %s; step 3's row names the record %q",
			served, result.Refusal, intake.StaleEntryCode, third.HandoffRecord)
	}
	if third.HandoffRecord != "" || third.Completed() {
		t.Fatalf("step 3's row after the refusal = %+v, want no report", third)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(3))
}

// A role's late report of its later step is the report of that step's run: the run whose entry it
// serves. In `implementer:a reviewer:r implementer:a reviewer:r`, step 4's run ends before the
// reviewer completes: the review's request for changes returns the task to step 1, its first
// implementation step, and the reviewer's move back to implementing returns it to step 3, between
// the reviewer's two steps. Either way, the reviewer's completion at step 4's entry is then taken,
// moving nothing: step 4's row records it, step 4's record names its report, and step 2's row and
// record keep their own. Before, the row read was the reviewer's place nearest where the task
// stands, step 2's, whose entry is below the one served, so the completion was refused
// HANDOFF_NOT_CURRENT_PHASE, and past that fence its report would have been written onto step 2's
// record.
func TestALateReportOfARolesLaterStepIsTakenOnThatStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		ends func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string)
		at   record.TaskPlace
	}{
		{"the review requests changes", func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			reviewRequestsChanges(t, pool, engine, key)
		}, stepPlace(1)},
		{"the reviewer moves the task back to implementing", func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			if result := applyFacts(t, pool, engine)("reviewer-back", intake.BackwardMove{Issue: key, Requester: claim.RoleReviewer, To: phase.Implementing,
				Reason: "the change needs a migration"}); result.Refusal != nil {
				t.Fatalf("the reviewer's move back to implementing was refused: %+v", result.Refusal)
			}
		}, stepPlace(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:r", "implementer:a", "reviewer:r")
			implementer := agentToken(t, "implementer")
			reviewer := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, "")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
			completesAs(t, pool, engine, claim.RoleReviewer, reviewer, "review", 1, "head")
			move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
			completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 2, "head")
			assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(4))
			served := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)[4].Entry
			tc.ends(t, pool, engine, root.Key)
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, tc.at)
			before := reportsAt(t, pool, root.Key, 2)

			report := reportOf(claim.RoleReviewer, reviewer, "review-2")
			report.Entry = served
			if result := applyFacts(t, pool, engine)("reviewer-late-step-4", report); result.Refusal != nil {
				t.Fatalf("the reviewer's completion at step 4's entry %d after the task returned to %+v = %d %s %q, want it taken on step 4",
					served, tc.at, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
			}
			assertWalksAt(t, pool, engine, root.Key, phase.Implementing, tc.at)
			rows := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)
			if rows[4].HandoffRecord != "review-2" || rows[4].Entry != served || rows[2].HandoffRecord != "review-1" {
				t.Fatalf("the reviewer's rows after its late report: step 2 %+v, step 4 %+v; want review-1 kept on step 2 and review-2 on step 4 at entry %d",
					rows[2], rows[4], served)
			}
			if got := reportsAt(t, pool, root.Key, 4); !slices.Equal(got, []string{"a:review-2"}) {
				t.Fatalf("the reports of step 4 = %v, want the late report a:review-2", got)
			}
			if got := reportsAt(t, pool, root.Key, 2); !slices.Equal(got, before) {
				t.Fatalf("the reports of step 2 = %v, want them as they stood, %v", got, before)
			}
		})
	}
}

// A return to a role's earlier step carries the record the role named last, as the first run of a
// later step does. In `implementer:a reviewer implementer:a tester`, the tester fails naming step 1,
// and agent a's completion of step 1 then names the record it wrote for step 3: it is refused
// HANDOFF_NOT_NEW, and a record written anew is taken. Before, the return raised step 1's row as it
// stood, still naming step 1's first record, so the record of step 3 passed as new work and later
// steps read it as step 1's report.
func TestAReturnToARolesEarlierStepRefusesTheRecordOfItsLaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "implementer:a", "tester")
	implementer := agentToken(t, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, implementer, "implement", 3, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Testing, stepPlace(4))
	apply := applyFacts(t, pool, engine)
	if result := apply("tester-fails-to-step-1", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleTester, Claim: agentToken(t, "tester"),
		Summary: "fails", Commit: "head", Verdict: "fail", ReturnTo: 1, HandoffRecord: "test-1", HandoffVersion: 1}); result.Refusal != nil {
		t.Fatalf("the tester's fail naming step 1 was refused: %+v", result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))

	stale := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer, Claim: implementer, Summary: "done", Commit: "head",
		HandoffRecord: "implement-3", HandoffVersion: 3}
	result := apply("implementer-step-1-names-step-3", stale)
	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_NEW" {
		t.Fatalf("agent a's completion of step 1 naming its step-3 record implement-3 = %+v, want HANDOFF_NOT_NEW; step 1's row is %+v",
			result.Refusal, rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1])
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
	fresh := stale
	fresh.HandoffRecord, fresh.HandoffVersion = "implement-4", 4
	if result := apply("implementer-step-1-anew", fresh); result.Refusal != nil {
		t.Fatalf("agent a's completion of step 1 naming a record written anew = %+v, want it taken", result.Refusal)
	}
	if first := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[1]; first.HandoffRecord != "implement-4" {
		t.Fatalf("step 1's row after the completion = %+v, want implement-4 recorded", first)
	}
}

// A keyed agent's next step after a new generation is numbered above every entry its claim was
// given, whatever rows the generation's clear folded away. In `implementer:a reviewer:b
// implementer:b`, key b, named reviewer, works steps 2 and 3 and the retro, the last of them on the
// implementer's row at place 0, so its claim is last given the retro's entry, the role's highest.
// The re-admission folds the implementer's rows into that one, and step 1 of the new generation
// raises it and writes key a's claim on it, so no row names key b above the reviewer's. Step 2's
// start must still carry an entry above the one key b's claim was given: the supervisor reads a
// start at or below that number as given already and queues nothing (supervise.Machine.given), and
// the admission pass reads the claim as level with its row (OwedEntry). Before, step 2 was numbered
// on the rows alone, at the reviewer's entry plus one, below the retro's, and its task never
// reached key b.
func TestAKeyedAgentsStepAfterANewGenerationRisesAboveWhatItsClaimWasGiven(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	steps := []string{"implementer:a", "reviewer:b", "implementer:b"}
	seedStepsPlan(t, pool, root.Key, root.Generation, steps...)
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "reviewer")
	if got := seedPhaseWorker(t, pool, root.Key, claim.RoleReviewer, ""); got != keyB {
		t.Fatalf("the reviewer's claim seeded is %q, want key b's %q", got, keyB)
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	completesAs(t, pool, engine, claim.RoleReviewer, keyB, "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	if got := issueOf(t, pool, engine, root.Key).Phase; got != phase.Retro {
		t.Fatalf("%s stands in %s after step 3, want its retro", root.Key, got)
	}
	retro := rolesRows(t, pool, engine, root.Key, claim.RoleImplementer)[0]
	if retro.Claim != keyB {
		t.Fatalf("the retro's row names %q, want key b's claim %q", retro.Claim, keyB)
	}
	// The supervisor writes what each start queued to the claim (supervise.Machine.queue); the
	// retro's task is the last key b was queued.
	seedGivenEntry(t, pool, keyB, retro.Entry)

	readmitRoot(t, pool, engine, root.Key)
	next := root.Generation + 1
	seedStepsPlan(t, pool, root.Key, next, steps...)
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if result := applyFacts(t, pool, engine)("implementer-implement-next", intake.HandoffComplete{Generation: next, Issue: root.Key,
		Role: claim.RoleImplementer, Claim: keyA, Summary: "done", Commit: "head", HandoffRecord: "implement-3", HandoffVersion: 3}); result.Refusal != nil {
		t.Fatalf("key a's completion of step 1 of generation %d was refused: %+v", next, result.Refusal)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	starts := roleStartsOf(t, pool, root.Key, claim.RoleReviewer)
	start := starts[len(starts)-1]
	reviewer := roleRowNow(t, pool, engine, root.Key, claim.RoleReviewer)
	if start.Generation != next || reviewer.Claim != keyB || start.Entry != reviewer.Entry || start.Entry <= retro.Entry {
		t.Fatalf("step 2's start of generation %d carries the entry %d (generation %d) and the reviewer's row names %q at %d, want key b's claim %q "+
			"at an entry above %d, the retro's entry key b's claim was given, which the supervisor reads as given already",
			next, start.Entry, start.Generation, reviewer.Claim, reviewer.Entry, keyB, retro.Entry)
	}
}

// Each start of a phase worker names the claim it was queued for (record.SuperviseRequest.Claim),
// the claim of the agent of the step the task stands at, so the outbox acts on that claim and a
// start the task has left since never reaches the claim the role's row names by then. In
// `implementer:a reviewer:b implementer:b`, step 1's start names key a, step 2's and step 3's key
// b, and so does the retro's, which the agent of the last implementation step runs; the retry of
// the held retro names key b again. Before, every start named no claim, and the outbox gave each
// to the worker the role's row named when the row ran.
func TestEachStartNamesTheClaimItWasQueuedFor(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:b", "implementer:b")
	keyA, keyB := agentToken(t, "implementer"), agentToken(t, "reviewer")
	named := func(what string, role claim.Role, want claim.Token) {
		t.Helper()
		starts := roleStartsOf(t, pool, root.Key, role)
		if start := starts[len(starts)-1]; start.Claim != want {
			t.Fatalf("%s names the claim %q, want %q", what, start.Claim, want)
		}
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	named("step 1's start", claim.RoleImplementer, keyA)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, "head")
	named("step 2's start", claim.RoleReviewer, keyB)
	completesAs(t, pool, engine, claim.RoleReviewer, keyB, "review", 1, "head")
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	named("step 3's start", claim.RoleImplementer, keyB)
	completesAs(t, pool, engine, claim.RoleImplementer, keyB, "implement", 2, "head")
	named("the retro's start", claim.RoleImplementer, keyB)
	holdFrom(t, pool, root.Key, phase.Retro)
	if result := applyFacts(t, pool, engine)("retry-retro", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision}); result.Refusal != nil {
		t.Fatalf("the retry of the held retro was refused: %+v", result.Refusal)
	}
	named("the held retro's retry", claim.RoleImplementer, keyB)
}

// completesAs applies role's completion of its step of LEGION-208 at commit, with the handoff
// record word-n at version n, under the claim token, the agent's own, as the route names the claim
// whose grant reported, which the completion writes on the role's row.
func completesAs(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, token claim.Token, word string, n int, commit string) {
	t.Helper()
	fact := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: role, Claim: token,
		Summary: "done", Commit: commit, HandoffRecord: word + "-" + string(rune('0'+n)), HandoffVersion: n}
	if role == claim.RoleTester {
		fact.Verdict = "pass"
	}
	if result := applyFacts(t, pool, engine)(string(role)+"-"+word+"-"+string(rune('0'+n)), fact); result.Refusal != nil {
		t.Fatalf("the %s's completion %s version %d was refused: %+v", role, word, n, result.Refusal)
	}
}

// When the lasting record (plan_key_names, record.KeyNames) holds a name for a key, default steps
// never take that key's name even when an earlier default run used it: the default tester gets a
// numbered name of its own (`tester-2`), leaving the key's name and claim for the key.
func TestADefaultStepIsHandedAKeysNameAnEarlierDefaultRunShowedBeforeTheKeyHeldIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		earlier []record.PlanStep
		held    record.KeyName
	}{
		{"the default rows are older than the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 1, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 2}},
		{"the control: the default rows came after the key's hold", []record.PlanStep{
			{Issue: "LEGION-208", Generation: 1, Task: "a", Position: 1, Role: "tester", AgentKey: "k", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 1, Role: "tester", Agent: "tester"},
			{Issue: "LEGION-208", Generation: 2, Task: defaultTask, Position: 2, Role: "reviewer", Agent: "reviewer"},
		}, record.KeyName{Name: "tester", AgentKey: "k", Generation: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := returnEngine()
			engine.cfg.DefaultSteps = []plan.Step{{Role: "tester"}, {Role: "reviewer"}}
			_, _, steps := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 3}, tc.earlier, []record.KeyName{tc.held}, nil)
			if steps[0].Agent != "tester-2" {
				t.Fatalf("the default tester is named %q, want tester-2 beside key k's held name tester", steps[0].Agent)
			}
		})
	}
}

// A name a key held stays that key's after a later version drops the key: version 1 names key k
// tester, version 2 drops k and its step, which writes over the rows that showed k's name, and
// version 3, whose tester step has no key, takes a numbered name of its own (`tester-2`) rather
// than k's name `tester`: the name is read from the lasting record of the names keys held
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

	mustTake(t, "version 3", apply(planID(key, gen, 3), ownersPlan(t, key, gen, 3, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester-2", "reviewer"}) {
		t.Fatalf("the agents of version 3 = %q, want implementer, tester-2 (keeping tester for key k) and reviewer", got)
	}
}

// The project's default steps after a change of owner are named from the lasting record of the
// names keys held (plan_key_names) and keep each key's name for that key: LEGION-209's first owner,
// the architect of LEGION-208, gave `implementer tester:k reviewer:k`, key k named tester, whose
// claim records key k's session; LEGION-209 is then admitted as a root of its own, its first step
// passes over the earlier owner's plan and deletes its rows, and it runs the default steps, whose
// tester step takes a numbered name of its own (`tester-2`), leaving key k's claim and session
// untouched on both the first run and the next run.
func TestTheDefaultStepsAfterAChangeOfOwnerLeaveTheSessionOfAKeysName(t *testing.T) {
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
	if len(steps) != 2 || steps[0].Role != "tester" || steps[0].Agent != "tester-2" || steps[1].Agent != "reviewer" {
		t.Fatalf("the default steps after the change of owner are %+v, want tester named tester-2 and reviewer named reviewer", steps)
	}
	next := issueOf(t, pool, engine, key)
	next.Generation, next.Phase, next.Status = 2, phase.Admitted, "in_progress"
	seedIssue(t, pool, next)

	firstStep(t, pool, engine, key)

	steps = planStandingOf(t, pool, engine, key, 2).plan.Steps[defaultTask]
	if len(steps) != 2 || steps[0].Agent != "tester-2" || steps[1].Agent != "reviewer" {
		t.Fatalf("the next run's default steps are %+v, want tester named tester-2 and reviewer named reviewer", steps)
	}
}
