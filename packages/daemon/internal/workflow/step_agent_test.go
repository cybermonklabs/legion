package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// storedPhaseRow is role's phases row of key as the record holds it, the zero row for none.
func storedPhaseRow(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var rows []record.PhaseRow
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) (err error) {
		rows, err = record.NewStore().Phases(t.Context(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the phases rows of %s: %v", key, err)
	}
	return record.PhaseRowOf(rows, role)
}

// startEntriesOf is the entry of each start row of key that starts role, oldest first.
func startEntriesOf(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select coalesce((payload->>'entry')::bigint, 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $2 order by id`, key, string(role))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var entries []int64
	for rows.Next() {
		var entry int64
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// keyedRun walks LEGION-208, whose owner gave `implementer:a tester:a reviewer`, from its gate to
// the tester's step: key a's agent ran the implementer's step and now runs the tester's.
func keyedRun(t *testing.T) (*pgxpool.Pool, record.Issue) {
	t.Helper()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "tester:a", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	return pool, root
}

// The two steps of key a are one agent: the tester's step starts the claim the implementer's step
// started, `…-implementer`. Its entry writes that claim on the tester's phases row with the step
// it runs, so the claim of the worker on the tester's step (record.WorkerClaim) is that claim, and
// the record of step 2 names it as its agent. An entry that named no agent started a fresh tester
// for the second step of key a.
func TestAKeyedAgentsTwoStepsStartOneClaim(t *testing.T) {
	pool, root := keyedRun(t)
	keyed := claim.Token(token(t, root.Key, claim.RoleImplementer))
	row := storedPhaseRow(t, pool, root.Key, claim.RoleTester)
	if row.Claim != keyed || row.StepTask != "a" || row.StepPlace != 2 {
		t.Fatalf("the tester's phases row names %q at %s/%d, want key a's claim %s at a/2", row.Claim, row.StepTask, row.StepPlace, keyed)
	}
	rows := []record.PhaseRow{storedPhaseRow(t, pool, root.Key, claim.RoleImplementer), row}
	if got, err := record.WorkerClaim("legion", root.Key, claim.RoleTester, rows); err != nil || got != keyed {
		t.Fatalf("the claim of the worker on the tester's step = %q, %v; want %s", got, err, keyed)
	}
	var agent string
	for _, write := range stepWrites(t, pool, root.Key) {
		if write.Kind == record.PlaceStep && write.Step == 2 {
			agent = write.Agent
		}
	}
	if agent != string(keyed) {
		t.Fatalf("the record of step 2 names the agent %q, want %s", agent, keyed)
	}
}

// The entries of one claim rise across its rows: the tester's row of key a's agent holds entry 2,
// one above the implementer's row's 1, and the tester's start carries it, so the claim's given
// entry, 1, does not refuse the task of its second step. Numbered on the tester's row alone, the
// entry was 1.
func TestAnEntryRisesAcrossTheRowsOfItsClaim(t *testing.T) {
	pool, root := keyedRun(t)
	if got := storedPhaseRow(t, pool, root.Key, claim.RoleImplementer).Entry; got != 1 {
		t.Fatalf("the implementer's row holds entry %d, want 1", got)
	}
	if got := storedPhaseRow(t, pool, root.Key, claim.RoleTester).Entry; got != 2 {
		t.Fatalf("the tester's row holds entry %d, want 2: one above every row of its claim", got)
	}
	if got := startEntriesOf(t, pool, root.Key, claim.RoleTester); len(got) != 1 || got[0] != 2 {
		t.Fatalf("the tester's starts carry the entries %v, want one start of entry 2", got)
	}
}

// An entry never falls below its row's own: the release before raised a phase worker's row to entry
// 3 and no completion wrote a claim on it. The next entry of that worker, on an issue with no plan
// rows, is 4. Numbered by the rows that name its claim alone, it fell to 1, which the completion
// route reads as behind.
func TestAnEntryNeverFallsBelowItsRowsOwn(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		for _, role := range claim.Roles {
			if role == claim.RoleArchitect {
				continue
			}
			if err := record.NewStore().PutPhase(t.Context(), tx, record.PhaseRow{Issue: root.Key, Role: role, Entry: 3}); err != nil {
				return err
			}
		}
		return nil
	})
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	role := phase.RoleFor(issueAt(t, pool, root.Key))
	if got := storedPhaseRow(t, pool, root.Key, role).Entry; got != 4 {
		t.Fatalf("the %s's row holds entry %d, want 4: one above its own 3", role, got)
	}
}

// An issue with no plan rows names no agent of a key: the worker of its first phase starts in its
// role's own claim, which its row names, at the row's own entry plus one (R20).
func TestARunWithNoPlanRowsNamesNoAgent(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	role := phase.RoleFor(issueAt(t, pool, root.Key))
	row := storedPhaseRow(t, pool, root.Key, role)
	if own := claim.Token(token(t, root.Key, role)); row.Claim != own || row.Entry != 1 || row.StepTask != "" {
		t.Fatalf("the %s's row = claim %q, entry %d, step %q; want %s, 1 and no step", role, row.Claim, row.Entry, row.StepTask, own)
	}
}
