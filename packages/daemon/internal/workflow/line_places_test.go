package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each reader of where a task stands on its line gives each position of the line one place: the
// start (startPlace), the first place of a phase (PlaceOf), a move back (backwardPlace), the phase
// of a step's run (phaseAt), the phase a place derives (issuePhaseOf) and the walk's table. The
// lines: a task with a sub-owner that merges, planning retro merging, at its parts, its retro and
// its merge; one that another task needs, planning alone, at its parts; a task of steps,
// implementer reviewer, at steps 1 and 2, then its retro and its merge; a run with no plan rows,
// today's line at today's places; and a task with a sub-owner whose plan rows hold a step, which
// the plan refuses and which the walk places all the same: its parts, then that step at step 1,
// then its retro and its merge. No step runs at the parts, so phaseAt finds no phase there.
//
// The fault it catches is a reader that maps a position of a line to a place by a rule of its own
// (its controls: phaseAt that places each position as a line with no sub-owner does, which finds
// step 1 at the planning of the last line and so answers planning for its implementer; the walk's
// table built on those places, whose gate's opening puts the first line's task at step 1).
func TestEachReaderGivesAPositionOfALineOnePlace(t *testing.T) {
	const key = "LEGION-208"
	task := func(id, owner string, needs ...string) record.PlanTask {
		return record.PlanTask{Issue: key, Generation: 1, Task: id, PlanVersion: 1, OwnerRole: owner, Needs: needs, State: record.TaskWaiting}
	}
	step := func(id string, position int, role string, typ plan.Type) record.PlanStep {
		return record.PlanStep{Issue: key, Generation: 1, Task: id, Position: position, Role: role, Type: typ}
	}
	walkIn := func(id string, tasks []record.PlanTask, steps ...record.PlanStep) Walk {
		t.Helper()
		stored := &record.StoredPlan{Version: record.PlanVersion{Issue: key, Generation: 1, Version: 1}, Tasks: tasks, Steps: map[string][]record.PlanStep{}}
		for _, s := range steps {
			stored.Steps[s.Task] = append(stored.Steps[s.Task], s)
		}
		for _, w := range walksOf(key, stored) {
			if w.Task == id {
				return w
			}
		}
		t.Fatalf("the plan holds no task %s", id)
		return Walk{}
	}
	parts, retro, merge := record.TaskPlace{Kind: record.PlaceParts}, record.TaskPlace{Kind: record.PlaceRetro}, record.TaskPlace{Kind: record.PlaceMerge}
	for _, tc := range []struct {
		name   string
		walk   Walk
		line   []phase.Phase
		places []record.TaskPlace
	}{
		{"a task with a sub-owner that merges", walkIn("f", []record.PlanTask{task("f", "architect")}),
			[]phase.Phase{phase.Planning, phase.Retro, phase.Merging}, []record.TaskPlace{parts, retro, merge}},
		{"a task with a sub-owner that another task needs",
			walkIn("f", []record.PlanTask{task("f", "architect"), task("h", "", "f")}, step("h", 1, "implementer", "implementation")),
			[]phase.Phase{phase.Planning}, []record.TaskPlace{parts}},
		{"a task of steps",
			walkIn("a", []record.PlanTask{task("a", "")}, step("a", 1, "implementer", "implementation"), step("a", 2, "reviewer", "review")),
			[]phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}, []record.TaskPlace{atStep(1), atStep(2), retro, merge}},
		{"a run with no plan rows", walksOf(key, nil)[0], phase.Order,
			[]record.TaskPlace{atStep(1), atStep(2), atStep(3), atStep(4), atStep(5), retro, merge}},
		{"a task with a sub-owner whose plan rows hold a step",
			walkIn("g", []record.PlanTask{task("g", "architect")}, step("g", 1, "implementer", "implementation")),
			[]phase.Phase{phase.Planning, phase.Implementing, phase.Retro, phase.Merging}, []record.TaskPlace{parts, atStep(1), retro, merge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.walk
			if !slices.Equal(w.Line, tc.line) {
				t.Fatalf("the line of task %s is %v, want %v", w.Task, w.Line, tc.line)
			}
			pair := func(p phase.Phase, place record.TaskPlace) string { return fmt.Sprintf("%s at %+v", p, place) }
			pairs := map[string]bool{}
			for at, p := range tc.line {
				pairs[pair(p, tc.places[at])] = true
			}
			if to, place, ok := w.startPlace(); !ok || to != tc.line[0] || place != tc.places[0] {
				t.Errorf("the start of task %s puts it in %s at %+v (%t), want %s at %+v", w.Task, to, place, ok, tc.line[0], tc.places[0])
			}
			last := w
			last.State, last.Place = record.TaskRunning, tc.places[len(tc.places)-1]
			for at, p := range tc.line {
				want := tc.places[at]
				if slices.Index(tc.line, p) == at && !slices.Contains(tc.line[at+1:], p) {
					if place, ok := w.PlaceOf(p); !ok || place != want {
						t.Errorf("PlaceOf(%s) of task %s = %+v, %t; want %+v", p, w.Task, place, ok, want)
					}
					if at < len(tc.line)-1 {
						if place, ok, _ := last.backwardPlace(p); !ok || place != want {
							t.Errorf("a move back of task %s from %+v to %s reaches %+v (%t), want %+v", w.Task, last.Place, p, place, ok, want)
						}
					}
				}
				wantPhase, wantFound := p, true
				if want == parts {
					wantPhase, wantFound = "", false
				}
				if got, found := phaseAt(w, want.StepPlace(w.Task)); got != wantPhase || found != wantFound {
					t.Errorf("phaseAt(%+v) of task %s = %q, %t; want %q, %t", want, w.Task, got, found, wantPhase, wantFound)
				}
				if got := issuePhaseOf(w, want, record.TaskRunning); got != p {
					t.Errorf("issuePhaseOf task %s running at %+v = %q, want %s", w.Task, want, got, p)
				}
			}
			seen := map[string]bool{}
			for _, row := range w.table() {
				if row.From != phase.Admitted {
					if from := pair(row.From, row.FromPlace); !pairs[from] {
						t.Errorf("the table of task %s holds the row %s/%s out of %s, a place its line does not give that phase", w.Task, row.From, row.Trigger, from)
					} else {
						seen[from] = true
					}
				}
				if row.To != phase.Done {
					if to := pair(row.To, row.ToPlace); !pairs[to] {
						t.Errorf("the table of task %s holds the row %s/%s into %s, a place its line does not give that phase", w.Task, row.From, row.Trigger, to)
					} else {
						seen[to] = true
					}
				}
			}
			for at, p := range tc.line {
				if !seen[pair(p, tc.places[at])] {
					t.Errorf("no row of the table of task %s moves into or out of %s", w.Task, pair(p, tc.places[at]))
				}
			}
		})
	}
}

// The record reads the running step of a run with no plan rows (record.RunningOf), which the
// issue's own read, the idle view and the served state go through, at today's places: each phase
// of phase.Order before the retro at its step from 1, then the retro and the merge, the places the
// release before gave them and the row "a run with no plan rows" of
// TestEachReaderGivesAPositionOfALineOnePlace holds for the walk.
//
// The fault it catches is today's places moved for the record (its controls: the record's reading
// of today's places one step on; and the one function of a line's places, record.LinePlaces,
// numbering its steps from 0, which moves the walk's places and the record's together, so a test
// that compares the two cannot see it, and this one fails).
func TestTheRecordReadsTodaysPlaces(t *testing.T) {
	want := []record.TaskPlace{atStep(1), atStep(2), atStep(3), atStep(4), atStep(5), {Kind: record.PlaceRetro}, {Kind: record.PlaceMerge}}
	for at, p := range phase.Order {
		steps := record.RunningOf(record.Issue{Key: "LEGION-208", Phase: p}, false, nil)
		if len(steps) != 1 || steps[0].Place != want[at] {
			t.Errorf("the running steps of an issue with no plan rows in %s = %+v, want one at %+v", p, steps, want[at])
		}
	}
}
