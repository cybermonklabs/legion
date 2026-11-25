package workflow

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// treeArchitectsRead is what an architect route reads of key for the architect of key's gate root
// on its Dispatch chain (record.Chains.StateTree; api's ownsOnChain and openStep): that
// architect's claim, the issue's generation and the run of the step it stands in. The daemon
// serves its routes only once it has booted, so the read comes after the boot's open of each run
// no start counted, by engine, the scenario's daemon (bootBefore): a run the release before
// started is read as the boot opened it, as its fact reads it.
func treeArchitectsRead(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) (claim.Token, uint64, record.StepEntry) {
	t.Helper()
	bootBefore(t, pool, engine)
	records := record.NewStore()
	var issue *record.Issue
	var entry record.StepEntry
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the route's read of %s: %v", key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if issue, err = records.Issue(ctx, tx, key); err != nil {
		t.Fatalf("the route's read of %s: %v", key, err)
	}
	if issue == nil {
		t.Fatalf("the route's read: %s is not recorded", key)
	}
	if entry, err = records.OpenStep(ctx, tx, key); err != nil {
		t.Fatalf("the route's read of %s's open step: %v", key, err)
	}
	chains, err := record.ReadChainsOf(ctx, tx, records, issue.Project, []string{key})
	if err != nil {
		t.Fatalf("the route's read of %s's chain: %v", key, err)
	}
	return claim.Token(treeRoleToken(t, chains.StateTree(key), claim.RoleArchitect)), issue.Generation, entry
}

// rootsSignOff is the sign_off of key by its gate root's architect, as the route of engine's
// daemon builds it.
func rootsSignOff(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) intake.SignOff {
	t.Helper()
	by, generation, entry := treeArchitectsRead(t, pool, engine, key)
	return intake.SignOff{Issue: key, By: by, Generation: generation, Entry: entry}
}

// rootsMoveBack is the request_backward_move of key to to by its gate root's architect, as the
// route of engine's daemon builds it.
func rootsMoveBack(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, to phase.Phase, reason string) intake.BackwardMove {
	t.Helper()
	by, generation, entry := treeArchitectsRead(t, pool, engine, key)
	return intake.BackwardMove{Issue: key, Requester: claim.RoleArchitect, To: to, Reason: reason, By: by, Generation: generation, Entry: entry}
}

// otherRoot is the root LEGION-300, standing at admitted, generation 1, with no parent on its
// Dispatch chain: the issue a person moves an issue of LEGION-208's under in Dispatch.
func otherRoot(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300",
		Project: "LEGION", Title: "another root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	return root
}

// reparentTo moves key under parent on its Dispatch chain, as a person's change of parent reaches
// the copy and admission records it on the record (seedIssue writes both): its run, its
// generation and its plan stay with it (R17 of dispatch://INFRA-3320).
func reparentTo(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, parent string) {
	t.Helper()
	moved := issueOf(t, pool, engine, key)
	moved.Parent = &parent
	seedIssue(t, pool, moved)
}

// newRunOf opens key's next run, standing at admitted at the next generation, through the one
// path that opens a run (NewRun, decision 8 of dispatch://INFRA-4730), as a person's todo on its
// closed record opens it, with parent the Dispatch parent the todo's reading carries: the window
// in which a route's read of the run that ended reaches a fact of the next.
func newRunOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, parent string) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.NewRun(ctx, tx, issue, intake.DispatchIssue{Key: key, ResolvedRepo: issue.Repo, Seq: issue.LastDispatchSeq + 1,
			Type: "issue.updated", Status: "todo", Title: issue.Title, Parent: parent, Rank: issue.Rank})
	})
}

// notOwned is the 403 ISSUE_NOT_OWNED the workflow answers op on key from by, an architect that
// does not act on key on its Dispatch chain when the fact commits (architectCallRefusal over
// intake.CallRefusal, the rule every architect's call is held to): it names owner, the issue
// whose architect acts on key now, and that architect's claim.
func notOwned(t *testing.T, op, key string, by claim.Token, owner string) *intake.Refusal {
	t.Helper()
	return &intake.Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf(
		"%s of %s is refused: on its Dispatch chain %s is neither the issue of %s nor under it; the architect of %s (%s) acts on it",
		op, key, key, by, owner, treeRoleToken(t, owner, claim.RoleArchitect))}
}

// todoChild is a child of LEGION-208 at the status todo, the record a release or a person's todo
// leaves it at, standing at admitted.
func todoChild(t *testing.T, pool *pgxpool.Pool, key string) record.Issue {
	t.Helper()
	child := stepChild(t, pool, key, phase.Admitted, nil)
	child.Status, child.DispatchStatus = "todo", "todo"
	seedIssue(t, pool, child)
	return child
}

// assertStopRefused fails unless result is exactly want, left unspent so the same stop applies
// again when asked again (its processed mark gone), and key stands as before stood.
func assertStopRefused(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, id string, result intake.Result, want *intake.Refusal, before recordAt) {
	t.Helper()
	if !reflect.DeepEqual(result.Refusal, want) {
		t.Fatalf("the stop = %+v, want the refusal %+v", result.Refusal, want)
	}
	if !result.Unspent || processed(t, pool, id) {
		t.Fatalf("the refused stop %s is spent (unspent %v), want it unspent and unmarked", id, result.Unspent)
	}
	if after := standingOf(t, pool, engine, key, true); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused stop changed %s:\n before %+v\n after  %+v", key, before, after)
	}
}

// assertStopped fails unless result took the stop and key is held for its stopped task a.
func assertStopped(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, result intake.Result) {
	t.Helper()
	if result.Refusal != nil || result.Unspent {
		t.Fatalf("the stop = %+v, want it taken", result)
	}
	issue := issueOf(t, pool, engine, key)
	if issue.Phase != phase.Held || issue.Hold == nil || issue.Hold.Reason != record.HoldTaskStopped {
		t.Fatalf("%s stands in %s with hold %+v, want it held for its stopped task", key, issue.Phase, issue.Hold)
	}
	if task := taskOf(t, pool, engine, key); task.State != record.TaskStopped {
		t.Fatalf("task a of %s is %s, want stopped", key, task.State)
	}
}

// The route of stop_task judges the caller and the run on reads it takes before the fact lock
// (api's ownsOnChain), and a change of parent in Dispatch or a new run can commit before the fact
// applies. The fact judges both again where it commits (requestRefusal): an architect that acts
// on the issue on its Dispatch chain no longer is refused ISSUE_NOT_OWNED, naming the architect
// that does, and a stop of a run the issue left TASK_STALE_GENERATION, before the task of the run
// that stands now is touched. Each refusal writes nothing and leaves the fact unspent. Each
// control is the same stop with no such change between the read and the fact, and it stops the
// task.
func TestAnOwnersStopIsJudgedOnTheRecordItsFactReads(t *testing.T) {
	t.Parallel()
	for _, moved := range []bool{false, true} {
		name := "control: the owner's stop of the run its route read stops the task"
		if moved {
			name = "a change of parent in Dispatch took the child from under the owner before the stop: ISSUE_NOT_OWNED"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			stepRoot(t, pool, phase.Implementing)
			child := todoChild(t, pool, "LEGION-209")
			seedStepsPlan(t, pool, child.Key, child.Generation, "implementer", "tester")
			stop := ownersStop(t, child, "a")
			if moved {
				reparentTo(t, pool, engine, child.Key, otherRoot(t, pool).Key)
			}
			before := standingOf(t, pool, engine, child.Key, true)
			result := apply("stop", stop)
			if !moved {
				assertStopped(t, pool, engine, child.Key, result)
				return
			}
			assertStopRefused(t, pool, engine, child.Key, "stop", result, notOwned(t, "stop_task", child.Key, stop.By, "LEGION-300"), before)
		})
	}

	for _, rerun := range []bool{false, true} {
		name := "control: the owner's stop of the run its route read stops the running task"
		if rerun {
			name = "a new run of the child opened before the stop: TASK_STALE_GENERATION"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			stepRoot(t, pool, phase.Implementing)
			child := stepChild(t, pool, "LEGION-209", phase.Admitted, nil)
			seedStepsPlan(t, pool, child.Key, child.Generation, "implementer", "tester")
			firstStep(t, pool, engine, child.Key)
			stop := ownersStop(t, issueOf(t, pool, engine, child.Key), "a")
			if rerun {
				// The same owner gives the new run a plan whose task has the same id.
				newRunOf(t, pool, engine, child.Key, "LEGION-208")
				now := issueOf(t, pool, engine, child.Key)
				if now.Generation != child.Generation+1 || now.Phase != phase.Admitted {
					t.Fatalf("premise: the new run left %s at generation %d in %s, want generation %d at admitted", child.Key, now.Generation, now.Phase, child.Generation+1)
				}
				mustTake(t, "the new run's plan", apply(planID(child.Key, now.Generation, 1),
					ownersPlan(t, child.Key, now.Generation, 1, givenTask(t, "The work again.", "implementer", "reviewer"))))
			}
			before := standingOf(t, pool, engine, child.Key, true)
			result := apply("stop", stop)
			if !rerun {
				assertStopped(t, pool, engine, child.Key, result)
				return
			}
			assertStopRefused(t, pool, engine, child.Key, "stop", result, intake.TaskStaleGeneration(child.Key, child.Generation, child.Generation+1), before)
		})
	}

	// The architect of an issue above the stopped one on its Dispatch chain acts on it, through
	// its own issue there (intake.CallRefusal). When a person moves the stopped issue out from
	// under that issue in Dispatch, that architect acts on it no longer, and its generation does
	// not move: only the judgment of who acts on the issue refuses the stop.
	for _, left := range []bool{false, true} {
		name := "control: the stop of the architect above the issue on its chain stops it"
		if left {
			name = "the issue left the chain under the caller's issue before the stop: ISSUE_NOT_OWNED at the same generation"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			apply := applyFacts(t, pool, engine)
			stepRoot(t, pool, phase.Implementing)
			above := todoChild(t, pool, "LEGION-209")
			target := stepChild(t, pool, "LEGION-210", phase.Admitted, nil)
			reparentTo(t, pool, engine, target.Key, above.Key)
			seedStepsPlan(t, pool, target.Key, target.Generation, "implementer", "tester")
			firstStep(t, pool, engine, target.Key)
			stop := intake.TaskStopped{Issue: target.Key, Task: "a", By: claim.Token(treeRoleToken(t, above.Key, claim.RoleArchitect)),
				Generation: issueOf(t, pool, engine, target.Key).Generation}
			if left {
				reparentTo(t, pool, engine, target.Key, "LEGION-208")
				if now := issueOf(t, pool, engine, target.Key); now.Generation != stop.Generation {
					t.Fatalf("premise: %s stands at generation %d, want generation %d", target.Key, now.Generation, stop.Generation)
				}
			}
			before := standingOf(t, pool, engine, target.Key, true)
			result := apply("stop", stop)
			if !left {
				assertStopped(t, pool, engine, target.Key, result)
				return
			}
			assertStopRefused(t, pool, engine, target.Key, "stop", result, notOwned(t, "stop_task", target.Key, stop.By, "LEGION-208"), before)
		})
	}
}

// The route of register_plan judges the giver and the generation on reads it takes before the
// fact lock (api's planOwner): the record, or, for an issue the daemon has not recorded, its
// Dispatch status and its chain in the copy, with generation 0. The fact judges both again on the
// record and the chain it reads. A plan given at generation 0 waits there for the first step of an
// issue the daemon recorded since only while that issue stands before its first step with no plan
// rows at its own generation; once that step took a plan, the plan is refused
// PLAN_STALE_GENERATION, and the owner's plan given again at the recorded generation meets the
// rules of a later version. A giver that acts on the issue on its Dispatch chain no longer is
// refused ISSUE_NOT_OWNED, naming the architect that does. Each refusal writes nothing and leaves
// the fact unspent.
func TestAnOwnersPlanIsJudgedOnTheRecordItsFactReads(t *testing.T) {
	t.Parallel()
	widget := func(t *testing.T) plan.Task { return givenTask(t, "Build the widget.", "implementer", "reviewer") }

	t.Run("control: a plan given at generation 0 for a child recorded since that has not started waits for its first step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		mustTake(t, "the plan at generation 0", applyFacts(t, pool, engine)(planID("LEGION-209", 0, 1), ownersPlan(t, "LEGION-209", 0, 1, widget(t))))
		firstStep(t, pool, engine, "LEGION-209")
		if after := walkStandingOf(t, pool, engine, "LEGION-209", 1); after.phase != phase.Implementing {
			t.Fatalf("the child walks %+v, want the plan given at generation 0 walked from implementing", after)
		}
	})

	t.Run("a plan given at generation 0 after the child's first step took the default steps: PLAN_STALE_GENERATION, then the later-version rules", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		firstStep(t, pool, engine, "LEGION-209")
		id := planID("LEGION-209", 0, 1)
		before, running := planStandingOf(t, pool, engine, "LEGION-209", 0), planStandingOf(t, pool, engine, "LEGION-209", 1)

		result := apply(id, ownersPlan(t, "LEGION-209", 0, 1, widget(t)))

		assertPlanRefusal(t, pool, engine, "LEGION-209", 0, id, result, intake.PlanStaleGeneration("LEGION-209", 0, 1), before)
		if now := planStandingOf(t, pool, engine, "LEGION-209", 1); !reflect.DeepEqual(now, running) {
			t.Fatalf("the refused plan changed the running generation:\n before %+v\n after  %+v", running, now)
		}
		// Given again at the generation the record holds, the plan meets the rules of a later
		// version over the default steps, which the child already runs.
		again := apply(planID("LEGION-209", 1, 1), ownersPlan(t, "LEGION-209", 1, 1, widget(t)))
		if want := intake.PlanRefused([]plan.Refusal{intake.RunningTaskChanged(plan.DefaultTask)}); !reflect.DeepEqual(again.Refusal, want) {
			t.Fatalf("the plan given again at generation 1 = %+v, want %+v", again.Refusal, want)
		}
	})

	t.Run("a plan given at generation 0 for a child the daemon recorded since under another root: ISSUE_NOT_OWNED", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-300", "LEGION-300")
		recordedAt(t, pool, "LEGION-209", "LEGION-300")
		id := planID("LEGION-209", 0, 1)
		before := planStandingOf(t, pool, engine, "LEGION-209", 0)

		given := ownersPlan(t, "LEGION-209", 0, 1, widget(t))
		result := applyFacts(t, pool, engine)(id, given)

		assertPlanRefusal(t, pool, engine, "LEGION-209", 0, id, result, notOwned(t, "register_plan", "LEGION-209", given.GivenBy, "LEGION-300"), before)
	})

	for _, left := range []bool{false, true} {
		name := "control: the plan of the architect above the issue on its chain is taken"
		if left {
			name = "the issue left the chain under the giver's issue before the plan: ISSUE_NOT_OWNED at the same generation"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			stepRoot(t, pool, phase.Implementing)
			above := todoChild(t, pool, "LEGION-209")
			target := stepChild(t, pool, "LEGION-210", phase.Admitted, nil)
			reparentTo(t, pool, engine, target.Key, above.Key)
			given := ownersPlan(t, target.Key, target.Generation, 1, widget(t))
			given.GivenBy = claim.Token(treeRoleToken(t, above.Key, claim.RoleArchitect))
			id := planID(target.Key, target.Generation, 1)
			if left {
				reparentTo(t, pool, engine, target.Key, "LEGION-208")
			}
			before := planStandingOf(t, pool, engine, target.Key, target.Generation)
			result := applyFacts(t, pool, engine)(id, given)
			if !left {
				mustTake(t, "the plan of the architect above", result)
				return
			}
			assertPlanRefusal(t, pool, engine, target.Key, target.Generation, id, result, notOwned(t, "register_plan", target.Key, given.GivenBy, "LEGION-208"), before)
		})
	}
}
