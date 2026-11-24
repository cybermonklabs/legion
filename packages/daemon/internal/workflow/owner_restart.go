package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// OwnerRestarts is how many times the daemon starts a child's own architect again after its claim
// failed in one run of a place only that architect ends (ownerAlone). Each start retries the
// claim with fresh budgets (supervise.RequestRetry), so without a bound a launch that always fails
// would be started for ever. Once they are spent the claim stays failed, the stranded list names
// it (Stranded), and the operator's retry of the claim gives them all back (restart); each new run
// of the place starts with none spent (record.Store.RaiseStepStarts).
const OwnerRestarts = 3

// ownerAlone says whether issue stands at a place only its own architect ends: a step of the key
// owner or the owner's retro (record.Issue.OwnersPlace), whose report complete_step takes from the
// issue's own architect alone (record.OwnerClaim). No phase start comes while the issue stands
// there, so nothing but a start of that architect moves it on. An acceptance the owner does is not
// such a place: an architect above it ends that with sign_off as well (awaitingAcceptance), and
// the phase start that follows starts a failed child architect again (reviveArchitect).
func ownerAlone(issue record.Issue) bool {
	return issue.OwnersPlace() && issue.Phase != phase.Accepting
}

// restartFailedOwner starts again issue's own architect, a child's, whose claim failed while issue
// stands at a place only that architect ends (ownerAlone), and answers the reason of the
// worker-died that tells the architect above of the failure (claimFailed), and whether the
// controller must be told too. The start is the plain one a phase start queues
// (childArchitectStart), and the owner is told its step again when it runs again in a session no
// owner-step of the step's run reached (claimReady's owedOwnersStep and toldThisSession), where no
// owner-step of that run is still queued. A start already
// queued is the start, and counts nothing (architectStartQueued). Each other
// start spends one of the OwnerRestarts of the step's run (record.Store.CountOwnerRestart). Once
// they are spent nothing is queued, and the architect above can do nothing about it: complete_step
// takes the step's report from the owner alone, and no route of an architect's retries a claim.
// So the controller is told too, as it is of a tree architect's failure, the daemon warns, and the
// reason names the two moves that start the claim again: the operator's retry and the controller's
// fresh_session, which the stranded list offers too. Anywhere else the child's next phase start
// brings the claim back (reviveArchitect), as the reason says. At an acceptance the owner does,
// that start comes only once the child moves, and the daemon does nothing else there: it queues no
// start, the stranded list does not name the claim, and the controller is not told. So the reason
// asks the architect above to sign_off for the child or to move it back.
func (e *Engine) restartFailedOwner(ctx context.Context, tx pgx.Tx, issue record.Issue) (reason string, tellController bool, err error) {
	next := fmt.Sprintf("%s's own architect failed, and its notices come to you until it runs again. The daemon starts it again at %s's next phase start. "+
		"At an acceptance that architect does, no phase start comes until %s moves: sign_off for %s, or move it back with request_backward_move",
		issue.Key, issue.Key, issue.Key, issue.Key)
	if !ownerAlone(issue) {
		return next, false, nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return "", false, err
	}
	step, ok := standingStep(w, issue)
	if !ok || !step.owner {
		return next, false, nil
	}
	run, child, err := e.ownArchitectRun(ctx, tx, issue)
	if err != nil || !child {
		return next, false, err
	}
	where := record.OwnersPlaceWords(w.Task, w.Place, w.Steps)
	if architectStartQueued(run, issue.Phase) {
		return fmt.Sprintf("%s's own architect failed at %s, which only it reports: a start of it is queued already, and its notices come to you until it runs again",
			issue.Key, where), false, nil
	}
	spent, counted, err := e.store.CountOwnerRestart(ctx, tx, issue.Key, step.place, OwnerRestarts)
	if err != nil {
		return "", false, err
	}
	if !counted {
		owner, err := record.OwnerClaim(issue)
		if err != nil {
			return "", false, err
		}
		e.warnOnCommit(ctx, "workflow: a failed owner's restarts at its step are spent, and it waits for the operator's retry or the controller's fresh_session",
			"issue", issue.Key, "claim", owner, "task", step.place.Task, "kind", step.place.Kind, "place", step.place.Place, "restarts", OwnerRestarts)
		return fmt.Sprintf("%s's own architect failed at %s, which only it reports, and the daemon has started it again %d times at this step: "+
			"it stays failed, and the stranded list names it, until the operator's retry (legion claims retry %s) or the controller's fresh_session starts it again; "+
			"until then the child's notices go to the architect above it",
			issue.Key, where, OwnerRestarts, owner), true, nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue)); err != nil {
		return "", false, err
	}
	e.logOnCommit(ctx, "workflow: a child's own architect that failed at its own step starts again",
		"issue", issue.Key, "task", step.place.Task, "kind", step.place.Kind, "place", step.place.Place, "restart", spent, "of", OwnerRestarts)
	return fmt.Sprintf("%s's own architect failed at %s, which only it reports: the daemon starts it again now (%d of %d at this step), and its notices come to you until it runs again",
		issue.Key, where, spent, OwnerRestarts), false, nil
}
