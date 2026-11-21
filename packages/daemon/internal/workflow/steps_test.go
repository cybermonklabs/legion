package workflow

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Today's phases are the steps of one task, default: the five phases that run work are its steps
// 1 to 5, and the retro and the merge are the two kinds no plan names, at place 0. A phase that
// runs no step (admitted, held, done, awaiting a person) has none.
func TestStepOfIsTodaysSequence(t *testing.T) {
	type step struct {
		place record.StepPlace
		role  stepRole
		typ   stepType
	}
	want := map[phase.Phase]step{
		phase.Planning:     {record.StepPlace{Task: "default", Kind: "step", Place: 1}, "planner", "planning"},
		phase.Implementing: {record.StepPlace{Task: "default", Kind: "step", Place: 2}, "implementer", "implementation"},
		phase.Testing:      {record.StepPlace{Task: "default", Kind: "step", Place: 3}, "tester", "testing"},
		phase.Reviewing:    {record.StepPlace{Task: "default", Kind: "step", Place: 4}, "reviewer", "review"},
		phase.Accepting:    {record.StepPlace{Task: "default", Kind: "step", Place: 5}, "acceptance", "review"},
		phase.Retro:        {record.StepPlace{Task: "default", Kind: "retro", Place: 0}, "implementer", ""},
		phase.Merging:      {record.StepPlace{Task: "default", Kind: "merge", Place: 0}, "merger", ""},
	}
	steps := 0
	for _, p := range phase.Order {
		place, role, typ, ok := stepOf(p)
		w, stepPhase := want[p]
		if ok != stepPhase {
			t.Fatalf("stepOf(%s) ok = %v, want %v", p, ok, stepPhase)
		}
		if !ok {
			continue
		}
		steps++
		if got := (step{place, role, typ}); got != w {
			t.Errorf("stepOf(%s) = %+v, want %+v", p, got, w)
		}
		// The settle names the phase of a run the store holds open by its place (phaseAt).
		if back, found := phaseAt(place); !found || back != p {
			t.Errorf("phaseAt(%+v) = %s, %v, want %s, the phase whose step runs there", place, back, found, p)
		}
	}
	for _, p := range []phase.Phase{phase.Admitted, phase.Held, phase.Done, phase.AwaitingPerson} {
		if _, _, _, ok := stepOf(p); ok {
			t.Errorf("stepOf(%s) is a step; that phase runs none", p)
		}
	}
	if steps != len(want) {
		t.Fatalf("phase.Order holds %d phases that run a step, and the want table %d: a phase of the table is no phase of the line", steps, len(want))
	}
}

// A move back that a role or a report asked for ends the step failed and names the step the work
// returned to; every other move ends it passed and returns nowhere.
func TestEndOfReadsTheTrigger(t *testing.T) {
	for _, tc := range []struct {
		trigger TriggerKind
		result  StepResult
		returns bool
	}{
		{TriggerTesterFailed, "failed", true},
		{TriggerReviewRejected, "failed", true},
		{TriggerBackward, "failed", true},
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

// The end of a move is the end of the step the issue stood in: a review a request for changes
// sends back from reviewing ends failed, returns the work to implementing's place and names the
// reviewer's report. No held issue is a case here, since none reaches a move's end: no row of the
// table leaves held, so transition returns before it names an end, and the moves out of a hold
// clear it first (retryPhase, closeMerged).
func TestTheEndOfAMoveReadsWhereTheIssueStands(t *testing.T) {
	review := record.PhaseRow{Role: claim.RoleReviewer, HandoffRecord: "6f1d2c3b-review"}
	end := endOfTransition(TriggerReviewRejected, phase.Reviewing, phase.Implementing, review)
	if end.Result != ResultFailed || end.ReturnedTo == nil || *end.ReturnedTo != 2 || end.Report != review.HandoffRecord {
		t.Errorf("the end of the review = %+v (returned to %v), want failed, returned to place 2, naming report %s",
			end, end.ReturnedTo, review.HandoffRecord)
	}
}

// The id of a record is a version 5 uuid of everything that tells one run of one step from every
// other: the issue, its run, the task, the kind, the place and the count of starts of that place.
// The start and the end of one run compute it apart, so it depends on nothing else.
func TestAStepIDIsStableAndDistinct(t *testing.T) {
	first := StepID("LEG-1", 1, record.StepPlace{Task: "default", Kind: "step", Place: 2}, 1)
	if again := StepID("LEG-1", 1, record.StepPlace{Task: "default", Kind: "step", Place: 2}, 1); again != first {
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
	// The generation, the place and the count are three different numbers here, so no two of the
	// name's numbers can trade places unseen.
	if got, want := StepID("LEG-1", 7, record.StepPlace{Task: "default", Kind: "step", Place: 3}, 4),
		uuid.NewSHA1(namespace, []byte("LEG-1/7/default/step/3/4")).String(); got != want {
		t.Fatalf("StepID of generation 7, place 3, count 4 = %s, want %s: the name is <issue>/<generation>/<task>/<kind>/<step>/<starts>", got, want)
	}
	seen := map[string]string{first: "the first"}
	for name, id := range map[string]string{
		"issue":      StepID("LEG-2", 1, record.StepPlace{Task: "default", Kind: "step", Place: 2}, 1),
		"generation": StepID("LEG-1", 2, record.StepPlace{Task: "default", Kind: "step", Place: 2}, 1),
		"task":       StepID("LEG-1", 1, record.StepPlace{Task: "a", Kind: "step", Place: 2}, 1),
		"kind":       StepID("LEG-1", 1, record.StepPlace{Task: "default", Kind: "retro", Place: 2}, 1),
		"step":       StepID("LEG-1", 1, record.StepPlace{Task: "default", Kind: "step", Place: 3}, 1),
		"starts":     StepID("LEG-1", 1, record.StepPlace{Task: "default", Kind: "step", Place: 2}, 2),
	} {
		if other, ok := seen[id]; ok {
			t.Errorf("another %s gives the id of %s: %s", name, other, id)
		}
		seen[id] = name
	}
}
