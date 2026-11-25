package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// backInWork is the engine's reaction to a newer status of a recorded issue that is not out of
// the workflow (dispatchIssue): Dispatch shows the issue todo, in_progress, testing, needs_review
// or retro, whoever wrote it (R20 of dispatch://INFRA-3320: the daemon acts on each issue's own
// status). Only two of decision 8's cases come through here and open a new run (NewRun):
//
//   - a todo on a record at phase done: an issue the daemon closed, or one the release before
//     left there when it left the workflow;
//   - a move out of Dispatch's done into any status in work of a record at phase done whose run's
//     pull request is recorded merged: the reopen of an issue its merge closed (opensRunOnReopen).
//
// The third, rerun_child on a child Dispatch shows in backlog or icebox, opens its new run in the
// call's own fact, whoever wrote that status, so the daemon keeps no mark of who stopped an issue.
//
// Any other status in work on a record at phase done opens nothing, and the issue stays out of
// Legion's work (R15): R25's fix moving an issue the daemon closed into in_progress is one. An
// issue a stop left in its phase that Dispatch shows in work again returns to work at a new entry
// (ReturnToWork), whoever stopped it and whoever wrote the status in work: a person's todo and
// release_children's alike.
//
// An issue already in Legion's work moves only on a todo: a todo on a held issue releases its hold
// (releaseHeldRoot), and a todo on a root whose architect claim failed starts that claim again
// (retryFailedArchitect). Any other status in work changes nothing here; admission records it.
func (e *Engine) backInWork(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	switch {
	case issue.Phase == phase.Done:
		opens, err := e.opensNewRun(ctx, tx, issue, fact)
		if err != nil || !opens {
			return err
		}
		return e.NewRun(ctx, tx, issue, fact)
	case !record.InLegionsWork(issue):
		return e.ReturnToWork(ctx, tx, issue, fact)
	case fact.Status != "todo":
		return nil
	case issue.Phase == phase.Held && issue.Hold != nil:
		return e.releaseHeldRoot(ctx, tx, issue, fact, false)
	}
	return e.retryFailedArchitect(ctx, tx, issue)
}

// opensNewRun says whether fact, a status in work, opens a new run of issue, a record at phase
// done (decision 8 of dispatch://INFRA-4730): a todo does, and so does a move out of Dispatch's
// done into any status in work while the pull request of the run that ended is recorded merged,
// the reopen of an issue its merge closed. The pull request the record holds is that run's: a new
// run clears every pull request of its issue that is not open (record.Store.ClearGeneration), and
// its own Generation counts CI settlements, not runs. A move from another status into work, such
// as R25's fix moving an issue from the backlog to in_progress, opens nothing.
func (e *Engine) opensNewRun(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if fact.Status == "todo" {
		return true, nil
	}
	if issue.DispatchStatus != "done" {
		return false, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return false, err
	}
	return pr.State == record.PullRequestMerged, nil
}

// ReturnToWork takes back into Legion's work an issue a stop left in its phase (leave) that
// Dispatch shows in work again, whoever wrote the status (R20 of dispatch://INFRA-3320). It
// resumes the phase it stood in with the same generation, its sessions and its run: its pull
// request, branch, phase records, design gate and, in awaiting_children, the start it holds
// (record.Issue.ChildrenWait, R13), and its task, which the stop left stopped at its step and
// which runs again there (resumeStoppedTask). Its record takes the observation and the status of
// the phase it stands in, which is queued to Dispatch where it differs from what Dispatch shows
// (Engine.status), as a move into that phase queues it; a phase with no status of its own keeps
// the status Dispatch shows.
//
// The worker of the phase it stands in is started at a new phase entry (start), raised as a move
// back raises one (enter: the return is from the phase itself), with the phase's resume task and the
// reason the entry is new (returnTask): so every completion of the entry before the stop is
// refused where it commits, and the worker is handed the work of this entry rather than the one
// the stop ended (R20). The start waits for a slot among the issues whose work came back, oldest
// first, from the moment the return wrote it (record.Waiting, LineReturning), unless the phase
// keeps none. A phase whose terminating fact is already met starts no worker (PhaseFinished): a
// merged issue stands in merging only for its close.
//
// Every other claim of the issue that has run is started again in its own session (decision 9:
// a stop suspended it, a person's done retired it), and the outbox's start brings each back
// through its suspended or retired branch, as `legion claims retry` does: the claims of the roles
// that finished their phases, which stay live until the issue closes, and the issue's own
// architect. A root that has still to make its first start (record.QueuedRoot) is the waiting
// line's: its promotion starts its architect, so nothing of it is started here. An issue that
// waits — at admitted for its first phase, in awaiting_children for its open children (R11:
// a return above a waiting parent starts no role of it), or on a person — starts no phase worker
// here: the line, the children's close or the person's answer is what starts it.
//
// A held issue returns held, its architect told nothing new: a todo releases the hold as a todo
// on any held issue in work does (releaseHeldRoot), except that the worker of the phase the hold
// came from is started at a new phase entry, as every return's worker is, and any other status
// leaves the hold to what ends it: the architect's retry of a failed claim, a later version of the
// plan for a stopped task (record.HoldTaskStopped), and the last issue beneath a parent held
// before its retro leaving the workflow (record.HoldForChildren). The architect above the issue,
// where an issue above it is in Legion's work, is told the issue is back in work.
func (e *Engine) ReturnToWork(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	back, err := e.takeBack(ctx, tx, issue, fact, PhaseStatus(issue.Phase),
		fmt.Sprintf("%s is %s again; it resumes %s at a new phase entry", issue.Key, fact.Status, issue.Phase))
	if err != nil {
		return err
	}
	if back.Phase == phase.Held && back.Hold != nil {
		// The role of the phase the hold is from starts only when the hold ends: its claim
		// failed, its task was stopped, or the parent waits for the issues beneath it before its
		// retro. Every other claim comes back, and a todo releases the hold.
		if err := e.reviveClaims(ctx, tx, back, RoleFor(back.Hold.From)); err != nil {
			return err
		}
		if fact.Status != "todo" {
			return nil
		}
		return e.releaseHeldRoot(ctx, tx, back, fact, true)
	}
	queued, err := e.queuedRoot(ctx, tx, back)
	if err != nil || queued {
		return err
	}
	return e.resumeRun(ctx, tx, back)
}

// ReturnMemberToWork takes back into Legion's work a member that the first boot's conversion
// brings back (the daemon's bringMembersBack: a member its root's departure suspended under the
// release before), as ReturnToWork takes back any issue a stop left in its phase, but judges R9 of
// dispatch://INFRA-3320 before the return starts anything, as the promotion of a carried run does
// (carriedStartHeld). Where an open Dispatch child of the member holds the start of the phase it
// stands in, planning through accepting (childrenHoldingStart, the children read from the daemon's
// copy of Dispatch's links), the member takes the return's record, status and task (takeBack) and
// moves to awaiting_children in place of the return's starts (awaitChildren), holding the start of
// that phase with the return's task (returnToWorkTask). No claim of it starts, its phase's worker
// or any other, and no step of it starts either (R11): a member in accepting, whose return starts
// no worker and brings back every role of it that ran, is held the same way. The return from its
// children starts that phase, or the step R12 gives it, once no open child is left
// (ReturnFromChildren). It holds the start whatever entry the phase's role stands on, as the
// promotion does: the return raises a new entry of no move, so the admission pass after it would
// stop that worker as any started parent's (StopForChildren). It says whether it held the start.
func (e *Engine) ReturnMemberToWork(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	pr, err := e.store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	open, err := e.childrenHoldingStart(ctx, tx, issue, issue.Phase, pr)
	if err != nil {
		return false, err
	}
	if len(open) == 0 {
		return false, e.ReturnToWork(ctx, tx, issue, fact)
	}
	back, err := e.takeBack(ctx, tx, issue, fact, PhaseStatus(phase.AwaitingChildren),
		fmt.Sprintf("%s is %s again; it waits on its open children before %s resumes", issue.Key, fact.Status, issue.Phase))
	if err != nil {
		return false, err
	}
	w, err := e.walk(ctx, tx, back)
	if err != nil {
		return false, err
	}
	at, err := e.standingPart(ctx, tx, back, w)
	if err != nil {
		return false, err
	}
	return true, e.awaitChildren(ctx, tx, back, w, childrenHold{Start: back.Phase, Task: returnToWorkTask(back, at), Open: open,
		Cause: fmt.Sprintf("the first boot of the release that stores no tree brought it back into Legion's work in %s", back.Phase)})
}

// takeBack writes what a return (ReturnToWork, ReturnMemberToWork) writes of issue's own record
// and tells, whatever it starts after: the record takes fact's observation and status, the status
// queued to Dispatch where it differs from what Dispatch shows (Engine.status; "" keeps the
// status Dispatch shows), the task its leave stopped runs again at its step (resumeStoppedTask),
// and the architect above the issue, where an issue above it is in Legion's work, is told the
// words told. It answers the record as it wrote it.
func (e *Engine) takeBack(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue, status, told string) (record.Issue, error) {
	back := observedIssue(issue, fact)
	back.Status = fact.Status
	if err := e.status(ctx, tx, back, status, ""); err != nil {
		return record.Issue{}, err
	}
	if status != "" {
		back.Status = status
	}
	if err := e.store.PutIssue(ctx, tx, back); err != nil {
		return record.Issue{}, err
	}
	if err := e.resumeStoppedTask(ctx, tx, back); err != nil {
		return record.Issue{}, err
	}
	e.logOnCommit(ctx, "workflow: an issue a stop left came back into Legion's work", "issue", back.Key, "phase", back.Phase,
		"generation", back.Generation, "status", back.Status)
	return back, e.tellAbove(ctx, tx, back, told)
}

// resumeStoppedTask sets running again, at the place it stood, the task its leave stopped
// (stopTaskAtLeave), as issue comes back into Legion's work (ReturnToWork), so the return's start
// resumes that step at a new entry. A task its owner stopped stays stopped: its issue is held for
// it (record.HoldTaskStopped), and only a later version of the plan, or a todo on the held issue
// (releaseHeldRoot), starts it again. A task in any other state, and a run with no plan rows, is
// left as it stands. The place does not move, so issues.phase stays the phase it derives.
func (e *Engine) resumeStoppedTask(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored || w.State != record.TaskStopped || record.HeldFor(issue, record.HoldTaskStopped) {
		return err
	}
	return e.store.MoveTask(ctx, tx, issue.Key, issue.Generation, w.Task, w.Place, record.TaskRunning)
}

// leaveTaskOfRun ends the task of issue's run that a new run replaces (NewRun), keeping its
// place, in the state endedState gives it: left, since the run that ends never passed its merge,
// unless the merge's close ended it done already (the reopen of an issue its merge closed). The
// new run takes a task of its own at its first step (planFirstStep).
func (e *Engine) leaveTaskOfRun(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored {
		return err
	}
	if state := endedState(w, false); state != w.State {
		return e.store.MoveTask(ctx, tx, issue.Key, issue.Generation, w.Task, w.Place, state)
	}
	return nil
}

// resumeRun starts the run issue stands in where its phase left it: every other claim of it that
// has run comes back in its own session (reviveClaims), and the worker of its phase is started at
// a new phase entry with the phase's resume task (returnToWorkTask). A phase no worker of it works
// starts no worker (runOwner), its step starting again where it stands (StartStepRecord), and a
// merge that landed while the issue was out of work closes it once nothing beneath it is in the
// workflow (closeAwaitingMerge). The start is a move back from the phase into itself (start's
// movedBack), which R20 of dispatch://INFRA-3320 raises as a move back raises one, so a completion
// of the entry before the stop is refused whatever head it names (record.PhaseRow.EntryMovedBack).
func (e *Engine) resumeRun(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	owner, err := e.runOwner(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.reviveClaims(ctx, tx, issue, owner); err != nil {
		return err
	}
	if owner == "" {
		if err := e.StartStepRecord(ctx, tx, issue); err != nil {
			return err
		}
		return e.closeAwaitingMerge(ctx, tx, issue)
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.start(ctx, tx, issue, w, owner, func(at Standing) string { return returnToWorkTask(issue, at) }, issue.Phase, true)
}

// runOwner is the role whose worker carries on the phase issue stands in, "" where none does: a
// phase no worker owns — admitted, awaiting_children, awaiting_person and accepting — and a phase
// whose terminating fact is already met (PhaseFinished): a merged issue stands in merging only for
// its close.
func (e *Engine) runOwner(ctx context.Context, tx pgx.Tx, issue record.Issue) (claim.Role, error) {
	switch issue.Phase {
	case phase.Admitted, phase.AwaitingChildren, phase.AwaitingPerson, phase.Accepting:
		return "", nil
	}
	owner := RoleFor(issue.Phase)
	if owner == "" {
		return "", nil
	}
	finished, err := e.phaseFinished(ctx, tx, issue)
	if err != nil || finished {
		return "", err
	}
	return owner, nil
}

// StartCarriedRun starts, at its promotion, the run a root a hand-over of the release before
// carried in brings with it (record.CarriedIn): each claim it ran with but its architect comes back
// in its own session (reviveClaims), the architect being the promotion's own start, and the worker
// of the phase it carries is handed that phase's work again at a raised entry
// (StartPromotedWorker). Its step starts again where it stands (StartStepRecord), whatever the
// promotion starts of it. A root carried in held starts no worker, and its architect is told of
// the hold as a root's (HeldNotice): the hold's first notice was routed in the tree it left. A
// worker's start is a phase start like any other (dispatch://INFRA-3320 R9): where an open
// Dispatch child of the root holds it, the root waits in awaiting_children instead, and nothing of
// its run starts (carriedStartHeld).
func (e *Engine) StartCarriedRun(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if root.Phase == phase.Held && root.Hold != nil {
		if err := e.enqueue(ctx, tx, root.Key, HeldNotice(root)); err != nil {
			return err
		}
		if err := e.reviveClaims(ctx, tx, root, claim.RoleArchitect, RoleFor(root.Hold.From)); err != nil {
			return err
		}
		return e.StartStepRecord(ctx, tx, root)
	}
	owner, err := e.runOwner(ctx, tx, root)
	if err != nil {
		return err
	}
	if owner != "" {
		held, err := e.carriedStartHeld(ctx, tx, root)
		if err != nil || held {
			return err
		}
	}
	if err := e.reviveClaims(ctx, tx, root, claim.RoleArchitect, owner); err != nil {
		return err
	}
	if owner != "" {
		if err := e.StartPromotedWorker(ctx, tx, root, owner); err != nil {
			return err
		}
	}
	return e.StartStepRecord(ctx, tx, root)
}

// carriedStartHeld holds the start of the phase root stands in, a root a hand-over of the release
// before carried in whose promotion would start that phase's worker (StartCarriedRun), where an
// open Dispatch child of root holds that start (childrenHoldingStart, the children read from the
// daemon's copy of Dispatch's links): root moves to awaiting_children in place of the start
// (awaitChildren), holding the phase's resume task the promoted worker would have been given
// (resumeTaskOn), and no claim of it starts, since no role of a parent that waits on its children
// works (R11 of dispatch://INFRA-3320). The return starts that phase once no open child is left
// (ReturnFromChildren). It says whether it held the start.
//
// It holds the start whatever entry the phase's role stands on, one a move out of retro or merging
// raised among them (acceptedEntry), which the release of a hold leaves to start
// (childrenHoldingSamePhaseStart): the promotion raises a new entry of no move
// (StartPromotedWorker), so the admission pass after it would stop that worker as any started
// parent's (StopForChildren), and the promotion holds the start that pass would hold instead.
func (e *Engine) carriedStartHeld(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	pr, err := e.store.PullRequestRow(ctx, tx, root.Key)
	if err != nil {
		return false, err
	}
	open, err := e.childrenHoldingStart(ctx, tx, root, root.Phase, pr)
	if err != nil || len(open) == 0 {
		return false, err
	}
	w, err := e.walk(ctx, tx, root)
	if err != nil {
		return false, err
	}
	task, err := e.resumeTaskOn(ctx, tx, root, w)
	if err != nil {
		return false, err
	}
	return true, e.awaitChildren(ctx, tx, root, w, childrenHold{Start: root.Phase, Task: task, Open: open,
		Cause: fmt.Sprintf("its promotion resumed the %s phase a hand-over of the release before carried it in at", root.Phase)})
}

// returnToWorkTask is what the worker of the phase an issue stands in is told when the issue comes
// back into Legion's work after a stop (ReturnToWork): the phase's resume task where its work
// stands (resumeText, at the standing the start reads), since the phase's work is under way, with
// why the entry it is handed is a new one. It is the work as it was composed, as every task is: the
// start that sends it ends a testing task of a parent with the base-merge sentence (enter,
// withBaseMergeRun).
func returnToWorkTask(issue record.Issue, at Standing) string {
	return continueLine(issue) + at.lines + fmt.Sprintf(" Resume the existing phase work: %s came back into Legion's work after a stop, and this is a new entry of the phase it stood in.", issue.Key) + at.owners
}

// queuedRoot says whether issue's run starts as a root's (record.Chains.StartsAsRoot) and has still
// to make its first start (record.QueuedRoot over its own design gate), the waiting line's queued
// root (record.LineFacts.Queued): the line's promotion is what starts such a root and its
// architect.
func (e *Engine) queuedRoot(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if issue.Phase != phase.Admitted {
		return false, nil
	}
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil || !chains.StartsAsRoot(issue.Key) {
		return false, err
	}
	gates, err := e.store.GatesOf(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	return record.QueuedRoot(issue, record.TreeGate{Steps: e.cfg.Steps, Rows: gates}), nil
}

// reviveClaims starts again, each in its own session, every claim of issue that has run but the
// claims of skip, whose starts are another path's (the phase's own start, the promotion's
// architect start): one plain start each, with no task, of the issue's generation and the phase
// it stands in, so the outbox's start brings a suspended claim back by the rule and relaunches a
// retired one, and the start acts only while the issue still stands in that phase. A role holds a
// claim row only once it has run (the read of record.Store.RoleRuns, one batched call): a role that
// never ran has no session to bring back, and a start would make one with nothing asked of it.
func (e *Engine) reviveClaims(ctx context.Context, tx pgx.Tx, issue record.Issue, skip ...claim.Role) error {
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	var keys []record.RoleRunKey
	for _, role := range claim.Roles {
		if slices.Contains(skip, role) {
			continue
		}
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			// An issue whose key yields no claim token holds no claim row at all.
			return nil
		}
		keys = append(keys, record.RoleRunKey{Token: token, Issue: issue.Key, Role: role, Generation: issue.Generation})
	}
	runs, err := e.store.RoleRuns(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if runs[key.Token].Claim == nil {
			continue
		}
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: key.Role,
			Generation: issue.Generation, Phase: issue.Phase}); err != nil {
			return err
		}
	}
	return nil
}

// tellAbove tells the architect above issue, where an issue above it is in Legion's work, reason as
// a child-status notice: the owner walk starts at the parent (record.NoticeStartsAtParent). An
// issue with nothing above it in Legion's work has no architect above to tell.
func (e *Engine) tellAbove(ctx context.Context, tx pgx.Tx, issue record.Issue, reason string) error {
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return err
	}
	if len(chains.Above(issue.Key)) == 0 {
		return nil
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: reason})
}

// NewRun opens a new run of issue: a new generation from its first phase, for decision 8's three
// cases (backInWork's two, and rerun_child's in its own fact). The run waits at admitted for its
// first start, at the status Dispatch shows, whichever status in work brought it. An issue that
// holds its own gate starts it as a re-admitted root does (R20 of dispatch://INFRA-3320,
// record.Chains.StartsAsRoot), whether it is a root or stands under a parent in Legion's work: on
// a todo it waits in the line for the promotion that starts its architect (record.LineFacts.Queued),
// and its first phase waits for its own design gate, which that architect designs again. Any other
// issue under a parent in Legion's work waits for its first phase (record.Waiting), as any first
// start does.
//
// It clears the issue's own run and nothing of another issue's (R17 of dispatch://INFRA-3320): the
// held start, the hold, the wait on a person and the close Dispatch refused go with the run that
// ended, so does the mark that the run stood in a phase (record.Issue.TakenBack, decision 19), and
// the generation's facts are cleared (record.Store.ClearGeneration): a merged or closed pull
// request, the rounds of
// an open one, the design gate and the recorded handoffs. A person's approval of a version the
// issue's document has since moved past is kept first (keepStaleApproval, R3), so the new run's
// architect is told to request approval of the latest version. An open production check closes as
// not run (markCheckClosed): the new run's generation fences a completion left by an interrupted
// phase, and the closed check refuses an old look's report, so every claim stays live. The claims
// come back on their sessions when the new run's starts reach them. The task of the run that
// ended is left where it stood (leaveTaskOfRun), unless the merge's close ended it done.
//
// A generation that ended takes what its architect handed back with it: the new run is a handout
// (record.Store.StartHandout), so an ask or a hand-back of the old run, delivered late, declares
// nothing in this one, and its catch-up cursor starts again (record.Store.RestartCaughtUp). The
// new generation is news of the issue whatever read brought it, so it is marked moved
// (record.Store.MarkIssueMoved). The architect above the issue, where an issue above it is in
// Legion's work, is told.
func (e *Engine) NewRun(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	if issue.Generation == ^uint64(0) {
		return fmt.Errorf("open a new run of %s: generation overflows", issue.Key)
	}
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if check != nil && check.Open() {
		if err := e.markCheckClosed(ctx, tx, check, record.CheckNotRun); err != nil {
			return err
		}
	}
	if err := e.keepStaleApproval(ctx, tx, issue.Key); err != nil {
		return err
	}
	if err := e.leaveTaskOfRun(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.store.ClearGeneration(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	run := observedIssue(issue, fact)
	run.Generation++
	run.Phase, run.Hold, run.Awaiting, run.ChildrenWait, run.PendingClose = phase.Admitted, nil, nil, nil, nil
	run.TakenBack = false
	run.Status = fact.Status
	if err := e.store.PutIssue(ctx, tx, run); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, run.Key, e.now()); err != nil {
		return err
	}
	if err := e.store.StartHandout(ctx, tx, run.Key); err != nil {
		return err
	}
	if err := e.store.RestartCaughtUp(ctx, tx, run.Key); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a new run of an issue opens", "issue", run.Key, "generation", run.Generation, "status", fact.Status)
	return e.tellAbove(ctx, tx, run, fmt.Sprintf("%s is %s; it opens a new run, generation %d, from its first phase", run.Key, fact.Status, run.Generation))
}

// keepStaleApproval keeps, for the architect start of the issue key a new run is clearing, the
// person's approval a gate of its holds of a version its document has since moved past: the new
// generation's architect registers each step again on a cleared gate, and its start task
// (ParkedDecisionTask) tells it to request approval of the latest version. One approval is kept,
// read from the first required step whose approved version sits below its latest, because
// readmitted_approvals keeps one row for each issue; it carries that step, so the task names the
// document the person reviewed rather than the spec of every deployment. The required steps are
// the ones the issue's tree is gated on (record.TreeSteps), so a Verification document its
// architect registered as criteria beside a deployment that gates spec and not criteria is read
// after the spec, as every other rule of the gate reads it.
//
// The approval read is the person's own (record.DesignGate.ApprovedByPerson), never the version
// an ancestor's approval opened the step at (dispatch://INFRA-3320, R2 and R3): a step that stands
// open on a basis at its latest version keeps the person's approval of an earlier version apart,
// and that is the approval kept, so the new run neither loses it nor names the basis as a person's
// approval.
//
// Where the deployment gates no step, which asks no person, nothing is kept and any pair an
// earlier new run kept is dropped: no start names one there, so no delivery would forget it.
// Otherwise an issue whose required steps hold no stale approval drops an earlier pair, which its
// gates have since outgrown; one with a required step it holds no gate for (an earlier new run
// cleared it and nothing has registered since) keeps the earlier pair, which no start has
// delivered yet.
func (e *Engine) keepStaleApproval(ctx context.Context, tx pgx.Tx, key string) error {
	steps := e.GatedSteps()
	if len(steps) == 0 {
		return e.store.DropReadmittedApproval(ctx, tx, key)
	}
	gates, err := e.store.GatesOf(ctx, tx, key)
	if err != nil {
		return fmt.Errorf("read the gate of %s for its new run: %w", key, err)
	}
	unregistered := false
	for _, step := range record.TreeSteps(steps, gates) {
		gate, ok := gates[step]
		if !ok {
			unregistered = true
			continue
		}
		if approved := gate.ApprovedByPerson(); approved != nil && approved.Version < gate.LatestVersion {
			return e.store.KeepReadmittedApproval(ctx, tx, key, record.KeptApproval{
				Step: step, Approved: approved.Version, Latest: gate.LatestVersion})
		}
	}
	if unregistered {
		return nil
	}
	return e.store.DropReadmittedApproval(ctx, tx, key)
}

// retryFailedArchitect starts again, in its session and with fresh budgets, the architect claim
// of issue, a root in Legion's work that holds its own gate, whose claim failed, as `legion claims
// retry` does (R20 of dispatch://INFRA-3320: a failed role is not restarted through a status). It
// is what a todo on such a root asks for: a person's todo the engine reads (backInWork), and the
// controller's set_status of todo, which writes no status and moves no issue under the root. The
// start is the plain one (childArchitectStart), with no phase, task or entry, which the outbox's
// start runs through supervise's retry of a failed claim (supervise.RequestRetry); a start already
// queued for that claim is that start, so no second one is queued. It writes nothing for any other
// issue or claim.
//
// The claim is read before the chain: a root whose architect has not failed is the common case,
// and the keyed reads answer it without reading the chain at all.
func (e *Engine) retryFailedArchitect(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if !record.InLegionsWork(issue) {
		return nil
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	token, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, claim.RoleArchitect, issue.Generation)
	if err != nil || run.Claim == nil || run.Claim.State != supervise.StateFailed {
		return err
	}
	holds, err := e.holdsOwnGate(ctx, tx, issue)
	if err != nil || !holds {
		return err
	}
	if architectStartQueued(run, issue.Phase) {
		return nil
	}
	e.logOnCommit(ctx, "workflow: a todo on a root whose architect failed starts that architect again", "issue", issue.Key, "phase", issue.Phase)
	return e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue))
}
