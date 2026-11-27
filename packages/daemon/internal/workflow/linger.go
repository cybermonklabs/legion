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
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// leave is an issue leaving the workflow for status (done, backlog, icebox, or triage). A root
// takes its tree with it into linger, and a root closed as done then opens the production check of
// each merged pull request of its run that has none (openCheckIfMerged, over the merges leaveMerges
// reads), behind the linger's rows: the root's own IssueSuspend finishes without acting on the
// check's start (store.IssueSuspension's restarted fence), and the start resumes the checking role
// in the still-live pod. A root already lingering, which a person parked and now sets done, opens
// them too, each first look a wake of the checking role's suspended claim. A child ends only itself:
// it leaves the table, its phase parked in done, so no transition or status write follows the
// human's move; the architect above it is told, and decides what the rest of its tree does, as the
// shipped daemon routes a child's close to the architect. A later todo re-enters the child. What
// becomes of the child's claims is the status's: a child parked in backlog, icebox or triage has
// every claim suspended with its session, for a later todo to resume; a child closed as done with
// no merged pull request of its run (close_root, a human's done) is finished, so each of its claims
// is closed (issue_close: retired, its session kept for a later todo). Its pod goes either way, and
// the disk it ran on with it, so its Sandbox is deleted either way (record.IssueSuspend), while its
// parent keeps running in a pod of its own. A child closed as done with a production
// check that stays open past the close — one of its checks open already, or the check of a merged
// pull request of its run that this leave opens — keeps the claim of each such check's agent
// (keptClaims): every other claim is closed, no IssueSuspend is written, and each check the leave
// opens starts in its agent's live session; the check's own close retires its claim and writes the
// suspension (closeCheck). A project whose `production_check` is off checks nothing in
// production and opens no check, so its merged child's close is like any other done: every claim is
// closed and the Sandbox deleted with the pod and the disk it ran on. A leave at any other status
// opens no check: a park suspends the issue's claims and its pod, which a look would need.
// A done child set back to todo opens a new run, and each of its claims comes back on the session
// it kept when a start of its role reaches it: the architect's and the planner's at its next first
// phase start. Every claim above is one of Legion's own; the issue's Dispatch claim is the
// board's, and Dispatch releases one at a close alone, so every departure short of a close queues
// the row that releases it (record.ClaimRelease).
//
// A member's leave may be the last one a merged issue above it waited for: an issue whose task
// stands at its merge with its pull request merged stands in merging while any issue beneath it is
// in the workflow (closeMerged), and its close is judged again at each change of what holds it
// (judgeClose), which closes it once the last of them leaves and no pull request of its set holds
// it. That is the tree root for a member of its tree, and any parent between them for a member
// below it, so the leave judges again the close of every ancestor the departing issue stood
// beneath. Every way a member stops holding one runs this function — a status moving to backlog,
// icebox, triage or done (dispatchIssue), a phase its tasks put in done (leaveByTasks), the
// architect's close_root, and LeaveUnslotted — and the boot's sweep is the backstop. The child's
// check opens after that close, so its start row follows the linger's suspends and resumes the
// checking role as a root's own check does, rather than being stopped by them.
//
// Whatever the departure, the held start goes with the phase it was held in: an issue that leaves
// from awaiting_children stands at done holding nothing (record.Issue.ChildrenWait, the children
// of the wait among it, which the record's shape ties to that phase), as a held issue's hold goes
// with its leave, while the fact of its run that outlives the phase, the moment it last waited,
// stays on the record. A root a person moves to the backlog while it waits on its children leaves
// like any root moved to the backlog, and a person's todo re-admits it as a new generation
// (admit's readmit).
//
// Whatever the departure, the issue carries no mark of a slot released for a wait afterwards.
// The mark is the daemon's answer to "this issue was started and its row is gone because it
// waits on another issue" (record.Issue.ReleasedForWaitAt, which lists every write that clears
// it). Admission's release reads the project's live issues alone (admit's releaseSlots), and a
// child this function parks at phase.Done is in neither half of that read
// (record.Postgres.LiveIssues) and may never see its tree re-admitted, so without this write
// nothing would ever clear it. Left standing, a person's later todo would
// re-enter the issue carrying it, and the line would stand it among the issues whose work came
// back, at a moment from before the leave, rather than among the issues whose first phase can
// start, which are ordered by Dispatch rank (record.Waiting). The write is made whatever the
// record in hand says, because that copy may have been read before the release wrote the mark,
// and a departure is rare enough that one statement costs nothing.
//
// Nor does its pull request keep a merge forward (clearMergeForwards): the ask, its landing and
// the review it asked for are of the run that is leaving, and a run that comes back asks about a
// conflict afresh. This is the one place an issue leaves the workflow, so it is the one clear of
// a departure.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: issue left the workflow", "issue", issue.Key, "tree", issue.Tree, "status", status)
	if err := e.store.ClearReleasedForWait(ctx, tx, issue.Key); err != nil {
		return fmt.Errorf("clear the slot release of %s for what it waits on: %w", issue.Key, err)
	}
	if err := e.untellUnresumedChanges(ctx, tx, issue); err != nil {
		return err
	}
	if _, err := e.clearMergeForwards(ctx, tx, issue); err != nil {
		return err
	}
	// The merges this leave checks are read before it writes the issue's tasks out of the workflow,
	// and they are the same after (leaveMerges).
	merges, err := e.leaveMerges(ctx, tx, issue, status)
	if err != nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		if issue.Lingers() {
			// A root that lingers left before, at a park, which suspended every claim of its tree:
			// its done checks each merge of its run, each first look a wake of that suspended claim.
			return e.openCheckIfMerged(ctx, tx, issue, merges, true)
		}
		// A root whose step rests on an ancestor's approval ends here the wait that ancestor's
		// sign-off and close hold for it (dispatch://INFRA-3320, R8), whichever way it leaves, so
		// that ancestor's architect is told, and a step of it that waits on an ancestor's approval
		// request ends that wait with nobody told, so no approval opens it later (leaveBases).
		if err := e.leaveBases(ctx, tx, issue, status); err != nil {
			return err
		}
		if held, err := record.OperatorHoldsTree(ctx, e.store, tx, issue); err != nil || held {
			if err != nil {
				return err
			}
			return e.leaveUnderOperator(ctx, tx, issue, status)
		}
		if err := e.beginLinger(ctx, tx, issue, status); err != nil {
			return err
		}
		if err := e.releaseParkedClaim(ctx, tx, issue, status); err != nil {
			return err
		}
		return e.openCheckIfMerged(ctx, tx, issue, merges, false)
	}
	// The leave ends the child's task with its run, where it stands (endTaskWithLeave).
	if issue.Phase != phase.Done {
		if err := e.endTaskWithLeave(ctx, tx, &issue, status); err != nil {
			return err
		}
	}
	open, err := e.toldPullRequests(ctx, tx, issue)
	if err != nil {
		return err
	}
	merged := slices.ContainsFunc(merges, func(pr record.PullRequest) bool { return pr.State == record.PullRequestMerged })
	op := record.SuperviseOp("suspend")
	if status == "done" {
		op = "issue_close"
	}
	// The claims a close keeps are read before the leave opens its checks, which it does last, so
	// the claim of each check it will open is computed here from the plan alone (checkClaim).
	except, err := e.keptClaims(ctx, tx, issue, status, merges)
	if err != nil {
		return err
	}
	if err := e.everyClaim(ctx, tx, issue, op, fmt.Sprintf("%s is %s", issue.Key, status), except); err != nil {
		return err
	}
	if err := e.releaseParkedClaim(ctx, tx, issue, status); err != nil {
		return err
	}
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return err
	}
	above := record.Above(issue, members)
	var root *record.Issue
	for i := range members {
		if members[i].Key == issue.Tree {
			root = &members[i]
			break
		}
	}
	if root == nil {
		return fmt.Errorf("suspend issue %s: tree root %s is not recorded", issue.Key, issue.Tree)
	}
	if len(except) == 0 {
		if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{
			Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation,
		}); err != nil {
			return err
		}
	}
	kind := record.NoticeKind("child-status")
	if status == "done" {
		kind = "child-closed"
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: leftNotice(issue, status, open, members)}); err != nil {
		return err
	}
	// This departure may be the last one any merged ancestor of the issue waited for, and not
	// only the tree root's: the wait reads the issues beneath an issue (record.OpenBeneath), so a
	// child that merged before the grandchild under it stands in merging too, and nothing
	// else would ever re-read it — it holds no slot, it is in neither part of the waiting line,
	// and while it stands there its root can be neither accepted nor closed. So the close of each
	// ancestor is judged again (judgeClose), which reads its record again rather than taking it
	// from the snapshot above, because closing the nearest one leaves (transition's own leave)
	// and may close the ones above it on the way, and which leaves an ancestor whose close a pull
	// request of its set holds where it stands (closeMerged).
	// The same departure may be the last one an ancestor held before its retro waited for, a
	// parent whose last step is not the owner's acceptance (record.HoldForChildren): the fact's settle starts
	// its retro (releaseHeldParents), as it does for every other change of what stands beneath
	// it.
	for _, ancestor := range above {
		if _, err := e.judgeClose(ctx, tx, ancestor.Key, nil, fmt.Sprintf("%s beneath it left the workflow", issue.Key)); err != nil {
			return err
		}
	}
	if !merged {
		return nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return err
	}
	return e.openCheckIfMerged(ctx, tx, issue, merges, lingers)
}

// keptClaims is the claim of each production check of issue that stays open past its leave at
// status, which that leave's walk over every claim leaves out (everyClaim): at a close (status
// done), the claim of each open check of the issue, and the claim of each check the leave will
// open, one for each merged pull request of merges that no check of the issue checks (checksMerge),
// as openCheck will write it (Generation the issue's, Task the pull request's, Role the project's
// checking role). Each claim is checkClaim's, the claim each row of that check names, so the claim
// kept here is the claim the check's own close retires. A leave at any other status keeps none: a
// park suspends every claim and the pod. A project that checks nothing in production opens no check,
// so it keeps no claim for one.
func (e *Engine) keptClaims(ctx context.Context, tx pgx.Tx, issue record.Issue, status string, merges []record.PullRequest) ([]claim.Token, error) {
	if status != "done" {
		return nil, nil
	}
	checks, err := e.store.ProductionChecksOf(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	var kept []record.ProductionCheck
	for _, check := range checks {
		if check.Open() {
			kept = append(kept, check)
		}
	}
	if role := e.checkRole(); role != "" {
		for _, pr := range merges {
			if pr.State != record.PullRequestMerged || slices.ContainsFunc(checks, func(check record.ProductionCheck) bool { return e.checksMerge(check, pr) }) {
				continue
			}
			kept = append(kept, record.ProductionCheck{Issue: issue.Key, Project: issue.Project, Generation: issue.Generation, Task: pr.Task, Role: role})
		}
	}
	var tokens []claim.Token
	for _, check := range kept {
		token, err := e.checkClaim(ctx, tx, check)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(tokens, token) {
			tokens = append(tokens, token)
		}
	}
	return tokens, nil
}

// leaveByTasks is the one place where an issue whose tasks put it in done leaves the workflow:
// written is the issue as its tasks put it there. Its callers are the doors from the tasks: a
// transition that writes the issue done (transition: a task's last step, the merge that ends the
// last task, and the close of a merge in hand that a judge of the issue's close makes,
// closeAwaitingMerge, from a child's leave, the boot's sweep and each change of the issue's set,
// each through closeMerged), a version that leaves only done tasks (leaveWhereDone), the release of
// an issue held for its pull requests where no merge was in hand (releaseHeldClose), and the merge
// that ends the issue in a lingering tree (closeMerged). A person's status, close_root and
// LeaveUnslotted are no door from the tasks and call leave themselves. In a running tree it is the
// leave at done (leave). In a lingering tree it calls no leave, since the linger suspended every
// claim and the tree's close retires them: a child tells the architect above it child-closed, and
// the check of each merged pull request of the run that has none opens, each first look a wake of
// the checking role's suspended claim.
func (e *Engine) leaveByTasks(ctx context.Context, tx pgx.Tx, written record.Issue) error {
	lingers, err := record.TreeLingers(ctx, e.store, tx, written.Tree)
	if err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: each task of the issue is done or left, and it leaves the workflow", "issue", written.Key,
		"tree", written.Tree, "lingers", lingers)
	if !lingers {
		return e.leave(ctx, tx, written, "done")
	}
	merges, err := e.leaveMerges(ctx, tx, written, "done")
	if err != nil {
		return err
	}
	if !claim.IsTreeRoot(written.Key, written.Tree) {
		if err := e.notice(ctx, tx, written.Key, record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is done", written.Key)}); err != nil {
			return err
		}
	}
	return e.openCheckIfMerged(ctx, tx, written, merges, true)
}

// leaveMerges is the pull request of each merge of issue's run (record.Store.MergePullRequests:
// each task that has a merge, done, to come, or cut short by a park, as the plan stood) that a
// leave at status checks: each of them at done, and none at any other status, since a park
// suspends the claims and the pod a look would need. Its rows are the same whether they are read
// before the leave writes the issue's tasks out of the workflow or after: that write changes no
// task's needs or merges, and a task it writes done at its merge stood among the tasks that merge.
func (e *Engine) leaveMerges(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) ([]record.PullRequest, error) {
	if status != "done" {
		return nil, nil
	}
	return e.store.MergePullRequests(ctx, tx, issue.Key)
}

// toldPullRequests is the number of each pull request of issue that is open and that no queued
// close of the issue's generation names (record.Store.QueuedPullRequestCloses), in number order:
// what the notice of a child's leave names (leftNotice). A merge that ends the issue queues the
// close of each other open pull request in its own transaction, before its leave, so that leave
// names none of them; a close that keeps failing keeps its row, and its pull requests are not
// named while it retries. The pull requests are read as rows alone
// (record.Store.TaskPullRequestRows): their numbers and states are all this reads, and nothing of
// them is written.
func (e *Engine) toldPullRequests(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]int, error) {
	prs, err := e.store.TaskPullRequestRows(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	closing, err := e.store.QueuedPullRequestCloses(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return nil, err
	}
	var open []int
	for _, pr := range prs {
		if pr.State == record.PullRequestOpen && !slices.Contains(closing, pr.Number) {
			open = append(open, pr.Number)
		}
	}
	slices.Sort(open)
	return open, nil
}

// leaveState is the state the task of w is in once its issue leaves the workflow at status: done
// where the status is done, which is a merged pull request's close and a person's close alike, and
// left at any other status (backlog, icebox, triage), where the issue goes out before its task is
// done. Either way the task keeps its place. A task that already left at an earlier leave of its
// run, done or left, keeps the state that leave wrote, since nothing moves it again.
func leaveState(w Walk, status string) record.TaskState {
	switch {
	case w.State == record.TaskDone || w.State == record.TaskLeft:
		return w.State
	case status == "done":
		return record.TaskDone
	}
	return record.TaskLeft
}

// endTaskWithLeave writes issue out of the workflow at status: each task of its run ends where it
// stands, in the state its leave gives it (moveTask on each task's scope, leaveState), which writes
// the phase done, and its hold, its wait on a person and the start it held while it waited on its
// children (record.Issue.ChildrenWait) are cleared, in the caller's transaction (WriteIssue, where
// the issue stands where its tasks put it), whatever the plan's count of tasks. Every leave ends
// its tasks here: a child's (leave), a root's that begins its linger (beginLinger) and a waiting
// root's while the operator runs a tree under its key (leaveUnderOperator). issue is the caller's
// record, which it writes with any field it set, and which stands as written after: the issue
// itself, read for none of its tasks.
func (e *Engine) endTaskWithLeave(ctx context.Context, tx pgx.Tx, issue *record.Issue, status string) error {
	scopes, err := e.scopes(ctx, tx, *issue)
	if err != nil {
		return err
	}
	for i := range scopes {
		s := &scopes[i]
		if err := e.moveTask(ctx, tx, s, phase.Done, s.walk.Place, leaveState(s.walk, status)); err != nil {
			return err
		}
		// The issue stands where the last move left its running steps, read fresh after it.
		issue.Running = s.issue.Running
	}
	issue.Phase, issue.Hold, issue.Awaiting, issue.ChildrenWait = phase.Done, nil, nil, nil
	return e.WriteIssue(ctx, tx, *issue)
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

// leftNotice is the reason of the child-closed or child-status notice that tells the architect
// above child that it left the workflow at status (leave). It starts with "<KEY> is <status>",
// and it names after that what the move leaves for that architect to act on
// (dispatch://INFRA-3178/spec R4). The first is each of the child's pull requests that is open, in
// number order (toldPullRequests): the daemon closes no pull request at a move, and a merge of one
// later closes nothing (merged); a pull request whose close is queued is not named. The second
// is each recorded issue under the child that is not done, with its status, in key order: the move
// ends the child alone, so an issue under it in the workflow runs on, and one out of the workflow
// stays out (record.Beneath, over members, the tree's records). A child with nothing to name gets
// the first sentence alone, so every reader of that bare reason stays valid. The record holds no
// state that says a pull request is in the merge queue (the daemon hears only GitHub's removal
// from it, intake.PullRequestDequeued), so the notice cannot say so.
func leftNotice(child record.Issue, status string, pullRequests []int, members []record.Issue) string {
	reason := fmt.Sprintf("%s is %s", child.Key, status)
	numbers := make([]string, len(pullRequests))
	for i, number := range pullRequests {
		numbers[i] = fmt.Sprintf("#%d", number)
	}
	switch {
	case len(numbers) == 1:
		reason += fmt.Sprintf(". Its pull request %s is open", numbers[0])
	case len(numbers) > 1:
		reason += fmt.Sprintf(". Its pull requests %s and %s are open", strings.Join(numbers[:len(numbers)-1], ", "), numbers[len(numbers)-1])
	}
	under := record.Beneath(child, members)
	slices.SortFunc(under, func(a, b record.Issue) int { return strings.Compare(a.Key, b.Key) })
	var open []string
	for _, member := range under {
		if member.Status != record.DoneStatus {
			open = append(open, fmt.Sprintf("%s (%s)", member.Key, member.Status))
		}
	}
	if len(open) > 0 {
		reason += ". Under it, not done: " + strings.Join(open, ", ")
	}
	return reason
}

// leaveUnderOperator takes a waiting root out of the workflow while the operator runs a tree under
// its key: its record's phase is done, so todo with the label re-admits it as a closed root's does,
// and its task, where it has one, ends as a leave's does; nothing else is written. Its claims, its
// pod and its tree's cleanup are the operator's, which share the root's tokens and names, so no
// suspend, IssueSuspend, linger close or disarm is queued for the workflow's record, and the
// workflow never ran a phase of it to check in production.
func (e *Engine) leaveUnderOperator(ctx context.Context, tx pgx.Tx, root record.Issue, status string) error {
	e.logOnCommit(ctx, "workflow: a waiting root left the workflow while the operator runs a tree under its key; the operator's tree is left alone",
		"issue", root.Key, "tree", root.Tree, "status", status)
	// The record's phase is done, and its task ends with its run where it stands, as every leave's
	// does (endTaskWithLeave), though the workflow never started it: the operator's tree runs the
	// work under this key. A todo re-admits it as a closed root's does.
	return e.endTaskWithLeave(ctx, tx, &root, status)
}

// LeaveUnslotted takes out of the workflow a root admission found in it with neither a slot nor a
// place in the waiting line (admit's releaseUnslotted), as a status leaving todo takes a waiting root
// out (leave, at the status the root stands at): its phase is done and its tree lingers, so nothing
// starts for it, and a todo with the label re-admits it as a lingering root's does. That status is
// the move, so no status write the daemon queued before it lands over it, as for any move out of
// the workflow (dispatchIssue).
func (e *Engine) LeaveUnslotted(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if err := e.store.DropStatusWrites(ctx, tx, root.Key); err != nil {
		return err
	}
	return e.leave(ctx, tx, root, root.Status)
}

// openCheckIfMerged opens the production check of each of merges, the merges of issue's run that a
// leave read (leaveMerges), that merged: a done issue with no merged pull request — a root closed
// before its first phase (close_root), a human's done on an issue short of each merge — has nothing
// to check in production and opens none, a merge that has a check opens no second one (openCheck),
// and neither does a merged one of a project that checks nothing in production, whose openCheck
// opens no record, queues no bound row and starts no look (Engine.checkRole). wake says the
// checking role's claim is suspended, so each first look is a wake of it.
func (e *Engine) openCheckIfMerged(ctx context.Context, tx pgx.Tx, issue record.Issue, merges []record.PullRequest, wake bool) error {
	for _, pr := range merges {
		if pr.State != record.PullRequestMerged {
			continue
		}
		if err := e.openCheck(ctx, tx, issue, pr, wake); err != nil {
			return err
		}
	}
	return nil
}

// awaitedMerge is a task of an issue that stands at its merge, on the copy of the issue read for
// it, and that task's pull request.
type awaitedMerge struct {
	scope TaskScope
	pr    record.PullRequest
}

// mergesAwaited is each task of issue's run that stands at its merge, with its pull request: the
// rows of the issue's merges (record.Store.MergePullRequests) whose task's copy awaits its merge
// (awaitsMerge), in task id order, and for an issue with no plan rows its one row where the issue
// itself awaits its merge. It is the one read of the merges an issue waits on, read on the issue as
// the store holds it, with no copy for a task: whether the merging of an issue is finished
// (PhaseFinished), the close of a merge in hand that a judge of the issue's close makes
// (closeAwaitingMerge) and the hand-over of a member left at its merge with its pull request open
// (unfinishedHandOvers) read it. An issue held for its pull requests (record.HoldForPullRequests)
// awaits no merge of a task, since its tasks are all done.
func mergesAwaited(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) ([]awaitedMerge, error) {
	prs, err := store.MergePullRequests(ctx, tx, issue.Key)
	if err != nil || len(prs) == 0 {
		return nil, err
	}
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return nil, err
	}
	var awaited []awaitedMerge
	for _, pr := range prs {
		if s, ok := scopeOrNoPlan(issue, stored, pr.Task); ok && awaitsMerge(s.issue) {
			awaited = append(awaited, awaitedMerge{scope: s, pr: pr})
		}
	}
	return awaited, nil
}

// judgedClose is what a judge of an issue's close found (judgeClose): whether the issue closed;
// where it did not, the merge in hand whose close waits (nil for none) and why it waits
// (closeWait). Both are empty where no close of a merge in hand was judged: an issue that awaits no
// merge, or whose task's merge is not met.
type judgedClose struct {
	closed bool
	merge  *record.PullRequest
	wait   closeWait
}

// judgeClose is the one judge again of the close of the issue keyed key, at each fact that makes
// what holds that close smaller (dispatch://INFRA-5910 decision 9), why naming the fact for the log
// and merge the merge it follows, nil for none: the departure of an issue beneath it (leave), the
// boot's sweep (sweepMerged), the merge of a pull request of its set that is no task's own, a
// linked one among them (mergedNotOwn), or of a task's own that it links whose task stands at no
// merge (merged), the close of the last open pull request of its set with no merge (closed), and a
// change of its links (takeLinks, pullRequestLinkRead, refuseLinkOfAnotherIssue). It reads the
// issue again, and does nothing where the issue awaits no merge (awaitsMerge). An issue held for
// its pull requests where no merge was in hand (record.HoldForPullRequests) is released where no
// link of its set holds the close any longer (releaseHeldClose). Any other closes each merge in
// hand that awaits its close (closeAwaitingMerge), which owns every condition of the close
// (closeMerged).
//
// No two facts judge at once: intake.SerializeFacts takes one advisory lock for every fact, so each
// judge reads the issue as the fact before it left it. A close writes done, so the issue awaits no
// merge at any later judge, and none closes twice; and each fact that frees the close judges it, so
// none is lost.
func (e *Engine) judgeClose(ctx context.Context, tx pgx.Tx, key string, merge *record.PullRequest, why string) (judgedClose, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !awaitsMerge(*issue) {
		return judgedClose{}, err
	}
	if record.HeldFor(*issue, record.HoldForPullRequests) {
		return e.releaseHeldClose(ctx, tx, *issue, merge, why)
	}
	return e.closeAwaitingMerge(ctx, tx, *issue, why)
}

// releaseHeldClose ends the hold of issue for its pull requests (record.HoldForPullRequests: held
// from merging over tasks that are all done, where no merge was in hand, holdForPullRequests and
// leaveWhereDone) once no link of its set holds its close any longer (setHolds), and the issue
// closes: it is written where its tasks put it, done, with the Dispatch status done (putScope, on
// the copy of a task, which stands in done), the stack's close of each open pull request of a task
// that the issue does not link is queued with the comment that the issue is done, naming the last
// merge of its set (queueDoneClose, lastMerge, decision 11), and it leaves the workflow through the
// one place of a leave its tasks make (leaveByTasks), as leaveWhereDone leaves. While a link still
// holds the close, the architect is told of each such link no notice named yet (tellHolds), and the
// hold stands.
func (e *Engine) releaseHeldClose(ctx context.Context, tx pgx.Tx, issue record.Issue, merge *record.PullRequest, why string) (judgedClose, error) {
	holds, err := setHolds(ctx, e.store, tx, issue.Key)
	if err != nil {
		return judgedClose{}, err
	}
	if len(holds) > 0 {
		return judgedClose{wait: closeWait{holds: holds}}, e.tellHolds(ctx, tx, issue, holds)
	}
	scopes, err := e.scopes(ctx, tx, issue)
	if err != nil {
		return judgedClose{}, err
	}
	last, err := e.lastMerge(ctx, tx, issue.Key, merge)
	if err != nil {
		return judgedClose{}, err
	}
	if err := e.queueDoneClose(ctx, tx, issue, last); err != nil {
		return judgedClose{}, err
	}
	written, err := e.putScope(ctx, tx, &scopes[0])
	if err != nil {
		return judgedClose{}, err
	}
	e.logOnCommit(ctx, "workflow: no pull request of its set holds the close of an issue held for them any longer, and it leaves the workflow",
		"issue", issue.Key, "tree", issue.Tree, "lastMerge", last, "why", why)
	return judgedClose{closed: true}, e.leaveByTasks(ctx, tx, written)
}

// lastMerge is the last merge of the set of issue's pull requests as the close of an issue with no
// merge in hand names it (releaseHeldClose, decision 11), as owner/name#number: merge, the merge the
// judge follows, where one is given; else, since the record keeps no moment of a merge, the merged
// pull request of the set (record.Store.PullRequestSetsOf) that GitHub changed last
// (record.PullRequest.HeadUpdatedAt, a row with none the oldest), then the higher number; "" where
// none merged.
func (e *Engine) lastMerge(ctx context.Context, tx pgx.Tx, issue string, merge *record.PullRequest) (string, error) {
	if merge != nil {
		return prName(merge.Repo, merge.Number), nil
	}
	sets, err := e.store.PullRequestSetsOf(ctx, tx, []string{issue})
	if err != nil {
		return "", err
	}
	var last *record.PullRequest
	for at, pr := range sets[issue] {
		if pr.State != record.PullRequestMerged {
			continue
		}
		if last == nil || pr.HeadUpdatedAt.After(last.HeadUpdatedAt) || pr.HeadUpdatedAt.Equal(last.HeadUpdatedAt) && pr.Number > last.Number {
			last = &sets[issue][at]
		}
	}
	if last == nil {
		return "", nil
	}
	return prName(last.Repo, last.Number), nil
}

// closeAwaitingMerge closes each merge in hand of merged, each task of mergesAwaited whose pull
// request merged, as that merge's own close would (closeMerged), on the copy of the issue read again
// for that task (scope), since the close of the one before it may have moved the issue: the merge
// of a task that is not the last moves that task to done and the issue goes on, and the merge of
// the task whose end ends the issue (lastToFinish) closes it unless its close waits, for an issue
// beneath it still in the workflow or a pull request of its set that holds it, which closeMerged
// owns. A merge in hand of a task that is not the last is one only the boot's sweep finds, which
// GitHub made while the daemon was down. It answers what it found (judgedClose): closed where the
// issue stands done after, else the merge whose close waits and why. It tells nobody of a wait for
// the issues beneath, which the architect heard as the close entered it (closeMergedTelling) and
// which this path meets at every departure and every boot; closeMerged tells the pull requests
// that hold it once each (tellHolds).
func (e *Engine) closeAwaitingMerge(ctx context.Context, tx pgx.Tx, merged record.Issue, why string) (judgedClose, error) {
	awaited, err := mergesAwaited(ctx, e.store, tx, merged)
	if err != nil {
		return judgedClose{}, err
	}
	var judged judgedClose
	for _, at := range awaited {
		if at.pr.State != record.PullRequestMerged {
			continue
		}
		pr := at.pr
		current, err := e.store.Issue(ctx, tx, merged.Key)
		if err != nil || current == nil {
			return judgedClose{}, err
		}
		s, ok, err := e.scope(ctx, tx, *current, at.scope.Task())
		if err != nil {
			return judgedClose{}, err
		}
		if !ok || !awaitsMerge(s.issue) {
			continue
		}
		wait, err := e.closeMerged(ctx, tx, s, &pr)
		if err != nil {
			return judgedClose{}, err
		}
		if wait.waits() {
			judged.merge, judged.wait = &pr, wait
			continue
		}
		e.logOnCommit(ctx, "workflow: a merge in hand is closed where the close of its issue is judged again", "issue", merged.Key,
			"task", s.Task(), "pullRequest", prName(pr.Repo, pr.Number), "why", why)
	}
	now, err := e.store.Issue(ctx, tx, merged.Key)
	if err != nil {
		return judgedClose{}, err
	}
	judged.closed = now == nil || now.Phase == phase.Done
	return judged, nil
}

// awaitsMerge says whether issue awaits its merge: in merging, or held from merging (a failed
// merger's claim, an escalation, a stopped task's, the hold the upgrade's data move left an issue
// that was held in production_check, or the hold of an issue whose tasks are all done while a pull
// request of its set holds its close, record.HoldForPullRequests). The merge is what ends merging,
// so a merged pull request there is a close still waiting on the rest of the tree, or on a pull
// request of its set, rather than a merger still at work. It asks the one predicate written over a
// hold (record.StandsIn) of the one phase, and stays a question of its own rather than folding into
// record.Accepted: a root in retro has been accepted and does not await its merge, and this is
// what closes an issue at its merge (merged, closeMerged), what judges the close of an issue again
// (judgeClose) and what keeps that merge against a later pull request (pullRequestOpened).
func awaitsMerge(issue record.Issue) bool {
	return issue.Phase == phase.Merging || issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == phase.Merging
}

// InWorkflow says whether an issue is in the workflow: its status is one the workflow runs in and
// its phase short of done. One still admitted counts, since it will run — a root reopened inside
// its linger (admit's readmit) stands in `admitted` with no role's phase until its architect
// reaches planning, and is running again from the reopen. It is the production check's "runs
// again" predicate (check.go) and the outbox's (checkLookHolds): a look of an issue that runs
// again would resume the implementer and relaunch its Sandbox under the run, so none starts.
//
// The rule itself is pods.Standing.InWorkflow, which the pod gate reads with the issue's linger
// (pods.InWorkflowNotLingering) and this reads without it: one statement, so a terminal phase
// added to it moves admission, the linger, the production check and the release gate together.
func InWorkflow(issue record.Issue) bool {
	return issue.Standing().InWorkflow(record.OutOfWorkflow)
}

// runningTree is issue's tree's root, and whether the daemon is running that tree: the root is
// recorded, holds an admission slot, and does not linger. byKey and slotted are one read of the
// issues and one of the slots, indexed (indexIssues). The boot's resume and the status repair both
// ask it, so the rule is stated once: a root waiting for a slot can carry a workflow phase of its
// own, and a lingering tree runs nothing.
func runningTree(byKey map[string]record.Issue, slotted map[string]bool, issue record.Issue) (record.Issue, bool) {
	root, ok := byKey[issue.Tree]
	return root, ok && slotted[root.Key] && !root.Lingers()
}

// indexIssues is issues by key and the keys slots holds, the pair runningTree reads.
func indexIssues(issues []record.Issue, slots []record.Slot) (map[string]record.Issue, map[string]bool) {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	slotted := make(map[string]bool, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = true
	}
	return byKey, slotted
}

// beginLinger suspends the root's whole tree and arms its linger deadline, ending the root's hold
// if it has one, as a child's leave does, and its task with the state its leave at status gives it
// (leaveState). Its one caller (leave) calls it once per close: a root that already lingers leaves
// no second time.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue, status string) error {
	until := e.lingerAt()
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	root.LingerUntil = &until
	// The root's leave writes it done, and ends its task where it stands (endTaskWithLeave).
	if err := e.endTaskWithLeave(ctx, tx, &root, status); err != nil {
		return err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "suspend", fmt.Sprintf("the tree of %s lingers", root.Key), nil); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, member.Key, record.IssueSuspend{
			Tree: member.Tree, Generation: member.Generation, TreeGeneration: root.Generation,
		}); err != nil {
			return err
		}
	}
	if err := e.enqueueAt(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation}, until); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: tree lingers", "tree", root.Key, "generation", root.Generation, "until", until)
	return nil
}

// unfinishedHandOvers is every member the close of a root's linger hands to Legion as a root of
// its own (handOver). A root that stayed done hands over every member it leaves in the workflow,
// whatever phase that member stands in. A root that did not stay done hands over only a member
// whose merge is still to land: one standing at phase.Merging with a task at its merge whose pull
// request is still open, or whose close a pull request of its set holds (setHolds,
// dispatch://INFRA-5910 decision 9), and one held for its pull requests where no merge was in hand
// (record.HoldForPullRequests), and nothing else. It is read before the close's own writes, because
// what the close hands over is also what it does not warn about: a decision a person made on one of
// these members, or on an issue below one, is owed to that member's architect once it is
// re-admitted, not left behind by this tree (warnKeptDecisionsOfAClosedTree).
//
// A reopen inside the linger resumes those members in the old tree instead: lingerExpired is
// fenced on the root generation, and the durable hand-over record makes a late daemon PATCH a
// no-op when the root came back before its event arrives. Each hand-over keeps the member's
// phase, gate, phase rows and pull request across a new root generation; promotion starts the
// phase's role again in that phase. A member that never started a phase carries no run
// (record.Issue.CarriesRun is false at phase.Admitted), so the new root carries none either, and
// its re-admission is another tree's work arriving rather than its own coming back: it is
// checked against its blockers as any other arrival is (blockers.Exempt).
//
// Under a done root the rule is the member's own state and not why it waited: its tree closed
// and it never started. A member standing at admitted is handed over whatever held it there — an
// unfinished Dispatch blocker, an ancestor the daemon had accepted (record.HeldUnderAccepted),
// such an ancestor that left the workflow during the linger, the children-first wait of a parent
// whose own children are still open, or the admission cap alone, which counts issues and so
// leaves a released child standing at admitted for a slot. The last two are ordinary rather than
// rare under that cap, which is why the state and not the hold is read. Nothing would start such
// a member where it stands: its tree has closed, the line offers a slot to nothing of a closed
// tree, and a status it already carries moves nobody, so a member left todo at admitted in a
// closed tree is left there for good — which is the whole of what the hand-over prevents. The
// hold is not read because it can be gone by the expiry while the member still stands there: an
// ancestor that held it can merge, or be moved to done or to backlog, during the linger.
//
// It is the record's status (record.Issue.Status) that says a member is in the workflow, under
// either rule, and under a done root the phase is not read at all: a member a person took out of
// the workflow is left with the tree (record.OutOfWorkflow), an unreleased child among them,
// while a member whose phase is done that a person set back to todo during the linger is handed
// over, and is admitted fresh because its phase carries no run.
//
// The clause for a root that did not stay done — one a person or a controller parked — is narrow
// on purpose. Such a root can be re-admitted, and a re-admission resumes a member of any other
// phase where it stood (admit's startMidPhaseMembers), so nothing of it is stranded by waiting.
// The one thing a re-admission cannot do is make the merge the park stopped: the merge is the
// merger's own call, made in its own turn, and that turn ended with the park. So the member this
// guard would otherwise strand is exactly the one holding an open pull request with no live tree
// left to merge it — green and approved, with nothing in the workflow that would ever merge it —
// and the one whose close an open pull request of its set holds, whose merge in hand closed
// nothing (closeMerged), or whose tasks are all done: left behind, the close of its tree would end
// that merge with the pull request that holds it still open. Whether a pull request is open is the
// one fact here that record.Issue does not carry, so it is read from the pull_requests row of each
// task of the member that stands at its merge (mergesAwaited) and from the links of its set
// (setHolds), and only for a member of a non-done root's tree that this clause could take — one in
// the workflow at phase.Merging or held for its pull requests — so a tree of many members costs a
// handful of reads at most.
//
// What the close does around the hand-over, for a member handed over under either rule: the same
// expiry still queues a tree_close for each claim of every member — one for each stored claim, and
// one by role for each role whose worker claim is none of those — and a workspace removal for
// every member, handed over or not (lingerExpired), so a handed member's claims are retired, their
// sessions kept, and its workspace removed here with the rest of the tree. Its workspace comes
// back when the first start of its new generation reaches one of those retired claims (the
// promotion's start of its architect, or the start of its phase role): the outbox's start row
// provisions the workspace in its supervise.StateRetired branch and then relaunches the claim
// (internal/daemon/outbox.go). The phase it comes back in is the phase its record carries, and a
// member carried in merging is merged by its own merger under its new tree, since the park
// stopped the merge that tree's merger had not made. A member added to the handed set also takes
// the decisions parked on it with it: warnKeptDecisionsOfAClosedTree logs those as carried to the
// child's own architect rather than warning that this close left them behind.
//
// The non-done clause reads the member's phase itself rather than a predicate written over a
// hold, because the phase is the spec's own rule and a hold from the merge phase is a state no
// live daemon writes for a merge still to make: a hold of a failure is written only where the role
// that works issue.WorkingPhase() is the role whose claim died (claimFailed), and the merger holds
// that phase, so its own death leaves the issue held from merging with its claim failed rather than
// held from a phase nobody works. The one hold from merging it reads is the hold for its pull
// requests, which stands over tasks that are all done and is no failure.
func (e *Engine) unfinishedHandOvers(ctx context.Context, tx pgx.Tx, root record.Issue, members []record.Issue) ([]record.Issue, error) {
	done := root.Status == "done"
	var handed []record.Issue
	for _, member := range members {
		if member.Key == root.Key || record.OutOfWorkflow(member.Status) {
			continue
		}
		if !done {
			if member.Phase != phase.Merging && !record.HeldFor(member, record.HoldForPullRequests) {
				continue
			}
			awaited, err := mergesAwaited(ctx, e.store, tx, member)
			if err != nil {
				return nil, err
			}
			if !slices.ContainsFunc(awaited, func(at awaitedMerge) bool { return at.pr.State == record.PullRequestOpen }) {
				// A merge in hand whose close a pull request of the set holds, or a hold for the
				// pull requests with no merge in hand: either stands while setHolds is not empty.
				inHand := slices.ContainsFunc(awaited, func(at awaitedMerge) bool { return at.pr.State == record.PullRequestMerged })
				if !inHand && !record.HeldFor(member, record.HoldForPullRequests) {
					continue
				}
				holds, err := setHolds(ctx, e.store, tx, member.Key)
				if err != nil {
					return nil, err
				}
				if len(holds) == 0 {
					continue
				}
			}
		}
		handed = append(handed, member)
	}
	return handed, nil
}

// handOver writes the durable fence before queuing the one Dispatch PATCH that sets the child todo
// and adds the legion label. intake consumes its daemon-authored event only while the fence names a
// root that still lingers at this generation; the direct child re-admission then carries state into
// the child's independent tree.
func (e *Engine) handOver(ctx context.Context, tx pgx.Tx, root, child record.Issue) error {
	if err := e.store.PutHandover(ctx, tx, record.Handover{Issue: child.Key, Tree: root.Tree, Generation: root.Generation}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a finished root hands its unfinished child to Legion", "issue", child.Key, "tree", root.Key,
		"phase", child.Phase, "status", child.Status)
	return e.enqueue(ctx, tx, child.Key, record.HandOverStatusWrite("todo", child.Status, root.Tree, root.Generation))
}

// lingerExpired closes the tree whose root's linger elapsed at the generation the fact names: a
// tree_close of every member's claims, the removal of every member's workspace, and the hand-over
// of each member unfinishedHandOvers names — every member a root that stayed done left in the
// workflow, or, under a root that did not, one left awaiting its merge with its pull request open,
// or with its close held by an open pull request of its set.
// The close waits while a member's production check is open: the check's looks run in the
// tree's Sandbox, on its volume, which the close deletes, so the fact returns a wait naming the
// moment the earliest open check is bound to have ended its last look (its bound plus the look
// bound), rolls back unapplied, and its row runs again then — or sooner, since the close of the
// tree's last open check re-arms the row (closeCheck). A tree whose root was reopened, or that
// lingers at a later generation, changes nothing.
func (e *Engine) lingerExpired(ctx context.Context, tx pgx.Tx, fact intake.LingerExpired) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || !issue.Lingers() {
		return intake.Result{}, err
	}
	open, err := e.store.OpenProductionChecksOfTree(ctx, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if len(open) > 0 {
		earliest := slices.MinFunc(open, func(a, b record.ProductionCheck) int { return a.BoundAt.Compare(b.BoundAt) })
		return intake.Result{}, wait.Until(earliest.BoundAt.Add(CheckLookBound),
			fmt.Sprintf("tree %s lingers past its deadline; member %s's production check is open", issue.Key, earliest.Issue))
	}
	members, err := e.treeMembers(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	// Each row names the root generation whose linger it expires: a member keeps its own generation
	// across re-admission, so only that tells this linger's rows from a later one's. The rows reach
	// the claims every request of every claim of a member reaches (claimTargets): each stored claim
	// by its token, so a claim of a role a keyed agent took over gets its close too, and each role
	// whose step's claim is none of those by the role alone, so a claim that comes up after this read
	// gets its close when the row runs. The tree's cleanup waits for every claim these rows reach.
	for _, member := range members {
		targets, err := e.claimTargets(ctx, tx, member, nil)
		if err != nil {
			return intake.Result{}, err
		}
		for _, target := range targets {
			closeRow := record.SuperviseRequest{Op: "tree_close", Tree: member.Tree, Role: target.role, Claim: target.token,
				Generation: member.Generation, Linger: issue.Generation}
			if err := e.enqueue(ctx, tx, member.Key, closeRow); err != nil {
				return intake.Result{}, err
			}
		}
		if err := e.enqueue(ctx, tx, member.Key, record.WorkspaceRemove{Linger: issue.Generation, Tree: member.Tree}); err != nil {
			return intake.Result{}, err
		}
	}
	e.logOnCommit(ctx, "workflow: linger closed the tree", "tree", issue.Key, "generation", issue.Generation, "issues", len(members))
	handed, err := e.unfinishedHandOvers(ctx, tx, *issue, members)
	if err != nil {
		return intake.Result{}, err
	}
	// The close ends the merge of each member it neither hands over nor leaves an open pull request:
	// that merge can no longer land, and the record keeps no mark of the close for the fact's settle
	// to read (closeLinger, mergeCanLand).
	if err := e.closeLinger(ctx, issue.Tree); err != nil {
		return intake.Result{}, err
	}
	if err := e.warnKeptDecisionsOfAClosedTree(ctx, tx, *issue, handed); err != nil {
		return intake.Result{}, err
	}
	for _, child := range handed {
		if err := e.handOver(ctx, tx, *issue, child); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

// warnKeptDecisionsOfAClosedTree names every decision a person made while nobody in this tree
// could hear it that the tree's close leaves behind: every row whose issue stands in this tree
// now, in whatever state — kept for a start or a catch-up to name, claimed by one, or held by the
// unconfirmed task queued on an architect's claim (Store.ParkedDecisions, whose reading is by the
// issue each row was decided on). The tree closes for good here, so no start task and no catch-up
// of it names them to an architect again, and each is one Warn line of its own, written once the
// close has committed (warnOnCommit), so a row run again after a failed commit writes nothing
// twice. The controller was told each when it was kept — that is what a person's decision on a
// tree nobody in it hears reaches (record.RelayParkedDecision's notice) — so the line says where
// the decision stands rather than that it is lost.
//
// A decision this same close hands to a child is no such decision: one made on a member the close
// hands to Legion (handed) goes with that member into the tree of its own, where it stands under
// its own issue with nothing to move, and that member's architect is told it at its start. Those
// are logged as handed over rather than warned about, so the Warn lines name what this close
// really leaves standing. A member the close leaves behind — one already out of the workflow,
// below a handed child or not — keeps what was decided on it in this closing tree, and that is a
// Warn line.
func (e *Engine) warnKeptDecisionsOfAClosedTree(ctx context.Context, tx pgx.Tx, root record.Issue, handed []record.Issue) error {
	goesWithAChild := make(map[string]bool, len(handed))
	for _, child := range handed {
		goesWithAChild[child.Key] = true
	}
	kept, err := e.store.ParkedDecisions(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	for _, decision := range kept {
		e.keptDecisionOfAClosedTree(ctx, root.Key, decision.Ref, decision.Issue, keptDecisionStands(decision), goesWithAChild)
	}
	return nil
}

// keptDecisionStands names, for the close's line, where a decision the closing tree still owes
// stands now: held by the unconfirmed task queued on an architect's claim, claimed by a start or
// a catch-up that has not settled it, or kept for the next one to name.
func keptDecisionStands(decision record.ParkedDecision) string {
	switch {
	case decision.HeldBy.Claim != "":
		return "held by its architect's unconfirmed task"
	case decision.ClaimedBy != 0:
		return "claimed by a start or a catch-up"
	default:
		return "kept for its issue's tree"
	}
}

// keptDecisionOfAClosedTree writes the one line this decision gets: the Warn for a decision the
// close leaves standing, and an Info for one that goes with a child the same close hands over,
// which names the child's own architect's start as what tells it.
func (e *Engine) keptDecisionOfAClosedTree(ctx context.Context, root, ref, issue, where string, goesWithAChild map[string]bool) {
	if goesWithAChild[issue] {
		e.logOnCommit(ctx, "workflow: the tree closed owing a person's decision; it goes to the architect of the child this close handed to Legion",
			"ref", ref, "root", root, "issue", issue, "where", where)
		return
	}
	e.warnOnCommit(ctx, "workflow: the tree closed while it still owed a person's decision to an architect; the controller already has its parked-decision notice",
		"ref", ref, "root", root, "issue", issue, "where", where)
}
