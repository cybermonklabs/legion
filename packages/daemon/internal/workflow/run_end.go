package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The end of a step's run follows from the record, at one point every fact passes, so that no way
// out of a step has to remember to end it. A run is one the store holds open: a start opens it,
// and the daemon's boot opens each run no start of this release counted before any fact applies
// (OpenUncountedRuns). It is over when (runOver):
//   - its issue no longer stands there: it has no record, or stands at another place, or in
//     another generation (standingStep);
//   - its place starts again (startStep): a new run of the place begins, as a return to work
//     starts the phase it stood in again at a new entry (ReturnToWork); or
//   - its issue is out of Legion's work (record.InLegionsWork), and the run is not a merge that
//     can still land (mergeCanLand): a stop keeps the issue's phase (R20 of
//     dispatch://INFRA-3320), so the stop, not a move of the phase, is what ends the run.
//
// The point is SettleFact, which intake.ApplyFact runs after a fact's last handler, in the fact's
// own transaction and before its commit, and which the boot reconcile's transaction runs the same
// way (intake.OpenSettlers), for the admission pass it applies. It reads the runs of the issues the
// fact touched — those whose record or pull request it wrote, which the store keeps
// (record.WithIssueWrites) — and of no other issue, so what a fact reads grows with what it
// changed, never with the store. A start ends the over runs of its own issue first (startStep), so
// the end of the run an issue leaves reaches Dispatch before the start of the run it moves into.
//
// What the record does not hold, a fact names: the result of a move of the table and the step its
// work returns to (nameMoveEnd). Every other end is stopped, so a way out added later that names
// nothing still ends its run.

// runAt is one run of a step: its issue, the issue's generation, and the place of its step.
type runAt struct {
	issue      string
	generation uint64
	place      record.StepPlace
}

// factRuns is what one fact names of the runs it ends that the record does not hold: the end each
// move of the table names for the run it leaves. unrested is each ancestor a root stopped resting
// on in the fact (restEnded), which writes no record of the ancestor. settled says the fact's runs
// are settled already: an engine and the admission that holds it share one fact.
type factRuns struct {
	ends     map[runAt]StepEnd
	unrested []string
	settled  bool
}

// factRunsKey is the context key of a fact's factRuns.
type factRunsKey struct{}

// errOutsideFact is a move named or a step started in a transaction no fact opened (OpenFact): its
// run's end would be decided nowhere.
var errOutsideFact = errors.New("a step's run is moved or started outside a fact's settle (workflow.Engine.OpenFact)")

// factRunsOf is the factRuns ctx carries, or errOutsideFact.
func factRunsOf(ctx context.Context) (*factRuns, error) {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok {
		return nil, errOutsideFact
	}
	return runs, nil
}

// endOf is the end the fact named for run, and stopped where it named none.
func (runs *factRuns) endOf(run runAt) StepEnd {
	if end, ok := runs.ends[run]; ok {
		return end
	}
	return stepStopped
}

// OpenFact answers ctx carrying what the engine keeps of one fact while its handlers apply it: the
// ends its moves name, the issues its writes touch (record.WithIssueWrites) and the rows it queues
// with the fence their issue stood at then (record.WithFencedRows). A ctx that carries them
// already, opened by another engine of the same fact, is answered as it is (intake.Settler).
func (e *Engine) OpenFact(ctx context.Context) context.Context {
	if _, ok := ctx.Value(factRunsKey{}).(*factRuns); !ok {
		ctx = context.WithValue(ctx, factRunsKey{}, &factRuns{ends: map[runAt]StepEnd{}})
	}
	return record.WithFencedRows(record.WithIssueWrites(ctx))
}

// SettleFact settles the record the fact left, once its last handler has applied it, in its
// transaction (intake.Settler). First each parent held before its retro, in each project of an
// issue the fact wrote or of an ancestor a root stopped resting on in the fact (restEnded), has its
// retro started where nothing holds it any longer (releaseHeldParents). Then each run the fact
// left over ends (runOver): the runs of each issue the fact touched, the released parents among
// them. Last, every row the fact queued takes the fence its issue stands at now
// (record.Store.RefenceFactRows): a fact's own moves raise an issue's phase entry after rows of it
// are queued — a transition's status before its start, a return's notice before its worker's new
// entry — and each such row is the entry's the fact leaves the issue at, so none is a late effect
// of the entry it was queued beside. A second settle of one fact settles nothing.
func (e *Engine) SettleFact(ctx context.Context, tx pgx.Tx) error {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok || runs.settled {
		return nil
	}
	runs.settled = true
	if err := e.releaseHeldParents(ctx, tx, slices.Concat(record.IssuesWritten(ctx), runs.unrested)); err != nil {
		return err
	}
	if err := e.settleRuns(ctx, tx, runs, record.IssuesWritten(ctx), nil); err != nil {
		return err
	}
	return e.store.RefenceFactRows(ctx, tx)
}

// nameMoveEnd names the end of the run issue stands in on w, the walk of its run, that a move on
// trigger into to ends: the table's own result for the trigger, the step the work returns to, and
// the report handoff names (endOfTransition). The move names it on the issue as it stood, before
// the write that moves it.
func (e *Engine) nameMoveEnd(ctx context.Context, w Walk, issue record.Issue, trigger TriggerKind, to phase.Phase, handoff record.PhaseRow) error {
	step, ok := standingStep(w, issue)
	if !ok {
		return nil
	}
	return e.nameEnd(ctx, issue, step, endOfTransition(w, trigger, step.at, to, handoff))
}

// nameEnd names end as the end of the run of step, the step issue stands in, which the fact's
// settle writes (settleRuns): a move of the table's (nameMoveEnd), and the stop of a task, which
// ends its step as the stop says (stopTask).
func (e *Engine) nameEnd(ctx context.Context, issue record.Issue, step standing, end StepEnd) error {
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	runs.ends[runAt{issue: issue.Key, generation: issue.Generation, place: step.place}] = end
	return nil
}

// restEnded names ancestor as one a root stopped resting on in this fact (dispatch://INFRA-3320
// R8): the root left the workflow (tellRestingLeft), or its step's wait on ancestor's approval
// request ended (tellPendingEnded). A parent held before its retro waits for each root still in
// the workflow that rests on it (StartHeldRetro), and neither end writes a record of the
// ancestor, so the fact's settle reads the parents held before their retro in the ancestor's
// project as it reads those in the project of an issue the fact wrote (releaseHeldParents). A
// transaction no fact opened has no settle to read it in, and names nothing: the next admission
// pass of the held parent's project reads it again (admit's childrenWaits).
func (e *Engine) restEnded(ctx context.Context, ancestor string) {
	if runs, ok := ctx.Value(factRunsKey{}).(*factRuns); ok && !slices.Contains(runs.unrested, ancestor) {
		runs.unrested = append(runs.unrested, ancestor)
	}
}

// settleStart ends, before a start of place on issue, each of the issue's runs that is over, the
// run of that place among them: so the end of the run the issue leaves, and of the run its place
// held, comes before the start that follows them.
func (e *Engine) settleStart(ctx context.Context, tx pgx.Tx, issue string, place record.StepPlace) error {
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	return e.settleRuns(ctx, tx, runs, []string{issue}, &runAt{issue: issue, place: place})
}

// settleRuns ends each open run of keys that is over (runOver), with the end the fact named for it
// or stopped. starting is the place a start of one issue is about to begin (its generation unread),
// whose settle reads that issue alone (settleStart); nil is the fact's own settle (SettleFact). It
// alone ends a run (endStep, and endOffWalk for a run on no step of its walk, which both write the
// end through endRun). Every run it can end has an open row: a start writes it, and the daemon's
// boot writes it for each run no start of this release counted (OpenUncountedRuns), before any
// fact applies. It ends each run whose end rule holds, so an issue may have several runs open at
// once, each ended on its own.
func (e *Engine) settleRuns(ctx context.Context, tx pgx.Tx, runs *factRuns, keys []string, starting *runAt) error {
	issues := map[string]*record.Issue{}
	read := func(key string) (*record.Issue, error) {
		if issue, ok := issues[key]; ok {
			return issue, nil
		}
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return nil, err
		}
		issues[key] = issue
		return issue, nil
	}
	var order []string
	for _, key := range keys {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	stored, err := e.store.StepRuns(ctx, tx, order)
	if err != nil {
		return err
	}
	for _, row := range stored {
		if row.Run != record.RunOpen {
			continue
		}
		run := runAt{issue: row.Issue, generation: row.Generation, place: row.Place}
		now, err := read(row.Issue)
		if err != nil {
			return err
		}
		restarted := starting != nil && starting.issue == row.Issue && starting.place == row.Place
		ended, err := e.runOver(ctx, tx, run, now, restarted)
		if err != nil {
			return err
		}
		if !ended {
			continue
		}
		stood := record.Issue{Key: row.Issue}
		if now != nil {
			stood = *now
		}
		// The run's own walk, at its generation, names the phase its place is the step of, so the
		// end is written for the step of that run's plan, whichever run the issue stands in now.
		stood.Generation, stood.Hold = row.Generation, nil
		w, err := e.walk(ctx, tx, stood)
		if err != nil {
			return err
		}
		var onWalk bool
		if stood.Phase, onWalk = phaseAt(w, row.Place); !onWalk {
			if err := e.endOffWalk(ctx, tx, row, runs.endOf(run)); err != nil {
				return err
			}
			continue
		}
		if err := e.endStep(ctx, tx, stood, w, runs.endOf(run)); err != nil {
			return err
		}
	}
	return nil
}

// endOffWalk ends row, an open run that is over and whose place no step of the walk of its own
// generation holds: the plan rows that named its step are gone, as the way back of a rollback
// drops them while a task's step runs, so no standing names its step, and endStep, which writes
// the end of the step a standing names, would write nothing and leave the run open for good. Its
// end is the record its start kept (record.IssueStepRun.Kept), written by endRun, the writer of
// endStep's ends too, so the two writes of the run agree. The log says why it ended. A row with no
// kept start's record breaks the store invariant: every writer of a row keeps the write in its
// transaction, so ending it without a record would mark it ended while Dispatch never learned it
// started.
func (e *Engine) endOffWalk(ctx context.Context, tx pgx.Tx, row record.IssueStepRun, end StepEnd) error {
	if row.Kept.ID == "" {
		return fmt.Errorf("end the off-walk run of %s %s/%s/%d: its open start row holds no kept record", row.Issue, row.Place.Task, row.Place.Kind, row.Place.Place)
	}
	ended, err := e.endRun(ctx, tx, row.Issue, row.Place, end, func(record.StepStart) record.StepRecord { return row.Kept })
	if err != nil || !ended {
		return err
	}
	e.logOnCommit(ctx, "workflow: a run on no step of the walk of its run is ended", "issue", row.Issue, "generation", row.Generation,
		"task", row.Place.Task, "kind", row.Place.Kind, "place", row.Place.Place, "result", end.Result,
		"reason", "no step of the walk of its run holds its place: the plan rows that named its step are gone")
	return nil
}

// runOver is the end rule, the one answer to whether run is over. now is its issue as it stands
// (nil for none) and restarted whether a start of its place is about to begin. A run is over when
// its issue has no record, or stands at another place on the walk of its run or in another
// generation (standingStep, which an issue its plan holds stands in no step of); when a start of
// its place begins; or when its issue is out of Legion's work, unless it is a merge that can still
// land (mergeCanLand).
func (e *Engine) runOver(ctx context.Context, tx pgx.Tx, run runAt, now *record.Issue, restarted bool) (bool, error) {
	if restarted || now == nil {
		return true, nil
	}
	if now.Generation != run.generation {
		return true, nil
	}
	w, err := e.walk(ctx, tx, *now)
	if err != nil {
		return false, err
	}
	if step, ok := standingStep(w, *now); !ok || step.place != run.place {
		return true, nil
	}
	if record.InLegionsWork(*now) {
		return false, nil
	}
	if !awaitsMerge(*now) {
		return true, nil
	}
	canLand, err := e.mergeCanLand(ctx, tx, *now)
	return !canLand, err
}

// uncountedRunOpen is the boot's answer (OpenUncountedRuns) to whether issue stands in an open run
// that no start of this release counted, and that run: issue stands in a step whose runs are
// recorded on the walk of its run (standingStep), the end rule does not hold that run over
// (runOver), the issue is not the operator's, which runs no step of the workflow
// (record.OperatorHolds), and its generation started a run (generationStarted).
func (e *Engine) uncountedRunOpen(ctx context.Context, tx pgx.Tx, issue record.Issue) (runAt, bool, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return runAt{}, false, err
	}
	step, ok := standingStep(w, issue)
	if !ok {
		return runAt{}, false, nil
	}
	run := runAt{issue: issue.Key, generation: issue.Generation, place: step.place}
	over, err := e.runOver(ctx, tx, run, &issue, false)
	if err != nil || over {
		return run, false, err
	}
	operator, err := record.OperatorHolds(ctx, e.store, tx, issue)
	if err != nil || operator {
		return run, false, err
	}
	started, err := e.generationStarted(ctx, tx, issue)
	return run, started, err
}

// generationStarted says whether the run issue stands in has started. It has not while the issue
// waits for the promotion that starts it (record.AwaitsPromotion): a root a person handed to Legion
// that waits in the line, whose first start is that promotion's. Every other issue that stands in a
// step started its run there.
func (e *Engine) generationStarted(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	gates, err := e.store.GatesOf(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	holds := slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == issue.Key })
	return !record.AwaitsPromotion(issue, record.TreeGate{Steps: e.cfg.Steps, Rows: gates}, holds), nil
}

// mergeCanLand says whether the merge of member, out of Legion's work while it awaits its landing,
// can still land: while its pull request is open, a person can merge it, and the merge is what ends
// the step. The pull request's state is all it reads, so it reads the row alone
// (record.Store.PullRequestRow), with none of what the pull request remembers of its settled
// heads.
func (e *Engine) mergeCanLand(ctx context.Context, tx pgx.Tx, member record.Issue) (bool, error) {
	pr, err := e.store.PullRequestRow(ctx, tx, member.Key)
	if err != nil {
		return false, err
	}
	return pr != nil && pr.State == record.PullRequestOpen, nil
}

// phaseAt is the phase whose step runs at place on w, the walk of the run: of its line (Walk.Line,
// phase.Order for a run with no plan rows), the phase whose step (placeOnWalk) is at place.
func phaseAt(w Walk, place record.StepPlace) (phase.Phase, bool) {
	for _, p := range w.Line {
		if at, _, _, ok := placeOnWalk(w, p); ok && at == place {
			return p, true
		}
	}
	return "", false
}
