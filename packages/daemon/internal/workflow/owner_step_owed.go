package workflow

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// owedStep is a step or a retro of the owner's that an issue stands in and whose run started and
// has not ended (owedOwnersStep): the step, its open run and the task the run's entry holds.
type owedStep struct {
	step standing
	run  record.IssueStepRun
	task string
}

// owedOwnersStep is the step of the owner's that issue stands in (standingStep, standing.owner)
// whose run started at the issue's generation and has no end yet, with the task the entry of that
// run holds: the text the start told the owner (startStep), which enter wrote on the step's
// phases row (record.PhaseRow.EntryTask). It answers false where nothing is owed: the issue has
// left the workflow; it stands in no step of the owner's (a worker's or a fresh agent's place, a
// held issue, one its plan holds, or a run with no plan rows, whose acceptance the reviewer's
// phase-finished tells as before); the run there has ended, since the owner's report or a move
// ended it; or an owner-step of that run is still queued (record.Store.OwnerStepPending), which
// tells the owner when it publishes, so a second would only repeat it. An owner-step of another
// run holds back nothing: the executor finishes it undelivered when it runs.
//
// The owner's step is the one place of a task that no worker holds and no start re-sends: its
// one notice told the owner, and a notice that reached no turn — dropped, or delivered to an
// agent that died or lost its session before reading it — leaves the step waiting with nothing
// that will tell anyone. Its two readers tell it again: the periodic pass over an owner idle past
// the silence bound (ownerSilent) and the owner's catch-up at a launch whose session was not given
// the notice (claimReady, toldThisSession). Both tell through the one writer of the notice
// (tellOwner), which goes to the issue's own architect, so a child's step reaches the child's own
// architect.
func (e *Engine) owedOwnersStep(ctx context.Context, tx pgx.Tx, issue record.Issue) (owedStep, bool, error) {
	if !InWorkflow(issue) {
		return owedStep{}, false, nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return owedStep{}, false, err
	}
	step, ok := standingStep(w, issue)
	if !ok || !step.owner {
		return owedStep{}, false, nil
	}
	runs, err := e.store.StepRuns(ctx, tx, []string{issue.Key})
	if err != nil {
		return owedStep{}, false, err
	}
	open := slices.IndexFunc(runs, func(run record.IssueStepRun) bool {
		return run.Place == step.place && run.Run == record.RunOpen && run.Generation == issue.Generation
	})
	if open < 0 {
		return owedStep{}, false, nil
	}
	if pending, err := e.store.OwnerStepPending(ctx, tx, issue.Key, stepRun(step.place, runs[open].Starts)); err != nil || pending {
		return owedStep{}, false, err
	}
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return owedStep{}, false, err
	}
	// The row is the one the step's start raised (startOwnersStep): its role is the role of the
	// place's worker (record.PlaceWorker), the plan's step's role at a step and the retro's role at
	// the retro, at the step's own place of the task.
	worker, _ := w.workerAt(w.Place)
	var task string
	if row, found := record.StepRowOf(rows, worker.Role, step.place.Task, step.place.Place); found == record.RowOfTheStep {
		task = row.EntryTask
	}
	if task == "" {
		if task, err = e.resumeTaskOn(ctx, tx, issue, w); err != nil {
			return owedStep{}, false, err
		}
	}
	return owedStep{step: step, run: runs[open], task: task}, true, nil
}

// toldThisSession says whether an owner-step of the run owed names already reached the owner of
// issue as its claim is ready now at launch (record.Store.OwnerStepTold, which the outbox writes
// as the listener takes the notice), by the one rule of told (toldNow). A run no owner-step reached
// is told again too. A delivered notice row is deleted, so the step's row is the one record of it.
func (e *Engine) toldThisSession(ctx context.Context, tx pgx.Tx, issue record.Issue, owed owedStep, launch uint64) (bool, error) {
	told, err := e.store.OwnerStepTold(ctx, tx, issue.Key, owed.step.place)
	if err != nil {
		return false, err
	}
	return toldNow(ctx, tx, e.store, issue, told, launch)
}

// toldNow is the one rule of whether a message whose mark is told (record.OwnerTold: an owner-step
// of a step's run, a merge forward's ask) reached the owner of issue as its claim is ready now at
// launch: published to the session the claim runs in now, whose transcript holds the message, at
// this launch or at an earlier one this launch resumed. The mark names the session the listener
// forwarded to, and a launch only where the claim the outbox read ran in that session (the
// daemon's ownerToldMark). A mark that names a session is decided by that session alone: a fresh
// session — after a loss, or in the same launch, whose resume of the session told failed — was not
// given the message, and the catch-up tells it again. Only a mark with no session, where the claim
// the outbox read recorded none, is decided by its launch. A mark that names nobody reached nobody.
func toldNow(ctx context.Context, tx pgx.Tx, store record.Store, issue record.Issue, told record.OwnerTold, launch uint64) (bool, error) {
	if told == (record.OwnerTold{}) {
		return false, nil
	}
	if told.Session == "" {
		return told.Launch == launch, nil
	}
	owner, err := record.OwnerClaim(issue)
	if err != nil {
		return false, nil
	}
	session, err := store.ClaimSession(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	return told.Session == session, nil
}

// tellOwedStep tells the owner of issue again the step owed names, with the task its entry holds,
// through the one writer of the notice (tellOwner), and logs it as the fact commits. by names the
// reader that found the step owed, for the log line.
func (e *Engine) tellOwedStep(ctx context.Context, tx pgx.Tx, issue record.Issue, owed owedStep, by string) error {
	if err := e.tellOwner(ctx, tx, issue, owed.step, owed.run.Starts, owed.task); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the owner of a step that started and has not ended is told it again",
		"issue", issue.Key, "task", owed.step.place.Task, "kind", owed.step.place.Kind, "place", owed.step.place.Place,
		"started", owed.run.StartedAt, "by", by)
	return nil
}

// ownerSilent applies one intake.WorkerSilent of an architect's claim (the daemon's
// tellSilentPhaseWorkers mints one for an architect idle past the silence bound since both its
// last step and the start of the open run of its issue, keyed on that run): the owner of a step
// that started and has not ended (owedOwnersStep) is told that step again, once for each such
// silence. It decides on the record read inside the fact's own transaction, as workerSilent does.
// The claim is the issue's own architect (record.OwnerClaim), the one the notice goes to: up and
// between turns, in no update round, with no step after the one the sweep read and no report of
// its own session explaining its quiet, which leaves the fact unspent as workerSilent leaves it.
// A run that started within the bound is another run than the one the sweep read: the start told
// that one, and its own silence is a later fact's.
func (e *Engine) ownerSilent(ctx context.Context, tx pgx.Tx, fact intake.WorkerSilent) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	owner, err := record.OwnerClaim(*issue)
	if err != nil {
		return intake.Result{}, nil
	}
	run, err := e.store.RoleRun(ctx, tx, owner, issue.Key, claim.RoleArchitect, issue.Generation)
	if err != nil {
		return intake.Result{}, err
	}
	c := run.Claim
	if c == nil || (c.State != supervise.StateReady && c.State != supervise.StateIdle) || c.UpdateRound != "" ||
		c.Step.LastStep().After(fact.LastStep) {
		return intake.Result{}, nil
	}
	now := e.now()
	if ReportExplainsQuiet(c.ActivityBackground, c.ActivityNextReminderAt, now) {
		return intake.Result{Unspent: true}, nil
	}
	owed, ok, err := e.owedOwnersStep(ctx, tx, *issue)
	if err != nil || !ok {
		return intake.Result{}, err
	}
	if started := owed.run.StartedAt; !started.IsZero() && !PastBound(started, fact.Bound, now) {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.tellOwedStep(ctx, tx, *issue, owed, "the periodic pass over an idle owner")
}
