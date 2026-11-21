package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A parent starts no phase while a Dispatch child of it is open (dispatch://INFRA-3320 R9). In
// place of a start of planning, implementing, testing, reviewing or accepting, the daemon moves
// the parent to awaiting_children, where it holds no admission slot and no role of it works, and
// keeps the start it held for the moment no open child is left (record.Issue.ChildrenWait). This
// file holds the one move every door takes — the transition's guard (Engine.transition), the
// release of a hold (retryPhase) and the admission pass (StopForChildren) — what the move says,
// and the return the admission pass makes once no open child is left (ReturnFromChildren).

// AwaitingChildrenNoticeKind is the kind of the notice a parent's architect is told when the
// parent enters awaiting_children (awaitingChildrenNotice). It replaces the phase-finished notice
// of the start the move held, since that notice tells the architect that a phase started
// (dispatch://INFRA-3320 R10).
const AwaitingChildrenNoticeKind = record.NoticeKind("awaiting-children")

// waitingOnChildren is the refusal code of request_backward_move and sign_off on a parent in
// awaiting_children (waitingRefusal): each acts on a phase, and the parent stands in none
// (dispatch://INFRA-3320 R11).
const waitingOnChildren = "WAITING_ON_CHILDREN"

// childrenHold is one move of a parent into awaiting_children (Engine.awaitChildren): the start
// the move holds and what the move tells. Each caller fills it from what it has already read —
// the transition from the row it chose and the task it built, the release of a hold from the
// phase it restores, and the admission pass from the phase the parent stands in (StopForChildren)
// — so the one move writes the same record, the same notice and the same message whichever door
// it came through.
type childrenHold struct {
	// Start is the phase whose start the move holds, one of planning through accepting
	// (record.OpenChildHolds).
	Start phase.Phase
	// Task is the task that start gives its worker, which the return carries
	// (record.ChildrenWait.Task): the task a transition built, a retry's, the task a start
	// written and not yet delivered carries, or the task the stopped phase's worker was given for
	// the entry the phase stands on, its last retry's or else the work the entry was raised with
	// (entryWork), each kept with no base-merge sentence (heldTask, withoutBaseMergeRun). The
	// return starts it where the start goes back as it was held, and builds a testing task of its
	// own where a held testing, reviewing or accepting comes back as testing (ReturnFromChildren).
	Task string
	// Cause is what the start was for: the reason the notice the move replaces carried — a worker's
	// own move back with its words — or else the completion, review, retry or stop that raised it
	// (heldCause). The move adds the open children to it for the held start's reason
	// (childrenWaitReason).
	Cause string
	// Open is each open Dispatch child of the parent, with the status that holds it and the issue
	// above it a blocked_by link of its own names (record.OpenChild, Engine.childrenOpen). A move
	// names at least one.
	Open []record.OpenChild
	// Summary and Verdict are the worker's summary and the tester's verdict the replaced
	// phase-finished notice carried, and empty where it carried none.
	Summary, Verdict string
	// Ended is the role whose phase the move ends and that is told so on its own topic
	// (record.PhaseEnded): the worker the admission pass stops in the middle of its phase, and ""
	// for a move the transition's guard holds.
	Ended claim.Role
}

// awaitChildren moves issue into awaiting_children in place of the start hold names, and is the
// one move into that phase: the transition's guard, the release of a hold and the admission pass
// all write it here. In one transaction it
//
//   - writes in_progress where the record shows another status (Engine.status): the parent's
//     work is under way in its children (dispatch://INFRA-3320 R11);
//   - writes the phase with the held start — its phase, its task, its reason naming the open
//     children, the moment the wait began and the keys of those children, which the return reads
//     for the ones that merged (record.ChildrenWait) — and the moment of the move, which outlives
//     the phase (record.Issue.ChildrenWaitedAt, leftItsPhase);
//   - marks the issue moved, since this is a phase write (the rule in dispatchIssue);
//   - cancels every open backstop of the parent's pod, since no role of it has a phase for one to
//     wake it into (R11), where the engine holds the pod tables (Config.Pods);
//   - drops the merge forward the pull request's record holds (clearMergeForward), as a move into
//     implementing does, so every queued message of it finishes undelivered (MergeForwardAt) and
//     wakes no role of the waiting parent (R11);
//   - tells the parent's architect once (awaitingChildrenNotice), in place of the phase-finished
//     notice the start would have sent (R10);
//   - tells the role whose phase the move ends, where there is one, that its phase ended
//     and no task of it follows (record.PhaseEnded into awaiting_children);
//   - and, where the merge forward it drops had not landed (record.MergeForward.Landed empty),
//     tells the implementer of it, named with its head and its base, and of what to do with a
//     merge of it begun or not (R10): in the phase-ended message where the phase the move ends
//     is the implementer's, and otherwise in one role message on the implementer's role topic
//     (record.DroppedMergeForward.RoleMessageBody), addressed as mergeForward addresses its ask
//     (addressRoles). The phase-ended message of any other worker carries it too, and tells that
//     worker to write and push nothing more, since the implementer may be merging in the
//     workspace they share (record.PhaseEnded.Message). A merge forward that landed is dropped
//     with no word of it.
//
// A released implementer gets nothing of that role message, which the outbox ends with no
// publish and no wake as it ends every role message to a released role (the daemon's
// outbox.roleMessage).
//
// An issue already in awaiting_children is left as it stands and nothing is sent again. No door
// passes one — each moves an issue standing in planning through accepting, and the admission
// pass's next look at a parent that waits adds any child newly open under it to the wait's
// children (KeepWaitChildren) and moves nothing — so that test is a defensive one. It writes no
// count: a move back counted before the call counts what it counts today (recordPass,
// recordRound), and the move itself counts nothing more (R10).
//
// The admission pass (internal/admit) reaches it through StopForChildren, which moves a parent in
// accepting, one whose start is written and not delivered, and one whose worker is in the middle
// of a phase through this same function.
func (e *Engine) awaitChildren(ctx context.Context, tx pgx.Tx, issue record.Issue, hold childrenHold) error {
	if issue.Phase == phase.AwaitingChildren {
		return nil
	}
	if !record.OpenChildHolds(hold.Start) || hold.Task == "" || hold.Cause == "" || len(hold.Open) == 0 {
		return fmt.Errorf("a move of %s into awaiting_children holds a start of planning through accepting with its task and its cause, for an open child; this one holds %q with a task of %d bytes, cause %q and %d open children",
			issue.Key, hold.Start, len(hold.Task), hold.Cause, len(hold.Open))
	}
	from, now := issue.Phase, e.now()
	reason := childrenWaitReason(issue.Key, hold)
	status := PhaseStatus(phase.AwaitingChildren)
	if err := e.status(ctx, tx, issue, status, ""); err != nil {
		return err
	}
	issue.Phase, issue.Hold, issue.Status = phase.AwaitingChildren, nil, status
	children := make([]string, len(hold.Open))
	for i, child := range hold.Open {
		children[i] = child.Key
	}
	issue.ChildrenWait = &record.ChildrenWait{Start: hold.Start, Task: hold.Task, Reason: reason, Since: now, Children: children}
	issue.ChildrenWaitedAt = &now
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, now); err != nil {
		return err
	}
	if e.cfg.Pods != nil {
		if err := e.cfg.Pods.CancelPodBackstops(ctx, tx, issue.Key); err != nil {
			return err
		}
	}
	// Nothing asks again during the wait (awaiting_children is in no phase of
	// record.MergeForwardPhases), and the return asks for none: a conflict still there fails the
	// tester's merge with the base, and the implementer merges forward once GitHub reads the pull
	// request CONFLICTING (R10). The merge forward the clear drops is named to the implementer where
	// it had not landed.
	cleared, err := e.clearMergeForward(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	var dropped *record.DroppedMergeForward
	if cleared != nil && cleared.Landed == "" {
		dropped = &record.DroppedMergeForward{Head: cleared.Head, Base: cleared.Base}
	}
	e.logOnCommit(ctx, "workflow: a parent waits on its open children", "issue", issue.Key, "tree", issue.Tree,
		"from", from, "holds", hold.Start, "children", openChildNames(hold.Open))
	if err := e.notice(ctx, tx, issue.Key, awaitingChildrenNotice(issue.Key, from, hold)); err != nil {
		return err
	}
	if dropped != nil && hold.Ended != claim.RoleImplementer {
		addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}, nil)
		if err != nil {
			return err
		}
		for _, to := range addressed {
			if err := e.enqueue(ctx, tx, to.issue, record.RoleMessage{Role: string(to.token), Body: dropped.RoleMessageBody(issue.Key)}); err != nil {
				return err
			}
		}
	}
	if hold.Ended == "" {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.PhaseEnded{Role: hold.Ended, From: from, To: phase.AwaitingChildren, Reason: reason,
		MergeForward: dropped})
}

// childrenHolding is the open Dispatch children of issue that hold its start of to (R9's guard),
// none where the rule does not reach that start. It reaches a start of planning through accepting
// of an issue standing in one of those phases (record.OpenChildHolds) whose pull request has not
// merged, so it leaves out the first phase (from admitted), a role's move back from retro or
// merging, and the sign-off into retro. The open children are read whatever the tree's
// design gate stands at (childrenOpen).
func (e *Engine) childrenHolding(ctx context.Context, tx pgx.Tx, issue record.Issue, to phase.Phase, pr *record.PullRequest) ([]record.OpenChild, error) {
	if !record.OpenChildHolds(issue.Phase) || !record.OpenChildHolds(to) || (pr != nil && pr.State == record.PullRequestMerged) {
		return nil, nil
	}
	return e.childrenOpen(ctx, tx, issue, nil)
}

// holdForChildren moves issue to awaiting_children in place of the start of to that trigger's row
// chose for it (awaitChildren). The hold keeps the task the start would have given its worker
// (heldTask). The notice it sends carries what the one it replaces (noticeFor) would have carried.
// It tells no role that its phase ended: each move it holds is the completion or the move back of
// the role it leaves, or the daemon's own move on a review it read, which leaves the reviewer that
// has just made that review.
func (e *Engine) holdForChildren(ctx context.Context, tx pgx.Tx, issue record.Issue, trigger TriggerKind, to phase.Phase, handoff record.PhaseRow,
	pr *record.PullRequest, reason string, open []record.OpenChild,
) error {
	replaced := noticeFor(trigger, issue.Phase, to, handoff, reason)
	return e.awaitChildren(ctx, tx, issue, childrenHold{Start: to, Task: heldTask(issue, to, handoff, pr, reason),
		Cause: heldCause(trigger, issue.Phase, replaced, reason), Open: open,
		Summary: replaced.Summary, Verdict: replaced.Verdict})
}

// heldTask is the task a start of to gives issue's worker, held while the parent waits on its
// children: built as transition builds one (task), but with no base-merge sentence, which the
// return adds once at the end of every testing task it starts, after the words that name the
// return and the children that merged (ReturnFromChildren, withBaseMergeRun). So the testing task
// the return builds for a held testing that the implementer's completion raised opens with the
// held task, since both are built over the implementer's handoff. The transition's guard
// (holdForChildren) and the release of a hold (retryPhase) both hold it.
func heldTask(issue record.Issue, to phase.Phase, handoff record.PhaseRow, pr *record.PullRequest, reason string) string {
	issue.Phase = to
	return task(issue, handoff, pr, reason, false)
}

// heldCause is what a start the transition's guard held was for (childrenHold.Cause): the reason
// the notice the move replaces carried — a worker's own move back, with the phase it asked for, the
// role and its words (noticeFor) — and otherwise the completion or the review that raised the
// start, with the review's own words where it asked for changes.
func heldCause(trigger TriggerKind, from phase.Phase, replaced record.Notice, reason string) string {
	switch {
	case replaced.Reason != "":
		return replaced.Reason
	case trigger == TriggerReviewApproved:
		return "a review approved the head"
	case trigger == TriggerReviewRejected && reason != "":
		return "a review asked for changes: " + reason
	case trigger == TriggerReviewRejected:
		return "a review asked for changes"
	}
	return fmt.Sprintf("the %s completed %s", RoleFor(from), from)
}

// childrenWaitReason is the reason the held start keeps (record.ChildrenWait.Reason) and the
// phase-ended message carries: what the start was for, and the open children that held it.
func childrenWaitReason(parent string, hold childrenHold) string {
	return fmt.Sprintf("%s, while these Dispatch children of %s were open: %s", hold.Cause, parent, openChildNames(hold.Open))
}

// openChildNames names each open child with the status Dispatch shows it in, as the sign-off's
// refusal and the children-first notice name them: "LEGION-209 (todo), LEGION-210 (backlog)".
func openChildNames(open []record.OpenChild) string {
	named := make([]string, len(open))
	for i, child := range open {
		named[i] = fmt.Sprintf("%s (%s)", child.Key, child.Status)
	}
	return strings.Join(named, ", ")
}

// awaitingChildrenNotice is what the parent's architect is told when the parent moves out of from
// into awaiting_children (awaitChildren): the parent, the start it holds and what that start was
// for, each open child with its Dispatch status, each child whose own blocked_by link names an
// issue above it on its parent chain (ChildCycleReason), since that wait never ends by itself,
// and what ends the wait. It carries the summary and the verdict the phase-finished notice it
// replaces carried, and the phase the parent left. The architect's levers are the children: the
// notice names no move of a child under another started issue, which would make that issue wait
// too (dispatch://INFRA-3320 R11).
func awaitingChildrenNotice(parent string, from phase.Phase, hold childrenHold) record.Notice {
	reason := fmt.Sprintf("%s is in awaiting_children, holding the start of %s: %s. No phase of %s starts while Dispatch shows an open child under it: %s.",
		parent, hold.Start, hold.Cause, parent, openChildNames(hold.Open)) + childCycleSentences(hold.Open)
	reason += fmt.Sprintf(" The wait ends when no child under %[1]s is open: each one released and run until it leaves the workflow, closed, moved to icebox, taken out with park_child, or moved out from under %[1]s to an issue outside its tree or to no parent."+
		" Then %[1]s goes back to the start it held by itself, a held testing, reviewing or accepting as testing unless its pull request merged meanwhile, on a task that names each child that merged during the wait; the return asks for no forward merge, and each testing round of %[1]s runs on its head merged with the tip of the pull request's base branch."+
		" It holds no admission slot while it waits, and request_backward_move and sign_off on it are refused (%[2]s)."+
		" Close a child, move it to icebox or take it out with park_child "+parkRule+", and cite that reason in a comment on the child.",
		parent, waitingOnChildren)
	return record.Notice{Kind: AwaitingChildrenNoticeKind, Role: claim.RoleArchitect, Phase: from, Summary: hold.Summary, Verdict: hold.Verdict, Reason: reason}
}

// StopForChildren is the admission pass's move of a started parent that an open Dispatch child
// now holds (dispatch://INFRA-3320 R9), through the one move into awaiting_children
// (awaitChildren), and answers whether it moved the parent. open is the parent's open children as
// the pass judged them (record.LineFacts.StartedParentWaits), and owed the starts the parent owes
// its workers that no lane holds (its own rows of record.LineFacts.OwedRows). The pass meets three
// cases no transition raises:
//
//   - a parent in accepting, where no worker runs: it holds accepting, with the task that start
//     is given (task, over the reviewer's handoff);
//   - a parent whose start is written and not yet delivered — a start of the phase it stands in,
//     owed its worker at its own generation: it holds that start with the task its queued row
//     carries (queuedTask), and no role is told, since none has been given that phase;
//   - a parent whose worker is in the middle of its phase: it holds that phase with the task its
//     worker was given for the entry the phase stands on, as the role's phases row records it
//     (entryWork) — after the architect's retry or the operator's restart, the retry's own
//     (record.PhaseRow.EntryRetryTask), and otherwise the work the entry was raised with, as
//     SendOwedEntry sends it — whether or not the turn that took that task has ended, and that
//     worker is told on its own topic that its phase ended (childrenHold.Ended,
//     record.PhaseEnded), which wakes a pod the idle rule released.
//
// Each of those tasks but the resume task entryWork builds for a row that records no task was
// built for a worker, so a testing task of a parent ends with the base-merge sentence, which the
// hold keeps none of (withoutBaseMergeRun): the return adds it once to the testing task it starts
// (returnTask).
//
// Every start the parent owes is finished in the same transaction (Store.FinishOutbox), so none
// delivers later: the wait keeps its start on the record (record.ChildrenWait), and the return
// writes a start of its own from the moment the wait ended (ReturnFromChildren). The phase stops
// at this pass, and the slot it held is the pass's own release to free, since awaiting_children
// keeps none (record.PhaseKeepsSlot).
//
// A parent standing anywhere else is left as it stands, by the phase check that opens this
// function (record.OpenChildHolds): held, whose release is a start the guard in retryPhase reads,
// retro, merging, admitted, and awaiting_children itself, so that a second pass sends nothing
// again. So is a parent whose pull request has merged, whose worker R9 leaves unmessaged.
//
// So is a parent whose phase's role stands on an entry a move out of retro or merging raised
// (record.PhaseRow.EntryFrom, acceptedEntry): a role's move back after the daemon accepted the
// issue, the implementer's out of retro or the merger's out of merging. R9 holds only the starts
// each from a phase before retro, and the transition's guard leaves that move's start alone
// (childrenHolding), so the pass leaves it too, whether that start is still written and not
// delivered or its worker is in the middle of the phase it gave. The exemption is that entry's
// alone: the parent's next start, out of that phase, is a start from a phase before retro, and the
// transition's guard holds it while a child is open.
//
// The worker the move stops has the task its claim holds for the stopped phase retired, in this
// transaction (a `retire_task` row, record.SuperviseRequest, which the outbox runs through
// supervise.Machine.RetireStoppedTask), bounded by the run and by the entry the phase stood on,
// so the return's own start, on an entry of its own, is never taken. While the parent waits,
// every send of a pending delivery drops such a task already (supervise's PhaseHolds), but the
// return puts the issue back in that same phase at the same run, where a task held back all the
// wait would pass that question again and reach the worker before the return's start has its
// slot.
func (e *Engine) StopForChildren(ctx context.Context, tx pgx.Tx, issue record.Issue, open []record.OpenChild, owed []record.OwedStart) (bool, error) {
	if !record.OpenChildHolds(issue.Phase) || len(open) == 0 {
		return false, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || (pr != nil && pr.State == record.PullRequestMerged) {
		return false, err
	}
	role := RoleFor(issue.Phase)
	entry, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil || acceptedEntry(entry) {
		return false, err
	}
	hold := childrenHold{Start: issue.Phase, Open: open}
	switch due, owes := undeliveredStart(issue, owed); {
	case issue.Phase == phase.Accepting:
		review, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return false, err
		}
		hold.Task, hold.Cause = task(issue, review, pr, "", false), "its reviewed head waited in accepting for the architect's sign-off"
	case owes:
		if hold.Task, err = e.queuedTask(ctx, tx, issue, due, entry); err != nil {
			return false, err
		}
		hold.Cause = fmt.Sprintf("its start of %s was written and had not reached the %s", issue.Phase, role)
	default:
		hold.Task = entryWork(issue, entry)
		hold.Cause, hold.Ended = fmt.Sprintf("the admission pass stopped the %s's %s phase", role, issue.Phase), role
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "retire_task", Tree: issue.Tree, Role: role,
			Generation: issue.Generation, Phase: issue.Phase, Entry: entry.Entry}); err != nil {
			return false, err
		}
	}
	hold.Task = withoutBaseMergeRun(hold.Task)
	for _, start := range owed {
		if err := e.store.FinishOutbox(ctx, tx, start.ID, start.LeaseToken); err != nil {
			return false, fmt.Errorf("finish the start of %s's %s before its wait on its children: %w", issue.Key, start.Role, err)
		}
	}
	return true, e.awaitChildren(ctx, tx, issue, hold)
}

// undeliveredStart is the start of the phase issue stands in that it owes that phase's worker at
// its own generation, among owed: the start written and not yet delivered that StopForChildren
// holds.
func undeliveredStart(issue record.Issue, owed []record.OwedStart) (record.OwedStart, bool) {
	for _, start := range owed {
		if start.Phase == issue.Phase && start.Generation == issue.Generation && start.Role == RoleFor(issue.Phase) {
			return start, true
		}
	}
	return record.OwedStart{}, false
}

// acceptedEntry says whether row's entry was raised by a move out of retro or merging, the phases
// past the daemon's acceptance of the issue (record.PhaseRow.EntryFrom): a role's move back out of
// one of them, whose start R9 does not hold. It is the one statement of that exemption, read by the
// admission pass (StopForChildren) and by the release of a hold, which restarts the held phase on
// that same entry (retryPhase).
func acceptedEntry(row record.PhaseRow) bool {
	return record.AcceptedPhase(row.EntryFrom)
}

// queuedTask is the task the start row due carries for issue, read from its role's queued
// supervise rows (Store.RoleRun): the very task that start would have delivered, which for the
// retry of a held phase is the retry's own and not its entry's. A row no longer queued — its lane
// finished it after the pass read it — leaves the task entry records, the role's phases row the
// caller read (entryWork), which for such a retry is the retry's too.
func (e *Engine) queuedTask(ctx context.Context, tx pgx.Tx, issue record.Issue, due record.OwedStart, entry record.PhaseRow) (string, error) {
	token, err := stepClaim(issue, issue.Key, due.Role)
	if err != nil {
		return "", err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, due.Role, due.Generation)
	if err != nil {
		return "", err
	}
	for _, queued := range run.Queued {
		if queued.ID == due.ID && queued.Request.Task != "" {
			return queued.Request.Task, nil
		}
	}
	return entryWork(issue, entry), nil
}

// ReturnFromChildren takes issue back from awaiting_children once no open Dispatch child stands
// under it (dispatch://INFRA-3320 R12): the admission pass calls it on the pass that finds the
// last open child gone (admit's childrenWaits), with nobody acting.
//
//   - A held start of planning or implementing starts that phase, with the task the start held,
//     which carries the reason its transition wrote. A held start of testing, reviewing or
//     accepting starts testing (record.ReturnsAsTesting), since no round before the wait tested
//     the head on a base that carries the children's merges, with a testing task of its own that
//     carries the Handoff line of the implementer's last handoff and names the held start and its
//     reason (returnTask).
//   - Each task it starts names every child of the wait (record.ChildrenWait.Children) that
//     merged during it, with its pull request (mergedChildren), and a testing task of a parent
//     ends with the base-merge sentence once (withBaseMergeRun). No task asks for a forward
//     merge: the tester's merge with the base finds a conflict that is still there, and the
//     implementer merges forward once GitHub reads the pull request CONFLICTING (R10).
//   - A parent whose pull request merged during the wait goes back to the very start it held, on
//     the task it held, and keeps today's path for a merged pull request from there: the return
//     starts what a move into that phase starts (startEntered). A held accepting goes back to
//     accepting, where no worker starts: a child's own architect whose claim failed is started
//     again (reviveArchitect), as at every phase start of a child, and the accepting step starts
//     under the architect that owns the issue, so the sign_off ends its run. Its architect learns
//     of an issue entering accepting from the notice of the move that took it there, which the
//     wait's own notice replaced, so the return tells it once, under the wait's kind
//     (AwaitingChildrenNoticeKind), that the issue waits on its sign_off.
//   - The start raises a new phase entry (enter, through start), not a resend of the entry the
//     wait stopped (restart), and marks it as a move back's out of awaiting_children
//     (record.PhaseRow.EntryMovedBack, record.PhaseRow.EntryFrom): the return asks for the
//     phase's work to be done again now that the children's work is in, so the completion
//     route's entry fence refuses a completion of an entry from before the wait whatever head it
//     names, the head the record holds among them (api/handoff.go, HANDOFF_STALE_ENTRY), and
//     leftItsPhase refuses one of a phase the issue has left. The starting role's handoff is
//     cleared, as a transition clears it (clearHandoff).
//   - The held start goes with the phase (record.Issue.ChildrenWait), and the moment of the last
//     wait stays (record.Issue.ChildrenWaitedAt), which leftItsPhase reads after the return.
//
// It counts nothing — no pass, no pair, no round — and does not wait for the design gate, which
// holds no phase already under way. The start goes through the one writer of a phase worker's
// start (start, StartOrOwe), and the parent holds no admission slot, since awaiting_children
// keeps none: a start of planning through reviewing therefore waits in the line for one, among the
// issues whose work came back, from the moment this return wrote it (record.Waiting, OwedStart),
// or there as a waiting issue while a blocked_by link still holds the parent
// (record.LineWaitingOnIssue), ordered by that same moment.
func (e *Engine) ReturnFromChildren(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if issue.Phase != phase.AwaitingChildren || issue.ChildrenWait == nil {
		return nil
	}
	held := *issue.ChildrenWait
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	back := issue
	back.Phase, back.Hold, back.ChildrenWait = held.Start, nil, nil
	merged := pr != nil && pr.State == record.PullRequestMerged
	asTesting := !merged && record.ReturnsAsTesting(held.Start)
	if asTesting {
		back.Phase = phase.Testing
	}
	role := RoleFor(back.Phase)
	starts := role != "" && role != claim.RoleArchitect
	work := ""
	if starts {
		if work, err = e.returnTask(ctx, tx, held, back, pr, asTesting); err != nil {
			return err
		}
	}
	status := PhaseStatus(back.Phase)
	if err := e.status(ctx, tx, issue, status, ""); err != nil {
		return err
	}
	if status != "" {
		back.Status = status
	}
	if err := e.store.PutIssue(ctx, tx, back); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a parent's open children are finished, so it goes back to the start it held", "issue", issue.Key,
		"tree", issue.Tree, "held", held.Start, "to", back.Phase, "merged", merged)
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	if err := e.startEntered(ctx, tx, back, role, work, phase.AwaitingChildren); err != nil {
		return err
	}
	if starts {
		return nil
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: AwaitingChildrenNoticeKind, Role: claim.RoleArchitect, Phase: phase.AwaitingChildren,
		Reason: fmt.Sprintf("%s left awaiting_children: no Dispatch child of it is open now, and its pull request merged during the wait, so it is back in %s with no forward merge. It waits on your sign_off, as an issue in %s does.",
			issue.Key, back.Phase, back.Phase)})
}

// returnTask is the task the return out of awaiting_children hands the role it starts, back being
// the issue in the phase the return takes it to (ReturnFromChildren). asTesting is a held testing,
// reviewing or accepting that comes back as testing on an open pull request: its task is built as
// a transition builds one (task) over the implementer's phases row, so it carries the Handoff line
// of the implementer's last handoff, which the tester reads, with a reason that names the held
// start and its reason. Every other start goes back on the task it held, which carries the reason
// its transition wrote. Either task names the children of held that merged during the wait
// (mergedChildren), and a testing task of a parent ends with the base-merge sentence once
// (withBaseMergeRun): no held task carries it (childrenHold.Task, withoutBaseMergeRun).
func (e *Engine) returnTask(ctx context.Context, tx pgx.Tx, held record.ChildrenWait, back record.Issue, pr *record.PullRequest,
	asTesting bool,
) (string, error) {
	merged, err := e.mergedChildren(ctx, tx, back.Key, held.Children)
	if err != nil {
		return "", err
	}
	parent, err := e.testingParent(ctx, tx, back)
	if err != nil {
		return "", err
	}
	if !asTesting {
		work := held.Task
		if merged != "" {
			work += " " + merged + "."
		}
		return withBaseMergeRun(work, back.Phase, parent), nil
	}
	implementer, err := e.phaseRow(ctx, tx, back.Key, claim.RoleImplementer)
	if err != nil {
		return "", err
	}
	instead := "testing, the start it held"
	if held.Start != phase.Testing {
		instead = fmt.Sprintf("testing in place of the start of %s it held", held.Start)
	}
	reason := fmt.Sprintf("%s goes back from awaiting_children now that no Dispatch child of it is open, and starts %s, since no round before the wait tested its head on a base that carries its children's merges. The held start of %s was for this: %s",
		back.Key, instead, held.Start, held.Reason)
	if merged != "" {
		reason += ". " + merged
	}
	return task(back, implementer, pr, reason, parent), nil
}

// mergedChildren names each of children, the children a wait of parent was on
// (record.ChildrenWait.Children), whose record stands at done with its pull request recorded
// merged, each with that pull request, in the order the wait recorded them: the words with which a
// task the return starts names the children that merged during the wait, "" where none did. A
// child the daemon holds no record of, one that left the workflow another way, and one whose pull
// request did not merge is no child that merged.
func (e *Engine) mergedChildren(ctx context.Context, tx pgx.Tx, parent string, children []string) (string, error) {
	if len(children) == 0 {
		return "", nil
	}
	records, err := e.store.IssuesOf(ctx, tx, e.cfg.Project, children)
	if err != nil {
		return "", err
	}
	var done []string
	for _, child := range records {
		if child.Phase == phase.Done {
			done = append(done, child.Key)
		}
	}
	if len(done) == 0 {
		return "", nil
	}
	prs, err := e.store.PullRequestsOf(ctx, tx, done)
	if err != nil {
		return "", err
	}
	var named []string
	for _, key := range children {
		if pr, ok := prs[key]; ok && pr.State == record.PullRequestMerged {
			named = append(named, fmt.Sprintf("%s (pull request #%d)", key, pr.Number))
		}
	}
	if len(named) == 0 {
		return "", nil
	}
	return fmt.Sprintf("These Dispatch children of %s merged during the wait: %s", parent, strings.Join(named, ", ")), nil
}

// KeepWaitChildren adds to the children issue's wait was on (record.ChildrenWait.Children) each of
// open, the children the admission pass finds open under it while it waits in awaiting_children
// (admit's childrenWaits), that the list does not hold yet: a child filed or released under the
// parent during the wait holds the wait as one the move named does, and the return names each of
// them that merged (ReturnFromChildren). It answers whether it wrote. It moves nothing and marks
// no movement, since the parent stays where it stands.
func (e *Engine) KeepWaitChildren(ctx context.Context, tx pgx.Tx, issue record.Issue, open []record.OpenChild) (bool, error) {
	if issue.Phase != phase.AwaitingChildren || issue.ChildrenWait == nil {
		return false, nil
	}
	wait := *issue.ChildrenWait
	wait.Children = slices.Clip(wait.Children)
	for _, child := range open {
		if !slices.Contains(wait.Children, child.Key) {
			wait.Children = append(wait.Children, child.Key)
		}
	}
	if len(wait.Children) == len(issue.ChildrenWait.Children) {
		return false, nil
	}
	issue.ChildrenWait = &wait
	return true, e.store.PutIssue(ctx, tx, issue)
}

// waitingRefusal refuses act — request_backward_move or sign_off (actBackwardMove, actSignOff) —
// on issue, which stands in awaiting_children (WAITING_ON_CHILDREN, dispatch://INFRA-3320 R11):
// each acts on a phase, and the parent stands in none. The reason names the open children as the
// daemon reads them now, the start the parent holds and where the return takes it (goesBack), and
// the architect's levers, which are the children. A parent whose last open child has gone since
// the last admission pass is told that the next pass brings it back.
func (e *Engine) waitingRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, act architectAct) (intake.Result, error) {
	open, err := e.childrenOpen(ctx, tx, issue, nil)
	if err != nil {
		return intake.Result{}, err
	}
	held := phase.Phase("")
	if issue.ChildrenWait != nil {
		held = issue.ChildrenWait.Start
	}
	if len(open) == 0 {
		return refused(waitingOnChildren, fmt.Sprintf("%s waits in awaiting_children, holding the start of %s, and no Dispatch child of it is open now: at the next admission pass it %s. It stands in no phase that %s acts on, so this %s changed nothing",
			issue.Key, held, goesBack(held), act, act)), nil
	}
	return refused(waitingOnChildren, fmt.Sprintf("%[1]s waits in awaiting_children on these open Dispatch children of it: %[2]s. It stands in no phase that %[3]s acts on, so this %[3]s changed nothing; once no child under it is open, %[1]s %[4]s. "+
		"Act on the children: release one that should run, or move one out from under %[1]s to an issue outside its tree or to no parent; close one, move it to icebox or take it out with park_child "+parkRule+", and cite that reason in a comment on the child",
		issue.Key, openChildNames(open), act, goesBack(held))), nil
}

// goesBack is where the return takes a parent holding the start of held, as the refusals of an
// act on it say (waitingRefusal): a held reviewing or accepting back as testing unless its pull
// request merged during the wait (record.ReturnsAsTestingInstead, ReturnFromChildren), and any
// other held start to that start.
func goesBack(held phase.Phase) string {
	if record.ReturnsAsTestingInstead(held) {
		return fmt.Sprintf("goes back to testing by itself, or to %s, the start it holds, if its pull request merged during the wait", held)
	}
	return fmt.Sprintf("goes back to %s, the start it holds, by itself", held)
}
