package workflow

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// issuePhaseLine is the line each walk of these tests walks: the steps `planner implementer
// tester reviewer acceptance:owner`, then Legion's retro and merge, so step 1 runs as planning,
// step 2 as implementing, step 3 as testing, step 4 as reviewing and step 5 as accepting.
var issuePhaseLine = plan.Line([]plan.Step{{Role: "planner"}, {Role: "implementer"}, {Role: "tester"}, {Role: "reviewer"},
	{Role: "acceptance", Key: "owner"}}, true)

// standsAt is a stored walk of task id on issuePhaseLine, in state at place, held for reason where
// it is held.
func standsAt(id string, state record.TaskState, place record.TaskPlace, reason record.HoldReason) Walk {
	return Walk{Stored: true, Task: id, Line: issuePhaseLine, State: state, Place: place, HoldReason: reason}
}

// stepAtPlace is the place of step n.
func stepAtPlace(n int) record.TaskPlace {
	return record.TaskPlace{Kind: record.PlaceStep, Position: n}
}

// An issue whose plan holds several tasks stands where its least finished work stands: each task
// done or left, done; else the phase of its running task that comes first in today's order, a step
// of a role the deployment defines at the place of implementing; else, where no task runs and one
// is held or stopped, held from the phase of the first such task in that order, for that task's
// hold reason, or task-stopped for a stopped task; else, each task that is not done waiting,
// admitted.
//
// The fault it catches is an issue whose slot or status follows the wrong task (its control:
// issuePhaseOfTasks answering the last running task's phase, which answers reviewing for the first
// case, and the issue would give its slot back while task a implements).
func TestTheIssueStandsAtTheFirstPhaseOfItsRunningTasks(t *testing.T) {
	noAct := record.TaskPlace{}
	stepLine := []phase.Phase{phase.Step, phase.Testing, phase.Retro, phase.Merging}
	for _, tc := range []struct {
		name  string
		tasks []Walk
		phase phase.Phase
		hold  *record.Hold
	}{
		{"a implementing and b reviewing",
			[]Walk{standsAt("a", record.TaskRunning, stepAtPlace(2), ""), standsAt("b", record.TaskRunning, stepAtPlace(4), "")},
			phase.Implementing, nil},
		{"a done and b reviewing",
			[]Walk{standsAt("a", record.TaskDone, stepAtPlace(5), ""), standsAt("b", record.TaskRunning, stepAtPlace(4), "")},
			phase.Reviewing, nil},
		{"a stopped at reviewing and b testing",
			[]Walk{standsAt("a", record.TaskStopped, stepAtPlace(4), ""), standsAt("b", record.TaskRunning, stepAtPlace(3), "")},
			phase.Testing, nil},
		{"a stopped at reviewing and b waiting on it",
			[]Walk{standsAt("a", record.TaskStopped, stepAtPlace(4), ""), standsAt("b", record.TaskWaiting, noAct, "")},
			phase.Held, &record.Hold{From: phase.Reviewing, Reason: record.HoldTaskStopped}},
		{"a held, escalated, at testing and b stopped at implementing",
			[]Walk{standsAt("a", record.TaskHeld, stepAtPlace(3), record.HoldEscalated), standsAt("b", record.TaskStopped, stepAtPlace(2), "")},
			phase.Held, &record.Hold{From: phase.Implementing, Reason: record.HoldTaskStopped}},
		{"a held, escalated, at reviewing and b waiting",
			[]Walk{standsAt("a", record.TaskHeld, stepAtPlace(4), record.HoldEscalated), standsAt("b", record.TaskWaiting, noAct, "")},
			phase.Held, &record.Hold{From: phase.Reviewing, Reason: record.HoldEscalated}},
		{"a done and b left",
			[]Walk{standsAt("a", record.TaskDone, record.TaskPlace{Kind: record.PlaceMerge}, ""), standsAt("b", record.TaskLeft, stepAtPlace(2), "")},
			phase.Done, nil},
		{"a and b waiting",
			[]Walk{standsAt("a", record.TaskWaiting, noAct, ""), standsAt("b", record.TaskWaiting, noAct, "")},
			phase.Admitted, nil},
		{"a at a step of the deployment's role and b testing",
			[]Walk{{Stored: true, Task: "a", Line: stepLine, State: record.TaskRunning, Place: stepAtPlace(1)},
				standsAt("b", record.TaskRunning, stepAtPlace(3), "")},
			phase.Step, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hold := issuePhaseOfTasks(tc.tasks)
			if got != tc.phase || !reflect.DeepEqual(hold, tc.hold) {
				t.Fatalf("the issue of %s stands in %s with hold %+v, want %s with hold %+v", tc.name, got, hold, tc.phase, tc.hold)
			}
		})
	}
}

// An issue whose plan holds one task stands where that task stands, as the phase of the task's
// place derived it before a plan held several (issuePhaseOf), for each state of a task and each
// place of the line: the issue stands in that phase (record.StandsAt), through a hold where its
// task is held or stopped, which holds it for the task's hold reason, or task-stopped for a stop.
//
// The fault it catches is an issue of one task that walks another way than before a plan held
// several.
func TestAnIssueOfOneTaskStandsWhereItsTaskStands(t *testing.T) {
	places := []record.TaskPlace{stepAtPlace(1), stepAtPlace(2), stepAtPlace(3), stepAtPlace(4), stepAtPlace(5),
		{Kind: record.PlaceRetro}, {Kind: record.PlaceMerge}}
	type state struct {
		state  record.TaskState
		reason record.HoldReason
		places []record.TaskPlace
	}
	for _, s := range []state{
		{record.TaskWaiting, "", []record.TaskPlace{{}}},
		{record.TaskRunning, "", places},
		{record.TaskHeld, "", places},
		{record.TaskHeld, record.HoldEscalated, places},
		{record.TaskHeld, record.HoldForChildren, []record.TaskPlace{{Kind: record.PlaceRetro}}},
		{record.TaskStopped, "", append([]record.TaskPlace{{}}, places...)},
		{record.TaskDone, "", places},
		{record.TaskLeft, "", places},
	} {
		for _, place := range s.places {
			t.Run(fmt.Sprintf("%s %q at %s %d", s.state, s.reason, place.Kind, place.Position), func(t *testing.T) {
				w := standsAt("a", s.state, place, s.reason)
				want := issuePhaseOf(w, place, s.state)
				got, hold := issuePhaseOfTasks([]Walk{w})
				var wantHold *record.Hold
				switch s.state {
				case record.TaskHeld:
					wantHold = &record.Hold{From: want, Reason: s.reason}
				case record.TaskStopped:
					wantHold = &record.Hold{From: want, Reason: record.HoldTaskStopped}
				}
				if stands := record.StandsAt(record.Issue{Phase: got, Hold: hold}); stands != want || !reflect.DeepEqual(hold, wantHold) {
					t.Fatalf("the issue of task a %s at %+v stands in %s (phase %s, hold %+v), want %s with hold %+v",
						s.state, place, stands, got, hold, want, wantHold)
				}
				if wantHold == nil && got != want {
					t.Fatalf("the issue of task a %s at %+v is in %s, want %s", s.state, place, got, want)
				}
			})
		}
	}
}
