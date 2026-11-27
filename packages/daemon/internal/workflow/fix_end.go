package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The end of a fix, and the way after it. The implementer's completion or move back of the fix
// ends it (fixCompleted, fixMovedBack, endFix). A step that ends while the fix is open, or after it
// ended with its way not taken, moves the task as besideFix decides. Once no step of the task
// runs, the way after the fix decides which steps run again (fixWayOf, afterFix, moveFromHold).

// runningFixOf is the walk of issue's run and its task's fix number n, where that fix runs (the
// fix the issue record names, record.Issue.RunningFix, with its entry raised): the fix a
// completion or a move back of the implementer's claim names ends it. A fix that reads its job
// still, has ended, or was never run is no fix a completion ends: the completion is refused as one
// of an entry the issue has moved past (HANDOFF_STALE_ENTRY), changing nothing.
func (e *Engine) runningFixOf(ctx context.Context, tx pgx.Tx, issue record.Issue, n int) (Walk, *record.TaskFix, *intake.Refusal, error) {
	w, fix, err := e.fixNamed(ctx, tx, issue, n)
	if err != nil || fix != nil {
		return w, fix, nil, err
	}
	return w, nil, refused("HANDOFF_STALE_ENTRY", fmt.Sprintf("the implementer's completion of fix %d of task %s of %s is of a fix that runs no more; this completion changed nothing",
		n, w.Task, issue.Key)).Refusal, nil
}

// fixNamed is the walk of issue's run and its task's fix number n where that fix runs, nil where
// it does not (runningFixOf).
func (e *Engine) fixNamed(ctx context.Context, tx pgx.Tx, issue record.Issue, n int) (Walk, *record.TaskFix, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored || issue.RunningFix != n {
		return w, nil, err
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Fix != n || fix.State != record.FixRunning {
		return w, nil, err
	}
	return w, fix, nil
}

// fixCompleted ends fix, which the implementer's completion fact ended: the fix passed, at the
// commit the completion reports, or, for one that names none, at the task's head as it stands
// (taskHead), since only the fix pushes while it runs. Its run ends with the completion's report.
func (e *Engine) fixCompleted(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix *record.TaskFix, fact intake.HandoffComplete, pr *record.PullRequest) error {
	endHead := fact.Commit
	if endHead == "" {
		_, endHead = taskHead(pr)
	}
	return e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultPassed, Report: fact.HandoffRecord}, endHead)
}

// fixMovedBack applies the implementer's move back while it runs fix fact.Fix: the fix's report of
// a failure, taken whatever phase the issue stands in. It ends the fix failed at the task's head,
// returning the task to the step the move names, which stands before the fix's own step on the
// task's walk; the issue does not move now, since the step beside the fix goes on. It is refused
// BACKWARD_REFUSED, changing nothing, where the fix it names runs no more, where the step it names
// is not before the fix's, and where the pull request has merged, and it is refused as every move
// back is for a reason Dispatch would refuse and on a lingering tree.
func (e *Engine) fixMovedBack(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.BackwardMove) (intake.Result, error) {
	w, fix, err := e.fixNamed(ctx, tx, issue, fact.Fix)
	if err != nil {
		return intake.Result{}, err
	}
	if fix == nil {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("the implementer's move of %s to %s names fix %d of task %s, which runs no more, so it changed nothing",
			issue.Key, fact.To, fact.Fix, w.Task)), nil
	}
	to, ok := w.PlaceOf(fact.To)
	if !ok || to.Kind != record.PlaceStep || to.Position >= fix.Place {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("a fix's move back is its report of a failure, which returns task %s of %s to a step before the fix's own, step %d; %s is no such step, so the implementer's move of fix %d changed nothing",
			w.Task, issue.Key, fix.Place, fact.To, fix.Fix)), nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	if pr != nil && pr.State == record.PullRequestMerged {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("the implementer's move of fix %d of %s to %s changed nothing: its pull request has merged; a merged change never moves back",
			fix.Fix, issue.Key, fact.To)), nil
	}
	if code, why := backwardReasonRefusal(phase.Implementing, fact.To, mover{role: claim.RoleImplementer}, fact.Reason); code != "" {
		return refused(code, why), nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(issue, fmt.Sprintf("the implementer's move of fix %d to %s", fix.Fix, fact.To)), nil
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	_, head := taskHead(pr)
	return intake.Result{}, e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultFailed, ReturnedTo: &to.Position}, head)
}

// endFix is the one writer of a fix's end: the fix row ends (record.FixEnded) with end's result,
// at endHead, the commit the fix's own completion reports, or the task's head where its end
// reports none. EndHead against Head is one of the two readings of new commits that the way after
// the fix counts, the fix's own (fixWayOf); the other is the task's head against Head, a push the
// fix did not make. A fix that failed returns the task to the earlier of the step a step beside it
// returned the task to and the step the fix's own report names (earlierReturn). The run of the
// step the fix ran again ends with end (endRun), its report and its return among it. The fix's own
// end decides what runs after it (afterFix), which waits while another step of the task runs.
func (e *Engine) endFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix *record.TaskFix, end StepEnd, endHead string) error {
	now := e.now()
	fix.State, fix.Result, fix.EndHead, fix.EndedAt = record.FixEnded, string(end.Result), endHead, &now
	end.Commit = endHead
	if end.Result == ResultFailed {
		fix.Failed = true
		if end.ReturnedTo != nil {
			fix.ReturnTo = earlierReturn(fix.ReturnTo, *end.ReturnedTo)
		}
	}
	if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
		return err
	}
	place := record.TaskPlace{Kind: record.PlaceStep, Position: fix.Place}
	if role, typ, ok := stepOfWalk(w, place); ok && recordsSteps(issue) {
		at := place.StepPlace(w.Task)
		drawn, err := e.variantOf(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if _, err := e.endRun(ctx, tx, issue.Key, at, end, func(start record.StepStart) record.StepRecord {
			return stepWrite(issue, drawn, at, role, typ, start.Agent, start.Starts)
		}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: a fix ends", "issue", issue.Key, "task", fix.Task, "fix", fix.Fix, "result", fix.Result,
		"check", fix.Check, "head", fix.Head, "endHead", fix.EndHead, "returnTo", fix.ReturnTo, "phase", issue.Phase)
	return e.afterFix(ctx, tx, issue, w, nil)
}

// earlierReturn is the earlier of two places a task returns to, by place from 1, where 0 is none.
func earlierReturn(stored, reported int) int {
	if stored == 0 || (reported > 0 && reported < stored) {
		return reported
	}
	return stored
}

// The ways a fix's end takes (record.TaskFix.Way), after R7a's three: wayReturn returns the task
// to an earlier step, wayAfterFix runs again the steps after the fix's own, and wayOn leaves each
// step its result.
const (
	wayReturn   = 1
	wayAfterFix = 2
	wayOn       = 3
)

// rowMove is a move of the table that transition took no row for while a fix's way was pending
// (besideFix): afterFix makes it where the way leaves each step its result.
type rowMove struct {
	trigger TriggerKind
	target  phase.Phase
	handoff record.PhaseRow
	pr      *record.PullRequest
	why     moveWhy
}

// wayMove is a move the way after a fix makes (afterFix), which its context carries: moveTo is the
// head the working copy of the task moves to before the started step's task (moveToOf), for the
// first two ways the task's head or, where the fix made commits, where the branch stands after it
// (headAfterFix), and none for the third; its presence marks the move as the way's own, which
// besideFix lets through.
type wayMove struct{ moveTo string }

// wayMoveKey is the context key of a wayMove.
type wayMoveKey struct{}

// moveToOf is the head the start of a step moves the task's working copy to: the head the first
// two ways after a fix chose (wayMove), "" for every other start.
func moveToOf(ctx context.Context) string {
	move, _ := ctx.Value(wayMoveKey{}).(wayMove)
	return move.moveTo
}

// copyMoveAfterFix is the head a start of a step of w's task moves the task's working copy to, and
// the task's latest fix whose move that start carries, nil for none, which start marks carried
// (record.TaskFix.MoveCarried) in the transaction that writes the start. The fix pushes from a
// working copy of its own, so the task's copy stands where the fix started until a start after the
// fix moves it; a later start finds the copy there or past it, where a move could take it back from
// a commit made since. So one start carries each fix's move, and the fix's mark, not any moment,
// records which: the way's own start, which moves the copy to the head the way chose (moveToOf, the
// first two ways), and otherwise the first start after the fix ended with commits of its own (its
// end is not the head it started at), which moves the copy to where the branch stands after it
// (headAfterFix). That is a restart of the task after a stop, a leave or a new generation ended the
// fix (wayNone), the retro, the merge or a step after a way that left each step its result (wayOn),
// and a start while the way is pending.
// A start after the one that carried the move answers "", and so does every start of a task that
// has had no fix, or whose latest fix is open or ended with no commit of its own outside a way's
// start.
func (e *Engine) copyMoveAfterFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (string, *record.TaskFix, error) {
	moveTo := moveToOf(ctx)
	if !w.Stored {
		return moveTo, nil, nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	switch {
	case err != nil || fix == nil || fix.State != record.FixEnded:
		return moveTo, nil, err
	case fix.MoveCarried:
		return "", nil, nil
	case moveTo != "":
		return moveTo, fix, nil
	case fix.EndHead == "" || fix.EndHead == fix.Head:
		return "", nil, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return "", nil, err
	}
	return headAfterFix(*fix, pr), fix, nil
}

// headAfterFix is where the task's branch stands after fix, which made commits of its own (its end
// is not the head it started at): the copy follows the branch from the fix's end by the moves the
// record holds, and stands at the fix's end where no move leads from it to the record's head.
// Only the two commits' ancestry says which holds the other, and the engine reads no repository,
// so it reads the record's moves (record.PullRequest.Moves): each the two heads of one push of the
// task's branch, recorded as the push and its synchronize arrive, the last classify.maxMoves of
// them.
//
// What that gives, case by case. A push over the fix's end moves the copy to it, and it holds the
// fix's commits. A reset or a force push to a commit before the fix's end moves the copy there
// too, and the fix's commits are gone from it: that is the branch, and a step that started at the
// fix's end would push them back. The fix's own completion can reach the engine before the
// synchronize of its end push, so the record's head can be a commit pushed before that end — a
// person's push the fix's take rebased onto — and no move leads from the end to it, so the copy
// stands at the end, which holds both. A synchronize that waits for its push makes no move
// (classify's advance), so a start in that window stands at the fix's end as well, and the step
// meets that push when it pushes.
func headAfterFix(fix record.TaskFix, pr *record.PullRequest) string {
	if _, head := taskHead(pr); head != "" && movedFrom(pr.Moves, fix.EndHead, head) {
		return head
	}
	return fix.EndHead
}

// movedFrom says whether the moves lead from the head from to the head to: to is from, or a chain
// of moves, each from the head the one before it moved to, goes from from to to.
func movedFrom(moves []record.Move, from, to string) bool {
	reached := map[string]bool{to: true}
	for grew := true; grew && !reached[from]; {
		grew = false
		for _, move := range moves {
			if reached[move.SHA] && !reached[move.Before] {
				reached[move.Before], grew = true, true
			}
		}
	}
	return reached[from]
}

// besideFix takes, in place of the row transition found, a move of a task whose latest fix is open
// or has ended with its way not taken, and answers whether it took the move; every other move is
// the row's.
//
// Beside an open fix, a step that passed moves the task on as the row says. A step that failed
// starts no step: its run ends failed, returning to the step the row returns to; where that step
// is the fix's own, the fix takes the report (GotReport) and the implementer is told of it, and
// otherwise the fix keeps the earliest step the task returns to (ReturnTo). The task is then held
// for its fix from the phase of the step that failed, which keeps that phase's slot, stops no
// claim, and moves nothing: the way after the fix makes the move (afterFix).
//
// After a fix ended while another step ran, the step that ends now has its end written as the row
// would write it, a failure's return kept on the fix, and then afterFix decides the way in place
// of the row, which it makes itself where each step keeps its result. So no step starts, and no
// retro starts, before the way is decided.
//
// Either way a review that requested changes counts its round here, as transition counts it; its
// return is counted by the way after the fix, which is the one return. A move the way after a fix
// makes itself is the row's (wayMove).
func (e *Engine) besideFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, trigger TriggerKind, target phase.Phase, row Row,
	handoff record.PhaseRow, pr *record.PullRequest, why moveWhy) (bool, error) {
	if _, way := ctx.Value(wayMoveKey{}).(wayMove); way || !w.Stored {
		return false, nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Way != 0 {
		return false, err
	}
	open := fix.State == record.FixReading || fix.State == record.FixRunning
	_, returns := endOf(trigger)
	step, stands := standingStep(w, issue)
	if (open && !returns) || !stands {
		return false, nil
	}
	if trigger == TriggerReviewRejected || (returns && step.typ == plan.TypeReview) {
		if err := e.recordRound(ctx, tx, issue, w.Task, pr, nil); err != nil {
			return false, err
		}
	}
	end := endOfTransition(w, trigger, step.at, row.To, handoff)
	// The failed step's branch beside the fix is the one its start was bound to, read before its
	// run ends.
	stepStart, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return false, err
	}
	gotReport := false
	if end.ReturnedTo != nil {
		if gotReport = open && *end.ReturnedTo == fix.Place; gotReport {
			fix.GotReport = true
			// A fix that still reads its job has raised no entry, so no message can name it: the
			// report waits on the fix's row, and the task of its start speaks it (fixTask).
			if fix.State == record.FixReading {
				fix.Reports += " " + reportSentence(step, handoff, stepStart.PushBranch)
			}
		} else {
			fix.ReturnTo = earlierReturn(fix.ReturnTo, *end.ReturnedTo)
		}
		if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
			return false, err
		}
	}
	drawn, err := e.variantOf(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	if _, err := e.endRun(ctx, tx, issue.Key, step.place, end, func(start record.StepStart) record.StepRecord {
		return stepWrite(issue, drawn, step.place, step.role, step.typ, start.Agent, start.Starts)
	}); err != nil {
		return false, err
	}
	if !open {
		return true, e.afterFix(ctx, tx, issue, w, &rowMove{trigger: trigger, target: target, handoff: handoff, pr: pr, why: why})
	}
	if gotReport && fix.State == record.FixRunning {
		if err := e.reportToFix(ctx, tx, issue, w, *fix, step, handoff, pr, stepStart.PushBranch); err != nil {
			return false, err
		}
	}
	from := issue.Phase
	issue.Phase, issue.Hold = phase.Held, &record.Hold{From: from, Reason: record.HoldForFix}
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return false, err
	}
	e.logOnCommit(ctx, "workflow: a failed step beside a fix waits for the fix", "issue", issue.Key, "task", w.Task, "fix", fix.Fix,
		"from", from, "trigger", trigger, "gotReport", gotReport, "returnTo", fix.ReturnTo)
	return true, nil
}

// reportSentence is what the fix is told of the report of step, a step beside it that failed and
// returned the task to the fix's own step, handoff being that step's completion, and branch the
// step's own branch beside the fix that its start was bound to, where its commits are ("" for
// none), which the fix takes before its end push: the one wording of the message a running fix
// gets (reportToFix) and of the task of the start of a fix that got the report before (fixTask).
// The step's summary is its own text, so it is quoted (quotedList): one value on one line, which
// nothing in it can turn into the daemon's words.
func reportSentence(step standing, handoff record.PhaseRow, branch string) string {
	body := fmt.Sprintf("The %s of step %d reports: %q. Its report is %s. It is part of your fix.", step.role, step.place.Place, handoff.Summary, handoff.HandoffRecord)
	if branch != "" {
		body += fmt.Sprintf(" Its commits are on its own branch %s: take them before your end push, as your task says.", branch)
	}
	return body
}

// reportToFix gives fix, which runs, the report of step (reportSentence): one step-report row to
// the implementer, for the fix's work (its number and entry), so the outbox wakes a released fix
// and finishes the row undelivered once the fix ended.
func (e *Engine) reportToFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, step standing, handoff record.PhaseRow,
	pr *record.PullRequest, branch string) error {
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	body := reportSentence(step, handoff, branch)
	number := 0
	if pr != nil {
		number = pr.Number
	}
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.CINews{Purpose: record.CINewsStepReport, Role: string(to.token), Generation: issue.Generation,
			Task: w.Task, Repo: prRepo(issue, pr), PullRequest: number, Head: fix.Head, Reader: fixReader(fix),
			Branch: branch, Body: body}); err != nil {
			return err
		}
	}
	return nil
}

// taskStepRuns says whether a step of w's task runs: the latest run of one of its places is open.
func (e *Engine) taskStepRuns(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (bool, error) {
	for place := range w.Steps {
		start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: place + 1})
		if err != nil || start.Run == record.RunOpen {
			return err == nil, err
		}
	}
	return false, nil
}

// fixWayOf is the way fix's end takes (R7a's three ways) on a task of steps steps, and the place
// of the step the task returns to for the first two. taskHead is the head of the task's branch
// when the way is taken, "" where the record holds none.
//
// The commits the way counts are the task's, not the fix's own alone: a branch that no longer
// stands at the head the fix's check failed at carries work the steps after the fix have not
// seen, whether the fix pushed it or anyone else did while the fix ran. Two readings say so, and
// either is enough. The fix's own end (EndHead) answers before the synchronize of its end push
// has moved the record's head, and taskHead, the head of the task's branch when the way is taken
// ("" where the record holds none), answers for a push the fix did not make. The first way: the
// fix failed, or a step beside it failed to a step other than the fix's, so the task returns to
// the earliest such step, or, where the branch holds new commits, to the step after the fix where
// that is earlier. The second: the branch holds new commits, or the fix got a failed step's
// report, so the steps after it run again. The third, in each other case, and where no step comes
// after the fix's: each step keeps its result.
func fixWayOf(fix record.TaskFix, steps int, taskHead string) (way, place int) {
	next := fix.Place + 1
	madeCommits := (fix.EndHead != "" && fix.EndHead != fix.Head) || (taskHead != "" && taskHead != fix.Head)
	switch {
	case fix.Failed || fix.ReturnTo > 0:
		place = fix.ReturnTo
		if place == 0 || (madeCommits && next < place) {
			place = next
		}
		way = wayReturn
	case madeCommits || fix.GotReport:
		way, place = wayAfterFix, next
	}
	if way == 0 || place > steps {
		return wayOn, 0
	}
	return way, place
}

// afterFix takes the way after the latest fix of w's task once (R7a's three ways): only where that
// fix has ended, its way is not taken (Way 0, never wayNone), and no step of the task runs; it
// does nothing otherwise, and the end of the last step running beside the fix calls it again
// (besideFix). It writes the way on the fix row in the transaction of its act, so the way is taken
// once. The first two ways return the task, held or not, by the one move out of a hold
// (moveFromHold), at the head the started step's working copy moves to: the task's head, or where
// the fix made commits, where the branch stands after it (headAfterFix). The third
// makes pending, the move of the table transition took no row for, where there is one. After the
// way's act, in its transaction, the conflict that waited while the fix held the task's branch is
// read again (mergeForwardAfterFix).
func (e *Engine) afterFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pending *rowMove) error {
	if !w.Stored {
		return nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.State != record.FixEnded || fix.Way != 0 {
		return err
	}
	if runs, err := e.taskStepRuns(ctx, tx, issue, w); err != nil || runs {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	_, head := taskHead(pr)
	way, place := fixWayOf(*fix, len(w.Steps), head)
	fix.Way = way
	if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
		return err
	}
	// The head the started step's working copy moves to: the task's head, and where the fix made
	// commits of its own, where the branch stands after it (headAfterFix), which is that head
	// where the record's moves lead to it from the fix's end and the fix's end otherwise. No guard
	// on an empty head: headAfterFix answers the fix's end there, which is the head the copy must
	// move to, and the log below names it.
	moveTo := head
	if fix.EndHead != "" && fix.EndHead != fix.Head {
		moveTo = headAfterFix(*fix, pr)
	}
	e.logOnCommit(ctx, "workflow: the way after a fix is taken", "issue", issue.Key, "task", w.Task, "fix", fix.Fix, "way", way,
		"place", place, "moveTo", moveTo)
	if way == wayOn {
		if pending != nil {
			if err := e.transition(context.WithValue(ctx, wayMoveKey{}, wayMove{}), tx, issue, pending.trigger, pending.target, pending.handoff, pending.pr, pending.why); err != nil {
				return err
			}
		}
		return e.mergeForwardAfterFix(ctx, tx, issue.Key)
	}
	ctx = context.WithValue(context.WithValue(ctx, wayMoveKey{}, wayMove{moveTo: moveTo}), moverKey{}, mover{fix: fix.Fix})
	handoff, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
	if err != nil {
		return err
	}
	steps := w.steps()
	reason := fmt.Sprintf("fix %d of task %s, for check %s failed at %s, ended at %s", fix.Fix, w.Task, fix.Check, fix.Head, fix.EndHead)
	if err := e.moveFromHold(ctx, tx, issue, w, steps[place-1], steps[fix.Place-1], handoff, pr, reason); err != nil {
		return err
	}
	return e.mergeForwardAfterFix(ctx, tx, issue.Key)
}

// moveFromHold is the one move that returns a held task, and the move of the way after a fix
// (afterFix): where issue is held for its fix or its CI (fixOrCIHold), the phase the hold stands
// in is put back and the hold cleared in one write; then, where target is another phase than
// that one, the task moves back to target on TriggerBackward, which transition counts as a return
// by the pair of the two phases, firing the deployment's hooks there. Where target is the phase
// the issue stands in, no row moves the task: the step there starts again as a return to it, its
// task naming handoff's report, and the return is counted by the pair (rerunFrom, target), the act
// that sends the task back being the fix's, out of its step into the step after it.
func (e *Engine) moveFromHold(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, target, rerunFrom phase.Phase, handoff record.PhaseRow,
	pr *record.PullRequest, reason string) error {
	current, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || current == nil {
		return err
	}
	issue = *current
	// The phase the start below moves the issue from: held, where the hold is put back, as a
	// release of a hold that raises an entry writes it (record.PhaseRow.EntryFrom).
	from := issue.Phase
	if fixOrCIHold(issue) {
		issue.Phase, issue.Hold = record.StandsAt(issue), nil
		if err := e.WriteIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	if target != issue.Phase {
		return e.transition(ctx, tx, issue, TriggerBackward, target, handoff, pr, moveWhy{Reason: reason})
	}
	if err := e.recordPass(ctx, tx, issue, w.Task, rerunFrom, target, pr, false); err != nil {
		return err
	}
	role := RoleFor(target)
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	if err := e.start(ctx, tx, issue, w, role, func(at Standing) string { return task(issue, at, handoff, pr, reason) }, from, true); err != nil {
		return err
	}
	return e.queueMovedNews(ctx, tx, issue)
}
