package workflow

import (
	"testing"

	"github.com/google/uuid"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Today's phases are the steps of one task, default: the five phases that run work are its steps
// 1 to 5, and the retro and the merge are the two kinds no plan names, at place 0. A phase that
// runs no step (admitted, held, done, awaiting a person) has none.
func TestStepOfIsTodaysSequence(t *testing.T) {
	type step struct {
		place record.StepPlace
		step  int
		role  string
		typ   string
	}
	want := map[phase.Phase]step{
		phase.Planning:     {record.StepPlace{Task: "default", Kind: "step", Place: 1}, 1, "planner", "planning"},
		phase.Implementing: {record.StepPlace{Task: "default", Kind: "step", Place: 2}, 2, "implementer", "implementation"},
		phase.Testing:      {record.StepPlace{Task: "default", Kind: "step", Place: 3}, 3, "tester", "testing"},
		phase.Reviewing:    {record.StepPlace{Task: "default", Kind: "step", Place: 4}, 4, "reviewer", "review"},
		phase.Accepting:    {record.StepPlace{Task: "default", Kind: "step", Place: 5}, 5, "acceptance", "review"},
		phase.Retro:        {record.StepPlace{Task: "default", Kind: "retro", Place: 0}, 0, "implementer", ""},
		phase.Merging:      {record.StepPlace{Task: "default", Kind: "merge", Place: 0}, 0, "merger", ""},
	}
	for _, p := range phase.Order {
		place, n, role, typ, ok := stepOf(p)
		w, stepPhase := want[p]
		if ok != stepPhase {
			t.Fatalf("stepOf(%s) ok = %v, want %v", p, ok, stepPhase)
		}
		if got := (step{place, n, role, typ}); ok && got != w {
			t.Errorf("stepOf(%s) = %+v, want %+v", p, got, w)
		}
	}
	for _, p := range []phase.Phase{phase.Admitted, phase.Held, phase.Done, phase.AwaitingPerson} {
		if _, _, _, _, ok := stepOf(p); ok {
			t.Errorf("stepOf(%s) is a step; that phase runs none", p)
		}
	}
	if len(want) != 7 {
		t.Fatalf("the want table holds %d steps; phase.Order's seven run them", len(want))
	}
}

// A move back that a role or a report asked for ends the step failed, a move back nobody asked for
// (a head that conflicts with its base) ends it stopped, and both name the step the work returned
// to; every other move ends it passed and returns nowhere.
func TestEndOfReadsTheTrigger(t *testing.T) {
	for _, tc := range []struct {
		trigger TriggerKind
		result  string
		returns bool
	}{
		{TriggerTesterFailed, "failed", true},
		{TriggerReviewRejected, "failed", true},
		{TriggerBackward, "failed", true},
		{TriggerConflict, "stopped", true},
		{TriggerGateOpened, "passed", false},
		{TriggerPlannerCompleted, "passed", false},
		{TriggerImplementationReady, "passed", false},
		{TriggerTesterPassed, "passed", false},
		{TriggerReviewApproved, "passed", false},
		{TriggerSignOff, "passed", false},
		{TriggerRetroCompleted, "passed", false},
		{TriggerPullRequestMerged, "passed", false},
	} {
		if result, returns := endOf(tc.trigger); result != tc.result || returns != tc.returns {
			t.Errorf("endOf(%s) = %q, %v; want %q, %v", tc.trigger, result, returns, tc.result, tc.returns)
		}
	}
}

// The id of a record is a version 5 uuid of everything that tells one run of one step from every
// other: the issue, its run, the task, the kind, the place and the count of starts of that place.
// The start and the end of one run compute it apart, so it depends on nothing else.
func TestAStepIDIsStableAndDistinct(t *testing.T) {
	first := StepID("LEG-1", 1, "default", "step", 2, 1)
	if again := StepID("LEG-1", 1, "default", "step", 2, 1); again != first {
		t.Fatalf("StepID twice = %s then %s; one run of one step has one id", first, again)
	}
	parsed, err := uuid.Parse(first)
	if err != nil || parsed.Version() != 5 {
		t.Fatalf("StepID = %s (%v); want a version 5 uuid", first, err)
	}
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-step-record"))
	if want := uuid.NewSHA1(namespace, []byte("LEG-1/1/default/step/2/1")).String(); first != want {
		t.Fatalf("StepID = %s, want %s: the name is <issue>/<generation>/<task>/<kind>/<step>/<starts>", first, want)
	}
	seen := map[string]string{first: "the first"}
	for name, id := range map[string]string{
		"issue":      StepID("LEG-2", 1, "default", "step", 2, 1),
		"generation": StepID("LEG-1", 2, "default", "step", 2, 1),
		"task":       StepID("LEG-1", 1, "a", "step", 2, 1),
		"kind":       StepID("LEG-1", 1, "default", "retro", 2, 1),
		"step":       StepID("LEG-1", 1, "default", "step", 3, 1),
		"starts":     StepID("LEG-1", 1, "default", "step", 2, 2),
	} {
		if other, ok := seen[id]; ok {
			t.Errorf("another %s gives the id of %s: %s", name, other, id)
		}
		seen[id] = name
	}
}
