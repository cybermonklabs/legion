package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
