package record

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The record names an agent the agent of a run on an issue while the latest start of a place of
// the issue wrote that agent and the run there is not ended: open, or uncounted after the roll
// forward of a rollback. An ended run, another agent's run, another issue's run and an issue no
// start counted name it nowhere. RunsAStep asks the same over every issue.
func TestTheRecordNamesTheAgentOfARunThatIsNotEnded(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const tester, architect = "legion-legion-legion-1-tester", "legion-legion-legion-2-architect"
	testing3, accepting5 := StepPlace{Task: "default", Kind: "step", Place: 3}, StepPlace{Task: "default", Kind: "step", Place: 5}
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		on := func(agent, issue string, want bool) {
			t.Helper()
			if got, err := RunsAStepOn(t.Context(), tx, agent, issue); err != nil || got != want {
				t.Fatalf("RunsAStepOn(%s, %s) = %v, %v; want %v", agent, issue, got, err, want)
			}
		}
		anywhere := func(agent string, want bool) {
			t.Helper()
			if got, err := RunsAStep(t.Context(), tx, agent); err != nil || got != want {
				t.Fatalf("RunsAStep(%s) = %v, %v; want %v", agent, got, err, want)
			}
		}
		on(tester, "LEGION-1", false)
		anywhere(tester, false)

		raise(t, tx, "LEGION-1", testing3, tester, at)
		raise(t, tx, "LEGION-3", accepting5, architect, at)
		on(tester, "LEGION-1", true)
		anywhere(tester, true)
		on(tester, "LEGION-3", false)
		on(architect, "LEGION-1", false)
		on(architect, "LEGION-3", true)
		anywhere(architect, true)

		if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || !ended {
			t.Fatalf("EndStepStart of the tester's run = %t, %v; want it ended now", ended, err)
		}
		on(tester, "LEGION-1", false)
		anywhere(tester, false)

		if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = 'LEGION-1'`); err != nil {
			t.Fatalf("mark the place uncounted as the roll forward does: %v", err)
		}
		on(tester, "LEGION-1", true)
		anywhere(tester, true)
	})
}

// RunsAStep asks over every issue, on every completion of an implementer and every adoption, of a
// table whose rows are never deleted, so the read is served by an index of the agents of the runs
// that are not ended rather than by a scan of every row ever written. The plan is read with
// sequential scans priced out, so the test asks whether an index can serve the read at all, which
// the planner of a small table would not otherwise show.
func TestRunsAStepIsServedByAnIndexOfTheRunsNotEnded(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), `set local enable_seqscan = off`); err != nil {
			t.Fatalf("price out sequential scans: %v", err)
		}
		rows, err := tx.Query(t.Context(), `explain select exists(select 1 from step_starts where agent = $1 and run <> $2)`,
			"legion-legion-legion-1-tester", RunEnded)
		if err != nil {
			t.Fatalf("explain RunsAStep's read: %v", err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the plan: %v", err)
		}
		if text := strings.Join(plan, "\n"); !strings.Contains(text, "step_starts_running_agent") {
			t.Fatalf("RunsAStep's read is planned as\n%s\nwant it served by the index step_starts_running_agent", text)
		}
	})
}
