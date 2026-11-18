package record

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// raise counts one start of place on issue, run by agent at at, through RaiseStepStarts, and
// answers the count that start took.
func raise(t *testing.T, tx pgx.Tx, issue string, place StepPlace, agent string, at time.Time) int64 {
	t.Helper()
	var starts int64
	if _, err := NewStore().RaiseStepStarts(t.Context(), tx, issue, place, func(count int64) StepRecord {
		starts = count
		return StepRecord{ID: fmt.Sprint(count), Task: place.Task, Step: place.Place, Kind: place.Kind, Agent: agent, StartedAt: &at}
	}); err != nil {
		t.Fatalf("RaiseStepStarts of %s %+v: %v", issue, place, err)
	}
	return starts
}

// A start keeps, beside its count, its record's write as the start named it: every identity field
// and the moment, which is all a start writes (workflow's startStep). It is what the roll forward
// of a rollback writes that record's end under where an earlier release ended the run (store
// migration 20261117064152). An end of an uncounted run, ended as it is written, keeps none.
func TestAStartKeepsTheIdentityOfItsRecord(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	testing3 := StepPlace{Task: "default", Kind: "step", Place: 3}
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		write, err := store.RaiseStepStarts(t.Context(), tx, "LEGION-1", testing3, func(starts int64) StepRecord {
			return StepRecord{ID: fmt.Sprintf("run %d", starts), Task: "default", Step: 3, Kind: "step", Role: "tester", Type: "testing",
				Agent: "legion-legion-legion-1-tester", Generation: 2, StartedAt: &at}
		})
		if err != nil || write.ID != "run 1" {
			t.Fatalf("RaiseStepStarts = %+v, %v; want the write run built for the count 1", write, err)
		}
		var kept []byte
		if err := tx.QueryRow(t.Context(), `select record from step_starts where issue = 'LEGION-1'`).Scan(&kept); err != nil {
			t.Fatalf("read the kept record: %v", err)
		}
		var got StepRecord
		if err := json.Unmarshal(kept, &got); err != nil {
			t.Fatalf("decode the kept record %s: %v", kept, err)
		}
		if !reflect.DeepEqual(got, write) {
			t.Fatalf("the kept record = %+v, want the start's write: %+v", got, write)
		}
		if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = 'LEGION-1'`); err != nil {
			t.Fatalf("mark the place uncounted as the roll forward does: %v", err)
		}
		if _, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", testing3, "legion-legion-legion-1-tester", at); err != nil || !ok {
			t.Fatalf("EndUncountedStep = %t, %v; want it written", ok, err)
		}
		var cleared bool
		if err := tx.QueryRow(t.Context(), `select record is null from step_starts where issue = 'LEGION-1'`).Scan(&cleared); err != nil || !cleared {
			t.Fatalf("the record after the uncounted end is null = %t, %v; want none kept", cleared, err)
		}
	})
}

// The count of starts of one step is what carries a step record's id from its start to its end
// (workflow's StepID): the start raises it and the end reads it unchanged, in a transaction of its
// own, possibly after a restart. So the count is a row of the store, raised by one per start of
// that one place, apart from every other place and every other issue, and it keeps the agent and
// the moment the latest start wrote, which the end writes again.
func TestTheCountOfStartsOfAStepRisesByOneAndTheEndReadsItUnchanged(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const tester, implementer, merger = "legion-legion-legion-1-tester", "legion-legion-legion-1-implementer", "legion-legion-legion-1-merger"
	testing3, merge := StepPlace{Task: "default", Kind: "step", Place: 3}, StepPlace{Task: "default", Kind: "merge", Place: 0}
	first := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	later, latest := first.Add(time.Hour), first.Add(2*time.Hour)
	inTx(t, st, func(tx pgx.Tx) {
		got, err := store.StepStarts(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || got != (StepStart{}) {
			t.Fatalf("StepStarts before any start = %+v, %v; want the zero count, no agent and no moment", got, err)
		}
		for want := int64(1); want <= 2; want++ {
			if starts := raise(t, tx, "LEGION-1", testing3, tester, first); starts != want {
				t.Fatalf("RaiseStepStarts = %d; want %d", starts, want)
			}
		}
		// A start of another place, of another kind (a retro and a merge, both at the place 0), of
		// another task and of another issue each count apart.
		for _, other := range []struct {
			issue string
			place StepPlace
			agent string
		}{
			{"LEGION-1", StepPlace{Task: "default", Kind: "step", Place: 2}, implementer},
			{"LEGION-1", StepPlace{Task: "default", Kind: "retro", Place: 0}, implementer},
			{"LEGION-1", merge, merger},
			{"LEGION-1", StepPlace{Task: "a", Kind: "step", Place: 3}, tester},
			{"LEGION-2", testing3, "legion-legion-legion-2-tester"},
		} {
			if starts := raise(t, tx, other.issue, other.place, other.agent, first); starts != 1 {
				t.Fatalf("RaiseStepStarts of %s %+v = %d; want its own first start", other.issue, other.place, starts)
			}
		}
		raise(t, tx, "LEGION-1", testing3, tester, later)
		// The merge GitHub already made starts no merger (workflow's startAgent), so the next start
		// of the merge names no agent, and the count keeps the agent and the moment of that latest
		// start.
		raise(t, tx, "LEGION-1", merge, "", later)
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := store.StepStarts(t.Context(), tx, "LEGION-1", merge)
		if err != nil || got != (StepStart{Starts: 2, Agent: "", StartedAt: later, Run: RunOpen}) {
			t.Fatalf("StepStarts of the merge after its second start = %+v, %v; want 2 starts and the latest start's empty agent at %s, open", got, err, later)
		}
		got, err = store.StepStarts(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || got != (StepStart{Starts: 3, Agent: tester, StartedAt: later, Run: RunOpen}) {
			t.Fatalf("StepStarts after three starts = %+v, %v; want 3 starts and the last start's agent at %s, open", got, err, later)
		}
		ended, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || mark != EndedNow || ended != (StepStart{Starts: 3, Agent: tester, StartedAt: later, Run: RunEnded}) {
			t.Fatalf("EndStepStart of the open third start = %+v, %d, %v; want that start, ended now", ended, mark, err)
		}
		raise(t, tx, "LEGION-1", testing3, tester, latest)
		got, err = store.StepStarts(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || got != (StepStart{Starts: 4, Agent: tester, StartedAt: latest, Run: RunOpen}) {
			t.Fatalf("StepStarts after the fourth start = %+v, %v; want 4 starts with its agent and moment, open", got, err)
		}
	})
}

// One run of a step gets one end, decided in the daemon's own store in the transaction that ends
// it: the first end marks the latest start ended, and every later end of that run finds the mark
// and writes nothing, until a new start raises the count and clears it. The settle of a fact is
// the one writer of an end (workflow's settleRuns), and the mark is what holds it to one end of a
// run, whichever fact asks it to end that run again.
func TestOneRunOfAStepEndsOnce(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	merge := StepPlace{Task: "default", Kind: "merge", Place: 0}
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		if _, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || mark != NotCounted {
			t.Fatalf("EndStepStart with no start counted = %d, %v; want a run no start counted", mark, err)
		}
		raise(t, tx, "LEGION-1", merge, "legion-legion-legion-1-merger", at)
		if ended, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || mark != EndedNow || ended.Starts != 1 {
			t.Fatalf("the first end = %+v, %d, %v; want the first start, ended now", ended, mark, err)
		}
		if _, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || mark != AlreadyEnded {
			t.Fatalf("a second end of the same run = %d, %v; want the run already ended", mark, err)
		}
		if _, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", merge, "", time.Time{}); err != nil || ok {
			t.Fatalf("EndUncountedStep of a place whose start ended = %t, %v; want none", ok, err)
		}
		if starts := raise(t, tx, "LEGION-1", merge, "legion-legion-legion-1-merger", at); starts != 2 {
			t.Fatalf("the next start = %d; want the count 2", starts)
		}
		if ended, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || mark != EndedNow || ended.Starts != 2 {
			t.Fatalf("the end of the next run = %+v, %d, %v; want the second start, ended now", ended, mark, err)
		}
	})
}

// A step that started under the release before has no count. Its first end writes the row, at
// the count 0 with the agent and the moment its start would have named (the moment unknown where
// no entry was raised), marked ended, so a second end of that run writes nothing and the next
// start counts 1.
func TestAStepNoStartCountedEndsOnce(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	accepting := StepPlace{Task: "default", Kind: "step", Place: 5}
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		if starts, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", accepting, "legion-legion-legion-1-architect", time.Time{}); err != nil || !ok || starts != 0 {
			t.Fatalf("the first end of an uncounted step = %d, %t, %v; want it written at the count 0", starts, ok, err)
		}
		got, err := store.StepStarts(t.Context(), tx, "LEGION-1", accepting)
		if err != nil || got != (StepStart{Agent: "legion-legion-legion-1-architect", Run: RunEnded}) {
			t.Fatalf("StepStarts after the end = %+v, %v; want the count 0, the agent, no moment, ended", got, err)
		}
		if _, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", accepting); err != nil || mark != AlreadyEnded {
			t.Fatalf("EndStepStart after it = %d, %v; want the run already ended", mark, err)
		}
		if _, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", accepting, "legion-legion-legion-1-architect", at); err != nil || ok {
			t.Fatalf("a second end of the uncounted step = %t, %v; want none", ok, err)
		}
		if starts := raise(t, tx, "LEGION-1", accepting, "legion-legion-legion-1-architect", at); starts != 1 {
			t.Fatalf("the next start = %d; want the count 1", starts)
		}
	})
}

// A re-admission clears a generation's facts (ClearGeneration, ClearTreeGeneration), and the count
// is none of them: the generation is part of a record's id already, and a count that started again
// would give a later run of the same generation and place the id of an earlier one. So each clear
// leaves every row whole — the count, the agent, the moment and the run — of the issue it clears,
// of each member of the tree it clears, and of an issue of another tree; the tree's clear reaches
// its root and its child, which the clear's own facts show it did.
func TestAClearedGenerationKeepsTheCountOfStarts(t *testing.T) {
	for _, clear := range []string{"the issue's", "the tree's"} {
		t.Run(clear, func(t *testing.T) {
			st := migratedStore(t)
			store := NewStore()
			testing3 := StepPlace{Task: "default", Kind: "step", Place: 3}
			at := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
			inTx(t, st, func(tx pgx.Tx) {
				for _, issue := range []struct{ key, tree string }{{"LEGION-1", "LEGION-1"}, {"LEGION-2", "LEGION-1"}, {"LEGION-3", "LEGION-3"}} {
					if _, err := tx.Exec(t.Context(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
						values ($1, $2, 'LEGION', $1, 'testing', 1, 'testing', 'A', 1)`, issue.key, issue.tree); err != nil {
						t.Fatalf("record %s: %v", issue.key, err)
					}
					// A handoff commit each clear empties, which shows the clear reached the issue.
					if _, err := tx.Exec(t.Context(), `insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
						values ($1, 'tester', '', 'head', 0, '')`, issue.key); err != nil {
						t.Fatalf("record the tester's row of %s: %v", issue.key, err)
					}
				}
				// Rows that differ in every field: the root's open at its third start, the child's
				// ended at its fifth, and the other tree's issue's uncounted at its second.
				want := map[string]StepStart{}
				for _, seed := range []struct {
					key   string
					count int
					agent string
					at    time.Time
					run   StepRun
				}{
					{"LEGION-1", 3, "legion-legion-legion-1-tester", at, RunOpen},
					{"LEGION-2", 5, "legion-legion-legion-2-tester", at.Add(time.Hour), RunEnded},
					{"LEGION-3", 2, "legion-legion-legion-3-tester", at.Add(2 * time.Hour), RunUncounted},
				} {
					for range seed.count {
						raise(t, tx, seed.key, testing3, seed.agent, seed.at)
					}
					if seed.run == RunEnded {
						if _, mark, err := store.EndStepStart(t.Context(), tx, seed.key, testing3); err != nil || mark != EndedNow {
							t.Fatalf("end the run of %s = %d, %v", seed.key, mark, err)
						}
					}
					if seed.run == RunUncounted {
						if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = $1`, seed.key); err != nil {
							t.Fatalf("mark %s uncounted: %v", seed.key, err)
						}
					}
					want[seed.key] = StepStart{Starts: int64(seed.count), Agent: seed.agent, StartedAt: seed.at, Run: seed.run}
				}
				cleared := []string{"LEGION-1"}
				if clear == "the issue's" {
					if err := store.ClearGeneration(t.Context(), tx, "LEGION-1", at); err != nil {
						t.Fatalf("ClearGeneration: %v", err)
					}
				} else {
					if err := store.ClearTreeGeneration(t.Context(), tx, "LEGION-1", at); err != nil {
						t.Fatalf("ClearTreeGeneration: %v", err)
					}
					cleared = append(cleared, "LEGION-2")
				}
				var reached []string
				rows, err := tx.Query(t.Context(), `select issue from phases where role = 'tester' and handoff_commit = '' order by issue`)
				if err != nil {
					t.Fatalf("read which issues the clear reached: %v", err)
				}
				for rows.Next() {
					var key string
					if err := rows.Scan(&key); err != nil {
						t.Fatalf("scan: %v", err)
					}
					reached = append(reached, key)
				}
				rows.Close()
				if !reflect.DeepEqual(reached, cleared) {
					t.Fatalf("%s clear reached %v (their handoff commits emptied), want %v: a clear that reaches no issue proves nothing of the count", clear, reached, cleared)
				}
				for key, start := range want {
					if got, err := store.StepStarts(t.Context(), tx, key, testing3); err != nil || got != start {
						t.Errorf("StepStarts of %s after %s clear = %+v, %v; want the row unchanged, %+v", key, clear, got, err, start)
					}
				}
			})
		})
	}
}

// The roll forward of a rollback marks each place of an issue the earlier release moved uncounted
// (store migration 20261117064152), the place's run open or ended at the rollback. Its row counted
// a run that is over, so an end there finds no counted run: it ends a run of its own, at the count
// after the row's, and the row's run keeps its id and its mark. A second end of that run writes
// nothing, and the next start counts on.
func TestAnEndOfAnUncountedPlaceTakesTheNextCount(t *testing.T) {
	for _, before := range []StepRun{RunOpen, RunEnded} {
		t.Run(string(before), func(t *testing.T) {
			st := migratedStore(t)
			store := NewStore()
			testing3 := StepPlace{Task: "default", Kind: "step", Place: 3}
			counted, later := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
			inTx(t, st, func(tx pgx.Tx) {
				for range 2 {
					raise(t, tx, "LEGION-1", testing3, "legion-legion-legion-1-tester", counted)
				}
				if before == RunEnded {
					if _, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || mark != EndedNow {
						t.Fatalf("EndStepStart of the second start = %d, %v; want it ended now", mark, err)
					}
				}
				if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = 'LEGION-1'`); err != nil {
					t.Fatalf("mark the place uncounted as the roll forward does: %v", err)
				}
				row, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3)
				if err != nil || mark != NotCounted || row != (StepStart{Starts: 2, Agent: "legion-legion-legion-1-tester", StartedAt: counted, Run: RunUncounted}) {
					t.Fatalf("EndStepStart of the uncounted place = %+v, %d, %v; want its row unchanged, no counted run", row, mark, err)
				}
				starts, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", testing3, "legion-legion-legion-1-tester", later)
				if err != nil || !ok || starts != 3 {
					t.Fatalf("EndUncountedStep of the uncounted place = %d, %t, %v; want the count 3 after the row's 2", starts, ok, err)
				}
				if got, err := store.StepStarts(t.Context(), tx, "LEGION-1", testing3); err != nil ||
					got != (StepStart{Starts: 3, Agent: "legion-legion-legion-1-tester", StartedAt: later, Run: RunEnded}) {
					t.Fatalf("StepStarts after the end = %+v, %v; want the third count, its start, ended", got, err)
				}
				if _, ok, err := store.EndUncountedStep(t.Context(), tx, "LEGION-1", testing3, "legion-legion-legion-1-tester", later); err != nil || ok {
					t.Fatalf("a second uncounted end = %t, %v; want none", ok, err)
				}
				if _, mark, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || mark != AlreadyEnded {
					t.Fatalf("a second end of that run = %d, %v; want the run already ended", mark, err)
				}
				if starts := raise(t, tx, "LEGION-1", testing3, "legion-legion-legion-1-tester", later); starts != 4 {
					t.Fatalf("the next start = %d; want the count 4", starts)
				}
			})
		})
	}
}
