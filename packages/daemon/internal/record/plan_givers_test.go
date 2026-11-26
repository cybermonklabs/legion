package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// readVersion is the version in force of the plan of issue at generation.
func readVersion(t *testing.T, tx pgx.Tx, issue string, generation uint64) PlanVersion {
	t.Helper()
	plan := readPlan(t, tx, issue, generation)
	if plan == nil {
		t.Fatalf("%s holds no plan at generation %d", issue, generation)
	}
	return plan.Version
}

// addGiver adds giver, at at, to version of the plan of issue at generation (AddPlanGiver), and
// fails unless the add answers changed: whether it changed the version's row.
func addGiver(t *testing.T, tx pgx.Tx, issue string, generation uint64, version int, giver claim.Token, at time.Time, changed bool) {
	t.Helper()
	got, err := NewStore().AddPlanGiver(context.Background(), tx, issue, generation, version, giver, at)
	must(t, err)
	if got != changed {
		t.Fatalf("AddPlanGiver(%s) of version %d of %s at generation %d changed its row: %t, want %t", giver, version, issue, generation, got, changed)
	}
}

// The givers of a version are a set. AddPlanGiver adds an owner that gives the version and is not
// yet one of its givers: the version names it in given_by, the giver the store added last, at the
// time of that give, and names the giver before it among its earlier givers, so a release before
// this column, which reads given_by alone, reads the new giver. A giver the version names already
// changes nothing, whether given_by names it or the earlier givers do, and AddPlanGiver answers
// so. A carry copies every giver with the version (CarryPlan).
//
// The faults it catches: an add that loses a giver (its control: AddPlanGiver writing given_by
// alone), an add of a giver the version names already (its control: AddPlanGiver without its
// guard, under which the earlier giver's give again makes it the last giver and names the other
// twice), and a carry that drops the earlier givers (its control: CarryPlan copying given_by
// alone).
func TestTheGiversOfAPlanVersionAreASet(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v, tasks, steps := ownersPlan(planIssue, 1, 1, planOwner, planGivenAt)
	later := planGivenAt.Add(time.Hour)
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		addGiver(t, tx, planIssue, 1, 1, otherOwner, later, true)
	})
	want := v
	want.GivenBy, want.GivenAt, want.EarlierGivers = otherOwner, later, []claim.Token{planOwner}
	inTx(t, st, func(tx pgx.Tx) {
		if got := readVersion(t, tx, planIssue, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("version 1 after another owner's give = %+v, want %+v", got, want)
		}
		var givenBy string
		must(t, tx.QueryRow(ctx, `select given_by from issue_plans where issue = $1 and generation = 1 and version = 1`, planIssue).Scan(&givenBy))
		if givenBy != string(otherOwner) {
			t.Fatalf("given_by = %s, want the last giver %s, which a release before reads", givenBy, otherOwner)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		addGiver(t, tx, planIssue, 1, 1, planOwner, later.Add(time.Hour), false)
		addGiver(t, tx, planIssue, 1, 1, otherOwner, later.Add(time.Hour), false)
	})
	inTx(t, st, func(tx pgx.Tx) {
		if got := readVersion(t, tx, planIssue, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("version 1 after each giver gave it again = %+v, want %+v unchanged", got, want)
		}
	})

	inTx(t, st, func(tx pgx.Tx) { must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 2, false)) })
	carried := want
	carried.Generation = 2
	inTx(t, st, func(tx pgx.Tx) {
		if got := readVersion(t, tx, planIssue, 2); !reflect.DeepEqual(got, carried) {
			t.Fatalf("version 1 carried to generation 2 = %+v, want %+v, every giver with it", got, carried)
		}
	})
}

// A version is its owners' while one of its givers acts on the issue on its Dispatch chain now
// (GivenByOwner), the rule each reader of the plans of an issue's owners holds: a version the
// architect of an issue above it on the chain gave last, which the issue's own architect gave
// before, is the issue's own architect's still once Dispatch moves the issue from under that issue,
// so it is no earlier owner's plan (PlansOfEarlierOwners), and the owners' newest plan
// (OwnersNewestPlan) reads it. That reader orders the versions by the time each was last given:
// version 3 at generation 0, given first, is the newest once another owner gives it again after
// version 5 was given at generation 1.
//
// The faults it catches: a version judged by its last giver alone (its control: GivenByOwner
// reading GivenBy alone, under which the issue's own version reads as an earlier owner's), and an
// add that leaves the time of the version's first give (its control: AddPlanGiver writing no
// given_at, under which the owners' newest plan is version 5 of generation 1).
func TestAVersionIsItsOwnersWhileOneOfItsGiversOwnsTheIssue(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	first, firstTasks, firstSteps := ownersPlan(planIssue, 0, 3, planOwner, planGivenAt)
	second, secondTasks, secondSteps := ownersPlan(planIssue, 1, 5, planOwner, planGivenAt.Add(time.Hour))
	inTx(t, st, func(tx pgx.Tx) {
		chainRow(t, tx, "LEGION-300", "", 1)
		chainRow(t, tx, planIssue, "LEGION-300", 1)
		putPlan(t, tx, first, firstTasks, firstSteps)
		putPlan(t, tx, second, secondTasks, secondSteps)
		// LEGION-300's architect, which acts on LEGION-208 from above it on its chain, gives version 3
		// of generation 0 again.
		addGiver(t, tx, planIssue, 0, 3, otherOwner, planGivenAt.Add(2*time.Hour), true)
	})
	// Dispatch then moves LEGION-208 from under LEGION-300: its architect owns nothing of LEGION-208.
	inTx(t, st, func(tx pgx.Tx) { chainRow(t, tx, planIssue, "", 2) })

	inTx(t, st, func(tx pgx.Tx) {
		earlier, err := PlansOfEarlierOwners(ctx, NewStore(), tx, issue)
		must(t, err)
		if len(earlier) != 0 {
			t.Errorf("the plans of earlier owners = %+v, want none: the root's architect gave each", earlier)
		}
		newest, err := OwnersNewestPlan(ctx, NewStore(), tx, issue)
		must(t, err)
		if newest == nil || newest.Version.Generation != 0 || newest.Version.Version != 3 {
			t.Errorf("the owners' newest plan = %+v, want version 3 of generation 0, given last after version 5", newest)
		}
	})
}
