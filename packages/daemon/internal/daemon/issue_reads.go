package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/githubrate"

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

// readIssuePullRequest reads the pull request of one task of an issue from GitHub and applies what
// it shows, as the webhook would have: the head GitHub holds it at (followHead, and the pushes that
// reached it), whether that head can be merged into its base and GitHub's own merge state
// (applyMergeability), and a deciding review the daemon has not recorded (readReviews), in that
// order. The task is the one a completion names, or the one an open pull request is of at the
// start (taskRead.task), and "" reads the task the record judges (judgedTask). A task whose record
// holds no open pull request is looked up by its branch instead (openPullRequestOn), and the
// opened its webhook would have carried is applied first, so the reads that follow judge the pull
// request that leaves.
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
// found the pull request open with `mergeable` null, or an answer the readers read again
// (answerIncomplete: `unknown`, or `blocked` missing part of what the round decides it on). The
// start read queues a ladder of reads for such an issue (readIssuesAtStart), which a handoff's
// read does not.
// A read whose reviews read must stop - ctx ended, or GitHub's rate limit, which the ladder's reads
// would meet as well - answers false.
func (w *workflowRuntime) readIssuePullRequest(ctx context.Context, issue, task string) (bool, error) {
	ctx = githubrate.WithCaller(ctx, callerPullRequestReads)
	recorded, err := w.issuePullRequest(ctx, issue, task)
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
	fact, err := w.applyMergeability(ctx, github, pr, pull, answeredAt, w.mergeabilityEventID(pr, answeredAt))
	if err != nil && ctx.Err() == nil {
		w.log.Warn("apply whether a pull request's head can be merged into its base; the issue's next read applies it again",
			"issue", pr.Issue, "repo", pr.Repo, "pullRequest", pr.Number, "head", pull.Head.SHA, "error", err)
	}
	if w.readReviews(ctx, prCtx, github, pr) {
		return false, nil
	}
	return pull.State == "open" && (pull.mergeability() == record.MergeabilityUnknown || answerIncomplete(fact)), nil
}

// issuePullRequest is the open pull request the per-issue read judges for key and task (task ""
// for the task the record judges): the pull request of the task the read judges
// (issueRecords.task), the record's when it holds one open, and otherwise the one GitHub shows open
// on that task's branch (openPullRequestOn), applied as the intake.PullRequestOpened its webhook
// would have carried - naming the repository as that webhook's fact would (heardSpelling), a reopen
// when the record holds the same pull request closed - and read back as the workflow recorded it.
// It is nil for an issue the daemon does not record, one with no task to judge, one with no
// repository to read, one whose task's branch GitHub holds no open pull request on - which an
// implementing task's architect is told of (tellMissing) - and one whose opened the workflow
// declined, which leaves the record as it was and nothing for the reads that follow to judge.
func (w *workflowRuntime) issuePullRequest(ctx context.Context, key, task string) (*record.PullRequest, error) {
	recorded, err := issueRecord(ctx, w.pool, w.records, key, task)
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
	// The base the list answer names goes with it, so a record found this way has the base its
	// reads of the merge state follow on (followOnBase) even when the detail read after it fails.
	fact := intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, Base: pull.Base.Ref, HeadSHA: pull.Head.SHA,
		HeadRepo: pull.HeadRepository(), Body: pull.Body, URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: pr != nil && pr.Number == pull.Number}
	eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, repo, pull.Number, time.Now().UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return nil, fmt.Errorf("apply the open pull request %s#%d read from GitHub: %w", repo, pull.Number, err)
	}
	again, err := issueRecord(ctx, w.pool, w.records, key, recorded.task)
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
// of its own task's pull request (handoffCompleted), and the daemon's start reads every issue whose
// implementer completed with no pull request recorded (readIssuesAtStart). A task in any other
// phase, or one whose record lets the implementation move, is told nothing. The event id names the
// issue's generation and the handoff commit, so the architect is told once per handoff commit,
// whichever read finds it, and again only for a completion at another commit. A failed apply is
// logged, and the next read of the issue tells it.
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
// and whether it has one (judgedTask); whether the issue has plan rows at its generation; its
// implementer's handoff commit; and each pull request of the issue, by task
// (record.Store.TaskPullRequests).
type issueRecords struct {
	issue   *record.Issue
	task    string
	branch  string
	judged  bool
	planned bool
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

// openAt is the issue's open pull request of task at head, nil for none. Task "" - the zero task
// of an issue with no plan rows, or a read of the task the record judges, which names none - takes
// the open pull request at head of any task of the issue.
func (r issueRecords) openAt(head, task string) *record.PullRequest {
	return r.find(func(pr record.PullRequest) bool {
		return pr.State == record.PullRequestOpen && pr.HeadSHA == head && (task == "" || pr.Task == task)
	})
}

func (r issueRecords) find(match func(record.PullRequest) bool) *record.PullRequest {
	if at := slices.IndexFunc(r.prs, match); at >= 0 {
		return &r.prs[at]
	}
	return nil
}

// implementing says whether the task the per-issue read judges stands in implementing: for an issue
// whose plan holds any count of tasks, one among them, a running step of that task stands there
// unheld (record.Issue.RunningAt), and a held task, which waits on its owner, does not; for an issue
// with no plan rows, the issue does.
func (r issueRecords) implementing() bool {
	if !r.planned {
		return r.issue.Phase == phase.Implementing
	}
	return slices.ContainsFunc(r.issue.RunningAt(phase.Implementing), func(step record.RunningStep) bool { return step.Task == r.task && !step.Held })
}

// judgedTask is the task of stored, the plan of the issue keyed key at its generation, whose pull
// request the per-issue read judges, and whether it has one, for a plan of any count of tasks, one
// among them: the task named, which a completion names (handoffCompleted), else the task whose step
// the issue's implementer runs or ran last (implementer's record.PhaseRow.StepTask), else, for an
// implementer's row raised before the plan, which names no step, the one task of the plan that has
// a merge (record.StoredPlan.OnlyTaskThatMerges): none where the plan holds no such task, or
// several. An issue with no plan rows judges its own work, the zero task, on the issue's branch.
func judgedTask(stored *record.StoredPlan, implementer record.PhaseRow, named string) (record.PlanTask, bool) {
	if stored == nil {
		return record.PlanTask{}, true
	}
	if named == "" {
		named = implementer.StepTask
	}
	if named == "" {
		named, _ = stored.OnlyTaskThatMerges()
	}
	return stored.TaskOf(named)
}

// issueRecord is key's records as the reads of its pull requests read them (issueRecords), in one
// transaction, for the task named (judgedTask; "" for the task the record judges): what the
// per-issue read and the merge-state ladder read before they read GitHub (issuePullRequest,
// readMergeState), and what the second read of a conflict reads before it acts (outbox's
// conflictConfirm). Each caller reads the pull requests' columns to decide what to read of GitHub
// and builds the facts it applies from them, and none writes the record back.
func issueRecord(ctx context.Context, pool *store.Pool, records record.Store, key, named string) (issueRecords, error) {
	var read issueRecords
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
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
		task, judged := judgedTask(stored, implementer, named)
		read.task, read.branch, read.judged, read.planned, read.handoff = task.Task, task.BranchOf(key), judged, stored != nil, implementer.HandoffCommit
		read.prs, err = records.TaskPullRequestRows(ctx, tx, key)
		return err
	}); err != nil {
		return issueRecords{}, fmt.Errorf("read the record of %s: %w", key, err)
	}
	return read, nil
}

// readIssuesAtStart reads the pull request of each task of each issue in the workflow once as the
// runtime starts (readIssuePullRequest): every open pull request recorded, each as its own task's,
// and the pull request of every issue whose implementer completed with none open or merged
// recorded, which this read looks up by the branch of the task the record judges. It is what
// covers a daemon that was down while GitHub made the deliveries nothing redelivers. A read that
// fails is logged and the boot goes on; the open pull request the record holds for its task is
// handed to the reads of its base (followOnBase), under that task, so a second task of the same
// issue whose read fails too is followed beside it, and a run of null `mergeable` reads it holds
// asks the round again (readFailed): a round that waits quietly on the head has no handoff coming
// that would read it. GitHub's rate limit ends the start read there, because every read after it
// on the same installation meets the same limit, and the panes share it; the hand-off makes no
// GitHub call, so the limited read and every one after it are handed to the reads of their base
// all the same, which wait out the limit.
//
// A read that finds GitHub still computing the merge of an open pull request's head - merge state
// `unknown`, or `mergeable` null - queues a ladder of reads for that task's pull request
// (queueMergeState), which climbs and then hands the head to the reads of its base, which follow
// it while GitHub goes on answering `unknown` (followOnBase, followBase).
// The reads that followed GitHub's merge state before the daemon stopped were held in memory, and
// this is what goes on with them after a restart (dispatch://INFRA-4374 R4). GitHub's `unknown` is
// the whole reason: no rule of classify.MergeStateReadOwed is read for it, and a read that answers
// a state queues nothing.
func (w *workflowRuntime) readIssuesAtStart(ctx context.Context) {
	reads, err := w.issuesInWorkflow(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues to read the pull request of as the daemon starts", "error", err)
		}
		return
	}
	for i, read := range reads {
		if ctx.Err() != nil {
			return
		}
		computing, err := w.readIssuePullRequest(ctx, read.issue, read.task)
		if err == nil {
			if computing {
				w.log.Info("github: the start read found GitHub's merge state unknown; a ladder reads the pull request again", "issue", read.issue, "task", read.task)
				// The task of the pull request the read judged: an open pull request's own task, or
				// "" - the task the record judges - for an issue handed off with none recorded.
				w.queueMergeState(read.issue, read.task)
			}
			continue
		}
		var answer *githubrest.Answer
		if errors.As(err, &answer) && answer.RateLimited {
			// Every read after this one on the same installation meets the same limit, so the start
			// reads no more issues; the hand-off below makes no GitHub call, so this issue and each
			// one after it with an open pull request recorded go to the reads of their base, which
			// wait out the limit, and one handed off with none open is read at its next handoff.
			w.log.Error("read an issue's pull request as the daemon starts: GitHub's rate limit; the start reads no more issues: each one left with an open pull request goes to the reads of its base, and one with none open is read at its next handoff",
				"issue", read.issue, "task", read.task, "left", len(reads)-i, "error", err)
			for _, left := range reads[i:] {
				w.startReadFailed(ctx, left)
			}
			return
		}
		w.log.Warn("read an issue's pull request as the daemon starts; with an open pull request recorded the reads of its base read it again, and with none its next handoff does",
			"issue", read.issue, "task", read.task, "error", err)
		w.startReadFailed(ctx, read)
	}
}

// startReadFailed hands the open pull request the record holds for read's task to the reads of
// its base after the start read of it failed (followOnBase), and asks its round again for a run of
// null `mergeable` reads the record holds (readFailed), so the head is read, and a persistent null
// is told, though no handoff is coming. A read whose record cannot be read, or holds no open pull
// request for its task, has nothing to hand.
func (w *workflowRuntime) startReadFailed(ctx context.Context, read taskRead) {
	recorded, err := issueRecord(ctx, w.pool, w.records, read.issue, read.task)
	pr := recorded.pullRequest()
	if err != nil || pr == nil || pr.State != record.PullRequestOpen {
		if err != nil && ctx.Err() == nil {
			w.log.Warn("read the record of an issue whose start read failed; its next handoff reads it", "issue", read.issue, "task", read.task, "error", err)
		}
		return
	}
	w.followOnBase(read.issue, *pr)
	w.readFailed(ctx, *pr)
}

// issuesInWorkflow is every read of a task's pull request the start makes (taskRead), in the order
// of their issues and then their tasks: the task of each open pull request recorded
// (record.PullRequest.Task), so each task's open pull request is read, and, for each issue whose
// implementer completed with none open or merged recorded
// (record.Store.HandedOffWithoutOpenOrMergedPullRequest), whose pull request GitHub may hold open
// with its opened lost, the task the record judges (""). An issue with neither - one still being
// planned - has no pull request to read, and costs no GitHub call.
func (w *workflowRuntime) issuesInWorkflow(ctx context.Context) ([]taskRead, error) {
	var open []record.PullRequest
	var waiting []record.HandedOff
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if open, err = w.records.OpenPullRequests(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		waiting, err = w.records.HandedOffWithoutOpenOrMergedPullRequest(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		return nil, err
	}
	reads := make([]taskRead, 0, len(open)+len(waiting))
	for _, pr := range open {
		reads = append(reads, taskRead{issue: pr.Issue, task: pr.Task})
	}
	for _, handedOff := range waiting {
		reads = append(reads, taskRead{issue: handedOff.Issue.Key})
	}
	slices.SortFunc(reads, compareTaskReads)
	return slices.Compact(reads), nil
}

// taskRead is one read of the pull request of one task of an issue: one a completion asks for
// (handoffCompleted), naming the task of its plan whose step completed, or one the start makes
// (issuesInWorkflow), naming the task of an open pull request; "" for an issue with no plan rows,
// and for a read of the task the record judges (judgedTask). It is also the key of every read of
// GitHub's merge state: the ladder of one task's pull request (workflowRuntime.mergeStateRunning)
// and its place in its base's batch (baseRead.owed, workflowRuntime.baseReadOf).
type taskRead struct {
	issue, task string
}

// compareTaskReads orders reads by their issues and then their tasks.
func compareTaskReads(a, b taskRead) int {
	return cmp.Or(cmp.Compare(a.issue, b.issue), cmp.Compare(a.task, b.task))
}

// newHandedOffSet is the queue of the reads completions ask for (workflowRuntime.handedOff), taken
// in the order of their issues and then their tasks.
func newHandedOffSet() *wakeSet[taskRead] {
	return newWakeSetBy(compareTaskReads)
}

// handoffCompleted queues the read of the pull request of task, the task of issue's plan whose step
// completed, for readHandedOff, once the API has recorded a worker's completion of its phase (the
// handoff route's own hook): the daemon decides that task's next step on its pull request's state,
// and this is when it is read. It only records the read and wakes it, so the route answers the
// agent without waiting for GitHub, and the read runs on the runtime's own goroutine, outside the
// completion's transaction.
func (w *workflowRuntime) handoffCompleted(issue, task string) {
	w.handedOff.add(taskRead{issue: issue, task: task})
}

// readHandedOff makes each read handoffCompleted queued, until ctx ends: each wake takes every read
// queued since the last one, in the order of their issues and tasks, and makes each once
// (readIssuePullRequest). A read that fails is logged and nothing is queued again: the task's next
// handoff, and the daemon's next start, read it again. Unlike the start read, a read that finds
// GitHub still computing the head's merge queues no ladder of its own: the merge-state reads a
// handed-off issue is owed are the workflow's to ask for (the workflow's askMergeState, by
// mergeStateOwed, which applies the gate classify.MergeStateReadOwed and the review round's own
// wait).
func (w *workflowRuntime) readHandedOff(ctx context.Context) {
	for {
		reads, ok := w.handedOff.take(ctx)
		if !ok {
			return
		}
		for _, read := range reads {
			if ctx.Err() != nil {
				return
			}
			if _, err := w.readIssuePullRequest(ctx, read.issue, read.task); err != nil {
				w.log.Warn("read the pull request of an issue whose agent handed off; the record stands as it was until the next read",
					"issue", read.issue, "task", read.task, "error", err)
			}
		}
	}
}
