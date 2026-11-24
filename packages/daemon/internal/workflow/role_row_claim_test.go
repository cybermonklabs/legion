package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// roleRowClaims is, for every row of kind issue's outbox holds, oldest first, the role it is
// addressed to, the claim it names ("" for none) and whether its task follows, read from the row's
// JSON as the outbox decodes it.
func roleRowClaims(t *testing.T, pool *pgxpool.Pool, issue, kind string) []struct{ role, claim, follows string } {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'role', coalesce(payload->>'claim', ''), coalesce(payload->>'taskFollows', 'false')
		from outbox where issue = $1 and kind = $2 order by id`, issue, kind)
	if err != nil {
		t.Fatalf("read the %s rows of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var got []struct{ role, claim, follows string }
	for rows.Next() {
		var row struct{ role, claim, follows string }
		if err := rows.Scan(&row.role, &row.claim, &row.follows); err != nil {
			t.Fatalf("scan a %s row of %s: %v", kind, issue, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s rows of %s: %v", kind, issue, err)
	}
	return got
}

// A hook that fires on a move back writes a prompt for each role it names, and each names the
// claim of the agent that works its role's step in the round the move opened, read once the move's
// start has written that agent: the outbox publishes on that claim's topic, whatever a later entry
// of the role's step names when the row runs. In the plan `implementer:a tester:a reviewer` key
// a's agent, `…-implementer`, works both steps, so both prompts name it, and not the role's own
// claim `…-tester`.
func TestAHookPromptNamesTheAgentOfItsRolesStep(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	engine.cfg.Hooks = []Hook{{Name: "work-back", On: HookEventWorkBack, Every: 1,
		Roles: []claim.Role{claim.RoleImplementer, claim.RoleTester}, Prompt: "Say what you have done so far."}}
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:a", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})

	move(t, pool, engine, root.Key, TriggerTesterFailed, record.TaskPlace{}, record.PhaseRow{})

	keyed := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Claim
	if keyed == claim.Token(token(t, root.Key, claim.RoleTester)) || keyed == "" {
		t.Fatalf("the implementer's phases row names %q after the move back, want key a's agent: the case's premise", keyed)
	}
	prompts := roleRowClaims(t, pool, root.Key, string(record.OutboxKindHookPrompt))
	if len(prompts) != 2 {
		t.Fatalf("the firing wrote the prompts %+v, want one for each role the hook names", prompts)
	}
	for _, prompt := range prompts {
		if want := roleRowOf(t, pool, engine, root.Key, claim.Role(prompt.role)).Claim; prompt.claim != string(want) || want != keyed {
			t.Errorf("the %s's prompt names the claim %q, want %s, key a's agent on its step", prompt.role, prompt.claim, keyed)
		}
	}
}

// A hook's role implementer names the agent that does the implementation work of the task where
// the move left it, whatever that agent's role is called (implementationAgent), as a merge
// forward's ask and a settlement's message do. In `coder tester`, coder a role of the plan of the
// type implementation, the tester's fail sends the work back to the coder's step, and the hook,
// which names the implementer and the tester, prompts the coder on the claim the start of its step
// names, and never the role implementer's own claim `…-implementer`, which no step of the task
// runs and so no agent holds.
func TestAHookPromptOfTheImplementerReachesTheAgentOfTheImplementationStep(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	engine.cfg.Hooks = []Hook{{Name: "work-back", On: HookEventWorkBack, Every: 1,
		Roles: []claim.Role{claim.RoleImplementer, claim.RoleTester}, Prompt: saySoFar}}
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlanDefining(t, pool, root.Key, root.Generation, map[string]plan.Type{"coder": plan.TypeImplementation}, "coder", "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})

	move(t, pool, engine, root.Key, TriggerTesterFailed, record.TaskPlace{}, record.PhaseRow{})

	coder := roleRowOf(t, pool, engine, root.Key, "coder").Claim
	if coder == "" || coder == claim.Token(token(t, root.Key, claim.RoleImplementer)) {
		t.Fatalf("the coder's phases row names %q after the move back, want the claim of the coder's step: the case's premise", coder)
	}
	prompts := roleRowClaims(t, pool, root.Key, string(record.OutboxKindHookPrompt))
	if len(prompts) != 2 {
		t.Fatalf("the firing wrote the prompts %+v, want one for each role the hook names", prompts)
	}
	want := map[string]claim.Token{
		string(claim.RoleImplementer): coder,
		string(claim.RoleTester):      roleRowOf(t, pool, engine, root.Key, claim.RoleTester).Claim,
	}
	for _, prompt := range prompts {
		if prompt.claim != string(want[prompt.role]) {
			t.Errorf("the %s's prompt names the claim %q, want %q, the claim of the agent of that work's step", prompt.role, prompt.claim, want[prompt.role])
		}
	}
}

// A hook's roles tester and reviewer name the agent of the task's step that does that role's work
// as the role implementer does: a step of the role's type whose role is the role itself or a role
// the deployment or the plan defines, whatever it is called. In `coder auditor checker
// acceptance:owner`, auditor a role of the plan of the type review and checker one of the type
// testing, the checker's fail sends the work back to the coder's step, and the hook prompts the
// coder, the checker and the auditor, each on the claim the start of its step names: never the
// roles' own claims `…-tester` and `…-reviewer`, which no step of the task runs, and never the
// owner for the reviewer, though the owner's acceptance is of the type review too. In `coder
// checker` no step does a reviewer's work, so the firing prompts nobody for the reviewer and says
// so.
func TestAHookPromptOfATesterOrAReviewerReachesTheAgentOfThatWorksStep(t *testing.T) {
	defined := map[string]plan.Type{"coder": plan.TypeImplementation, "auditor": plan.TypeReview, "checker": plan.TypeTesting}
	for _, tc := range []struct {
		name  string
		steps []string
		// passes is the number of steps that pass before the checker's, the last of them.
		passes int
		// want names, for each role the firing prompts, the role whose step's claim its prompt names.
		want map[claim.Role]claim.Role
	}{
		{name: "coder auditor checker acceptance:owner", steps: []string{"coder", "auditor", "checker", "acceptance:owner"}, passes: 2,
			want: map[claim.Role]claim.Role{claim.RoleImplementer: "coder", claim.RoleTester: "checker", claim.RoleReviewer: "auditor"}},
		{name: "coder checker", steps: []string{"coder", "checker"}, passes: 1,
			want: map[claim.Role]claim.Role{claim.RoleImplementer: "coder", claim.RoleTester: "checker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepEngine(nil)
			engine.cfg.Hooks = []Hook{{Name: "work-back", On: HookEventWorkBack, Every: 1,
				Roles: []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}, Prompt: saySoFar}}
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlanDefining(t, pool, root.Key, root.Generation, defined, tc.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			for range tc.passes {
				move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
			}

			move(t, pool, engine, root.Key, TriggerStepFailed, record.TaskPlace{}, record.PhaseRow{})

			prompts := roleRowClaims(t, pool, root.Key, string(record.OutboxKindHookPrompt))
			if len(prompts) != len(tc.want) {
				t.Fatalf("the firing wrote the prompts %+v, want one for each of %v", prompts, tc.want)
			}
			for _, prompt := range prompts {
				worker, ok := tc.want[claim.Role(prompt.role)]
				if !ok {
					t.Fatalf("the firing prompted the %s on %q, want no prompt of it: no step of %s does its work", prompt.role, prompt.claim, tc.name)
				}
				want := roleRowOf(t, pool, engine, root.Key, worker).Claim
				if want == "" || want == claim.Token(token(t, root.Key, claim.Role(prompt.role))) {
					t.Fatalf("the %s's phases row names %q, want the claim of its own step: the case's premise", worker, want)
				}
				if prompt.claim != string(want) {
					t.Errorf("the %s's prompt names the claim %q, want %q, the claim of the %s's step", prompt.role, prompt.claim, want, worker)
				}
			}
			if posted := firingMessages(t, pool); len(posted) != 1 {
				t.Errorf("the firing posted %q, want one message", posted)
			}
		})
	}
}
