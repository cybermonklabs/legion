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
// migration 20261117064152).
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
		ended, closed, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || !closed || ended != (StepStart{Starts: 3, Agent: tester, StartedAt: later, Run: RunEnded}) {
			t.Fatalf("EndStepStart of the open third start = %+v, %t, %v; want that start, ended now", ended, closed, err)
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
		if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || ended {
			t.Fatalf("EndStepStart with no start counted = %t, %v; want no run ended", ended, err)
		}
		raise(t, tx, "LEGION-1", merge, "legion-legion-legion-1-merger", at)
		if start, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || !ended || start.Starts != 1 {
			t.Fatalf("the first end = %+v, %t, %v; want the first start, ended now", start, ended, err)
		}
		if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || ended {
			t.Fatalf("a second end of the same run = %t, %v; want the run already ended", ended, err)
		}
		if starts := raise(t, tx, "LEGION-1", merge, "legion-legion-legion-1-merger", at); starts != 2 {
			t.Fatalf("the next start = %d; want the count 2", starts)
		}
		if start, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", merge); err != nil || !ended || start.Starts != 2 {
			t.Fatalf("the end of the next run = %+v, %t, %v; want the second start, ended now", start, ended, err)
		}
	})
}

// A step that started under the release before has no count. The daemon's boot opens its run
// (OpenUncountedStep) at the count 0, with the agent its start would have named (the moment unknown
// where no entry was raised), and keeps the write built at that count as a start keeps its own. A
// second open writes nothing, the run's end ends it as it ends a counted run, once, and the next
// start counts 1.
func TestTheBootOpensAStepNoStartCountedAtTheCount0(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	accepting := StepPlace{Task: "default", Kind: "step", Place: 5}
	const architect = "legion-legion-legion-1-architect"
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	open := func(tx pgx.Tx) bool {
		t.Helper()
		opened, err := store.OpenUncountedStep(t.Context(), tx, "LEGION-1", accepting, func(starts int64) StepRecord {
			return StepRecord{ID: fmt.Sprintf("run %d", starts), Task: "default", Step: 5, Kind: "step", Role: "architect", Agent: architect, Generation: 1}
		})
		if err != nil {
			t.Fatalf("OpenUncountedStep: %v", err)
		}
		return opened
	}
	inTx(t, st, func(tx pgx.Tx) {
		if !open(tx) {
			t.Fatal("the boot's first open of the uncounted step wrote nothing, want its run opened")
		}
		if got, err := store.StepStarts(t.Context(), tx, "LEGION-1", accepting); err != nil || got != (StepStart{Agent: architect, Run: RunOpen}) {
			t.Fatalf("StepStarts after the open = %+v, %v; want the count 0, the agent, no moment, open", got, err)
		}
		if open(tx) {
			t.Fatal("a second open of the same run wrote it again, want nothing")
		}
		if start, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", accepting); err != nil || !ended || start.Starts != 0 {
			t.Fatalf("the end of the opened run = %+v, %t, %v; want the count 0, ended now", start, ended, err)
		}
		if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", accepting); err != nil || ended {
			t.Fatalf("a second end of that run = %t, %v; want none", ended, err)
		}
		if starts := raise(t, tx, "LEGION-1", accepting, architect, at); starts != 1 {
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
						if _, ended, err := store.EndStepStart(t.Context(), tx, seed.key, testing3); err != nil || !ended {
							t.Fatalf("end the run of %s = %t, %v", seed.key, ended, err)
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
// a run that is over, so the daemon's boot opens the run now there at the count after the row's
// (OpenUncountedStep), and the row's run keeps its id. The end ends that run at that count, a
// second end writes nothing, and the next start counts on.
func TestTheBootOpensAnUncountedPlaceAtTheNextCount(t *testing.T) {
	for _, before := range []StepRun{RunOpen, RunEnded} {
		t.Run(string(before), func(t *testing.T) {
			st := migratedStore(t)
			store := NewStore()
			testing3 := StepPlace{Task: "default", Kind: "step", Place: 3}
			const tester = "legion-legion-legion-1-tester"
			counted, later := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
			inTx(t, st, func(tx pgx.Tx) {
				for range 2 {
					raise(t, tx, "LEGION-1", testing3, tester, counted)
				}
				if before == RunEnded {
					if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || !ended {
						t.Fatalf("EndStepStart of the second start = %t, %v; want it ended now", ended, err)
					}
				}
				if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = 'LEGION-1'`); err != nil {
					t.Fatalf("mark the place uncounted as the roll forward does: %v", err)
				}
				if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || ended {
					t.Fatalf("EndStepStart of the uncounted place before the boot = %t, %v; want no run ended", ended, err)
				}
				opened, err := store.OpenUncountedStep(t.Context(), tx, "LEGION-1", testing3, func(starts int64) StepRecord {
					return StepRecord{ID: fmt.Sprintf("run %d", starts), Task: "default", Step: 3, Kind: "step", Role: "tester", Agent: tester, Generation: 1, StartedAt: &later}
				})
				if err != nil || !opened {
					t.Fatalf("OpenUncountedStep of the uncounted place = %t, %v; want its run opened", opened, err)
				}
				if got, err := store.StepStarts(t.Context(), tx, "LEGION-1", testing3); err != nil ||
					got != (StepStart{Starts: 3, Agent: tester, StartedAt: later, Run: RunOpen}) {
					t.Fatalf("StepStarts after the open = %+v, %v; want the third count, its start, open", got, err)
				}
				if start, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || !ended || start.Starts != 3 {
					t.Fatalf("the end of the opened run = %+v, %t, %v; want the count 3, ended now", start, ended, err)
				}
				if _, ended, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3); err != nil || ended {
					t.Fatalf("a second end of that run = %t, %v; want none", ended, err)
				}
				if starts := raise(t, tx, "LEGION-1", testing3, tester, later); starts != 4 {
					t.Fatalf("the next start = %d; want the count 4", starts)
				}
			})
		})
	}
}
