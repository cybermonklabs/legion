package workflow

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The place of a phase's step is read from the steps, never stated: the step of each working
// phase is the step of DefaultSteps whose role works that phase, at one plus its index, under that
// role. The place logic takes the steps as a parameter, so a list without the tester shows that the
// place is read: the reviewer moves up to place 3, and testing runs no step of that list. Nothing
// global is swapped: DefaultSteps answers a fresh copy, so a caller that changes the slice it got
// changes no later answer.
func TestStepOfReadsTheDefaultSteps(t *testing.T) {
	works := map[string]phase.Phase{"planner": phase.Planning, "implementer": phase.Implementing, "tester": phase.Testing,
		"reviewer": phase.Reviewing, "acceptance": phase.Accepting}
	steps := DefaultSteps()
	if want := []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}; !slices.Equal(steps, want) {
		t.Fatalf("DefaultSteps() = %q, want %q", steps, want)
	}
	for i, step := range steps {
		role, _, _ := strings.Cut(step, ":")
		at, ok := works[role]
		if !ok {
			t.Fatalf("the default step %q names a role that works no phase", step)
		}
		place, n, got, _, ok := stepOf(at)
		if want := (record.StepPlace{Task: "default", Kind: "step", Place: i + 1}); !ok || place != want || n != i+1 || got != role {
			t.Errorf("stepOf(%s) = %+v, step %d, role %q, ok %v; want %+v, step %d, role %q", at, place, n, got, ok, want, i+1, role)
		}
	}

	withoutTester := slices.DeleteFunc(DefaultSteps(), func(step string) bool { return step == "tester" })
	if place, n, role, typ, ok := stepIn(withoutTester, phase.Reviewing); !ok || n != 3 || place.Place != 3 || role != "reviewer" || typ != "review" {
		t.Errorf("the reviewing step of %q = %+v, step %d, role %q, type %q, ok %v; want place 3 under the reviewer, of type review",
			withoutTester, place, n, role, typ, ok)
	}
	if place, n, role, _, ok := stepIn(withoutTester, phase.Testing); ok {
		t.Errorf("the testing step of %q = %+v, step %d, role %q; want none, since no step of that list is the tester's", withoutTester, place, n, role)
	}
	// A retro and a merge are no step of any list: the kind tells them apart, at place 0.
	for at, kind := range map[phase.Phase]string{phase.Retro: "retro", phase.Merging: "merge"} {
		if place, n, _, _, ok := stepIn(nil, at); !ok || place != (record.StepPlace{Task: "default", Kind: kind}) || n != 0 {
			t.Errorf("the %s step of no list = %+v, step %d, ok %v; want kind %s at place 0", at, place, n, ok, kind)
		}
	}

	changed := DefaultSteps()
	changed[0] = "architect"
	if again := DefaultSteps(); again[0] != "planner" {
		t.Fatalf("DefaultSteps() after a caller changed its answer = %q, want a fresh copy starting with the planner", again)
	}
}

// planWrites is every plan record row issue's outbox holds, in the order they were written.
func planWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.PlanRecord {
	t.Helper()
	var got []record.PlanRecord
	for _, row := range issueOutboxOf(t, pool, issue, string(record.OutboxKindPlanRecord)) {
		var plan record.PlanRecord
		if err := json.Unmarshal(row.payload, &plan); err != nil {
			t.Fatalf("decode a plan record row of %s: %v", issue, err)
		}
		got = append(got, plan)
	}
	return got
}

// The work of an issue starts at its move out of admitted, and that move writes the plan the run
// carries out: one plan record of the issue, with the source default and one task, default, whose
// steps are DefaultSteps. Its id is a version 5 uuid of the issue and its run, in the plan records'
// own namespace, so a write sent again lands on the same plan and the next run gets a plan of its
// own. No later move of the run writes another.
func TestTheWorkStartsWithTheDefaultPlan(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Admitted)
	engine := stepEngine(nil)
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect})

	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-plan-record"))
	want := record.PlanRecord{ID: uuid.NewSHA1(namespace, []byte("LEGION-208/1")).String(), Generation: 1, Source: "default",
		Tasks: []record.PlanRecordTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}
	if plans := planWrites(t, pool, root.Key); len(plans) != 1 || !reflect.DeepEqual(plans[0], want) {
		t.Fatalf("the move out of admitted wrote the plans %+v, want the one %+v", plans, want)
	}
	if other := PlanID(root.Key, 2); other == want.ID {
		t.Fatalf("the plan of run 2 has the id %s of run 1's", other)
	}

	move(t, pool, engine, root.Key, TriggerPlannerCompleted, "", record.PhaseRow{})
	if plans := planWrites(t, pool, root.Key); len(plans) != 1 {
		t.Fatalf("the move out of planning left %d plan records, want the one the start wrote", len(plans))
	}
}

// The agent that does the retro rates the plan on the completion that ends the retro, and the
// rating is written onto the record of that retro's run: a step record row of its own, whose id is
// the id of the retro's end, carrying the rating, its reason and every identity field of the run,
// so it makes the record where the retro's start and end have not landed yet, and no moment and no
// result, which are the end's. A retro from a plugin of the release before the rating completes
// with none, and the daemon takes it and writes no rating.
func TestTheRetrosRatingReachesItsRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rating int
		reason string
	}{
		{name: "a retro that rates the plan", rating: 2, reason: "The plan put the tests after the review."},
		{name: "a retro of a plugin before the rating", rating: 0, reason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := stepRoot(t, pool, phase.Accepting)
			seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := stepEngine(nil)
			move(t, pool, engine, root.Key, TriggerSignOff, "", record.PhaseRow{})
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer,
				Claim: "implement-claim", Summary: "retro done", Commit: "head", PlanRating: tc.rating, PlanReason: tc.reason})
			assertPhase(t, pool, phase.Merging)

			var end *record.StepRecord
			var rated []record.StepRecord
			for _, write := range stepWrites(t, pool, root.Key) {
				if write.Kind == "retro" && isEnd(write) {
					end = &write
				}
				if write.Rating != nil || write.RatingReason != nil {
					rated = append(rated, write)
				}
			}
			if end == nil {
				t.Fatalf("the retro's completion wrote no end of the retro: %+v", stepWrites(t, pool, root.Key))
			}
			if tc.rating == 0 {
				if len(rated) != 0 {
					t.Fatalf("a retro with no rating wrote the ratings %+v, want none", rated)
				}
				return
			}
			if len(rated) != 1 {
				t.Fatalf("the retro's completion wrote %d ratings (%+v), want one", len(rated), rated)
			}
			rating := rated[0]
			want := record.StepRecord{ID: end.ID, Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
				Agent: end.Agent, Generation: 1, Rating: new(tc.rating), RatingReason: new(tc.reason)}
			if !reflect.DeepEqual(rating, want) {
				t.Fatalf("the rating = %+v (rating %v, reason %v), want %+v on the retro's record %s", rating, deref(rating.Rating),
					deref(rating.RatingReason), want, end.ID)
			}
			if wantID := StepID(root.Key, 1, "default", "retro", 0, 1); rating.ID != wantID {
				t.Fatalf("the rating's id = %s, want %s, the first run of the retro", rating.ID, wantID)
			}
			if rating.Agent != token(t, root.Key, claim.RoleImplementer) {
				t.Fatalf("the rating names the agent %q, want the implementer's claim, which started the retro", rating.Agent)
			}
		})
	}
}

// deref is a pointer's value, or "<absent>" for nil, so a failure prints what a field says.
func deref[T any](value *T) any {
	if value == nil {
		return "<absent>"
	}
	return *value
}
