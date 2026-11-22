package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// issueRead bounds one GitHub read the per-issue read makes: the pull request itself, the list of
// the issue's branch when the record holds none, the reviews, and the compare of an approved head
// with the head. Each is a single answer that comes in well under a second, so a read that runs
// this long holds a request GitHub never answered.
const issueRead = 30 * time.Second

// readIssuePullRequest reads one issue's pull request from GitHub and applies what it shows, as the
// webhook would have: the head GitHub holds it at (followHead, and the pushes that reached it),
// whether that head can be merged into its base and GitHub's own merge state (applyMergeability),
// and a deciding review the daemon has not recorded (readReviews), in that order. An issue
// whose record holds no open pull request is looked up by its branch instead (openPullRequestOn),
// and the opened its webhook would have carried is applied first, so the reads that follow judge
// the pull request that leaves.
//
// It is the backstop for a webhook that never arrived: GitHub can fail to deliver one, and
// Dispatch's redelivery sweep gives up after five attempts, which leaves the record holding a head,
// a review or a mergeability GitHub has moved past, and every later decision judged against it.
//
// It runs at two moments and at no other: as the workflow runtime starts (readIssuesAtStart), which
// covers the deliveries made while the daemon was down, and once an agent's handoff has been
// recorded (readHandedOff), the moment the daemon next decides on the pull request's state. It
// reads one issue, not every open pull request, so a handoff costs the reads of its own pull
// request alone.
//
// Every fact it applies goes through intake.ApplyFact under an event id of its own, and the
// workflow's own fences decide each against the record: a second read of a pull request nothing
// changed on records the moment of the read and nothing else (applyMergeability,
// record.PullRequest.StateReadAt), which is what tells a head read a minute ago from one nothing
// has read for a day. A failed read is returned to the caller, which
// logs it; the record then stands as it was, and the next read of the issue asks again.
//
// It answers whether GitHub is still computing the merge of the pull request's head: its read
// found the pull request open with merge state `unknown` or `mergeable` null. The start read
// queues a ladder of reads for such an issue (readIssuesAtStart), which a handoff's read does not.
// A read whose reviews read must stop - ctx ended, or GitHub's rate limit, which the ladder's reads
// would meet as well - answers false.
func (w *workflowRuntime) readIssuePullRequest(ctx context.Context, issue string) (bool, error) {
	recorded, err := w.issuePullRequest(ctx, issue)
	if err != nil || recorded == nil {
		return false, err
	}
	pr := *recorded
	read, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, err := w.readPullRequest(read, pr)
	cancel()
	if err != nil {
		return false, fmt.Errorf("read pull request %s#%d and its mergeability: %w", pr.Repo, pr.Number, err)
	}
	// The moment GitHub's answer was in hand, taken before the head follow below: that follow can
	// make more GitHub calls and wait on the fact lock, and the stamp is the age of this answer, not
	// of the work that came after it (applyMergeability, record.PullRequest.StateReadAt).
	answeredAt := time.Now().UTC()
	if pr, err = w.followHead(ctx, github, pr, pull); err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		// The reads below go on against the record as it stands when a comparison GitHub keeps
		// failing stops this one following the head: the next read of the issue follows it.
		w.log.Warn("follow the head GitHub shows for an issue's pull request; this read judges it at the recorded head",
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "error", err)
	}
	// The reads after the head start their bound only now: following a head waits on the fact lock,
	// which the GitHub consumer's backlog can hold for a while.
	prCtx, cancelReads := context.WithTimeout(ctx, issueRead)
	defer cancelReads()
	// A failed apply is logged there, and the next read of the issue applies it again.
	_ = w.applyMergeability(ctx, github, pr, pull, answeredAt, w.mergeabilityEventID(pr, answeredAt))
	if w.readReviews(ctx, prCtx, github, pr) {
		return false, nil
	}
	return pull.State == "open" && (pull.mergeState() == record.MergeStateUnknown || pull.mergeability() == record.MergeabilityUnknown), nil
}

// issuePullRequest is the open pull request the per-issue read judges for key: the pull request of
// the task the read judges (issueRecords.task), the record's when it holds one open, and otherwise
// the one GitHub shows open on that task's branch (openPullRequestOn), applied as the
// intake.PullRequestOpened its webhook would have carried - naming the repository as that
// webhook's fact would (heardSpelling), a reopen when the record holds the same pull request
// closed - and read back as the workflow recorded it. It is nil for an issue the daemon does not
// record, one with no task to judge, one with no repository to read, one whose task's branch GitHub
// holds no open pull request on - which an implementing task's architect is told of (tellMissing) -
// and one whose opened the workflow declined, which leaves the record as it was and nothing for
// the reads that follow to judge.
func (w *workflowRuntime) issuePullRequest(ctx context.Context, key string) (*record.PullRequest, error) {
	recorded, err := issueRecord(ctx, w.pool, w.records, key)
	if err != nil || recorded.issue == nil || !recorded.judged {
		return nil, err
	}
	issue, pr := recorded.issue, recorded.pullRequest()
	if pr != nil && pr.State == record.PullRequestOpen {
		return pr, nil
	}
	if issue.GitHubRepo == "" {
		return nil, nil
	}
	branch := recorded.branch
	read, cancel := context.WithTimeout(ctx, issueRead)
	pull, found, err := w.openPullRequestOn(read, issue.GitHubRepo, branch)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("list the open pull requests of %s on %s: %w", issue.GitHubRepo, branch, err)
	}
	if !found {
		w.tellMissing(ctx, recorded, pr)
		return nil, nil
	}
	repo := w.heardSpelling(issue.GitHubRepo)
	// The opened this read applies carries GitHub's updated_at and nothing of when the head
	// arrived: a listed review names the head it read in its own Legion footer
	// (intake.PullRequestReview), so no clock of this read is weighed against a review's.
	w.log.Warn("workflow: GitHub holds an open pull request for an issue the daemon records none for; applying the opened its webhook would",
		"repo", repo, "pullRequest", pull.Number, "issue", key, "branch", branch, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt)
	fact := intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, HeadSHA: pull.Head.SHA, Body: pull.Body,
		URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: pr != nil && pr.Number == pull.Number}
	eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, repo, pull.Number, time.Now().UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return nil, fmt.Errorf("apply the open pull request %s#%d read from GitHub: %w", repo, pull.Number, err)
	}
	again, err := issueRecord(ctx, w.pool, w.records, key)
	if err != nil {
		return nil, err
	}
	after := again.pullRequest()
	if after == nil || after.State != record.PullRequestOpen || after.Number != pull.Number {
		w.log.Info("workflow: the workflow did not record the open pull request this read found; the next read of the issue applies it again",
			"repo", repo, "pullRequest", pull.Number, "issue", key)
		return nil, nil
	}
	return after, nil
}

// tellMissing tells the architect of recorded's issue that the implementer of the task the read
// judges completed and GitHub lists no open pull request on that task's branch, with none open or
// merged recorded for that task either (intake.PullRequestMissing, which the workflow decides and
// tells: workflow's pullRequestMissing, which reads the task from the branch the fact names). The
// per-issue read reaches it where the pass once did: an implementer's completion queues the read
// of its own issue (handoffCompleted), and the daemon's start reads every issue whose implementer
// completed with no pull request recorded (readIssuesAtStart). A task in any other phase, or one
// whose record lets the implementation move, is told nothing. The event id names the issue's
// generation and the handoff commit, so the architect is told once per handoff commit, whichever
// read finds it, and again only for a completion at another commit. A failed apply is logged,
// and the next read of the issue tells it.
func (w *workflowRuntime) tellMissing(ctx context.Context, recorded issueRecords, pr *record.PullRequest) {
	issue, commit := *recorded.issue, recorded.handoff
	if !recorded.implementing() || record.LetsImplementationMove(pr) || commit == "" {
		return
	}
	branch := recorded.branch
	eventID := fmt.Sprintf("pull-request-missing:%s:%s:%d:%s", w.dispatchProject, issue.Key, issue.Generation, commit)
	w.log.Warn("workflow: GitHub lists no open pull request on the branch of a task whose implementer completed; telling its architect",
		"repo", issue.GitHubRepo, "issue", issue.Key, "task", recorded.task, "branch", branch, "handoff", commit)
	fact := intake.PullRequestMissing{Issue: issue.Key, Repo: issue.GitHubRepo, Branch: branch, HandoffCommit: commit}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply a missing pull request read from GitHub", "repo", issue.GitHubRepo, "issue", issue.Key, "error", err)
	}
}

// issueRecords is what the reads of an issue's pull requests read of its record, in one
// transaction (issueRecord): the issue, nil where it is not recorded; the task of its plan at its
// generation whose pull request the per-issue read judges, its branch (record.PlanTask.BranchOf)
// and whether it has one (judgedTask); its implementer's handoff commit; and each pull request of
// the issue, by task (record.Store.TaskPullRequests).
type issueRecords struct {
	issue   *record.Issue
	task    string
	branch  string
	judged  bool
	several bool
	handoff string
	prs     []record.PullRequest
}

// pullRequest is the pull request of the task the per-issue read judges, nil for none.
func (r issueRecords) pullRequest() *record.PullRequest {
	if !r.judged {
		return nil
	}
	return r.find(func(pr record.PullRequest) bool { return pr.Task == r.task })
}

// numbered is the issue's pull request numbered number, nil for none.
func (r issueRecords) numbered(number int) *record.PullRequest {
	return r.find(func(pr record.PullRequest) bool { return pr.Number == number })
}

// openAt is the issue's open pull request at head, nil for none.
func (r issueRecords) openAt(head string) *record.PullRequest {
	return r.find(func(pr record.PullRequest) bool { return pr.State == record.PullRequestOpen && pr.HeadSHA == head })
}

func (r issueRecords) find(match func(record.PullRequest) bool) *record.PullRequest {
	if at := slices.IndexFunc(r.prs, match); at >= 0 {
		return &r.prs[at]
	}
	return nil
}

// implementing says whether the task the per-issue read judges stands in implementing: the issue
// does, for an issue whose plan holds one task or none, and one of the issue's running steps of
// that task does (record.Issue.RunningAt), for one whose plan holds several.
func (r issueRecords) implementing() bool {
	if !r.several {
		return r.issue.Phase == phase.Implementing
	}
	return slices.ContainsFunc(r.issue.RunningAt(phase.Implementing), func(step record.RunningStep) bool { return step.Task == r.task })
}

// judgedTask is the task of stored, the plan of the issue keyed key at its generation, whose pull
// request the per-issue read judges, and whether it has one: the one task of a plan of one, the
// issue's own work for one with no plan rows (the zero task, on the issue's branch), and for a plan
// of several the task whose step the issue's implementer runs or ran last (implementer's
// record.PhaseRow.StepTask), which is the task an implementer's completion queues the read for:
// none where that row names no task of the plan.
func judgedTask(stored *record.StoredPlan, implementer record.PhaseRow) (record.PlanTask, bool) {
	switch {
	case stored == nil:
		return record.PlanTask{}, true
	case len(stored.Tasks) == 1:
		return stored.Tasks[0], true
	}
	at := slices.IndexFunc(stored.Tasks, func(task record.PlanTask) bool { return task.Task == implementer.StepTask })
	if at < 0 {
		return record.PlanTask{}, false
	}
	return stored.Tasks[at], true
}

// issueRecord is key's records as the reads of its pull requests read them (issueRecords), in one
// transaction: what the per-issue read and the merge-state ladder read before they read GitHub
// (issuePullRequest, readMergeState), and what the second read of a conflict reads before it acts
// (outbox's conflictConfirm).
func issueRecord(ctx context.Context, pool *store.Pool, records record.Store, key string) (issueRecords, error) {
	var read issueRecords
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if read.issue, err = records.Issue(ctx, tx, key); err != nil || read.issue == nil {
			return err
		}
		stored, err := records.Plan(ctx, tx, key, read.issue.Generation)
		if err != nil {
			return err
		}
		rows, err := records.Phases(ctx, tx, key)
		if err != nil {
			return err
		}
		implementer := record.PhaseRowOf(rows, claim.RoleImplementer)
		task, judged := judgedTask(stored, implementer)
		read.task, read.branch, read.judged, read.handoff = task.Task, task.BranchOf(key), judged, implementer.HandoffCommit
		read.several = stored != nil && len(stored.Tasks) > 1
		read.prs, err = records.TaskPullRequests(ctx, tx, key)
		return err
	}); err != nil {
		return issueRecords{}, fmt.Errorf("read the record of %s: %w", key, err)
	}
	return read, nil
}

// readIssuesAtStart reads the pull request of each issue in the workflow once as the runtime starts
// (readIssuePullRequest): every issue with an open pull request recorded, and every issue whose
// implementer completed with none open or merged recorded, which is the pull request this read
// looks up by the branch. It is what covers a daemon that was down while GitHub made the
// deliveries nothing redelivers. A read that fails is logged and the boot goes on, since the
// issue's next handoff reads the same pull request; GitHub's rate limit ends the start read there,
// because every read after it on the same installation meets the same limit, and the panes share
// it.
//
// A read that finds GitHub still computing the merge of an open pull request's head - merge state
// `unknown`, or `mergeable` null - queues a ladder of reads for the issue (queueMergeState), which
// climbs and then follows the head while GitHub goes on answering `unknown` (followMergeState).
// The reads that followed GitHub's merge state before the daemon stopped were held in memory, and
// this is what goes on with them after a restart (dispatch://INFRA-4374 R4). GitHub's `unknown` is
// the whole reason: no rule of classify.MergeStateReadOwed is read for it, and a read that answers
// a state queues nothing.
func (w *workflowRuntime) readIssuesAtStart(ctx context.Context) {
	issues, err := w.issuesInWorkflow(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues to read the pull request of as the daemon starts", "error", err)
		}
		return
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		computing, err := w.readIssuePullRequest(ctx, issue)
		if err == nil {
			if computing {
				w.log.Info("github: the start read found GitHub's merge state unknown; a ladder reads the pull request again", "issue", issue)
				w.queueMergeState(issue)
			}
			continue
		}
		var answer *githubrest.Answer
		if errors.As(err, &answer) && answer.RateLimited {
			w.log.Error("read an issue's pull request as the daemon starts: GitHub's rate limit; the start reads no more issues, and each issue's next handoff reads it",
				"issue", issue, "error", err)
			return
		}
		w.log.Warn("read an issue's pull request as the daemon starts; the issue's next handoff reads it", "issue", issue, "error", err)
	}
}

// issuesInWorkflow is every issue of the project the per-issue read has something to read for, by
// key: those with an open pull request recorded, and those whose implementer completed with none
// open or merged recorded (record.Store.HandedOffWithoutOpenOrMergedPullRequest), whose pull
// request GitHub may hold open with its opened lost. An issue with neither - one still being
// planned - has no pull request to read, and costs no GitHub call.
func (w *workflowRuntime) issuesInWorkflow(ctx context.Context) ([]string, error) {
	var open []record.PullRequest
	var waiting []record.HandedOff
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if open, err = w.records.OpenPullRequests(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		waiting, err = w.records.HandedOffWithoutOpenOrMergedPullRequest(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, pr := range open {
		keys[pr.Issue] = true
	}
	for _, handedOff := range waiting {
		keys[handedOff.Issue.Key] = true
	}
	return slices.Sorted(maps.Keys(keys)), nil
}

// handoffCompleted queues the read of issue's pull request for readHandedOff, once the API has
// recorded a worker's completion of its phase (the handoff route's own hook): the daemon decides
// the issue's next step on the pull request's state, and this is when it is read. It only records
// the issue and wakes the read, so the route answers the agent without waiting for GitHub, and the
// read runs on the runtime's own goroutine, outside the completion's transaction.
func (w *workflowRuntime) handoffCompleted(issue string) {
	w.handedOff.add(issue)
}

// readHandedOff reads the pull request of each issue handoffCompleted queued, until ctx ends: each
// wake takes every issue queued since the last one, in the order of their keys, and reads each once
// (readIssuePullRequest). A read that fails is logged and nothing is queued again: the issue's next
// handoff, and the daemon's next start, read it again. Unlike the start read, a read that finds
// GitHub still computing the head's merge queues no ladder of its own: the merge-state reads a
// handed-off issue is owed are the workflow's to ask for (the workflow's askMergeStateFor, which
// applies the gate classify.MergeStateReadOwed and the review round's own wait).
func (w *workflowRuntime) readHandedOff(ctx context.Context) {
	for {
		issues, ok := w.handedOff.take(ctx)
		if !ok {
			return
		}
		for _, issue := range issues {
			if ctx.Err() != nil {
				return
			}
			if _, err := w.readIssuePullRequest(ctx, issue); err != nil {
				w.log.Warn("read the pull request of an issue whose agent handed off; the record stands as it was until the next read",
					"issue", issue, "error", err)
			}
		}
	}
}
