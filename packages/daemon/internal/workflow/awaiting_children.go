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
// release of a hold (retryPhase), the restart of a stopped task (startAtFirstStep) and the
// admission pass (StopForChildren) — what the move says, and the return the admission pass makes
// once no open child is left (ReturnFromChildren). Each door reads the task whose start the move
// holds on its own scope (TaskScope), and the move writes the issue itself (WriteIssue).

// AwaitingChildrenNoticeKind is the kind of the notice a parent's architect is told when the
// parent enters awaiting_children (awaitingChildrenNotice). It replaces the phase-finished notice
// of the start the move held, since that notice tells the architect that a phase started
// (dispatch://INFRA-3320 R10).
const AwaitingChildrenNoticeKind = record.NoticeKind("awaiting-children")

// waitingOnChildren is the refusal code of request_backward_move, sign_off and stop_task on a
// parent in awaiting_children (waitingRefusal): each acts on a phase or on the task standing in
// one, and the parent stands in none (dispatch://INFRA-3320 R11).
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
	// (record.ChildrenWait.Task): the task a transition built, a retry's, or the task the role's
	// phases row keeps as the one its worker was given for the entry the phase stands on, a start
	// of which is written and not delivered or in whose middle its worker stands (entryWork). Each
	// is kept as it was composed, with none of the daemon's decorations of a send, the base-merge
	// sentence and the reviewer's round sentences (task, record.PhaseRow.Given). The return starts
	// it where the start goes back as it was held, and builds a task of its own where a held
	// testing, reviewing or accepting comes back as the first step of the walk after the
	// implementer's (ReturnFromChildren); the start decorates either once (start).
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
	// Back is the phase the return starts for Start where the pull request has not merged
	// (record.ReturnStep on the walk of the issue's run), which the notice names; the move fills
	// it (awaitChildren), and "" reads as Start.
	Back phase.Phase
}

// awaitChildren moves the issue of s into awaiting_children in place of the start hold names, and
// is the one move into that phase: the transition's guard, the release of a hold, the restart of
// a stopped task at its first step (startAtFirstStep) and the admission pass all write it here. s
// is the scope of the task whose start the move holds, as the caller holds it: its copy stands in
// the phase the issue leaves, and its walk is the task's after any move of it (moveTask), so each
// door has read both already and the move reads them no second time. In one transaction it
//
//   - writes in_progress where the record shows another status (Engine.status): the parent's
//     work is under way in its children (dispatch://INFRA-3320 R11);
//   - writes the phase with the held start — its phase, its task, its reason naming the open
//     children, the moment the wait began and the keys of those children, which the return reads
//     for the ones that merged (record.ChildrenWait) — and the moment of the move, which outlives
//     the phase (record.Issue.ChildrenWaitedAt, leftItsPhase), on the issue itself, never a copy
//     read for one of its tasks (WriteIssue), which holds an issue with plan rows to its tasks
//     standing at the phase of the held start (derivedStanding): the transition's guard
//     (holdForChildren) and the restart of a stopped task (startAtFirstStep) move the task there,
//     and the release of a hold and the admission pass hold the start of the phase the task
//     already stands at;
//   - marks the issue moved, since this is a phase write (the rule in dispatchIssue);
//   - cancels every open backstop of the parent's pod, since no role of it has a phase for one to
//     wake it into (R11), where the engine holds the pod tables (Config.Pods);
//   - drops the merge forward of each pull request of the issue (clearMergeForwards), as a move
//     into implementing drops its task's, so every queued message of them finishes undelivered
//     (MergeForwardAt) and wakes no role of the waiting parent, whatever task it was asked for
//     (R11);
//   - tells the parent's architect once (awaitingChildrenNotice), in place of the phase-finished
//     notice the start would have sent (R10);
//   - tells the role whose phase the move ends, where there is one, that its phase ended
//     and no task of it follows (record.PhaseEnded into awaiting_children);
//   - and, for each merge forward it drops that had not landed (record.MergeForward.Landed empty),
//     tells the role it was asked of (record.MergeForward.Asked, the implementer for an ask that
//     names none) of it, named with its head and its base, and of what to do with a merge of it
//     begun or not (R10): in the phase-ended message where the merge forward is of the pull
//     request of s's task and the phase the move ends is that role's, and otherwise in one role
//     message on that role's topic (record.DroppedMergeForward.RoleMessageBody), addressed as
//     mergeForward addresses its ask (addressRoles). The phase-ended message of any other worker
//     carries the merge forward of s's task's pull request too, and tells that worker to write and
//     push nothing more, since the role asked may be merging in the working copy they share
//     (record.PhaseEnded.Message). A merge forward that landed is dropped with no word of it.
//
// A released role gets nothing of that role message, which the outbox ends with no publish and no
// wake as it ends every role message to a released role (the daemon's outbox.roleMessage).
//
// An issue already in awaiting_children is left as it stands and nothing is sent again. No door
// passes one — each moves an issue standing in planning through accepting, except
// startAtFirstStep, whose issue stands in the phase its stopped task's hold is from (its from),
// the retro or the merge among them, and the admission pass's next look at a parent that waits
// adds any child newly open under it to the wait's children (KeepWaitChildren) and moves nothing —
// so that test is a defensive one. It writes no count: a move back counted before the call counts
// what it counts today (recordPass, recordRound), and the move itself counts nothing more (R10).
//
// The admission pass (internal/admit) reaches it through StopForChildren, which moves a parent in
// accepting, one whose start is written and not delivered, and one whose worker is in the middle
// of a phase through this same function.
func (e *Engine) awaitChildren(ctx context.Context, tx pgx.Tx, s TaskScope, hold childrenHold) error {
	issue := s.issueItself()
	if issue.Phase == phase.AwaitingChildren {
		return nil
	}
	if !record.OpenChildHolds(hold.Start) || hold.Task == "" || hold.Cause == "" || len(hold.Open) == 0 {
		return fmt.Errorf("a move of %s into awaiting_children holds a start of planning through accepting with its task and its cause, for an open child; this one holds %q with a task of %d bytes, cause %q and %d open children",
			issue.Key, hold.Start, len(hold.Task), hold.Cause, len(hold.Open))
	}
	hold.Back = record.ReturnStep(s.walk.steps(), hold.Start)
	from, now := issue.Phase, e.now()
	reason := childrenWaitReason(issue.Key, hold)
	// The pull request of s's task, whose dropped merge forward the phase-ended message names.
	own, err := e.taskPullRequestRow(ctx, tx, s)
	if err != nil {
		return err
	}
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
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
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
	// Nothing asks again during the wait (no merge forward stands in awaiting_children, which is on
	// no task's line, classify.MergeForwardStands), and the return asks for none: a conflict still there fails the
	// tester's merge with the base, and the implementer merges forward once GitHub reads the pull
	// request CONFLICTING (R10). Each merge forward the clear drops is named to the role it was
	// asked of where it had not landed: the implementer for an ask made before merging, and the
	// merger for one made in merging that a move back out of merging left in the record.
	cleared, err := e.clearMergeForwards(ctx, tx, issue)
	if err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a parent waits on its open children", "issue", issue.Key, "tree", issue.Tree,
		"from", from, "holds", hold.Start, "children", openChildNames(hold.Open))
	if err := e.notice(ctx, tx, issue.Key, awaitingChildrenNotice(issue.Key, from, hold)); err != nil {
		return err
	}
	var ended *record.DroppedMergeForward
	for _, pr := range cleared {
		asked := pr.MergeForward
		if asked.Landed != "" {
			continue
		}
		dropped := &record.DroppedMergeForward{Head: asked.Head, Base: asked.Base, Role: asked.Asked()}
		if own != nil && pr.Repo == own.Repo && pr.Number == own.Number {
			ended = dropped
			if hold.Ended == dropped.Role {
				continue
			}
		}
		addressed, err := e.addressRoles(ctx, tx, issue.Project, []roleRecipient{{issue: issue.Key, role: dropped.Role, row: issue.Key}}, nil)
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
		MergeForward: ended})
}

// childrenHolding is the open Dispatch children of issue that hold its start of to (R9's guard),
// none where the rule does not reach that start. It reaches a start of planning through accepting
// of an issue standing in one of those phases (record.OpenChildHolds) whose pull request has not
// merged, so it leaves out the first phase (from admitted), a role's move back from retro or
// merging, and the sign-off into retro. The open children are read whatever the tree's
// design gate stands at (childrenOpen).
func (e *Engine) childrenHolding(ctx context.Context, tx pgx.Tx, issue record.Issue, to phase.Phase, pr *record.PullRequest) ([]record.OpenChild, error) {
	if !record.OpenChildHolds(issue.Phase) {
		return nil, nil
	}
	return e.childrenHoldingStart(ctx, tx, issue, to, pr)
}

// childrenHoldingStart is R9's guard on a start of to whatever issue stands in: the open Dispatch
// children of issue that hold it, none where to is no phase of planning through accepting or the
// pull request has merged. The guard of a move between phases reads it after the phase the issue
// leaves (childrenHolding); the restart of a stopped task at its first step reads it alone
// (startAtFirstStep), since that start is raised from held whatever place the stop stood at.
func (e *Engine) childrenHoldingStart(ctx context.Context, tx pgx.Tx, issue record.Issue, to phase.Phase, pr *record.PullRequest) ([]record.OpenChild, error) {
	if !record.OpenChildHolds(to) || (pr != nil && pr.State == record.PullRequestMerged) {
		return nil, nil
	}
	return e.childrenOpen(ctx, tx, issue, nil)
}

// childrenHoldingSamePhaseStart is R9's guard on a start of the phase the copy of s stands in,
// which a retry of a held phase (retryPhase) and the promotion of a re-admitted tree's mid-phase
// worker (holdPromotedStart) each make: the open Dispatch children of s's issue that hold it
// (childrenHoldingStart, which holds no start once the pull request of s's task has merged), none
// where the phase's role stands on an entry R9 exempts (samePhaseExempt). It answers the pull
// request of s's task it read too, the whole record (taskPullRequest), which the retry composes
// its task from and reads its review round on, whether or not children hold the start.
func (e *Engine) childrenHoldingSamePhaseStart(ctx context.Context, tx pgx.Tx, s TaskScope) ([]record.OpenChild, *record.PullRequest, error) {
	pr, err := e.taskPullRequest(ctx, tx, s)
	if err != nil {
		return nil, nil, err
	}
	open, err := e.childrenHoldingStart(ctx, tx, s.issue, s.issue.Phase, pr)
	if err != nil || len(open) == 0 {
		return nil, pr, err
	}
	if _, exempt, err := e.samePhaseExempt(ctx, tx, s); err != nil || exempt {
		return nil, pr, err
	}
	return open, pr, nil
}

// samePhaseExempt says whether R9 leaves alone a start of the phase the copy of s stands in that
// open children would hold because the role of the step s's task stands at (Walk.endingRole: a
// role the deployment defines at step, the role of the phase for a run with no plan rows) stands
// on an entry a move out of retro or merging raised (acceptedEntry), and answers that role's
// phases row.
func (e *Engine) samePhaseExempt(ctx context.Context, tx pgx.Tx, s TaskScope) (record.PhaseRow, bool, error) {
	entry, err := e.stepRow(ctx, tx, s, s.walk.endingRole(s.issue.Phase))
	if err != nil {
		return record.PhaseRow{}, false, err
	}
	return entry, acceptedEntry(entry), nil
}

// holdForChildren moves the issue of s to awaiting_children in place of the start of to, at
// place, that trigger's row of s's walk chose for it (awaitChildren). The move first names the end
// of the step the copy stood in as the move would have (nameMoveEnd): the parent's wait is what
// follows the step, not how it ended. The task then moves to place (moveTask), as a move into to
// moves it, and stands there through the wait, which the issue's write holds it to (WriteIssue,
// derivedStanding). The hold keeps the task the start would have given its worker, built as
// transition builds one (task) on the copy standing in to, at the standing of the work there
// (standingPart, on pr, the pull request of s's task as the move read it), as it was composed, with
// none of the decorations the start would have added, which the return's start adds once to the
// task it gives, after the words that name the return and the children that merged
// (ReturnFromChildren, start). So the testing task the return builds for a held testing that the
// implementer's completion raised opens with the held task, since both are built over the
// implementer's handoff. The release of a hold (retryPhase) holds the task the same way, built on
// the copy it has put back in the held phase. The notice it sends carries what the one it replaces
// (noticeFor) would have carried. It tells no role that its phase ended: each move it holds is the
// completion or the move back of the role it leaves, or the daemon's own move on a review it read,
// which leaves the reviewer that has just made that review.
func (e *Engine) holdForChildren(ctx context.Context, tx pgx.Tx, s TaskScope, trigger TriggerKind, to phase.Phase, place record.TaskPlace,
	handoff record.PhaseRow, pr *record.PullRequest, reason string, open []record.OpenChild,
) error {
	if err := e.nameMoveEnd(ctx, s.walk, s.issue, trigger, placeNamed(place, to), handoff); err != nil {
		return err
	}
	from := s.issue.Phase
	replaced := noticeFor(trigger, from, to, handoff, reason, s.walk.endingRole(from))
	moved := s
	if err := e.moveTask(ctx, tx, &moved, to, place, record.TaskRunning); err != nil {
		return err
	}
	at, err := e.standingPart(ctx, tx, moved, readPull(pr))
	if err != nil {
		return err
	}
	// The move holds the start of to: the issue leaves from, and the task stands at place.
	waiting := moved
	waiting.issue.Phase, waiting.issue.Hold = from, s.issue.Hold
	return e.awaitChildren(ctx, tx, waiting, childrenHold{Start: to, Task: task(moved.issue, at, handoff, reason),
		Cause: heldCause(trigger, from, s.walk.endingRole(from), replaced, reason), Open: open,
		Summary: replaced.Summary, Verdict: replaced.Verdict})
}

// heldCause is what a start the transition's guard held was for (childrenHold.Cause): the reason
// the notice the move replaces carried — a worker's own move back, with the phase it asked for, the
// role and its words (noticeFor) — and otherwise the completion or the review that raised the
// start, with the review's own words where it asked for changes. ended is the role whose step the
// move ends (Walk.endingRole), which names a role the deployment defines at step.
func heldCause(trigger TriggerKind, from phase.Phase, ended claim.Role, replaced record.Notice, reason string) string {
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
	return fmt.Sprintf("the %s completed %s", ended, from)
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
// what ends the wait, and the step the return goes to where that is not the held start
// (childrenHold.Back). It carries the summary and the verdict the phase-finished notice it
// replaces carried, and the phase the parent left. The architect's levers are the children: the
// notice names no move of a child under another started issue, which would make that issue wait
// too (dispatch://INFRA-3320 R11).
func awaitingChildrenNotice(parent string, from phase.Phase, hold childrenHold) record.Notice {
	reason := fmt.Sprintf("%s is in awaiting_children, holding the start of %s: %s. No phase of %s starts while Dispatch shows an open child under it: %s.",
		parent, hold.Start, hold.Cause, parent, openChildNames(hold.Open)) + childCycleSentences(hold.Open)
	back := ""
	if hold.Back != "" && hold.Back != hold.Start {
		back = fmt.Sprintf(", as %s unless its pull request merged meanwhile", hold.Back)
	}
	reason += fmt.Sprintf(" The wait ends when no child under %[1]s is open: each one released and run until it leaves the workflow, closed, moved to icebox, or moved out from under %[1]s to an issue outside its tree or to no parent."+
		" Then %[1]s goes back to the start it held by itself%[4]s, on a task that names each child that merged during the wait; the return asks for no forward merge, and each testing round of %[1]s runs on its head merged with the tip of the pull request's base branch."+
		" It holds no admission slot while it waits, and request_backward_move, sign_off and stop_task on it are refused (%[2]s)."+
		" %[3]s",
		parent, waitingOnChildren, childrenLeave, back)
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
//     is given (task, over the handoff of the step before accepting, acceptedRole, as the
//     sign-off reads it);
//   - a parent whose start is written and not yet delivered — a start of the phase it stands in,
//     owed its worker at its own generation: it holds that start with the task the role's phases
//     row keeps as the one its worker was given for the entry the phase stands on (entryWork),
//     which each start of that entry the daemon writes is composed from — the transition's raise
//     of the entry and a retry of it each write it there first, the send of the entry to a claim
//     behind it sends the text that raise stored, and a resume task sent at no entry names none of
//     the entry's work — and no role is told, since none has been given that phase;
//   - a parent whose worker is in the middle of its phase: it holds that phase with the task that
//     same row keeps (entryWork) — after the architect's retry or the operator's restart, the
//     retry's own, and otherwise the work the entry was raised with — whether or not the turn
//     that took that task has ended, and that worker is told on its own topic that its phase
//     ended (childrenHold.Ended, record.PhaseEnded), which wakes a pod the idle rule released.
//
// Each of those tasks is held as it was composed, before the daemon decorated its send with the
// base-merge sentence of a testing task of a parent and the reviewer's round sentences: the row
// keeps the task that way (record.PhaseRow.Given), a fact of the record rather than a reading of
// the task's text, and the return's start decorates the task it gives once (start). A row that
// keeps no such task for its entry in the issue's generation, as on an entry a release before this
// one raised, has entryWork say what is held for it.
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
	// The task whose phase the pass stops: the one whose standing decides the issue's.
	s, err := e.waitingScope(ctx, tx, issue, "")
	if err != nil {
		return false, err
	}
	pr, err := e.taskPullRequestRow(ctx, tx, s)
	if err != nil || (pr != nil && pr.State == record.PullRequestMerged) {
		return false, err
	}
	entry, exempt, err := e.samePhaseExempt(ctx, tx, s)
	if err != nil || exempt {
		return false, err
	}
	role := s.walk.endingRole(s.issue.Phase)
	at, err := e.standingPart(ctx, tx, s, readPull(pr))
	if err != nil {
		return false, err
	}
	hold := childrenHold{Start: s.issue.Phase, Open: open}
	if s.issue.Phase == phase.Accepting {
		accepted, err := e.acceptedRole(s)
		if err != nil {
			return false, err
		}
		report, err := e.stepRow(ctx, tx, s, accepted)
		if err != nil {
			return false, err
		}
		hold.Task, hold.Cause = task(s.issue, at, report, ""), "the head the step before accepting passed waited in accepting for the architect's sign-off"
	} else {
		hold.Task = entryWork(s.issue, entry, at)
		if undeliveredStart(s.issue, role, owed) {
			hold.Cause = fmt.Sprintf("its start of %s was written and had not reached the %s", s.issue.Phase, role)
		} else {
			// The row names no claim: its executor retires the task of the claim of the worker on
			// the role's step (the daemon outbox's retireStoppedTask, workerToken).
			hold.Cause, hold.Ended = fmt.Sprintf("the admission pass stopped the %s's %s phase", role, s.issue.Phase), role
			if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "retire_task", Tree: issue.Tree, Role: role,
				Generation: issue.Generation, Phase: s.issue.Phase, Entry: entry.Entry}); err != nil {
				return false, err
			}
		}
	}
	for _, start := range owed {
		if err := e.store.FinishOutbox(ctx, tx, start.ID, start.LeaseToken); err != nil {
			return false, fmt.Errorf("finish the start of %s's %s before its wait on its children: %w", issue.Key, start.Role, err)
		}
	}
	return true, e.awaitChildren(ctx, tx, s, hold)
}

// StopPromotedWorker moves issue to awaiting_children through the admission pass's stop
// (StopForChildren), owed being its owed starts, where open Dispatch children hold the phase it
// stands in, and otherwise changes nothing. It answers whether it moved the issue.
func (e *Engine) StopPromotedWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, owed []record.OwedStart) (bool, error) {
	open, err := e.childrenHoldingStart(ctx, tx, issue, issue.Phase, nil)
	if err != nil {
		return false, err
	}
	return e.StopForChildren(ctx, tx, issue, open, owed)
}

// holdPromotedStart is R9's guard on a phase worker's start at promotion or the first pass that
// finds its re-admitted tree running, on s, the scope of the task of the role's step
// (PromotedScope). It returns true when the issue moves to awaiting_children holding the resume
// task, without a launch, a new entry or a phase-ended message. Besides the merged pull request and
// accepted-entry exemptions (childrenHoldingSamePhaseStart), carriedSlot exempts the start: it is
// k12's exemption of "a root handed over from another tree carrying its slot in `implementing`
// (`record.QueuedRoot`)".
func (e *Engine) holdPromotedStart(ctx context.Context, tx pgx.Tx, s TaskScope, role claim.Role, task string, carriedSlot bool) (bool, error) {
	if carriedSlot {
		return false, nil
	}
	open, _, err := e.childrenHoldingSamePhaseStart(ctx, tx, s)
	if err != nil || len(open) == 0 {
		return false, err
	}
	issue := s.issue
	e.logOnCommit(ctx, "workflow: a re-admitted tree's first run holds a member's phase for its open Dispatch children", "issue", issue.Key,
		"tree", issue.Tree, "phase", issue.Phase, "role", role, "children", openChildNames(open))
	return true, e.awaitChildren(ctx, tx, s, childrenHold{Start: issue.Phase, Task: task, Open: open,
		Cause: fmt.Sprintf("the re-admitted tree's first run brought the %s's %s phase back", role, issue.Phase)})
}

// undeliveredStart says whether owed holds a start of the phase issue stands in that it owes role,
// the worker of that phase's step, at its own generation: a start written and not yet delivered,
// which StopForChildren finishes undelivered with every start the parent owes, names in the held
// start's cause, and tells no worker of. The held task is the one the role's phases row keeps
// (entryWork), as for a phase stopped in its middle, and never the start row's own.
func undeliveredStart(issue record.Issue, role claim.Role, owed []record.OwedStart) bool {
	for _, start := range owed {
		if start.Phase == issue.Phase && start.Generation == issue.Generation && start.Role == role {
			return true
		}
	}
	return false
}

// acceptedEntry says whether row's entry was raised by a move out of retro or merging, the phases
// past the daemon's acceptance of the issue (record.PhaseRow.EntryFrom): a role's move back out of
// one of them, whose start R9 does not hold. samePhaseExempt reads it, for StopForChildren,
// retryPhase and holdPromotedStart.
func acceptedEntry(row record.PhaseRow) bool {
	return record.AcceptedPhase(row.EntryFrom)
}

// ReturnFromChildren takes issue back from awaiting_children once no open Dispatch child stands
// under it (dispatch://INFRA-3320 R12): the admission pass calls it on the pass that finds the
// last open child gone (admit's childrenWaits), with nobody acting.
//
//   - A held start of planning or implementing starts that phase, with the task the start held,
//     which carries the reason its transition wrote. A held start of testing, reviewing or
//     accepting starts the first step of the issue's walk after the implementer's, which stands
//     at or before the held start (record.ReturnStep): testing on today's line, and reviewing on
//     a walk of an implementer, a reviewer and the owner's acceptance, since no round before the
//     wait ran on a base that carries the children's merges. That step's worker gets a task of its
//     own that carries the Handoff line of the implementer's last handoff and names the held start
//     and its reason (returnTask).
//   - Each task it starts names every child of the wait (record.ChildrenWait.Children) that
//     merged during it, with its pull request (mergedChildren). The task is composed here, and
//     its start decorates it once, as every start into a phase does (start, enter): a testing
//     task of a parent ends with the base-merge sentence (withBaseMergeRun), and a reviewer's
//     with the sentences of its round, which name the entry the return raises
//     (namingTheReviewRound); the held task carries neither (childrenHold.Task). No task asks for
//     a forward merge: the tester's merge with the base finds a conflict that is still there, and
//     the implementer merges forward once GitHub reads the pull request CONFLICTING (R10).
//   - A parent whose pull request merged during the wait goes back to the very start it held, on
//     the task it held, and keeps today's path for a merged pull request from there: the return
//     starts what a move into that phase starts (start). A held accepting goes back to
//     accepting, where no worker starts: a child's own architect whose claim failed is started
//     again (reviveArchitect), as at every phase start of a child, and the accepting step starts
//     under the architect that owns the issue, so the sign_off ends its run. Its architect learns
//     of an issue entering accepting from the notice of the move that took it there, which the
//     wait's own notice replaced, so the return tells it once, under the wait's kind
//     (AwaitingChildrenNoticeKind), that the issue waits on its sign_off. A held accepting on a
//     walk with no step between the implementer's and the owner's acceptance comes back to
//     accepting whatever its pull request did, and is told the same way.
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
//   - News of the approved design reaches the return as it reaches every move into a phase. The
//     start the return makes carries what its holder is owed when the start runs, as every
//     entry's start does (the daemon's start executor), so the held task is sent with that news
//     once and the merged children's sentence and the base-merge sentence once each: the held task
//     carries none of the three (record.PhaseRow.Given). A task of news alone that the holder has
//     not taken gives way to that start, since it raises an entry (supervise's queue). The return
//     then queues the reconcile of the tree's news where the move leaves the holder owed news
//     (queueMovedNews), as a transition does, so news whose change set no start could carry yet
//     is sent once the reconcile has recorded it. While the parent waits it is owed nothing
//     (record.UntoldAwaitingChildren).
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
	// The task whose start the wait holds, standing at the place of that start (awaitChildren).
	s, err := e.waitingScope(ctx, tx, issue, "")
	if err != nil {
		return err
	}
	pr, err := e.taskPullRequestRow(ctx, tx, s)
	if err != nil {
		return err
	}
	back := s
	back.issue.Hold, back.issue.ChildrenWait = nil, nil
	to := held.Start
	merged := pr != nil && pr.State == record.PullRequestMerged
	rebuilt := !merged && record.ReturnsPastImplementing(held.Start)
	if rebuilt {
		to = record.ReturnStep(s.walk.steps(), held.Start)
	}
	// The task goes where the return goes, as a move into that phase moves it: an issue with plan
	// rows stood at the place of the held start through the wait (awaitChildren), and a return to
	// an earlier step moves it to the place of that step on its line (Walk.PlaceOf).
	place := s.walk.Place
	if to != held.Start {
		at, ok := s.walk.PlaceOf(to)
		if s.walk.Stored && !ok {
			return fmt.Errorf("take %s back from awaiting_children to %s: the line %v of its task %s holds no such step", issue.Key, to, s.walk.Line, s.walk.Task)
		}
		place = at
	}
	if err := e.moveTask(ctx, tx, &back, to, place, record.TaskRunning); err != nil {
		return err
	}
	role := RoleFor(to)
	if back.walk.Stored {
		role = back.walk.RoleAt(back.walk.Place)
	}
	starts := role != "" && role != claim.RoleArchitect
	work := ""
	if starts {
		at, err := e.standingPart(ctx, tx, back, readPull(pr))
		if err != nil {
			return err
		}
		if work, err = e.returnTask(ctx, tx, held, back.issue, at, rebuilt); err != nil {
			return err
		}
	}
	status := PhaseStatus(to)
	if err := e.status(ctx, tx, issue, status, ""); err != nil {
		return err
	}
	if status != "" {
		back.issue.Status = status
	}
	if _, err := e.putScope(ctx, tx, &back); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a parent's open children are finished, so it goes back to the step its return takes", "issue", issue.Key,
		"tree", issue.Tree, "held", held.Start, "to", to, "merged", merged)
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	if err := e.start(ctx, tx, back, role, readPull(pr), func(Standing) string { return work }, phase.AwaitingChildren, true); err != nil {
		return err
	}
	if err := e.queueMovedNews(ctx, tx, back.issue); err != nil {
		return err
	}
	if starts {
		return nil
	}
	why := "its pull request merged during the wait"
	if !merged {
		why = fmt.Sprintf("its walk holds no step between the implementer's and %s", to)
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: AwaitingChildrenNoticeKind, Role: claim.RoleArchitect, Phase: phase.AwaitingChildren,
		Reason: fmt.Sprintf("%s left awaiting_children: no Dispatch child of it is open now, and %s, so it is back in %s with no forward merge. It waits on your sign_off, as an issue in %s does.",
			issue.Key, why, to, to)})
}

// returnTask is the task the return out of awaiting_children hands the role it starts, back being
// the copy of the issue in the phase the return takes it to (ReturnFromChildren), as it is
// composed: the start decorates it once (start). rebuilt is a held testing, reviewing or accepting
// that comes back on an open pull request as the first step of the walk after the implementer's
// (record.ReturnStep): its task is built as a transition builds one (task) over the implementer's
// phases row, so it carries the Handoff line of the implementer's last handoff, which the tester or
// the reviewer reads, with a reason that names the step it starts, the held start and its reason.
// Every other start goes back on the task it held, which carries the reason its transition wrote.
// Either task names the children of held that merged during the wait (mergedChildren). at is the
// standing of the work in the phase back stands in (standingPart), which carries the pull request
// of back's task, and which a task built here carries as every start's task does.
func (e *Engine) returnTask(ctx context.Context, tx pgx.Tx, held record.ChildrenWait, back record.Issue, at Standing, rebuilt bool) (string, error) {
	merged, err := e.mergedChildren(ctx, tx, back.Key, held.Children)
	if err != nil {
		return "", err
	}
	if !rebuilt {
		work := held.Task
		if merged != "" {
			work += " " + merged + "."
		}
		return work, nil
	}
	implementer, err := e.phaseRow(ctx, tx, back.Key, claim.RoleImplementer)
	if err != nil {
		return "", err
	}
	instead := fmt.Sprintf("%s, the start it held", back.Phase)
	if held.Start != back.Phase {
		instead = fmt.Sprintf("%s in place of the start of %s it held", back.Phase, held.Start)
	}
	ran := "tested its head"
	if back.Phase != phase.Testing {
		ran = "ran"
	}
	reason := fmt.Sprintf("%s goes back from awaiting_children now that no Dispatch child of it is open, and starts %s, since no round before the wait %s on a base that carries its children's merges. The held start of %s was for this: %s",
		back.Key, instead, ran, held.Start, held.Reason)
	if merged != "" {
		reason += ". " + merged
	}
	return task(back, at, implementer, reason), nil
}

// mergedChildren names each of children, the children a wait of parent was on
// (record.ChildrenWait.Children), whose record stands at done with its pull request recorded
// merged, each with that pull request, its merge (record.Store.PullRequestsOf), named as
// owner/name#number (dispatch://INFRA-5910 decision 21), in the order the wait recorded them: the
// words with which a task the return starts names the children that merged during the wait, ""
// where none did. A child the daemon holds no record of, one that left the workflow another way,
// and one whose pull request did not merge is no child that merged.
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
			named = append(named, fmt.Sprintf("%s (pull request %s)", key, prName(pr.Repo, pr.Number)))
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
	return true, e.WriteIssue(ctx, tx, issue)
}

// waitingRefusal refuses act — request_backward_move, sign_off or stop_task (actBackwardMove,
// actSignOff, actStopTask) — of requester on issue, which stands in awaiting_children
// (WAITING_ON_CHILDREN, dispatch://INFRA-3320 R11): each acts on a phase or on the task standing
// in one, and the parent stands in none, so none writes anything. The reason names the open
// children as the daemon reads them now, the start the parent holds and where the return takes it
// on w, the walk of its run, which the caller has read (goesBack). To the architect, whose
// sign-off, move back and stop these are, it ends with the architect's levers, which are the
// children, the sentence on a status not taken yet and the rules for ending a child
// (record.StatusNotTakenYet, record.EndChildRule), as every refusal over children still in the
// workflow does. A phase worker's move back is refused here before the guard reads its role
// (backward), and a worker acts on no child, so its refusal ends by naming the architect that does
// instead. A parent whose last open child has gone since the last admission pass is told that the
// next pass brings it back.
func (e *Engine) waitingRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, act architectAct, requester claim.Role) (intake.Result, error) {
	open, err := e.childrenOpen(ctx, tx, issue, nil)
	if err != nil {
		return intake.Result{}, err
	}
	held := phase.Phase("")
	if issue.ChildrenWait != nil {
		held = issue.ChildrenWait.Start
	}
	back := goesBack(held, record.ReturnStep(w.steps(), held))
	if len(open) == 0 {
		return refused(waitingOnChildren, fmt.Sprintf("%s waits in awaiting_children, holding the start of %s, and no Dispatch child of it is open now: at the next admission pass it %s. It stands in no phase that %s acts on, so this %s changed nothing",
			issue.Key, held, back, act, act)), nil
	}
	var ending string
	if requester == claim.RoleArchitect {
		ending = fmt.Sprintf("Act on the children: release one that should run, or move one out from under %s to an issue outside its tree or to no parent. %s",
			issue.Key, endChildWords)
	} else {
		ending = fmt.Sprintf("The architect of %s acts on its children; tell it what you found.", issue.Key)
	}
	return refused(waitingOnChildren, fmt.Sprintf("%[1]s waits in awaiting_children on these open Dispatch children of it: %[2]s. It stands in no phase that %[3]s acts on, so this %[3]s changed nothing; once no child under it is open, %[1]s %[4]s. %[5]s",
		issue.Key, openChildNames(open), act, back, ending)), nil
}

// waitingScope is the scope of the task whose start a parent in awaiting_children holds (the
// refusals of an act on it, waitingRefusal, and the return, ReturnFromChildren), and of the task
// whose phase the admission pass stops (StopForChildren): task where the caller names one the
// plan holds, else the task whose standing decides the issue's (standingOfTasks), which is the
// task the wait stands at, since the issue's write holds the wait to its tasks' phase
// (derivedStanding). A run with no plan rows is its issue (noPlanScope), and a plan with no task
// is an error.
func (e *Engine) waitingScope(ctx context.Context, tx pgx.Tx, issue record.Issue, task string) (TaskScope, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return TaskScope{}, err
	}
	if stored == nil {
		return noPlanScope(issue), nil
	}
	if task != "" {
		if s, ok := scopeOn(issue, stored, task); ok {
			return s, nil
		}
	}
	scopes := scopesOn(issue, stored)
	walks := make([]Walk, len(scopes))
	for i, s := range scopes {
		walks[i] = s.walk
	}
	_, _, decides := standingOfTasks(walks)
	if decides < 0 {
		return TaskScope{}, fmt.Errorf("read the task of %s at generation %d whose start its wait holds: its plan holds no task", issue.Key, issue.Generation)
	}
	return scopes[decides], nil
}

// goesBack is where the return takes a parent holding the start of held, as the refusals of an
// act on it say (waitingRefusal), back being the step the walk of its run returns that start to
// (record.ReturnStep): a held testing, reviewing or accepting to the first step of the walk after
// the implementer's, where that step stands at or before the held start, testing on today's line,
// unless its pull request merged during the wait (ReturnFromChildren); and to the held start
// itself where the walk holds no such step, where the start is a held planning or implementing,
// or where the pull request merged during the wait.
func goesBack(held, back phase.Phase) string {
	if back != held {
		return fmt.Sprintf("goes back to %s by itself, or to %s, the start it holds, if its pull request merged during the wait", back, held)
	}
	return fmt.Sprintf("goes back to %s, the start it holds, by itself", held)
}
