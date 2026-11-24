package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// childAtOwnersStep records LEGION-208's tree with its child LEGION-209 at step 1 of its plan
// `implementer:owner reviewer` and starts that step, which opens its run and tells the child's own
// architect its owner-step. The start runs as the promotion's does, with no boot pass before it: a
// boot pass would open the step's uncounted run first, and the start would then find it running.
func childAtOwnersStep(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, "LEGION-209", phase.Implementing, nil)
	seedStepsAt(t, pool, child.Key, child.Generation, 1, nil, "implementer:owner", "reviewer")
	factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, child.Key)
		if err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, *issue)
	})
	return child
}

// ownerAtItsStep records LEGION-208's tree with its child LEGION-209 at step 1 of its plan
// `implementer:owner reviewer`: the step started, which opened its run and told the child's own
// architect its owner-step, that notice gone (delivered, or finished undelivered), and the child's
// own architect's claim failed.
func ownerAtItsStep(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	child := childAtOwnersStep(t, pool, engine)
	if told := ownerSteps(t, pool, child.Key); len(told) != 1 {
		t.Fatalf("the child's step start told %+v, want one owner-step on the child", told)
	}
	dropNotices(t, pool, child.Key)
	seedIssuesArchitect(t, pool, child.Key, supervise.StateFailed, "")
	return child
}

// ownerFailed applies the n-th failure of key's own architect's claim: its launches never
// registered, until they ran out.
func ownerFailed(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, n int) {
	t.Helper()
	applyOwned(t, pool, engine, fmt.Sprintf("owner-failed-%d", n), intake.ClaimFailed{Issue: key, Role: claim.RoleArchitect,
		Reason: "launch failures ran out", Cause: supervise.CauseNeverRegistered})
}

// startsRan is the outbox running every start key's outbox holds: each row is gone, as a run that
// succeeds deletes it.
func startsRan(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from outbox where kind = 'supervise' and issue = $1 and payload->>'op' = 'start'`, key); err != nil {
		t.Fatalf("run the starts of %s: %v", key, err)
	}
}

// lastWorkerDied is the reason of the newest worker-died key's outbox holds.
func lastWorkerDied(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	died := noticesOf(t, pool, key, "worker-died")
	if len(died) == 0 {
		t.Fatalf("%s's outbox holds no worker-died", key)
	}
	return died[len(died)-1].Reason
}

// A child's own architect that fails while its child stands at a step of its own, which only that
// architect reports (complete_step takes the report from the child's own architect alone), is
// started again at once: no phase start comes while the child stands there, and the start the
// child's next phase start would queue (reviveArchitect) would never come. The start is the plain
// one a phase start queues (childArchitectStart), and the worker-died tells the architect above
// that it runs again. The launch's ready tells the owner its step again (the catch-up's owed step)
// where its session has not had the step's owner-step, as here: that notice never reached a
// session.
//
// The control is a child at a step its owner does not do: its architect's failure queues no start
// and says the next phase start brings it back, as before.
func TestAChildsOwnArchitectThatFailsAtItsOwnStepIsStartedAgainAndToldTheStep(t *testing.T) {
	t.Run("at the owner's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		child := ownerAtItsStep(t, pool, engine)

		ownerFailed(t, pool, engine, child.Key, 1)
		if got, want := architectStartsOf(t, pool, child.Key), []record.SuperviseRequest{plainArchitectStart(child.Generation)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the child's architect starts after its failure at its own step = %#v, want the plain start %#v", got, want)
		}
		if got := lastWorkerDied(t, pool, child.Key); got == "" {
			t.Fatalf("the child's architect's failure at its own step wrote no worker-died")
		}

		claimReady(t, pool, engine, child.Key, claim.RoleArchitect, 2)
		if told := ownerSteps(t, pool, child.Key); len(told) != 1 || told[0].Role != claim.RoleImplementer {
			t.Fatalf("the restarted owner's ready told %+v, want its owner-step of step 1 again", told)
		}
	})

	t.Run("at a worker's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		stepRoot(t, pool, phase.Implementing)
		child := stepChild(t, pool, "LEGION-209", phase.Reviewing, nil)
		seedStepsAt(t, pool, child.Key, child.Generation, 2, nil, "implementer:owner", "reviewer")
		seedIssuesArchitect(t, pool, child.Key, supervise.StateFailed, "")

		ownerFailed(t, pool, engine, child.Key, 1)
		if got := architectStartsOf(t, pool, child.Key); len(got) != 0 {
			t.Fatalf("the child's architect starts after its failure at the reviewer's step = %#v, want none: the next phase start brings it back", got)
		}
		if got := lastWorkerDied(t, pool, child.Key); got == "" {
			t.Fatalf("the child's architect's failure at the reviewer's step wrote no worker-died")
		}
	})
}

// The daemon starts a failed owner again at most OwnerRestarts times in one run of its step: a
// launch that always fails is started that many times, and then left failed. The stranded list then
// names it for the operator's retry or the controller's fresh_session, as the last worker-died says.
// Only the controller and the operator can start it then — the architect above cannot report the
// step, and has no retry of a claim — so each worker-died past the restarts goes to the controller
// as well, and the daemon warns. A failure the daemon starts again tells neither. A start already
// queued is the start, and counts nothing. The operator's retry of the owner's claim
// (intake.RestartPhase of the architect, what `legion claims retry` and the controller's
// fresh_session apply) gives the daemon its restarts again: the next failure is started again. It
// also tells the owner its step again with one more owner-step, since no owner-step of the step's
// open run is still queued here.
func TestAFailedOwnersRestartsAtOneStepAreBoundedAndTheOperatorsRetryResetsThem(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	var logged bytes.Buffer
	engine.log = slog.New(slog.NewTextHandler(&logged, nil))
	child := ownerAtItsStep(t, pool, engine)

	started := 0
	for n := 1; n <= OwnerRestarts+2; n++ {
		logged.Reset()
		ownerFailed(t, pool, engine, child.Key, n)
		starts := len(architectStartsOf(t, pool, child.Key))
		spent := n > OwnerRestarts
		if want := 1; spent {
			if starts != 0 {
				t.Fatalf("failure %d queued %d starts, want none: the daemon started the owner again %d times at this step already", n, starts, OwnerRestarts)
			}
		} else if starts != want {
			t.Fatalf("failure %d queued %d starts, want %d", n, starts, want)
		}
		copies, wantCopies := controllerWorkerDied(t, pool, child.Key), 0
		if spent {
			wantCopies = n - OwnerRestarts
		}
		if copies != wantCopies {
			t.Fatalf("after failure %d the controller holds %d worker-died copies of %s, want %d", n, copies, child.Key, wantCopies)
		}
		if warned := strings.Contains(logged.String(), "level=WARN") && strings.Contains(logged.String(), "restarts="+strconv.Itoa(OwnerRestarts)) &&
			strings.Contains(logged.String(), "issue="+child.Key); warned != spent {
			t.Fatalf("failure %d warned %t, want %t: the log reads %q", n, warned, spent, logged.String())
		}
		if n == 1 {
			// A second report while the first start waits queues none beside it and counts nothing.
			ownerFailed(t, pool, engine, child.Key, 100)
			if again := len(architectStartsOf(t, pool, child.Key)); again != 1 {
				t.Fatalf("a failure while a start waits left %d starts, want the one", again)
			}
		}
		started += starts
		startsRan(t, pool, child.Key)
	}
	if started != OwnerRestarts {
		t.Fatalf("a launch that always fails was started %d times, want %d", started, OwnerRestarts)
	}
	token := claim.Token(treeRoleToken(t, child.Key, claim.RoleArchitect))
	if got := lastWorkerDied(t, pool, child.Key); got == "" {
		t.Fatalf("the spent failure of %s wrote no worker-died", child.Key)
	}

	if result := applyOwned(t, pool, engine, "operator-retry", intake.RestartPhase{Issue: child.Key, Role: claim.RoleArchitect, Claim: token}); result.Refusal != nil {
		t.Fatalf("the operator's retry of the owner = %+v, want it applied", result.Refusal)
	}
	if told := ownerSteps(t, pool, child.Key); len(told) != 1 || told[0].Role != claim.RoleImplementer {
		t.Fatalf("the operator's retry told %+v, want one more owner-step of step 1", told)
	}
	ownerFailed(t, pool, engine, child.Key, OwnerRestarts+3)
	if got := architectStartsOf(t, pool, child.Key); len(got) != 1 {
		t.Fatalf("the failure after the operator's retry queued %#v, want one start: the retry gave the daemon its restarts again", got)
	}
}

// The operator's retry of a failed owner whose owner-step of the step's run is still queued —
// held for the owner's slot, or for the claim the retry relaunches — queues no second one: the
// queued notice tells the owner when it publishes, and a second would tell the same session the
// same step again. The retry still gives the daemon its restarts of the run again. The control is
// the retry where that notice is gone (TestAFailedOwnersRestartsAtOneStepAreBoundedAndTheOperatorsRetryResetsThem),
// which tells the step once more.
//
// Before, the retry told the step whatever the outbox held (restart's tellOwner), so the owner
// got the step twice.
func TestTheOperatorsRetryOfAnOwnerWhoseStepIsStillQueuedTellsNothingMore(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	child := childAtOwnersStep(t, pool, engine)
	seedIssuesArchitect(t, pool, child.Key, supervise.StateFailed, "")
	ownerFailed(t, pool, engine, child.Key, 1)
	startsRan(t, pool, child.Key)
	told := ownerSteps(t, pool, child.Key)
	if len(told) != 1 {
		t.Fatalf("the step's start told %+v, want one owner-step still queued", told)
	}

	token := claim.Token(treeRoleToken(t, child.Key, claim.RoleArchitect))
	if result := applyOwned(t, pool, engine, "operator-retry", intake.RestartPhase{Issue: child.Key, Role: claim.RoleArchitect, Claim: token}); result.Refusal != nil {
		t.Fatalf("the operator's retry of the owner = %+v, want it applied", result.Refusal)
	}
	if again := ownerSteps(t, pool, child.Key); len(again) != 1 || again[0].Reason != told[0].Reason {
		t.Fatalf("the retry with the step's owner-step queued holds %+v, want that one alone", again)
	}
	var restarts int
	if err := pool.QueryRow(t.Context(), `select owner_restarts from step_starts where issue = $1 and task = 'a' and kind = 'step' and place = 1`,
		child.Key).Scan(&restarts); err != nil {
		t.Fatalf("read the owner's restarts of step 1's run: %v", err)
	}
	if restarts != 0 {
		t.Fatalf("the retry left %d restarts of step 1's run spent, want none: it gives the daemon its restarts again", restarts)
	}
}

// An owner-step still queued for another run of the step — an earlier run, which the daemon's
// executor finishes undelivered when the row next runs (ownerStepRuns) — holds back no telling of
// the open run: the operator's retry tells the open run's step once, and so does a launch's
// catch-up. Only an owner-step of the open run itself is the telling that a second would repeat
// (record.Store.OwnerStepPending); an owner-step that names no run holds back nothing.
//
// Before, any owner-step of the issue still queued stopped both (the store's read of a pending
// notice of the kind, NoticePending), so the owner of the open run was told nothing until the
// stale row ran.
func TestAStaleOwnerStepOfAnotherRunHoldsBackNoTellingOfTheOpenRun(t *testing.T) {
	stale := record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer, Phase: phase.Implementing,
		Reason: "the earlier run's step", OwnerStep: &record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 0}}
	started := func(t *testing.T) (*pgxpool.Pool, *Engine, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := returnEngine()
		child := ownerAtItsStep(t, pool, engine)
		factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.notice(ctx, tx, child.Key, stale)
		})
		return pool, engine, child
	}
	toldOpenRun := func(t *testing.T, pool *pgxpool.Pool, key, by string) {
		t.Helper()
		told := ownerSteps(t, pool, key)
		if len(told) != 2 || told[1].OwnerStep == nil || told[1].OwnerStep.Starts != 1 {
			t.Fatalf("%s with an earlier run's owner-step queued holds %+v, want that one and one owner-step of the open run (starts 1)", by, told)
		}
	}

	t.Run("the operator's retry", func(t *testing.T) {
		pool, engine, child := started(t)
		token := claim.Token(treeRoleToken(t, child.Key, claim.RoleArchitect))
		if result := applyOwned(t, pool, engine, "operator-retry", intake.RestartPhase{Issue: child.Key, Role: claim.RoleArchitect, Claim: token}); result.Refusal != nil {
			t.Fatalf("the operator's retry of the owner = %+v, want it applied", result.Refusal)
		}
		toldOpenRun(t, pool, child.Key, "the retry")
	})

	t.Run("a launch's catch-up", func(t *testing.T) {
		pool, engine, child := started(t)
		claimReady(t, pool, engine, child.Key, claim.RoleArchitect, 2)
		toldOpenRun(t, pool, child.Key, "the launch's ready")
	})
}

// The stranded list names a child's own architect that failed while its child stands at a place
// only that architect ends — a step of its own other than its acceptance, or its retro — with no
// start of it queued. Only the operator's retry or the controller's fresh_session starts it
// then. It names none where a start is queued (the daemon's own restart), none at a worker's step
// (the child's next phase start brings it back), and none at an acceptance the owner does, which
// the architect above ends with sign_off as well.
func TestStrandedNamesAFailedChildArchitectAtAPlaceOnlyItEnds(t *testing.T) {
	owners := func(key string, at phase.Phase) record.Issue {
		issue := strandedIssue(key, "LEGION-1", at)
		issue.PlaceRole = claim.RoleArchitect
		return issue
	}
	issues := []record.Issue{
		strandedIssue("LEGION-1", "LEGION-1", phase.Implementing),
		owners("LEGION-2", phase.Testing),
		owners("LEGION-3", phase.Testing),
		strandedIssue("LEGION-4", "LEGION-1", phase.Testing),
		owners("LEGION-5", phase.Accepting),
		owners("LEGION-6", phase.Retro),
	}
	stuck := strandedClaim(t, "LEGION-1", "LEGION-2", claim.RoleArchitect, supervise.StateFailed)
	restarting := strandedClaim(t, "LEGION-1", "LEGION-3", claim.RoleArchitect, supervise.StateFailed)
	worker := strandedClaim(t, "LEGION-1", "LEGION-4", claim.RoleArchitect, supervise.StateFailed)
	accepting := strandedClaim(t, "LEGION-1", "LEGION-5", claim.RoleArchitect, supervise.StateFailed)
	retro := strandedClaim(t, "LEGION-1", "LEGION-6", claim.RoleArchitect, supervise.StateFailed)
	claims := []supervise.Claim{stuck, restarting, worker, accepting, retro}
	stored := map[claim.Token]supervise.Claim{}
	for _, c := range claims {
		stored[c.Token] = c
	}
	s := &strandedStore{issues: issues, claims: stored, slots: slotsOf("LEGION-1"),
		queued: map[claim.Token][]record.QueuedSupervise{restarting.Token: {{ID: 7,
			Request: record.SuperviseRequest{Op: "start", Tree: "LEGION-1", Role: claim.RoleArchitect, Generation: 2}}}}}

	got, err := Stranded(context.Background(), nil, s, "LEGION", nil, claims, nil, strandedNow)
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}
	if want := []supervise.Claim{stuck, retro}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stranded = %v, want %v", names(got), names(want))
	}
}

// controllerWorkerDied counts the worker-died copies the controller's outbox holds for key.
func controllerWorkerDied(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = $1 and issue = $2 and payload->>'kind' = 'worker-died'`,
		string(record.OutboxKindControllerNotice), key).Scan(&rows); err != nil {
		t.Fatalf("count the controller's worker-died copies of %s: %v", key, err)
	}
	return rows
}
