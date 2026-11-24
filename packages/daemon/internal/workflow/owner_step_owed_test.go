package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// owedBound is the silence bound the owner's facts below carry (config.Config.WorkerSilence).
const owedBound = 30 * time.Minute

// owedIdleSince is the moment the owner last stepped: an hour before stepClock, the moment the
// steps below start at (stepEngine's clock), so the owner was idle when its step started.
func owedIdleSince() time.Time { return stepClock.Add(-time.Hour) }

// engineAt is returnEngine with its clock at now: the pass that reads an owner idle past the bound
// runs after the step's start.
func engineAt(now time.Time) *Engine {
	engine := returnEngine()
	engine.cfg.Clock = func() time.Time { return now }
	return engine
}

// seedIdleOwner writes the claim of key's own architect, idle since owedIdleSince with nothing to
// say of its quiet: the owner an owner-step reached no turn of.
func seedIdleOwner(t *testing.T, pool *pgxpool.Pool, key, tree string, generation uint64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, update_round, step_at, given_entry, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $3, 'architect', $4, 'ses', '/sessions/ses.jsonl', 'idle', '', $5, 0, 0, 0, 0, 0)`,
		treeRoleToken(t, key, claim.RoleArchitect), tree, key, int64(generation), owedIdleSince()); err != nil {
		t.Fatalf("seed the idle owner of %s: %v", key, err)
	}
}

// dropNotices deletes every notice key's outbox holds: a notice that reached no turn.
func dropNotices(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from outbox where kind = $1 and issue = $2`, string(record.OutboxKindNotice), key); err != nil {
		t.Fatalf("drop the notices of %s: %v", key, err)
	}
}

// ownerSilence is the fact the daemon's sweep mints for key's architect idle since owedIdleSince.
func ownerSilence(key string, generation uint64) intake.WorkerSilent {
	return intake.WorkerSilent{Issue: key, Role: claim.RoleArchitect, Generation: generation, LastStep: owedIdleSince(), Bound: owedBound}
}

// applyOwned applies fact under id through engine, failing the test on an error.
func applyOwned(t *testing.T, pool *pgxpool.Pool, engine *Engine, id string, fact intake.Fact) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "supervise", id, fact, engine, admissionStub{})
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// An owner's step whose owner-step reached no turn is told to the owner again: the root's own
// step `implementer:owner` starts and tells the owner once, that notice is dropped, and the owner
// stays idle. The periodic pass over an owner idle past the bound tells the step again, once,
// with the task the step's entry holds, and starts no claim; the same pass's fact again, and a
// later pass's while that notice is still queued, tell nothing more. Once the owner's report has
// ended the step, the issue stands at the reviewer's step and the pass tells nothing. A step
// whose run started within the bound is not told by the pass either: the start told it.
//
// The control: before the owner's silence was read, the pass read an architect's silence as a
// phase worker's, which no worker of the place answers, and told nothing.
func TestAnOwnersStepNoTurnHeardIsToldAgainByThePeriodicPass(t *testing.T) {
	started := func(t *testing.T) (*pgxpool.Pool, record.Notice) {
		t.Helper()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		move(t, pool, returnEngine(), root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		told := ownerSteps(t, pool, root.Key)
		if len(told) != 1 {
			t.Fatalf("the gate's opening told %+v, want one owner-step", told)
		}
		dropNotices(t, pool, root.Key)
		seedIdleOwner(t, pool, root.Key, root.Key, root.Generation)
		return pool, told[0]
	}

	t.Run("the step that runs is told once", func(t *testing.T) {
		pool, first := started(t)
		later := engineAt(stepClock.Add(time.Hour))
		applyOwned(t, pool, later, "owner-silent:1", ownerSilence("LEGION-208", 1))

		told := ownerSteps(t, pool, "LEGION-208")
		if len(told) != 1 || told[0].Role != claim.RoleImplementer || told[0].Phase != phase.Implementing || told[0].Reason != first.Reason {
			t.Fatalf("the pass told %+v, want the one owner-step of step 1 again, with the task its entry holds (%q)", told, first.Reason)
		}
		assertNoClaimStarts(t, pool, "LEGION-208")
		if result := applyOwned(t, pool, later, "owner-silent:1", ownerSilence("LEGION-208", 1)); !result.Duplicate {
			t.Fatalf("the same silence again = %+v, want a duplicate", result)
		}
		applyOwned(t, pool, later, "owner-silent:2", ownerSilence("LEGION-208", 1))
		if told := ownerSteps(t, pool, "LEGION-208"); len(told) != 1 {
			t.Fatalf("a later pass with the owner-step still queued told %+v, want the one already queued", told)
		}
	})

	t.Run("a step the owner's report ended is not told", func(t *testing.T) {
		pool, _ := started(t)
		ownerReportsStep1(t, pool, returnEngine(), "LEGION-208")
		dropNotices(t, pool, "LEGION-208")
		applyOwned(t, pool, engineAt(stepClock.Add(time.Hour)), "owner-silent:1", ownerSilence("LEGION-208", 1))
		if told := ownerSteps(t, pool, "LEGION-208"); len(told) != 0 {
			t.Fatalf("the pass after the owner's report told %+v, want nothing: the step ended", told)
		}
	})

	t.Run("a run that started within the bound is not told", func(t *testing.T) {
		pool, _ := started(t)
		applyOwned(t, pool, engineAt(stepClock.Add(owedBound/2)), "owner-silent:1", ownerSilence("LEGION-208", 1))
		if told := ownerSteps(t, pool, "LEGION-208"); len(told) != 0 {
			t.Fatalf("the pass within the bound of the step's start told %+v, want nothing", told)
		}
	})
}

// The owner's catch-up at a launch tells again the owner's step that started and has not ended,
// where no owner-step of its run is still queued and the launch's session has not had one. The
// launch's ready queues the catch-up, which names the step, then the step's owner-step, once for
// the launch. A child's step goes to the child's own architect: its owner-step is on the
// child's key (record.OwnerStepNotice goes by OwnerClaim), and the child's architect's silence
// tells it there, where the root architect's silence tells nothing of the child.
//
// The control: before the catch-up told the step, a launch's ready queued the catch-up alone.
func TestAnOwnersCatchUpAndAChildsSilenceTellTheOwnersStepAgain(t *testing.T) {
	t.Run("the owner's launch", func(t *testing.T) {
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:owner", "reviewer")
		move(t, pool, returnEngine(), root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		first := ownerSteps(t, pool, root.Key)[0]
		dropNotices(t, pool, root.Key)

		claimReady(t, pool, returnEngine(), root.Key, claim.RoleArchitect, 2)
		if catchUp := noticesOf(t, pool, root.Key, "catch-up"); len(catchUp) != 1 {
			t.Fatalf("the launch queued the catch-ups %+v, want one", catchUp)
		}
		if _, ownStep := catchUpNames(t, pool, root.Key); ownStep == "" {
			t.Fatalf("the catch-up of %s names no step of the owner's, want its step 1", root.Key)
		}
		told := ownerSteps(t, pool, root.Key)
		if len(told) != 1 || told[0].Reason != first.Reason {
			t.Fatalf("the launch told %+v, want the owner-step of step 1 again with its entry's task", told)
		}
		claimReady(t, pool, returnEngine(), root.Key, claim.RoleArchitect, 2)
		if told := ownerSteps(t, pool, root.Key); len(told) != 1 {
			t.Fatalf("the launch's ready applied again told %+v, want the one owner-step", told)
		}
	})

	t.Run("a child's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		child := childAtOwnersStep(t, pool, engine)
		if told := ownerSteps(t, pool, child.Key); len(told) != 1 {
			t.Fatalf("the child's step start told %+v, want one owner-step on the child", told)
		}
		dropNotices(t, pool, child.Key)
		seedIdleOwner(t, pool, "LEGION-208", "LEGION-208", 1)
		seedIdleOwner(t, pool, child.Key, "LEGION-208", child.Generation)
		later := engineAt(stepClock.Add(time.Hour))

		applyOwned(t, pool, later, "owner-silent:root", ownerSilence("LEGION-208", 1))
		if told := ownerSteps(t, pool, child.Key); len(told) != 0 {
			t.Fatalf("the root architect's silence told %+v on the child, want nothing: the child's step is its own architect's", told)
		}
		applyOwned(t, pool, later, "owner-silent:child", ownerSilence(child.Key, child.Generation))
		if told := ownerSteps(t, pool, child.Key); len(told) != 1 || told[0].Role != claim.RoleImplementer {
			t.Fatalf("the child architect's silence told %+v, want the child's owner-step again", told)
		}
		if told := ownerSteps(t, pool, "LEGION-208"); len(told) != 0 {
			t.Fatalf("the root's outbox holds the owner-steps %+v, want none", told)
		}
	})
}
