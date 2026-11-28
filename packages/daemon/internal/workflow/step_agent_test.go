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

// Every entry of an issue rises above every earlier entry of its rows. Key a's agent works the
// implementer's and tester's steps in order: their entries are 1 and 2 in this fixture, and the
// tester's start carries 2, above the entry its claim was given. Counting only the tester's row
// would give 1 again and lose the second step's task.
func TestAnEntryRisesAcrossTheRowsOfItsClaim(t *testing.T) {
	pool, root := keyedRun(t)
	if got := storedPhaseRow(t, pool, root.Key, claim.RoleImplementer).Entry; got != 1 {
		t.Fatalf("the implementer's row holds entry %d, want 1", got)
	}
	if got := storedPhaseRow(t, pool, root.Key, claim.RoleTester).Entry; got != 2 {
		t.Fatalf("the tester's row holds entry %d, want 2: one above every row of the issue", got)
	}
	if got := roleStartsOf(t, pool, root.Key, claim.RoleTester); len(got) != 1 || got[0].Entry != 2 {
		t.Fatalf("the tester's starts are %+v, want one start of entry 2", got)
	}
}

// An entry never falls below any row of its issue: the release before raised every phase worker's
// row to entry 3 and no completion wrote a claim on it. The next entry is 4. Counting only rows
// that name the claim would give 1, which the completion route reads as behind.
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
		t.Fatalf("the %s's row holds entry %d, want 4: one above every row's seeded 3", role, got)
	}
}

// An issue with no plan rows names no agent of a key: the worker of its first phase starts in its
// role's own claim, which its row names, at entry 1, one above every row of the issue (R20).
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

// highestEntryOf is the highest entry among starts, 0 for none.
func highestEntryOf(starts []record.SuperviseRequest) int64 {
	var highest int64
	for _, start := range starts {
		highest = max(highest, start.Entry)
	}
	return highest
}

// Every entry a claim is handed is above every entry that claim was handed before, in every
// version and generation, since each entry of an issue is one above the highest entry of every row
// of the issue. Version 1 `implementer tester:k reviewer:k` runs key k's agent, named tester, on the
// tester's and then the reviewer's step, so the claim `…-tester` is handed the reviewer's entry.
// The owner stops the task and gives version 2 `implementer reviewer:j tester:k`: step 2 moves the
// reviewer's row to key j's claim, and at step 3 the one row that names `…-tester` is the tester's,
// below the entry that claim was handed last. Numbered over the claim's own rows, step 3's entry
// was that same number, so the claim's queue took the task as given already, no sweep owed it, and
// the tester's step was never sent.
func TestAnEntryIsAboveEveryEntryItsClaimWasHandedAcrossVersions(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	keyed := claim.Token(token(t, key, claim.RoleTester))
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:k", "reviewer:k"))))
	move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
	reviewer := storedPhaseRow(t, pool, key, claim.RoleReviewer)
	if reviewer.Claim != keyed {
		t.Fatalf("after version 1's step 3 the reviewer's row names %q, want key k's claim %s", reviewer.Claim, keyed)
	}
	handed := reviewer.Entry
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget, checked twice.", "implementer", "reviewer:j", "tester:k"))))
	move(t, pool, engine, key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})

	tester := storedPhaseRow(t, pool, key, claim.RoleTester)
	if tester.Claim != keyed || tester.StepPlace != 3 {
		t.Fatalf("after version 2's step 3 the tester's row names %q at place %d, want key k's claim %s at place 3", tester.Claim, tester.StepPlace, keyed)
	}
	all := superviseRowsOf(t, pool, key, "start")
	last := all[len(all)-1]
	if last.Role != claim.RoleTester || last.Entry != tester.Entry {
		t.Fatalf("the last start is %+v, want the tester's step's start at the row's entry %d", last, tester.Entry)
	}
	if last.Entry <= handed {
		t.Fatalf("key k's claim is handed entry %d at version 2's step 3, want above %d, the entry it was handed at version 1's step 3: its queue would take the task as given already", last.Entry, handed)
	}
	if highest := highestEntryOf(all[:len(all)-1]); last.Entry <= highest {
		t.Fatalf("the tester's step's entry is %d, want above %d, every earlier entry of the issue", last.Entry, highest)
	}
}
