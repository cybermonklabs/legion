package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// drawnInto records key as drawn into variant of experiment, an experiment of unit issue whose
// arm says how its issues are done (plan), as the draw at the issue's first record writes it
// (experiment.DrawAndRecord).
func drawnInto(t *testing.T, pool *pgxpool.Pool, key, experiment, variant, plan string) {
	t.Helper()
	at := time.Date(2026, 11, 17, 19, 7, 25, 0, time.UTC)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().PutAssignments(t.Context(), tx, key, []record.Assignment{
			{Experiment: experiment, Unit: "issue", Variant: variant, Mode: "random", DrawnAt: &at, Plan: plan}})
		return err
	})
}

// inPlanningB is how a notice names the group of key in the variant b of the experiment planning,
// whose issues run the project's default steps.
func inPlanningB(key string) string {
	return key + " is in the variant b of the experiment planning, whose issues run the project's default steps."
}

// The first step of an issue whose group runs the project's default steps takes the default steps
// and no plan of its owner: a plan given at generation 0, before the daemon recorded the child and
// drew it, is deleted rather than carried, and the child's architect is told in one plan-not-taken
// notice that names the plan and the group. The same child drawn into the arm whose owner gives
// the plan carries that plan and is told nothing. A child of the group whose owner gave no plan
// runs the default steps and is told nothing either.
func TestAPlanGivenBeforeTheDrawIsPassedOver(t *testing.T) {
	t.Parallel()
	const child = "LEGION-209"
	given := func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
		t.Helper()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		mustTake(t, "version 1 at generation 0", applyFacts(t, pool, engine)(planID(child, 0, 1),
			ownersPlan(t, child, 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		recordedAt(t, pool, child, "LEGION-208")
	}

	t.Run("drawn into the default steps, the plan is passed over and told", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		given(t, pool, engine)
		drawnInto(t, pool, child, "planning", "b", "default")

		firstStep(t, pool, engine, child)

		assertDefaultPlan(t, planStandingOf(t, pool, engine, child, 1).plan, child, 1, todaysStoredSteps)
		want := walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, todaysStoredSteps}
		if got := walkStandingOf(t, pool, engine, child, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v", child, got, want)
		}
		if got := planRowsOf(t, pool, child); got != [3]int{1, 1, len(todaysDefaultSteps)} {
			t.Fatalf("the plan rows of %s = %v, want the default plan alone: the plan of generation 0 deleted", child, got)
		}
		told := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of " + child +
			" took no plan: version 1 of document " + planArtifact + ", given by " + treeRoleToken(t, "LEGION-208", claim.RoleArchitect) +
			". " + inPlanningB(child)}}
		if got := noticesOf(t, pool, child, "plan-not-taken"); !reflect.DeepEqual(got, told) {
			t.Fatalf("the plan-not-taken notices = %+v, want %+v", got, told)
		}
	})

	t.Run("the control: drawn into the owner's plan, the plan is carried and nothing is told", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		given(t, pool, engine)
		drawnInto(t, pool, child, "planning", "a", "owner")

		firstStep(t, pool, engine, child)

		want := walkStanding{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"1 owner"},
			[]string{"implementer/implementation", "reviewer/review"}}
		if got := walkStandingOf(t, pool, engine, child, 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v, the owner's plan carried", child, got, want)
		}
		if got := noticesOf(t, pool, child, "plan-not-taken"); len(got) != 0 {
			t.Fatalf("the plan-not-taken notices = %+v, want none", got)
		}
	})

	t.Run("drawn into the default steps with no plan given, nothing is told", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, child, "LEGION-208")
		drawnInto(t, pool, child, "planning", "b", "default")

		firstStep(t, pool, engine, child)

		assertDefaultPlan(t, planStandingOf(t, pool, engine, child, 1).plan, child, 1, todaysStoredSteps)
		if got := noticesOf(t, pool, child, "plan-not-taken"); len(got) != 0 {
			t.Fatalf("the plan-not-taken notices = %+v, want none: the store held no plan of its owner", got)
		}
	})
}

// An issue whose owner changed keeps the group of its first draw. Where its group runs the
// project's default steps, the plan an earlier owner gave is told in the one notice of the group,
// which names the group and never tells the owner to give the plan again with register_plan: that
// call is answered PLAN_NOT_TAKEN for this issue. Here the architect of LEGION-208 gave the plan of
// LEGION-209 at generation 0, and LEGION-209 was then recorded as a root of its own, whose owner
// is its own architect. The fault caught is the notice of an earlier owner's plan alone
// (tellPassedOver) written for an issue of the group, which offers register_plan.
func TestAnEarlierOwnersPlanIsToldWithTheGroup(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-209"
	pool := migratedPool(t)
	engine := returnEngine()
	mustTake(t, "the earlier owner's version 1 at generation 0", applyFacts(t, pool, engine)(planID(issue, 0, 1),
		ownersPlan(t, issue, 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
	recordedAt(t, pool, issue, issue)
	drawnInto(t, pool, issue, "planning", "b", "default")

	firstStep(t, pool, engine, issue)

	assertDefaultPlan(t, planStandingOf(t, pool, engine, issue, 1).plan, issue, 1, todaysStoredSteps)
	told := noticesOf(t, pool, issue, "plan-not-taken")
	want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of " + issue +
		" took no plan: version 1 of document " + planArtifact + ", given by " + treeRoleToken(t, "LEGION-208", claim.RoleArchitect) +
		". " + inPlanningB(issue)}}
	if !reflect.DeepEqual(told, want) {
		t.Fatalf("the plan-not-taken notices = %+v, want %+v", told, want)
	}
}

// The first step of an issue whose group runs the project's default steps reads the plan of the
// run's own generation and passes it over only where its owner gave it. An owner's plan stored at
// that generation is one a release that reads no group took, during the roll back the migration
// of the column `plan` describes: this release answers PLAN_NOT_TAKEN to the same registration, so
// the test gives the plan before it writes the group's row. That plan is deleted, the run takes the
// default steps, and the one notice of the group names it. The control is a plan of that
// generation that no owner gave, the default a hand-over carried: it stands as it was, and nothing
// is told.
func TestTheGroupDefaultPassesOverOnlyAnOwnersPlanOfTheRunsGeneration(t *testing.T) {
	t.Parallel()
	t.Run("an owner's plan of the run's generation is passed over and told", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		mustTake(t, "version 1 at the run's generation", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		drawnInto(t, pool, root.Key, "planning", "b", "default")

		firstStep(t, pool, engine, root.Key)

		assertDefaultPlan(t, planStandingOf(t, pool, engine, root.Key, root.Generation).plan, root.Key, root.Generation, todaysStoredSteps)
		if got := planRowsOf(t, pool, root.Key); got != [3]int{1, 1, len(todaysDefaultSteps)} {
			t.Fatalf("the plan rows of %s = %v, want the default plan alone: the owner's plan deleted", root.Key, got)
		}
		want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of " + root.Key +
			" took no plan: version 1 of document " + planArtifact + ", given by " + treeRoleToken(t, "LEGION-208", claim.RoleArchitect) +
			". " + inPlanningB(root.Key)}}
		if got := noticesOf(t, pool, root.Key, "plan-not-taken"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan-not-taken notices = %+v, want %+v", got, want)
		}
	})

	t.Run("the control: a carried default of the run's generation stands and is not told", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedDefaultPlan(t, pool, root.Key, root.Generation)
		drawnInto(t, pool, root.Key, "planning", "b", "default")
		before := planStandingOf(t, pool, engine, root.Key, root.Generation).plan.Version

		firstStep(t, pool, engine, root.Key)

		if after := planStandingOf(t, pool, engine, root.Key, root.Generation).plan.Version; !reflect.DeepEqual(after, before) {
			t.Fatalf("the version in force of %s after its first step = %+v, want the carried default %+v as it stood", root.Key, after, before)
		}
		if got := noticesOf(t, pool, root.Key, "plan-not-taken"); len(got) != 0 {
			t.Fatalf("the plan-not-taken notices = %+v, want none: no owner gave the carried default", got)
		}
	})
}

// After a way back that ran two or more generations, an issue holds a copy of its owner's plan at
// each of them: each new run carried the plan from its start (record.Store's CarryPlan), the time
// it was given with it. The first step of an issue whose group runs the project's default steps
// passes over every copy and names the version once in its one notice, so the next run's first
// step finds no plan of the owner and tells nothing. The fault caught is a pass-over of the newest
// copy alone: the next run's first step then finds the copy left and tells the same version again.
func TestTheGroupDefaultPassesOverEveryCopyOfItsOwnersPlanOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedOwnersPlan(t, pool, root.Key, 1)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if err := record.NewStore().CarryPlan(t.Context(), tx, root.Key, 1, 2, false); err != nil {
			return err
		}
		return record.NewStore().CarryPlan(t.Context(), tx, root.Key, 2, 3, false)
	})
	execSQL(t, pool, `update issues set generation = 4 where key = $1`, root.Key)
	drawnInto(t, pool, root.Key, "planning", "b", "default")

	firstStep(t, pool, engine, root.Key)
	execSQL(t, pool, `update issues set generation = generation + 1, phase = 'admitted' where key = $1`, root.Key)
	firstStep(t, pool, engine, root.Key)

	want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of " + root.Key +
		" took no plan: version 2 of document plan-artifact, given by " + treeRoleToken(t, root.Key, claim.RoleArchitect) +
		". " + inPlanningB(root.Key)}}
	if got := noticesOf(t, pool, root.Key, "plan-not-taken"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan-not-taken notices of two runs = %+v, want the one notice of the first run, naming the version once", got)
	}
	if got := planRowsOf(t, pool, root.Key); got != [3]int{2, 2, 2 * len(todaysDefaultSteps)} {
		t.Fatalf("the plan rows of %s = %v, want the default plans of the two runs alone: every copy of the owner's plan deleted", root.Key, got)
	}
}

// planNotTakenOfB is the answer to a plan given for key, in the variant b of planning.
func planNotTakenOfB(key string) *intake.Refusal {
	return &intake.Refusal{Status: 409, Code: "PLAN_NOT_TAKEN", Message: inPlanningB(key) +
		" Legion takes no plan from its owner for this issue, only a version that changes nothing but the text of its running task default. " +
		"This plan changed nothing."}
}

// drawnForMemory records key's tree as drawn into the arm memory of the `memory` experiment,
// which names no plan: a group of no experiment that sets how an issue is done.
func drawnForMemory(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	at := time.Date(2026, 11, 17, 19, 7, 25, 0, time.UTC)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().PutAssignments(t.Context(), tx, key, []record.Assignment{
			{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &at}})
		return err
	})
}

// An issue whose group runs the project's default steps takes the project's default steps at its
// first step, from planning, under a plan whose source is the default. The control is the same
// issue drawn into the arm whose owner gives the plan, with its owner's plan stored: the plan is
// taken and its first step is the implementer's.
func TestTheGroupDefaultRunsTheDefaultSteps(t *testing.T) {
	t.Parallel()
	given := func(t *testing.T, key string, generation uint64) (string, intake.PlanGiven) {
		return planID(key, generation, 1), ownersPlan(t, key, generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))
	}

	t.Run("the group default runs the default steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "b", "default")

		firstStep(t, pool, engine, root.Key)

		assertDefaultPlan(t, planStandingOf(t, pool, engine, root.Key, root.Generation).plan, root.Key, root.Generation, todaysStoredSteps)
		want := walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, todaysStoredSteps}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v", root.Key, got, want)
		}
	})

	t.Run("the control: the group owner takes the plan and starts implementing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "a", "owner")
		id, plan := given(t, root.Key, root.Generation)

		mustTake(t, "version 1", applyFacts(t, pool, engine)(id, plan))
		firstStep(t, pool, engine, root.Key)

		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		if got := planStandingOf(t, pool, engine, root.Key, root.Generation).versions; !reflect.DeepEqual(got, []string{"1 owner"}) {
			t.Fatalf("the versions of %s = %v, want the owner's alone", root.Key, got)
		}
	})
}

// An issue whose group runs the project's default steps takes no plan from its owner before its
// first step: register_plan is answered 409 PLAN_NOT_TAKEN with the group's clause, the
// registration stays unspent, and nothing is recorded. The control is the same issue drawn into the
// arm whose owner gives the plan: the plan is taken.
func TestTheGroupDefaultTakesNoPlan(t *testing.T) {
	t.Parallel()
	t.Run("the group default takes no plan", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "b", "default")
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		id := planID(root.Key, root.Generation, 1)
		given := ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))

		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id, applyFacts(t, pool, engine)(id, given), planNotTakenOfB(root.Key), before)
		if got := planRowsOf(t, pool, root.Key); got != [3]int{} {
			t.Fatalf("the plan rows of %s = %v, want none: Legion records nothing when it takes no plan", root.Key, got)
		}
	})

	t.Run("the control: the group owner takes the plan", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "a", "owner")
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		if got := planStandingOf(t, pool, engine, root.Key, root.Generation).versions; !reflect.DeepEqual(got, []string{"1 owner"}) {
			t.Fatalf("the versions of %s = %v, want the owner's alone", root.Key, got)
		}
	})
}

// A plan given at generation 0 is the route's registration for an issue it found unrecorded. Given
// for an issue the daemon recorded since, in the group that runs the project's default steps, it
// is answered by what the issue's run holds. Where the run holds no plan but one no giver of which
// owns the issue now (an earlier owner's, record.GivenByEarlierOwners), the issue's first step
// would pass that plan over and take the plan of generation 0, so the plan given is of the run, and
// answered PLAN_NOT_TAKEN. Where the run holds a plan one of whose givers owns the issue, the run
// went on without the plan of generation 0, which is answered PLAN_STALE_GENERATION. Where the run
// holds no plan, the plan given would wait at generation 0 for a first step that passes it over,
// and is answered PLAN_NOT_TAKEN. So is the same version given again where it waits at generation
// 0 already, given there by the architect of LEGION-209, a child of the tree and so an owner of
// the issue (record.ArchitectOfTree): it is the owner's version, not one the group stores as its
// own (record.PlanSourceDefault). None stores or changes anything, at the run's generation or at
// generation 0. The plans are written as the store keeps them: the run's given by the architect of
// LEGION-300's tree, which owns nothing of LEGION-208, or by LEGION-208's own architect.
//
// The faults it catches: a run holding an earlier owner's plan read as having taken it (its
// control: waitsAtZero keeping every plan of the run, whoever gave it, under which the first row
// answers PLAN_STALE_GENERATION), a run holding its owner's plan read as still waiting (its
// control: waitsAtZero reading no row of the run, under which the second row answers
// PLAN_NOT_TAKEN), and an owner's version given again taken as the group's own (its control:
// versionRefusal taking every version in force given again, under which the fourth row is
// answered taken and adds LEGION-208's architect to the givers of the version at generation 0).
func TestAPlanGivenAtGenerationZeroForAnIssueOfTheGroupDefaultIsJudgedByItsRunsPlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		giver   string
		waiting string
		want    func(root record.Issue) *intake.Refusal
	}{
		{"the run holds an earlier owner's plan", "LEGION-300", "",
			func(root record.Issue) *intake.Refusal { return planNotTakenOfB(root.Key) }},
		{"the run holds its owner's plan", "LEGION-208", "",
			func(root record.Issue) *intake.Refusal {
				return intake.PlanStaleGeneration(root.Key, 0, root.Generation)
			}},
		{"the run holds no plan", "", "",
			func(root record.Issue) *intake.Refusal { return planNotTakenOfB(root.Key) }},
		{"the same version waits at generation 0", "", "LEGION-209",
			func(root record.Issue) *intake.Refusal { return planNotTakenOfB(root.Key) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			drawnInto(t, pool, root.Key, "planning", "b", "default")
			seedVersion := func(generation uint64, giver string) {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().PutPlan(t.Context(), tx, record.PlanVersion{Issue: root.Key, Generation: generation, Version: 1,
						ArtifactID: planArtifact, Source: record.PlanSourceOwner, GivenBy: claim.Token(treeRoleToken(t, giver, claim.RoleArchitect)),
						GivenAt: time.Date(2026, 11, 17, 19, 7, 25, 0, time.UTC)},
						[]record.PlanTask{{Issue: root.Key, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}}, nil)
				})
			}
			if tc.giver != "" {
				seedVersion(root.Generation, tc.giver)
			}
			if tc.waiting != "" {
				recordedAt(t, pool, tc.waiting, root.Key)
				seedVersion(0, tc.waiting)
			}
			before := planStandingOf(t, pool, engine, root.Key, root.Generation)
			atZero := planStandingOf(t, pool, engine, root.Key, 0)
			id := planID(root.Key, 0, 1)
			given := ownersPlan(t, root.Key, 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))

			result := applyFacts(t, pool, engine)(id, given)
			if after := planStandingOf(t, pool, engine, root.Key, 0); !reflect.DeepEqual(after, atZero) {
				t.Errorf("the plan given changed %s at generation 0:\n before %v %+v\n after  %v %+v", root.Key, atZero.versions, atZero.plan, after.versions, after.plan)
			}
			assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id, result, tc.want(root), before)
		})
	}
}

// An issue of no group runs as it did before any experiment set how an issue is done: an issue
// the daemon recorded before any experiment set how an issue is done holds no row, and one whose
// rows name no plan — the `memory` experiment's alone here — is of no group either. Each takes the
// plan its owner gives, a plan an earlier owner gave is told as it always was (tellPassedOver), and
// its owner stops a task of it.
func TestAnIssueWithNoGroupRunsAsBuilt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drawn func(t *testing.T, pool *pgxpool.Pool, key string)
	}{
		{"no row", func(*testing.T, *pgxpool.Pool, string) {}},
		{"a row of memory alone", drawnForMemory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The earlier owner's plan is the plan of LEGION-208's architect, given while the child
			// stood in that tree; the child is then recorded as a root of its own, whose architect
			// gives its plan at generation 0 (first_step_test.go's earlier owner, as built).
			const child = "LEGION-209"
			pool := migratedPool(t)
			engine := returnEngine()
			owner := claim.Token(treeRoleToken(t, child, claim.RoleArchitect))
			recordedAt(t, pool, child, "LEGION-208")
			mustTake(t, "the earlier owner's version 1", applyFacts(t, pool, engine)(planID(child, 1, 1),
				ownersPlan(t, child, 1, 1, givenTask(t, "Build the widget.", "implementer", "tester"))))
			recordedAt(t, pool, child, child)
			tc.drawn(t, pool, child)
			given := ownersPlan(t, child, 0, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))
			given.GivenBy = owner
			mustTake(t, "the owner's version 2 at generation 0", applyFacts(t, pool, engine)(planID(child, 0, 2), given))
			firstStep(t, pool, engine, child)

			assertWalksAt(t, pool, engine, child, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
			told := noticesOf(t, pool, child, "plan-not-taken")
			if len(told) != 1 || !strings.Contains(told[0].Reason, "took no plan of an earlier owner") ||
				!strings.Contains(told[0].Reason, "give it again with register_plan as your own.") {
				t.Fatalf("the plan-not-taken notices = %+v, want the one notice of an earlier owner's plan, as built", told)
			}
			stop := intake.TaskStopped{Issue: child, Task: "a", By: owner, Generation: 1}
			if result := applyFacts(t, pool, engine)("stop", stop); result.Refusal != nil {
				t.Fatalf("the owner's stop of a = %+v, want it taken", result.Refusal)
			}
			if got := walkStandingOf(t, pool, engine, child, 1).state; got != record.TaskStopped {
				t.Fatalf("task a after its owner's stop is %s, want stopped", got)
			}
		})
	}
}

// Legion takes no stop of its tasks from the owner of an issue whose group runs the project's
// default steps: its stop_task is answered 409 PLAN_NOT_TAKEN with the sentence that names the
// group and that reason, and the task goes on running. The control is the
// same issue drawn into the arm whose owner gives the plan, whose owner gave none: it runs the
// default task, which its owner did not write, and its owner's stop of that task is taken.
func TestTheGroupDefaultStopsNoTask(t *testing.T) {
	t.Parallel()
	t.Run("the group default refuses the stop, and the task runs on", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "b", "default")
		firstStep(t, pool, engine, root.Key)

		result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, plan.DefaultTask))
		want := &intake.Refusal{Status: 409, Code: "PLAN_NOT_TAKEN", Message: inPlanningB(root.Key) +
			" Legion takes no stop of its tasks from its owner. This stop changed nothing."}
		if !reflect.DeepEqual(result.Refusal, want) {
			t.Fatalf("the owner's stop of default = %+v, want the refusal %+v", result.Refusal, want)
		}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); got.state != record.TaskRunning || got.phase != phase.Planning {
			t.Fatalf("%s after the refused stop stands at %+v, want default running at planning", root.Key, got)
		}
	})

	t.Run("the control: the group owner stops its task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		drawnInto(t, pool, root.Key, "planning", "a", "owner")
		firstStep(t, pool, engine, root.Key)

		if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
			t.Fatalf("the owner's stop of default = %+v, want it taken", result.Refusal)
		}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation).state; got != record.TaskStopped {
			t.Fatalf("default after its owner's stop is %s, want stopped", got)
		}
	})
}
