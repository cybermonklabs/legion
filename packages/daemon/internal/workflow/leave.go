package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// leave is an issue leaving Legion's work for status (done, backlog, icebox or triage): the one
// path every issue the daemon records takes when Dispatch shows it out of work, whatever stands
// above or below it (R20 of dispatch://INFRA-3320: the daemon acts on each issue's own status).
// Every way an issue stops runs it — a status out of work (dispatchIssue), a phase reaching done
// (transition), the architect's close_root and LeaveUnslotted — and the boot's sweep is the
// backstop.
//
// Its slot is freed by the release of the same fact (admit's releaseSlots), which reads the
// issue's own status (record.NeedsSlot), and its claims stop. A stop (backlog, icebox, triage)
// suspends each claim with its session and queues the row that releases the issue's Dispatch claim
// (record.ClaimRelease), since Dispatch releases a claim at a close alone. A close (done) retires
// each claim (issue_close), keeping its session, after the production check the issue owes: an
// issue closed at its merge, where the project opens one (Engine.checkRole), keeps the checking
// role's claim and its pod for the check, which opens in that role's live session, and the check's
// own close writes the suspension (closeCheck). A project whose `production_check` is off checks
// nothing in production, so its merged close is like any other done. The pod goes either way,
// with the disk it ran on, by the issue's own identity (record.IssueSuspend), unless the check
// keeps it. Every claim above is one of Legion's own.
//
// What stays is the issue's run: its phase, its hold, its wait on a person, the start it holds in
// awaiting_children (record.Issue.ChildrenWait, R13), its generation, its sessions, its branch and
// its pull request. Its task stays standing at the step it ran, stopped as stop_task stops it
// (stopTaskAtLeave): a person's done that is not the merge's close is such a stop too. A return to
// work resumes that phase and that task at a new entry (ReturnToWork), and only decision 8's three
// cases open a new run (NewRun). No other issue's status is written, a child's and a parent's
// included, and nothing above or below it stops: an issue under it in Legion's work goes on, and
// so does the issue above it.
//
// The architect above it, where an issue above it is in Legion's work, is told (child-status, or
// child-closed at a close): the owner walk starts at the parent (record.NoticeStartsAtParent). The
// notice names what the departure leaves for that architect to act on (leftNotice). An issue with
// nothing above it in Legion's work has no architect above to tell. A root whose step
// rests on an ancestor's approval ends here the wait that ancestor's sign-off and close hold for
// it (dispatch://INFRA-3320, R8), whichever way it leaves, so that ancestor's architect is told,
// and a step of it that waits on an ancestor's approval request ends that wait with nobody told,
// so no approval opens it later (leaveBases).
//
// The departure may be the last one a merged issue above it waited for: an issue whose pull
// request merged stands in merging while a Dispatch child of it is not done (closeMerged), and
// queues its close once the last of them is done (closeAwaitingMerge), so a done reads again the
// wait of every merged issue above it on the Dispatch chain. The close queued is a status write
// alone, which moves no record (closeLanded waits for Dispatch's done).
//
// An issue whose own lifecycle the operator holds (record.OperatorHolds) has claims, a pod and a
// cleanup that are the operator's, which share the issue's tokens and names, so nothing of them is
// queued for the workflow's record. The one row such a leave queues is the move's Dispatch claim
// release (releaseParkedClaim), as every move short of a close does: it releases nothing while the
// operator holds the issue and writes why into the issue's record (operator-tree), and if the
// operator's hold has ended by the time it runs, it judges the claim as any move's row does.
//
// Whatever the departure, the issue carries no mark of a slot released for a wait afterwards.
// The mark is the daemon's answer to "this issue was started and its row is gone because it
// waits on another issue" (record.Issue.ReleasedForWaitAt), and this write is the third of the
// three that clear it: the other two are the grant of a slot and admission's release, which reads
// the issues that still need a slot. Left standing, the mark would stand the issue's later return
// among the issues whose work came back at a moment from before the leave. The write is made
// whatever the record in hand says, because that copy may have been read before the release wrote
// the mark, and a departure is rare enough that one statement costs nothing.
//
// Nor does its pull request keep a merge forward (clearMergeForward): the ask, its landing and
// the review it asked for are of the work that is leaving, and work that comes back asks about a
// conflict afresh. This is the one place an issue leaves Legion's work, so it is the one clear of
// a departure.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: issue left Legion's work", "issue", issue.Key, "status", status, "phase", issue.Phase)
	if err := e.store.ClearReleasedForWait(ctx, tx, issue.Key); err != nil {
		return fmt.Errorf("clear the slot release of %s for what it waits on: %w", issue.Key, err)
	}
	if err := e.untellUnresumedChanges(ctx, tx, issue); err != nil {
		return err
	}
	if _, err := e.clearMergeForward(ctx, tx, issue.Key); err != nil {
		return err
	}
	if err := e.leaveBases(ctx, tx, issue, status); err != nil {
		return err
	}
	if err := e.stopTaskAtLeave(ctx, tx, issue, status); err != nil {
		return err
	}
	held, err := record.OperatorHolds(ctx, e.store, tx, issue)
	if err != nil {
		return err
	}
	if held {
		e.logOnCommit(ctx, "workflow: an issue left Legion's work while the operator holds its lifecycle; its claims and pod are the operator's",
			"issue", issue.Key, "status", status)
		// The move's claim release is queued all the same: its executor leaves the claim while
		// the operator holds the issue and writes that reason (operator-tree) into the issue's
		// record, so a role that reads the record learns why the claim stays without the
		// daemon's log.
		return e.releaseParkedClaim(ctx, tx, issue, status)
	}
	pr, err := e.store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	merged := status == "done" && pr != nil && pr.State == record.PullRequestMerged
	op, except := record.SuperviseOp("suspend"), claim.Role("")
	if status == "done" {
		op = "issue_close"
	}
	if merged {
		except = e.checkRole()
	}
	if err := e.everyClaim(ctx, tx, issue, op, fmt.Sprintf("%s is %s", issue.Key, status), except); err != nil {
		return err
	}
	if err := e.releaseParkedClaim(ctx, tx, issue, status); err != nil {
		return err
	}
	if except == "" {
		if err := e.suspendPod(ctx, tx, issue); err != nil {
			return err
		}
	}
	// The issues above this one on its Dispatch chain are its own chain's: the read walks it alone
	// (ReadChainsOf), so a leave costs the same whatever the number of other issues at work.
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return err
	}
	above := chains.Above(issue.Key)
	if len(above) > 0 {
		kind := record.NoticeKind("child-status")
		if status == "done" {
			kind = "child-closed"
		}
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: leftNotice(issue, status, pr, chains.Children(issue.Key))}); err != nil {
			return err
		}
	}
	for _, key := range above {
		current, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if current == nil {
			continue
		}
		if err := e.closeAwaitingMerge(ctx, tx, *current); err != nil {
			return err
		}
	}
	if !merged {
		return nil
	}
	return e.openCheck(ctx, tx, issue, *pr, true)
}

// leftNotice is the reason of the child-closed or child-status notice that tells the architect
// above child that it left Legion's work at status (leave). It starts with "<KEY> is <status>",
// and it names after that what the departure leaves for that architect to act on
// (dispatch://INFRA-3178/spec R4). The first is the child's pull request while it is open: the
// daemon closes no pull request, and a merge of it later closes nothing (merged). The second is
// each Dispatch child of child that is not done, with the status Dispatch shows it at, in key
// order (children, the copy's rows, record.Chains.Children): the departure ends the child alone
// (R20 of dispatch://INFRA-3320), so an issue under it in Legion's work goes on, and one out of it
// stays out, and Dispatch closes child only once each of them is done (R24). A child with nothing
// to name gets the first sentence alone, so every reader of that bare reason stays valid. The
// record holds no state that says a pull request is in the merge queue (the daemon hears only
// GitHub's removal from it, intake.PullRequestDequeued), so the notice cannot say so.
func leftNotice(child record.Issue, status string, pr *record.PullRequest, children []record.DispatchIssueRow) string {
	reason := fmt.Sprintf("%s is %s", child.Key, status)
	if pr != nil && pr.State == record.PullRequestOpen {
		reason += fmt.Sprintf(". Its pull request #%d is open", pr.Number)
	}
	under := slices.Clone(children)
	slices.SortFunc(under, func(a, b record.DispatchIssueRow) int { return strings.Compare(a.Key, b.Key) })
	var open []string
	for _, row := range under {
		if row.Status != record.DoneStatus {
			open = append(open, fmt.Sprintf("%s (%s)", row.Key, row.Status))
		}
	}
	if len(open) > 0 {
		reason += ". Under it, not done: " + strings.Join(open, ", ")
	}
	return reason
}

// suspendPod queues the deletion of issue's pod with the disk it ran on (record.IssueSuspend),
// fenced by the issue's own generation. The row goes on carrying the tree the issue's record
// stores and that tree root's generation, which the release before requires of it
// (record.IssueSuspend) and which no decision of this release reads; an issue whose stored tree
// root is not recorded carries its own generation there.
func (e *Engine) suspendPod(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	treeGeneration := issue.Generation
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil {
		return err
	}
	if root != nil {
		treeGeneration = root.Generation
	}
	return e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: treeGeneration})
}

// releaseParkedClaim queues the row that clears the issue's Dispatch claim when the issue is
// parked rather than closed: Dispatch releases a claim at a close alone. The row carries
// nothing of the holder, which it reads with the issue's record when it runs.
func (e *Engine) releaseParkedClaim(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	if status == "done" {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.ClaimRelease{})
}

// LeaveUnslotted ends the run of a root admission found in Legion's work with no way to run
// (admit's releaseUnslotted): its phase is done, so it is out of Legion's work and nothing starts
// for it, its claims stop as a stop at the status the root stands at stops them (leave), and a
// person's todo on it opens a new run (NewRun), as for any issue at phase done. That status is the
// move, so no status write the daemon queued before it lands over it, as for any move out of
// Legion's work (dispatchIssue).
func (e *Engine) LeaveUnslotted(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if err := e.store.DropStatusWrites(ctx, tx, root.Key); err != nil {
		return err
	}
	w, err := e.walk(ctx, tx, root)
	if err != nil {
		return err
	}
	if err := e.endTaskWithLeave(ctx, tx, &root, w, false); err != nil {
		return err
	}
	return e.leave(ctx, tx, root, root.Status)
}

// stopTaskAtLeave ends the running step of issue's task as stop_task ends it, as the issue leaves
// Legion's work at status (leave), whoever wrote the status: a backlog, icebox, triage, or a done
// that is not the merge's close (dispatch://INFRA-4730, the architect's decision of 2026-11-23).
// The end of the step the issue stands in is named stopped (nameEnd), which the fact's settle
// writes, and the task stays at that step, stopped, with the leave as its reason
// (record.Store.StopTask): neither done nor left, since its run goes on. Its claims stop with the
// leave itself (everyClaim), so no step_stop is queued, and no hold is written, since the issue is
// out of work. A todo resumes the task at a new entry (ReturnToWork, resumeStoppedTask), and a
// later version of the plan may change it as it may change any stopped task (versionRefusal). A
// task that is not running keeps where and how it stands: one waiting for its first step, one its
// owner stopped already, and one that is done or left. So does a run with no plan rows.
func (e *Engine) stopTaskAtLeave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored || w.State != record.TaskRunning {
		return err
	}
	if step, ok := standingStep(w, issue); ok {
		if err := e.nameEnd(ctx, issue, step, stepStopped); err != nil {
			return err
		}
	}
	return e.store.StopTask(ctx, tx, issue.Key, issue.Generation, w.Task, fmt.Sprintf("%s is %s", issue.Key, status))
}

// endedState is the state the task of w ends its run in: done where its last step passed, which
// is the merge's close (merged), and left at every other end of its run: a close_root, a root
// admission found with no way to run, and each new run (NewRun), whose old task did not pass and
// whose run takes a task of its own. Either way the task keeps its place. A task that already
// ended, done or left, keeps the state that end wrote, since nothing moves it again.
func endedState(w Walk, merged bool) record.TaskState {
	switch {
	case w.State == record.TaskDone || w.State == record.TaskLeft:
		return w.State
	case merged:
		return record.TaskDone
	}
	return record.TaskLeft
}

// endTaskWithLeave writes issue at phase done, its run ended: its task ends with the run where it
// stands on w, the walk of its run the caller read, in the state endedState gives it (merged for
// the merge's close), through moveTask, which writes the phase done, and its hold, its wait on a
// person and the start it holds in awaiting_children are cleared, in the caller's transaction
// (writeIssueOn). Every end of a run that writes phase done comes here: a close that landed
// (closeLanded) and a root admission found with no way to run (LeaveUnslotted). A leave that keeps
// the run (leave) stops the task instead (stopTaskAtLeave). issue is the caller's record, which it
// writes with any field it set, and which stands as written after.
func (e *Engine) endTaskWithLeave(ctx context.Context, tx pgx.Tx, issue *record.Issue, w Walk, merged bool) error {
	if err := e.moveTask(ctx, tx, issue, &w, phase.Done, endedState(w, merged)); err != nil {
		return err
	}
	issue.Hold, issue.Awaiting, issue.ChildrenWait = nil, nil, nil
	return e.writeIssueOn(ctx, tx, *issue, w)
}

// closeAwaitingMerge queues the close of merged when its own merge was waiting on its children: an
// issue whose pull request merged stands in merging (or held from it) while a Dispatch child of it
// is not done (closeMerged), and queues its close once the last of them is done. Which children
// still hold it is closeMerged's to judge, so this runs on every merged ancestor of an issue that
// leaves the workflow and changes nothing for any other. It tells nobody: the architect heard the
// wait when the close entered it (merged), and this path is walked at every departure.
func (e *Engine) closeAwaitingMerge(ctx context.Context, tx pgx.Tx, merged record.Issue) error {
	if !awaitsMerge(merged) {
		return nil
	}
	pr, err := e.store.PullRequestRow(ctx, tx, merged.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestMerged {
		return err
	}
	waiting, err := e.closeMerged(ctx, tx, merged, pr)
	if err != nil || len(waiting) > 0 {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merged issue's close is queued once its last child not done is done", "issue", merged.Key, "pullRequest", pr.Number)
	return nil
}

// awaitsMerge says whether issue awaits its merge: in merging, or held from merging (a failed
// merger's claim, an escalation, a stopped task's, or the hold the upgrade's data move left an
// issue that was held in production_check). The merge is what ends merging, so a merged pull
// request there is a close still waiting on the issues beneath it rather than a merger still at
// work. It asks the one predicate written over a hold (record.StandsIn) of the one phase, and
// stays a question of its own rather than folding into record.Accepted: a root in retro has been
// accepted and does not await its merge, and this is what closes an issue at its merge (merged,
// closeMerged), what closes an issue on a merge already recorded (closeAwaitingMerge) and what
// keeps that merge against a later pull request (pullRequestOpened).
func awaitsMerge(issue record.Issue) bool {
	return issue.Phase == phase.Merging || issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == phase.Merging
}
