package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A run no start of this release counted is open only where this release's own end rule holds it
// open (runOver) and its generation started a run (generationStarted): the boot gives no row to
// any other, and the settle of a fact records none. Three runs that are not open:
//   - a member of a lingering tree that awaits no merge: the root's linger is over it. The control
//     is the same member under a running root, whose run the boot opens;
//   - a root a hand-over carried in, waiting for the promotion that opens its tree, with no slot
//     or with the row it carried from the tree it left: its carried phase starts its run at its
//     promotion, so its generation started none. The control is the same root promoted, holding
//     the slot its own tree granted it, whose run the boot opens;
//   - a member of a tree admitted again, whose root waits for the promotion that opens the tree
//     again: the re-admission cleared the linger that ended the member's run, and the promotion
//     starts the member again. The control is the same root holding its slot, whose member's run
//     the boot opens.
//
// A fact that writes the issue — the member's rank, the person's move of the root or of the
// member to backlog — writes no record of the run that was not open.
func TestNoRunTheEndRuleHoldsOverOrNoStartBeganIsOpenedOrRecorded(t *testing.T) {
	t.Parallel()
	t.Run("a member of a lingering tree", func(t *testing.T) {
		for _, lingers := range []bool{true, false} {
			pool := migratedPool(t)
			root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			if lingers {
				until := stepClock.Add(time.Hour)
				root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
			}
			seedIssue(t, pool, root)
			seedTreeLifecycle(t, pool, root.Key)
			stepChild(t, pool, "LEGION-209", phase.Testing, nil)
			engine := stepEngine(nil)
			// The running root's own run of implementing is open too; the lingering root runs none.
			want, member := 2, 1
			if lingers {
				want, member = 0, 0
			}
			if opened := openUncountedRuns(t, pool, engine); opened != want || countStepStarts(t, pool, "LEGION-209") != member {
				t.Fatalf("lingering root %t: the boot opened %d runs, %d of the member; want %d, %d of the member", lingers, opened, countStepStarts(t, pool, "LEGION-209"), want, member)
			}
			if !lingers {
				continue
			}
			onStored(t, pool, engine, "LEGION-209", func(ctx context.Context, tx pgx.Tx, member record.Issue) error {
				member.Rank = "X"
				return engine.store.PutIssue(ctx, tx, member)
			})
			if writes := stepWrites(t, pool, "LEGION-209"); len(writes) != 0 || countStepStarts(t, pool, "LEGION-209") != 0 {
				t.Fatalf("a fact that wrote the lingering tree's member left step rows %+v and %d step_starts rows, want none: its run was over before it began",
					writes, countStepStarts(t, pool, "LEGION-209"))
			}
		}
	})
	t.Run("a carried root waiting for the promotion that opens its tree", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// slotted gives the root a slot row; promoted is the promotion's write of the root: its
			// status in_progress and the hand-over's mark gone.
			slotted, promoted bool
			want              int
		}{
			{name: "with no slot"},
			{name: "with the row it carried from the tree it left", slotted: true},
			{name: "control: promoted, with the slot of its own tree", slotted: true, promoted: true, want: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				carried := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300",
					Project: "LEGION", Title: "carried", Phase: phase.Testing, Generation: 2, Status: "todo", Rank: "W", LastDispatchSeq: 1, HandedOver: true,
					HandedOverFrom: "LEGION-208"}
				if tc.promoted {
					carried.Status, carried.HandedOverFrom = "in_progress", ""
				}
				seedIssue(t, pool, carried)
				seedTreeLifecycle(t, pool, "LEGION-300")
				seedRecord(t, pool, func(tx pgx.Tx) error {
					store := record.NewStore()
					if err := store.PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: "LEGION-300", Generation: 2}); err != nil {
						return err
					}
					if !tc.slotted {
						return nil
					}
					return store.PutSlot(t.Context(), tx, record.Slot{Issue: "LEGION-300", Index: 0, AdmittedAt: stepClock})
				})
				engine := stepEngine(nil)
				if opened := openUncountedRuns(t, pool, engine); opened != tc.want {
					t.Fatalf("the boot opened %d runs, want %d", opened, tc.want)
				}
				if tc.promoted {
					return
				}
				applyFacts(t, pool, engine)("backlog", intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
					Status: "backlog", Title: "carried", Rank: "W"})
				if writes := stepWrites(t, pool, "LEGION-300"); len(writes) != 0 || countStepStarts(t, pool, "LEGION-300") != 0 {
					t.Fatalf("the move to backlog of the carried root left step rows %+v and %d step_starts rows, want none: its generation started no run",
						writes, countStepStarts(t, pool, "LEGION-300"))
				}
			})
		}
	})
	t.Run("a member of a tree admitted again, waiting for its promotion", func(t *testing.T) {
		for _, slotted := range []bool{false, true} {
			pool := migratedPool(t)
			// The re-admission put the root back at admitted in todo at its next generation, with
			// its linger cleared, and left the lifecycle of the run that ended standing.
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "U", LastDispatchSeq: 1, HandedOver: true})
			seedTreeLifecycle(t, pool, "LEGION-208")
			stepChild(t, pool, "LEGION-209", phase.Testing, nil)
			if slotted {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: stepClock})
				})
			}
			engine := stepEngine(nil)
			want := 0
			if slotted {
				want = 1
			}
			if opened := openUncountedRuns(t, pool, engine); opened != want || countStepStarts(t, pool, "LEGION-209") != want {
				t.Fatalf("root holding its slot %t: the boot opened %d runs, %d of the member; want %d", slotted, opened, countStepStarts(t, pool, "LEGION-209"), want)
			}
			if slotted {
				continue
			}
			applyFacts(t, pool, engine)("backlog", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"})
			if got := issueAt(t, pool, "LEGION-209"); got == phase.Testing {
				t.Fatalf("the move to backlog left LEGION-209 at %s, want it out of testing", got)
			}
			if writes := stepWrites(t, pool, "LEGION-209"); len(writes) != 0 || countStepStarts(t, pool, "LEGION-209") != 0 {
				t.Fatalf("the move to backlog of the member before its promotion left step rows %+v and %d step_starts rows, want none: its run ended with the linger",
					writes, countStepStarts(t, pool, "LEGION-209"))
			}
		}
	})
}

// countStepStarts is how many rows of step_starts issue has.
func countStepStarts(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `select count(*) from step_starts where issue = $1`, issue).Scan(&count); err != nil {
		t.Fatalf("count the step_starts rows of %s: %v", issue, err)
	}
	return count
}

// A run that no start of this release counted — one the release before this one started, with no
// row, or one an earlier release started after a rollback, whose row the roll forward marked
// uncounted — gets an open row at the daemon's boot (OpenUncountedRuns): at the count its end
// would have taken, with the agent and the moment its start would have named, and with no record
// queued. A second boot opens nothing. A child in accepting, whose step's start no entry records,
// gets the architect that owns it (acceptingAgent: here the tree's, since the child's own has no
// claim) and no moment. An issue in a phase that runs no step, and one in a tree
// the operator holds, get no row. The run's end is then the settle's end of an open run, written
// once, under that count's id.
func TestTheBootOpensTheRunOfEachIssueInAStepNoStartCounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// uncounted marks the tester's counted row uncounted, as the roll forward of a rollback
		// does, where false deletes it, as the release before left the store.
		uncounted bool
		count     int64
	}{
		{name: "a run the release before started", count: 0},
		{name: "a run an earlier release started after a rollback", uncounted: true, count: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Implementing)
			move(t, pool, stepEngine(nil), "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
			raised := stepClock.Add(30 * time.Minute)
			execSQL(t, pool, `update phases set entry_raised_at = $1 where issue = 'LEGION-208' and role = 'tester'`, raised)
			if tc.uncounted {
				execSQL(t, pool, `update step_starts set run = 'uncounted' where issue = 'LEGION-208'`)
			} else {
				execSQL(t, pool, `delete from step_starts where issue = 'LEGION-208'`)
			}
			stepChild(t, pool, "LEGION-209", phase.Accepting, nil)
			stepChild(t, pool, "LEGION-210", phase.Admitted, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300",
				Project: "LEGION", Title: "operator", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1})
			seedOperatorTree(t, pool, "LEGION-300")
			written := len(stepWrites(t, pool, "LEGION-208"))

			engine := stepEngine(nil)
			if opened := openUncountedRuns(t, pool, engine); opened != 2 {
				t.Fatalf("the boot opened %d runs, want the tester's and the acceptance's", opened)
			}
			testing3 := placeOf("step", 3)
			tester := treeRoleToken(t, "LEGION-208", claim.RoleTester)
			want := stepStartRow{starts: tc.count, agent: tester, startedAt: raised, run: record.RunOpen,
				id: StepID("LEGION-208", 1, testing3, tc.count), generation: 1}
			opened := readStepStartRow(t, pool, "LEGION-208", testing3)
			if opened != want {
				t.Fatalf("the tester's row after the boot = %+v, want %+v", opened, want)
			}
			accepting5 := placeOf("step", 5)
			acceptance := stepStartRow{starts: 0, agent: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), run: record.RunOpen,
				id: StepID("LEGION-209", 3, accepting5, 0), generation: 3}
			if got := readStepStartRow(t, pool, "LEGION-209", accepting5); got != acceptance {
				t.Fatalf("the acceptance's row after the boot = %+v, want %+v, with no moment", got, acceptance)
			}
			var momentless bool
			if err := pool.QueryRow(t.Context(), `select started_at is null and record->'started_at' is null from step_starts where issue = 'LEGION-209'`).
				Scan(&momentless); err != nil || !momentless {
				t.Fatalf("the acceptance's row holds a moment = %t, %v; want none in the column and none in the kept record", !momentless, err)
			}
			var others int
			if err := pool.QueryRow(t.Context(), `select count(*) from step_starts where issue not in ('LEGION-208', 'LEGION-209')`).Scan(&others); err != nil || others != 0 {
				t.Fatalf("rows of the other issues = %d, %v; want none for an issue at admitted or in the operator's tree", others, err)
			}
			if got := len(stepWrites(t, pool, "LEGION-209")); got != 0 {
				t.Fatalf("the boot queued %d step records of the acceptance, want none", got)
			}
			if got := len(stepWrites(t, pool, "LEGION-208")); got != written {
				t.Fatalf("the boot queued %d step records, want none", got-written)
			}
			if again := openUncountedRuns(t, pool, engine); again != 0 || readStepStartRow(t, pool, "LEGION-208", testing3) != want {
				t.Fatalf("a second boot opened %d runs and left the row %+v; want none and the row unchanged", again, readStepStartRow(t, pool, "LEGION-208", testing3))
			}

			move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
			after := stepWrites(t, pool, "LEGION-208")[written:]
			if len(after) != 2 {
				t.Fatalf("writes after the move = %+v, want the end of the tester's run and the start of reviewing", after)
			}
			assertEnd(t, after[0], "step", 3, 1, "passed", nil)
			if end := after[0]; end.ID != want.id || end.Agent != tester || end.StartedAt == nil || !end.StartedAt.Equal(raised) {
				t.Fatalf("the end = %+v, want the record %s, the tester's, started at %s", end, want.id, raised)
			}
			want.run = record.RunEnded
			if ended := readStepStartRow(t, pool, "LEGION-208", testing3); ended != want {
				t.Fatalf("the tester's row after its end = %+v, want %+v", ended, want)
			}
		})
	}
}

// openUncountedRuns runs the boot's pass in a transaction of its own and answers how many runs it
// opened.
func openUncountedRuns(t *testing.T, pool *pgxpool.Pool, engine *Engine) int {
	t.Helper()
	var opened int
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		opened, err = engine.OpenUncountedRuns(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatalf("open the uncounted runs: %v", err)
	}
	return opened
}

// stepStartRow is what step_starts holds of one place: the count, the agent and the moment of its
// latest start, its run, and the id and the generation of the record that start kept.
type stepStartRow struct {
	starts     int64
	agent      string
	startedAt  time.Time
	run        record.StepRun
	id         string
	generation uint64
}

// readStepStartRow is the row of place on issue.
func readStepStartRow(t *testing.T, pool *pgxpool.Pool, issue string, place record.StepPlace) stepStartRow {
	t.Helper()
	var row stepStartRow
	var at *time.Time
	if err := pool.QueryRow(t.Context(), `select starts, agent, started_at, run, coalesce(record->>'id', ''), coalesce((record->>'generation')::bigint, 0)
		from step_starts where issue = $1 and task = $2 and kind = $3 and place = $4`, issue, place.Task, place.Kind, place.Place).
		Scan(&row.starts, &row.agent, &at, &row.run, &row.id, &row.generation); err != nil {
		t.Fatalf("read the row of %s %+v: %v", issue, place, err)
	}
	if at != nil {
		row.startedAt = at.UTC()
	}
	return row
}
