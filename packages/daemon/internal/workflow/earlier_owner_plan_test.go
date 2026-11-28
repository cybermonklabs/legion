package workflow

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner of an issue changes where Dispatch moves it out from under the issue whose architect
// gave its plan, an orphan that is admitted again as a root of its own among them, and a plan that
// an earlier owner gave is then passed over at the issue's first step (StartFirstPhase's
// gateOpened), never in silence: one log line for each version passed over, naming the issue, the
// generation, the version, its document, its giver and the plan the first step runs; one
// plan-not-taken notice to the issue's architect that names the same; and no row of the old plan
// left in the three plan tables, so its walk never reads it, also where it stood at the issue's
// own generation: the first step stores the project's default steps in their place. The owner is
// any architect that acts on the issue on its Dispatch chain now (record.PlanOwners): the plan of
// the architect of an issue above it is the owner's, and so is the root architect's plan for its
// child (the control); each is carried to the issue's generation and its first step starts, and
// nobody is told or logged. The twin: an issue given no plan starts planning on the default steps,
// with no notice and no log line.
func TestAPlanOfAnEarlierOwnerIsNotTaken(t *testing.T) {
	t.Parallel()
	const notTaken = "workflow: a plan of an earlier owner is not taken"
	earlier, owner := architectOf(t, "LEGION-208"), claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	give := func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64, version int, by claim.Token, words ...string) {
		t.Helper()
		fact := ownersPlan(t, key, generation, version, givenTask(t, "Build the widget.", words...))
		fact.GivenBy = by
		mustTake(t, fmt.Sprintf("version %d of %s at generation %d by %s", version, key, generation, by), applyFacts(t, pool, engine)(planID(key, generation, version), fact))
	}
	// passedOver fails unless the first step logged the one line of a version passed over and told
	// the one plan-not-taken notice whose reason is reason.
	passedOver := func(t *testing.T, pool *pgxpool.Pool, logged string, line, reason string) {
		t.Helper()
		if strings.Count(logged, notTaken) != 1 || !strings.Contains(logged, line) {
			t.Fatalf("the log =\n%s\nwant the one line %s", logged, line)
		}
		want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: reason}}
		if got := noticesOf(t, pool, "LEGION-209", "plan-not-taken"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan-not-taken notices = %+v, want %+v", got, want)
		}
	}
	// kept fails unless the first step told and logged nothing of a plan not taken, and key holds
	// rows after it.
	kept := func(t *testing.T, pool *pgxpool.Pool, logged, key string, rows [3]int) {
		t.Helper()
		if strings.Contains(logged, notTaken) {
			t.Fatalf("the log =\n%s\nwant no plan of an earlier owner", logged)
		}
		if got := noticesOf(t, pool, key, "plan-not-taken"); len(got) != 0 {
			t.Fatalf("the plan-not-taken notices = %+v, want none", got)
		}
		if got := planRowsOf(t, pool, key); got != rows {
			t.Fatalf("the plan rows of %s after its first step = %v, want %v", key, got, rows)
		}
	}

	t.Run("the plan of the architect of an issue the issue left on its chain is not taken: the first step runs the default steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		give(t, pool, engine, "LEGION-209", 0, 1, earlier, "implementer", "reviewer")
		recordedAt(t, pool, "LEGION-209", "LEGION-209")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		if got := issueOf(t, pool, engine, "LEGION-209").Phase; got != phase.Planning {
			t.Fatalf("LEGION-209 after its first step stands in %s, want planning, the first of the default steps", got)
		}
		passedOver(t, pool, logged.String(),
			fmt.Sprintf(`msg="%s" issue=LEGION-209 generation=0 version=1 document=%s giver=%s runs="default steps"`, notTaken, planArtifact, earlier),
			"The first step of LEGION-209 took no plan of an earlier owner: version 1 of document "+planArtifact+", given by "+string(earlier)+
				". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own.")
		if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{1, 1, 5} {
			t.Fatalf("the plan rows of LEGION-209 after its first step = %v, want none of the old plan: the default steps alone, one version, one task, five steps", got)
		}
	})

	t.Run("the earlier owner's plan at the issue's own generation is not walked either", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		// LEGION-208 is recorded too: who acts on LEGION-209 is read from its Dispatch chain.
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		give(t, pool, engine, "LEGION-209", 1, 1, earlier, "implementer", "reviewer")
		recordedAt(t, pool, "LEGION-209", "LEGION-209")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		if got := issueOf(t, pool, engine, "LEGION-209").Phase; got != phase.Planning {
			t.Fatalf("LEGION-209 after its first step stands in %s, want planning, the first of the default steps", got)
		}
		passedOver(t, pool, logged.String(),
			fmt.Sprintf(`msg="%s" issue=LEGION-209 generation=1 version=1 document=%s giver=%s runs="default steps"`, notTaken, planArtifact, earlier),
			"The first step of LEGION-209 took no plan of an earlier owner: version 1 of document "+planArtifact+", given by "+string(earlier)+
				". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own.")
		if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{1, 1, 5} {
			t.Fatalf("the plan rows of LEGION-209 after its first step = %v, want none of the old plan: the default steps alone, one version, one task, five steps", got)
		}
	})

	t.Run("the new owner's plan given before the first step runs, and the log line and the notice name it", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		give(t, pool, engine, "LEGION-209", 0, 1, earlier, "implementer", "reviewer")
		recordedAt(t, pool, "LEGION-209", "LEGION-209")
		give(t, pool, engine, "LEGION-209", 1, 2, owner, "implementer", "tester")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		assertWalksAt(t, pool, engine, "LEGION-209", phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		runs := "version 2 of document " + planArtifact + ", given by " + string(owner)
		passedOver(t, pool, logged.String(),
			fmt.Sprintf(`msg="%s" issue=LEGION-209 generation=0 version=1 document=%s giver=%s runs="%s"`, notTaken, planArtifact, earlier, runs),
			"The first step of LEGION-209 took no plan of an earlier owner: version 1 of document "+planArtifact+", given by "+string(earlier)+
				". It runs "+runs+". To run a plan it did not take, give it again with register_plan as your own.")
		if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{1, 1, 2} {
			t.Fatalf("the plan rows of LEGION-209 after its first step = %v, want the new owner's alone: one version, one task, two steps", got)
		}
	})

	t.Run("the plan of the architect of an issue above it on its chain is the owner's: carried, and nobody told", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		give(t, pool, engine, "LEGION-210", 0, 1, owner, "implementer", "reviewer")
		recordedAt(t, pool, "LEGION-210", "LEGION-208")
		reparentTo(t, pool, engine, "LEGION-210", "LEGION-209")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-210")

		assertWalksAt(t, pool, engine, "LEGION-210", phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		kept(t, pool, logged.String(), "LEGION-210", [3]int{2, 2, 4})
	})

	t.Run("control: the root architect's plan for its child is the owner's: carried, and nobody told", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		give(t, pool, engine, "LEGION-209", 0, 1, earlier, "implementer", "reviewer")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		assertWalksAt(t, pool, engine, "LEGION-209", phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		kept(t, pool, logged.String(), "LEGION-209", [3]int{2, 2, 4})
	})

	t.Run("the twin: an issue given no plan starts planning on the default steps, with no notice and no log line", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		recordedAt(t, pool, "LEGION-209", "LEGION-209")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		if got := issueOf(t, pool, engine, "LEGION-209").Phase; got != phase.Planning {
			t.Fatalf("LEGION-209 given no plan after its first step stands in %s, want planning, the first of the default steps", got)
		}
		kept(t, pool, logged.String(), "LEGION-209", [3]int{1, 1, 5})
	})
}

// A rerun carries the owner's plan to its new generation, with the time it was given (record.Store's
// CarryPlan), so an issue that ran three generations under one owner holds a copy of that owner's
// version at each. When Dispatch then moves the issue out from under the owner's issue, its first
// step passes over every copy and names the version once in its one notice, and it logs each copy
// it deleted, with its generation. The fault caught is a notice that names the version once for
// each copy.
func TestAnEarlierOwnersPlanCarriedToSeveralGenerationsIsNamedOnce(t *testing.T) {
	t.Parallel()
	const notTaken = "workflow: a plan of an earlier owner is not taken"
	earlier := architectOf(t, "LEGION-208")
	pool := migratedPool(t)
	engine, logged := loggedEngine()
	// LEGION-208 is recorded first: who acts on LEGION-209 is read from its Dispatch chain.
	recordedAt(t, pool, "LEGION-208", "LEGION-208")
	recordedAt(t, pool, "LEGION-209", "LEGION-208")
	fact := ownersPlan(t, "LEGION-209", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
	fact.GivenBy = earlier
	mustTake(t, "version 1 of LEGION-209 at generation 1", applyFacts(t, pool, engine)(planID("LEGION-209", 1, 1), fact))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if err := record.NewStore().CarryPlan(t.Context(), tx, "LEGION-209", 1, 2, false); err != nil {
			return err
		}
		return record.NewStore().CarryPlan(t.Context(), tx, "LEGION-209", 2, 3, false)
	})
	recordedAt(t, pool, "LEGION-209", "LEGION-209")
	execSQL(t, pool, `update issues set generation = 4 where key = $1`, "LEGION-209")
	logged.Reset()

	firstStep(t, pool, engine, "LEGION-209")

	want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of LEGION-209 took no plan of an earlier owner: version 1 of document " +
		planArtifact + ", given by " + string(earlier) + ". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own."}}
	if got := noticesOf(t, pool, "LEGION-209", "plan-not-taken"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan-not-taken notices = %+v, want %+v, the version named once", got, want)
	}
	for _, generation := range []int{1, 2, 3} {
		if line := fmt.Sprintf(`msg="%s" issue=LEGION-209 generation=%d version=1`, notTaken, generation); !strings.Contains(logged.String(), line) {
			t.Fatalf("the log =\n%s\nwant the line of the copy at generation %d: %s", logged.String(), generation, line)
		}
	}
	if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{1, 1, 5} {
		t.Fatalf("the plan rows of LEGION-209 after its first step = %v, want the default steps alone: every copy of the earlier owner's plan deleted", got)
	}
}

// A give of the version in force by another owner adds that owner to the copy of its own
// generation alone (record.Store's AddPlanGiver), so the copies of one version that a first step
// passes over can name different givers: the architect of LEGION-210, the issue LEGION-209 stands
// under on its Dispatch chain, gives version 1 at generation 1, a rerun carries it to generation 2,
// the root's architect gives it again there, and Dispatch then moves the issue out from under
// both. Its one notice names the version once, with the givers of both copies together, the giver
// the store added last first. The fault caught is a notice that tells the copies apart by their
// givers and names the version once for each.
func TestAVersionWhoseCopiesNameDifferentGiversIsNamedOnceWithEveryGiver(t *testing.T) {
	t.Parallel()
	sub, root := claim.Token(treeRoleToken(t, "LEGION-210", claim.RoleArchitect)), architectOf(t, "LEGION-208")
	pool := migratedPool(t)
	engine := returnEngine()
	// LEGION-209 stands under LEGION-210, under LEGION-208, on its Dispatch chain: the architect of
	// each acts on it.
	recordedAt(t, pool, "LEGION-208", "LEGION-208")
	recordedAt(t, pool, "LEGION-210", "LEGION-208")
	recordedAt(t, pool, "LEGION-209", "LEGION-210")
	first := ownersPlan(t, "LEGION-209", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
	first.GivenBy = sub
	mustTake(t, "version 1 of LEGION-209 at generation 1 by LEGION-210's architect", applyFacts(t, pool, engine)(planID("LEGION-209", 1, 1), first))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().CarryPlan(t.Context(), tx, "LEGION-209", 1, 2, false)
	})
	execSQL(t, pool, `update issues set generation = 2 where key = $1`, "LEGION-209")
	again := ownersPlan(t, "LEGION-209", 2, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
	again.GivenBy = root
	mustTake(t, "version 1 of LEGION-209 at generation 2 by the root's architect", applyFacts(t, pool, engine)(planID("LEGION-209", 2, 1), again))
	recordedAt(t, pool, "LEGION-209", "LEGION-209")
	execSQL(t, pool, `update issues set generation = 3 where key = $1`, "LEGION-209")

	firstStep(t, pool, engine, "LEGION-209")

	want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of LEGION-209 took no plan of an earlier owner: version 1 of document " +
		planArtifact + ", given by " + string(root) + " and " + string(sub) +
		". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own."}}
	if got := noticesOf(t, pool, "LEGION-209", "plan-not-taken"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan-not-taken notices = %+v, want %+v, the version named once with the givers of both copies", got, want)
	}
}

// Two versions an earlier owner gave are each named, in the order they were given: the owner gives
// version 1 at generation 1, a rerun carries it to generation 2, the owner gives version 2 there,
// and Dispatch then moves the issue out from under the owner's issue. Its first step passes over
// the version in force of each generation, version 1 and version 2, and its one notice names both.
// The fault caught is a notice that keeps the first version alone, which would hide the later one
// from the new owner.
func TestTwoVersionsOfAnEarlierOwnerAreBothNamed(t *testing.T) {
	t.Parallel()
	earlier := architectOf(t, "LEGION-208")
	pool := migratedPool(t)
	engine := returnEngine()
	// LEGION-208 is recorded first: who acts on LEGION-209 is read from its Dispatch chain.
	recordedAt(t, pool, "LEGION-208", "LEGION-208")
	recordedAt(t, pool, "LEGION-209", "LEGION-208")
	first := ownersPlan(t, "LEGION-209", 1, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
	first.GivenBy = earlier
	mustTake(t, "version 1 of LEGION-209 at generation 1", applyFacts(t, pool, engine)(planID("LEGION-209", 1, 1), first))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().CarryPlan(t.Context(), tx, "LEGION-209", 1, 2, false)
	})
	execSQL(t, pool, `update issues set generation = 2 where key = $1`, "LEGION-209")
	second := ownersPlan(t, "LEGION-209", 2, 2, givenTask(t, "Build the widget.", "implementer", "tester"))
	second.GivenBy = earlier
	mustTake(t, "version 2 of LEGION-209 at generation 2", applyFacts(t, pool, engine)(planID("LEGION-209", 2, 2), second))
	recordedAt(t, pool, "LEGION-209", "LEGION-209")
	execSQL(t, pool, `update issues set generation = 3 where key = $1`, "LEGION-209")

	firstStep(t, pool, engine, "LEGION-209")

	want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of LEGION-209 took no plan of an earlier owner: version 1 of document " +
		planArtifact + ", given by " + string(earlier) + "; version 2 of document " + planArtifact + ", given by " + string(earlier) +
		". It runs the default steps. To run a plan it did not take, give it again with register_plan as your own."}}
	if got := noticesOf(t, pool, "LEGION-209", "plan-not-taken"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan-not-taken notices = %+v, want %+v, both versions named in the order given", got, want)
	}
	if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{1, 1, 5} {
		t.Fatalf("the plan rows of LEGION-209 after its first step = %v, want the default steps alone: both versions of the earlier owner deleted", got)
	}
}
