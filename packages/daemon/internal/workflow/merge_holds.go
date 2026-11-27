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

// mergeHoldsChanged tells each role that a line of the project's merge holds document can hold
// that the document changed (intake.MergeHoldsChanged). The architect that owns each issue with a
// task in accepting, whose sign_off a line can hold, gets a merge-holds-changed notice. The merger
// of each issue with a task in merging, whose merge a line can hold, gets a row of its own
// (record.MergeHoldsChanged), since a notice reaches only an architect. Both carry one text
// (record.MergeHoldsChangedReason), and an issue with a task in each phase gets both.
//
// Where a task stands is the copy of the issue read for it (scopesOn), not the issue's own phase:
// an issue with plan rows stands in its least finished task's phase (issuePhaseOfTasks), so one
// task waits at its acceptance or its merge while the issue stands in implementing for another. A
// run with no plan rows is the issue itself, as before plans.
//
// A role that a hold stopped ends its turn with a WAITING line and subscribes to nothing: under
// `runtime: kubernetes` a Dispatch document event does not wake a released pod, and an outbox
// delivery does (the daemon's wakeForDelivery). So these rows are what wake a held role when the
// controller lifts the hold, and what tell a role that a new hold applies to it.
//
// The daemon does not read the document, so it does not know which issue a line holds. It tells
// every issue with a task in the two phases, held or not, and each role reads the document
// itself. An issue none of whose tasks still works the phase is told nothing (tasksLeftPhase: out
// of the workflow, its tree lingers, or the pull request of each task in merging has merged). The
// plans of the live issues are read in one read (record.Store.PlansOf), and the pull requests that
// rule weighs in one query for every issue it reaches with a task in merging
// (record.Store.TaskPullRequestRowsOf), not one read per issue. A fact of a project other than the
// engine's writes nothing, and the decoder gives none (intake's decodeDispatchDocumentFact). A
// second delivery of the same event writes nothing, since ApplyFact records the event and skips
// it. The fact moves no phase, writes no slot and wakes no claim itself: each row wakes its own
// role when it runs.
func (e *Engine) mergeHoldsChanged(ctx context.Context, tx pgx.Tx, fact intake.MergeHoldsChanged) (intake.Result, error) {
	if fact.Project != e.cfg.Project {
		return intake.Result{}, nil
	}
	issues, err := e.store.LiveIssues(ctx, tx, e.cfg.Project)
	if err != nil {
		return intake.Result{}, err
	}
	plans, err := e.store.PlansOf(ctx, tx, issues)
	if err != nil {
		return intake.Result{}, err
	}
	reason := record.MergeHoldsChangedReason(fact.Project)
	lingering := map[string]bool{}
	// reached is each issue with a task in either phase, with the scope of each of its tasks and
	// whether its tree lingers, in the order the record lists them, and merging the keys of those
	// with a task still in merging by every test but its pull request's, which one read answers for
	// all of them below.
	type reachedIssue struct {
		issue   record.Issue
		scopes  []TaskScope
		lingers bool
	}
	var reached []reachedIssue
	var merging []string
	for _, issue := range issues {
		scopes := scopesOn(issue, plans[issue.Key])
		if !slices.ContainsFunc(scopes, func(s TaskScope) bool {
			return s.issue.Phase == phase.Accepting || s.issue.Phase == phase.Merging
		}) {
			continue
		}
		lingers, read := lingering[issue.Tree]
		if !read {
			if lingers, err = record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil {
				return intake.Result{}, err
			}
			lingering[issue.Tree] = lingers
		}
		reached = append(reached, reachedIssue{issue: issue, scopes: scopes, lingers: lingers})
		if readsMergePullRequests(scopes, lingers) {
			merging = append(merging, issue.Key)
		}
	}
	requests, err := e.store.TaskPullRequestRowsOf(ctx, tx, merging)
	if err != nil {
		return intake.Result{}, err
	}
	var architects, mergers []string
	for _, r := range reached {
		key, prs := r.issue.Key, requests[r.issue.Key]
		if tasksLeftPhase(r.scopes, phase.Accepting, r.lingers, prs) == "" {
			if err := e.notice(ctx, tx, key, record.Notice{Kind: "merge-holds-changed", Role: claim.RoleArchitect,
				Phase: phase.Accepting, Reason: reason}); err != nil {
				return intake.Result{}, err
			}
			architects = append(architects, key)
		}
		if tasksLeftPhase(r.scopes, phase.Merging, r.lingers, prs) == "" {
			if err := e.enqueue(ctx, tx, key, record.MergeHoldsChanged{Project: fact.Project}); err != nil {
				return intake.Result{}, err
			}
			mergers = append(mergers, key)
		}
	}
	e.logOnCommit(ctx, "workflow: the project's merge holds changed; the architects in accepting and the mergers in merging are told",
		"event", fact.Event, "accepting", architects, "merging", mergers)
	return intake.Result{}, nil
}

// MergingNow reads in tx what the merging phase of a merge holds change is judged by: the issue,
// the scope of each of its tasks (scopesOn, over its plan at its generation, the issue itself for
// a run with no plan rows), whether its tree lingers (record.TreeLingers) and the rows of its pull
// requests (record.Store.TaskPullRequestRows), which it reads only where a task is still in
// merging by every other test (readsMergePullRequests). It returns the issue, and why the workflow
// works no merge of the issue's tasks for a merge hold (tasksLeftPhase), "" while it works one. An
// issue the record does not hold is a nil issue and no reason. The fact that writes the merger's
// row of a merge holds change (mergeHoldsChanged) and the daemon's executor of that row judge the
// issue by this one rule, so the two do not disagree about an issue that left merging. It is
// exported for that executor (the daemon's mergeHoldsChanged).
func MergingNow(ctx context.Context, store record.Store, tx pgx.Tx, key string) (*record.Issue, string, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, "", err
	}
	lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree)
	if err != nil {
		return nil, "", err
	}
	stored, err := store.Plan(ctx, tx, key, issue.Generation)
	if err != nil {
		return nil, "", err
	}
	scopes := scopesOn(*issue, stored)
	var prs []record.PullRequest
	if readsMergePullRequests(scopes, lingers) {
		if prs, err = store.TaskPullRequestRows(ctx, tx, key); err != nil {
			return nil, "", err
		}
	}
	return issue, tasksLeftPhase(scopes, phase.Merging, lingers, prs), nil
}

// readsMergePullRequests says whether the merge holds' rule reads the pull requests of an issue
// whose tasks' scopes are scopes, lingers being whether its tree lingers: a task of it still works
// merging by every test of the rule but its pull request's (leftPhase).
func readsMergePullRequests(scopes []TaskScope, lingers bool) bool {
	return slices.ContainsFunc(scopes, func(s TaskScope) bool { return leftPhase(s.issue, phase.Merging, lingers) == "" })
}

// tasksLeftPhase says why the workflow works no task of an issue at at for a merge hold, "" while
// it works one: heldPhaseLeft over the copy of the issue read for each task (scopes, scopesOn),
// lingers being whether the issue's tree lingers, and in merging that task's own pull request
// among prs, the rows of the issue's pull requests (scopePullRequest, finishedBy). One task that
// works at is enough: the role is told once for the issue, whatever its count of tasks there, and
// reads the document itself. The reason is read in the one log line of a row that finishes
// undelivered (the daemon's mergeHoldsChanged), so it names the task of each copy of a run with
// plan rows, whatever the plan's count of them, and a run with no plan rows says what it always
// said. No scope at all works nothing, and the reason says so.
func tasksLeftPhase(scopes []TaskScope, at phase.Phase, lingers bool, prs []record.PullRequest) string {
	left := make([]string, 0, len(scopes))
	for _, s := range scopes {
		why := heldPhaseLeft(s.issue, at, lingers, finishedBy(s.issue, scopePullRequest(s, prs)))
		if why == "" {
			return ""
		}
		if s.Planned() {
			why = fmt.Sprintf("for task %s, %s", s.Task(), why)
		}
		left = append(left, why)
	}
	if len(left) == 0 {
		return fmt.Sprintf("the issue has no task to stand in %s", at)
	}
	return strings.Join(left, "; ")
}

// scopePullRequest is the pull request of s's task among prs, the rows of its issue's pull
// requests in task order (record.Store.TaskPullRequestRows, TaskPullRequestRowsOf): the first row
// of the task, as record.Store.PullRequestRowOfTask reads it, and for a run with no plan rows the
// first row of the task "", the issue's merge (record.Store.MergePullRequestRow); nil where it has
// none.
func scopePullRequest(s TaskScope, prs []record.PullRequest) *record.PullRequest {
	task := ""
	if s.Planned() {
		task = s.Task()
	}
	if at := slices.IndexFunc(prs, func(pr record.PullRequest) bool { return pr.Task == task }); at >= 0 {
		return &prs[at]
	}
	return nil
}

// heldPhaseLeft says why the workflow no longer works issue's phase at for a merge hold, "" while
// it does, from what its caller read: issue is the copy read for one task (tasksLeftPhase), lingers
// is whether the issue's tree lingers (record.TreeLingers), and merged whether the phase's own
// terminating fact has arrived for that task: the merge of its own pull request (finishedBy). The
// copy must still work at (leftPhase), and that fact must not have arrived. A root whose pull
// request merged stands in merging until every issue under it leaves the workflow (closeMerged),
// and a hold stops nothing there: its merger merged already, or none ran because the pull request
// had merged when the phase began. A row for such a root would either wait, tried again, on a
// merger no machine holds, keeping the pod from its idle release (the store's idleWaitingKinds),
// or wake a released merger to begin step 1 again with nothing to merge. The merge-queue rows pass
// over the same root, since their rule (leftMergingAt) needs an open pull request. Accepting has
// no such fact, so a copy in accepting is judged by leftPhase alone. It reads nothing, so the fact
// that weighs every issue at once (mergeHoldsChanged) and the executor that weighs one
// (MergingNow) each read what it weighs in its own way and decide by this one rule, through
// tasksLeftPhase.
func heldPhaseLeft(issue record.Issue, at phase.Phase, lingers, merged bool) string {
	if left := leftPhase(issue, at, lingers); left != "" {
		return left
	}
	if merged {
		return fmt.Sprintf("the pull request is %s", record.PullRequestMerged)
	}
	return ""
}
