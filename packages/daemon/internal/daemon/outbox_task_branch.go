package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// branchClient is the client a write of one of issue's branches is made with, and the repository
// it writes: the repository GitHub holds the one the issue's record names under now (serving), as
// the implement App for that repository's owner, the App of the roles that write an issue's
// branches (appauth.AppRoleFor) and the one workspace provisioning clones with. Each write is a
// REST call, so a tree recorded before a transfer gets its branches on the repository as GitHub
// holds it today rather than a write to an owner whose installation has lost it.
func (r *outbox) branchClient(ctx context.Context, issue record.Issue) (githubrest.Client, ghrepo.Repository, error) {
	recorded, err := issue.Repository()
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, err
	}
	repo := r.repositories.serving(recorded)
	github, err := implementClient(ctx, r.tokens, r.githubAPI, repo)
	if err != nil {
		return githubrest.Client{}, ghrepo.Repository{}, fmt.Errorf("mint the implement App token for %s: %w", repo, err)
	}
	return github, repo, nil
}

// taskBranch makes the GitHub branch of the row's task (record.PlanTask.BranchOf) start from the
// joined work of the tasks it needs, the work its first step starts on. That work is on the
// branches the task starts from (record.StoredPlan.StartsFrom): for a part, the branch of the task
// it is a part of first, which holds that task's joined work, so each part starts from it whatever
// else it needs; then the branch of each need whose work did not merge, in the order of its needs,
// then the default branch the issue's record names once where the work of any need merged, so the
// branch of a merged need, which GitHub can have deleted at its merge, is never read. A task that
// starts from none, a task of the top level that needs none, starts at the head of its base
// (record.StoredPlan.BaseOf), the default branch. One that starts from some starts at the head of
// the first. A task whose pull request is based on its join branch
// (record.StoredPlan.BaseOf: a task that starts from several branches and does not merge)
// joins them there rather than on its branch, which then starts at the join branch's head, so its
// pull request shows its own work alone. Each branch it starts from is then merged into the branch
// the join is made on in that order (ghbranch.Merge), whether the row created the branch or GitHub
// had it: a branch GitHub has keeps its head, so the row joins each branch whose head the branch
// does not hold, and GitHub answers one the branch holds with nothing to merge, which moves no ref,
// so a row that runs again changes nothing. Each start of the task queues the row (workflow's
// startTaskCopy, at the start of every step), the first and a start again, so a need a later version
// gave a stopped task is joined into its branch when it starts again. A merge GitHub refuses as a
// conflict is no refusal: the branch stands on the needs that joined. A task none of whose steps
// implements then gets its pull request from Legion, where its branch holds work its base lacks
// (openTaskPullRequest), since no step of it opens one. Once its merges end, the end of the join is
// applied as a fact (intake.TaskJoined, joined) at the row's own generation, naming the branch,
// the needs it joined, each need GitHub refused as a conflict and whether the join branch merged
// into the task's branch (joinBranch): the workflow writes those on the task, for its first step to
// repair, and starts the step that waits for that join, and changes nothing where the task no
// longer waits for it, a task a stop and a later version changed while
// the row joined, whose start again queued a join of its own, or a run a re-admission carried to
// the next generation, whose join the settle of that re-admission queued again there (workflow's
// joinAgain). A row of a task its plan no longer holds, of a generation the issue has left, or of a
// tree that lingers finishes without acting, as issueBranch's does.
func (r *outbox) taskBranch(ctx context.Context, row record.OutboxRow, payload record.TaskBranch) error {
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox task branch serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"task", payload.Task, "generation", payload.Generation, "current", issue.Generation)
		return nil
	}
	root, err := r.root(ctx, issue)
	if err != nil {
		return err
	}
	if root != nil && root.Lingers() {
		r.log.Info("outbox task branch of a member of a lingering tree; finished without acting", "row", row.ID, "issue", issue.Key,
			"task", payload.Task, "tree", issue.Tree)
		return nil
	}
	var stored *record.StoredPlan
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		stored, err = r.records.Plan(ctx, tx, issue.Key, payload.Generation)
		return err
	}); err != nil {
		return fmt.Errorf("read the plan of %s for the branch of its task %s: %w", issue.Key, payload.Task, err)
	}
	task, ok := stored.TaskOf(payload.Task)
	if !ok {
		r.log.Info("outbox task branch of a task the plan no longer holds; finished without acting", "row", row.ID, "issue", issue.Key,
			"task", payload.Task, "generation", payload.Generation)
		return nil
	}
	branch := task.BranchOf(issue.Key)
	if task.Place.Kind == record.PlaceMerge {
		return r.mergeBase(ctx, row, issue, task, branch, payload.Generation)
	}
	starts, err := stored.StartsFrom(issue, task.Task)
	if err != nil {
		return fmt.Errorf("make the branch %s of task %s of %s: %w", branch, task.Task, issue.Key, err)
	}
	base, join, err := stored.BaseOf(issue, task.Task)
	if err != nil {
		return fmt.Errorf("make the branch %s of task %s of %s: %w", branch, task.Task, issue.Key, err)
	}
	onto := branch
	if join {
		onto = base
	}
	conflicts, unmerged, err := r.joinBranch(ctx, issue, branch, onto, base, starts, task.Needs)
	var missing *needBranchMissing
	if errors.As(err, &missing) {
		return r.joinCannotEnd(ctx, row, issue, task, missing)
	}
	if err != nil {
		return fmt.Errorf("make the branch %s of task %s of %s, which its first step waits for: %w", branch, task.Task, issue.Key, err)
	}
	if !stored.Implements(task.Task) {
		if err := r.openTaskPullRequest(ctx, issue, task.Task, branch, base); err != nil {
			return fmt.Errorf("open the pull request of task %s of %s, which no step of it implements: %w", task.Task, issue.Key, err)
		}
	}
	return r.joined(ctx, row, intake.TaskJoined{Issue: issue.Key, Generation: payload.Generation, Task: task.Task, Branch: branch, Needs: task.Needs,
		Conflicts: conflicts, Unmerged: unmerged})
}

// mergeBase serves the row the start of a task's merge queued (workflow's Engine.start), where the
// task's open pull request is based on a branch other than the issue's default branch: the base its
// start fixed, a need's branch or its join branch, where a later version made the task merge. It
// creates no branch, merges nothing and reads no join branch. It reads the task's open pull request
// on GitHub and sets its base to the default branch the issue's record names where GitHub's differs
// (ghbranch.SetBase), as branchClient makes each write of a branch, within branchRowTimeout, then
// applies the end of the join with that base (intake.TaskJoined.Base), which records it and starts
// the merge. A row that runs again finds the base set and sends no second PATCH. A task with no open
// pull request recorded has no base to move, and its merge starts. A failure fails the row, which
// tries again on its backoff, and the merge waits for it.
func (r *outbox) mergeBase(ctx context.Context, row record.OutboxRow, issue record.Issue, task record.PlanTask, branch string, generation uint64) error {
	var pr *record.PullRequest
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = r.records.PullRequestOfTask(ctx, tx, issue.Key, task.Task)
		return err
	}); err != nil {
		return fmt.Errorf("read the pull request of task %s of %s, whose merge waits for its base: %w", task.Task, issue.Key, err)
	}
	if pr != nil && pr.State == record.PullRequestOpen {
		client, repo, err := r.branchClient(ctx, issue)
		if err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, branchRowTimeout)
		defer cancel()
		moved, err := ghbranch.SetBase(writeCtx, client, repo, pr.Number, issue.DefaultBranch)
		if err != nil {
			return fmt.Errorf("move the pull request of task %s of %s to its default branch at its merge: %w", task.Task, issue.Key, err)
		}
		r.log.Info("outbox task branch: the pull request of a task at its merge is based on the default branch", "row", row.ID, "issue", issue.Key,
			"task", task.Task, "pullRequest", pr.Number, "base", issue.DefaultBranch, "moved", moved)
	}
	return r.joined(ctx, row, intake.TaskJoined{Issue: issue.Key, Generation: generation, Task: task.Task, Branch: branch, Needs: task.Needs,
		Base: issue.DefaultBranch})
}

// openTaskPullRequest opens the pull request of task of issue, on branch against base, as
// branchClient makes each write of a branch, within branchRowTimeout: the pull request of a task
// none of whose steps implements, which no step opens. It is opened once the join leaves branch
// holding a commit base lacks (githubrest.Comparison.AheadBy above 0, whatever the compare's
// status: ahead, or diverged once base moved on after the task's needs started), with the title
// `<KEY>: <task id>` and a body holding the line `Dispatch: <KEY>`, as an implementer's does. A
// branch that holds nothing base lacks gets none, and a pull request open on branch already is kept
// (ghbranch.OpenPullRequest), so a row that runs again opens none.
func (r *outbox) openTaskPullRequest(ctx context.Context, issue record.Issue, task, branch, base string) error {
	ctx, cancel := context.WithTimeout(ctx, branchRowTimeout)
	defer cancel()
	github, repo, err := r.branchClient(ctx, issue)
	if err != nil {
		return err
	}
	comparison, err := github.Compare(ctx, base, branch)
	if err != nil {
		return fmt.Errorf("compare %s with %s on %s: %w", branch, base, repo, err)
	}
	if comparison.AheadBy <= 0 {
		return nil
	}
	body := fmt.Sprintf("No step of task %s of %s implements, so Legion opened this pull request for the work its branch holds.\n\nDispatch: %s",
		task, issue.Key, issue.Key)
	_, err = ghbranch.OpenPullRequest(ctx, github, repo, branch, base, fmt.Sprintf("%s: %s", issue.Key, task), body)
	return err
}

// closeTaskPullRequests closes each pull request the row names, of tasks of the row's issue (those
// whose work a merge holds, a removed task's at the version that removed it, an earlier run's at
// the first step of a run started again), as branchClient makes each write of a branch (the
// implement App), within branchRowTimeout: each is closed, then the row's comment is written on it
// once, as the implement App's login (ghbranch.ClosePullRequest), which the outbox's token source
// answers, as the boot requires of it (appauth.Logins). A row that runs again finds each close and
// each comment an earlier try made. A row of a generation the issue has left finishes without
// acting.
//
// It reads each answer as one of these (ghbranch.RefusedForGood decides which failure is for
// good), and logs one line of each number:
//   - closed now, closed by Legion before, or merged: the number is done;
//   - closed by someone else, or closed with no closer GitHub names: the number is done, with no
//     comment, and the line names that login (INFO), the null (INFO) or GitHub's answer (WARN);
//   - closed, and its comment refused for good: the pull request is closed, so the number is done,
//     and a WARN line names GitHub's answer;
//   - its close refused for good, so it stays open: the row goes on to the rest, and then answers
//     *pullRequestsCloseRefused, naming each such number, which ends the row and tells the issue's
//     architect once (permanentRefusal, finishRefused);
//   - any other failure can pass: the row answers it at once and runs again whole on its backoff.
func (r *outbox) closeTaskPullRequests(ctx context.Context, row record.OutboxRow, payload record.TaskPullRequestsClose) error {
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox close of task pull requests serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"generation", payload.Generation, "current", issue.Generation)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, branchRowTimeout)
	defer cancel()
	github, repo, err := r.branchClient(ctx, issue)
	if err != nil {
		return err
	}
	logins, ok := r.tokens.(appauth.Logins)
	if !ok {
		return fmt.Errorf("close the pull requests of tasks of %s: the GitHub App token source answers no App login, which the comment is written as", issue.Key)
	}
	login, err := logins.AppLogin(ctx, appauth.Implement)
	if err != nil {
		return fmt.Errorf("read the implement App's login to close the pull requests of tasks of %s: %w", issue.Key, err)
	}
	refused := &pullRequestsCloseRefused{issue: issue.Key, comment: payload.Comment}
	for _, number := range payload.Numbers {
		closed, err := ghbranch.ClosePullRequest(ctx, github, repo, number, payload.Comment, login)
		line := []any{"row", row.ID, "issue", issue.Key, "pullRequest", number}
		switch {
		case err == nil && closed.Now:
			r.log.Info("outbox closed the pull request of a task of the issue", append(line, "comment", payload.Comment)...)
		case err == nil && closed.Merged:
			r.log.Info("outbox left the merged pull request of a task of the issue as it is", line...)
		case err == nil && closed.By == login:
			r.log.Info("outbox found the pull request of a task of the issue closed by Legion before, and its comment written once", append(line, "comment", payload.Comment)...)
		case err == nil && closed.By != "":
			r.log.Info("outbox left the pull request of a task of the issue as its closer left it, with no comment: Legion did not close it", append(line, "closedBy", closed.By)...)
		case err == nil && closed.Unread == ghbranch.ClosedByNull:
			r.log.Info("outbox left the pull request of a task of the issue closed, with no comment: GitHub names no one who closed it", append(line, "unread", closed.Unread)...)
		case err == nil:
			r.log.Warn("outbox left the pull request of a task of the issue closed, with no comment: GitHub refused the read of who closed it", append(line, "unread", storetext.FarSideError(closed.Unread))...)
		case (closed.Now || closed.By != "") && ghbranch.RefusedForGood(err):
			r.log.Warn("outbox closed the pull request of a task of the issue, and GitHub refused its comment for good", append(line, "error", storetext.FarSideError(err.Error()))...)
		case ghbranch.RefusedForGood(err):
			refused.refused = append(refused.refused, closeRefused{number: number, err: err})
		default:
			return fmt.Errorf("close the pull request #%d of a task of %s: %w", number, issue.Key, err)
		}
	}
	if len(refused.refused) > 0 {
		return refused
	}
	return nil
}

// pullRequestsCloseRefused is a row that closes pull requests of tasks of issue, with comment, that
// GitHub refused for good to close each pull request of refused: each stays open, and a try again
// would meet the same answer, so the row ends and the issue's architect is told once (notice).
type pullRequestsCloseRefused struct {
	issue   string
	comment string
	refused []closeRefused
}

// closeRefused is a pull request whose close GitHub refused for good, and the failure it answered.
type closeRefused struct {
	number int
	err    error
}

func (e *pullRequestsCloseRefused) Error() string {
	return fmt.Sprintf("GitHub refused the close of %s of %s for good: %s", e.named(), e.issue, e.answers())
}

// numbers is the number of each pull request whose close GitHub refused, in the row's order.
func (e *pullRequestsCloseRefused) numbers() []int {
	numbers := make([]int, len(e.refused))
	for i, refused := range e.refused {
		numbers[i] = refused.number
	}
	return numbers
}

// named is the pull requests whose close GitHub refused, as words: `pull request #43`, `pull
// requests #43 and #45`, `pull requests #43, #45 and #47`.
func (e *pullRequestsCloseRefused) named() string {
	numbers := make([]string, len(e.refused))
	for i, refused := range e.refused {
		numbers[i] = fmt.Sprintf("#%d", refused.number)
	}
	if len(numbers) == 1 {
		return "pull request " + numbers[0]
	}
	return "pull requests " + strings.Join(numbers[:len(numbers)-1], ", ") + " and " + numbers[len(numbers)-1]
}

// answers is GitHub's answer to each refused close, each bounded as a far side's text is
// (storetext.FarSideError): the one answer alone, or each after its number, `#43: <answer>; #45:
// <answer>`. A GitHub answer reads `<status> to <method> <path>: <body>`; a refusal of the
// private-name rules reads as its own error.
func (e *pullRequestsCloseRefused) answers() string {
	answers := make([]string, len(e.refused))
	for i, refused := range e.refused {
		text := refused.err.Error()
		var answer *githubrest.Answer
		if errors.As(refused.err, &answer) {
			text = fmt.Sprintf("%d to %s %s: %s", answer.Status, answer.Method, answer.Path, answer.Body)
		}
		answers[i] = storetext.FarSideError(text)
		if len(e.refused) > 1 {
			answers[i] = fmt.Sprintf("#%d: %s", refused.number, answers[i])
		}
	}
	return strings.Join(answers, "; ")
}

// notice is the pull-request-close-refused notice that tells the issue's architect which pull
// requests stay open, that Legion will not try again, how to close each, the comment Legion was
// writing and GitHub's answer to each.
func (e *pullRequestsCloseRefused) notice() record.Notice {
	it := "it"
	if len(e.refused) > 1 {
		it = "each"
	}
	return record.Notice{Kind: "pull-request-close-refused", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("GitHub refused Legion's close of %s of %s, as it would refuse %s again, so %s stays open and Legion will not try again. Close %s with gh pr close where its work should not land. The comment Legion was writing: %q GitHub answered: %s",
			e.named(), e.issue, it, it, it, e.comment, e.answers())}
}

// joined applies fact, the end of the join row made (intake.TaskJoined), through the outbox's
// handlers, which start the step that waits for it, under an id of the issue, the generation, the
// task and the row, so a row that runs again applies one fact. A fact that fails fails the row,
// which joins again on its backoff and finds each need joined.
func (r *outbox) joined(ctx context.Context, row record.OutboxRow, fact intake.TaskJoined) error {
	id := fmt.Sprintf("task-joined:%s:%d:%s:%d", fact.Issue, fact.Generation, fact.Task, row.ID)
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", id, fact, r.handlers...); err != nil {
		return fmt.Errorf("apply the end of the join of task %s of %s: %w", fact.Task, fact.Issue, err)
	}
	return nil
}

// joinBranch makes onto, the branch the join of starts, the branches the task starts from
// (record.StoredPlan.StartsFrom), is made on, on GitHub at the head of the first of starts, or at
// from, the task's base (record.StoredPlan.BaseOf), where it names none, unless GitHub has it,
// and merges each of starts into it in order, with the message `<KEY>: join the work of task
// <id>` for a need's branch and `<KEY>: join <default branch>, which holds the work of task <id>`
// for the default branch, as branchClient makes each write of a branch, within branchRowTimeout.
// Where onto is not branch, the task's join branch, branch is then made at onto's head, unless
// GitHub has it, and onto is merged into it, with the message `<KEY>: join the work of tasks
// <needs>`, so a branch GitHub has, with the task's own work on it, takes what its needs did
// since. It answers the id of each need whose work GitHub refused to merge into onto as a conflict
// (for the default branch, each need whose work it holds), nil for none, which the task's first
// step repairs on onto, and whether GitHub refused the merge of onto into branch as a conflict
// (unmerged), which only the task's own work on branch makes and which that step repairs on
// branch: two facts, since the first step repairs each on another branch, and a repair on branch
// of a need's conflict would show that need's work in the task's pull request, which is based on
// onto. GitHub's 404 for a branch of starts, to the read of the first one's head or to a merge of
// one, is a *needBranchMissing naming that branch and the tasks whose work it holds
// (record.StartBranch.Holds), a part's task among them (record.StartBranch.OfTask); a 404 for
// from, read only where starts names none, is an error the row tries again on its backoff.
func (r *outbox) joinBranch(ctx context.Context, issue record.Issue, branch, onto, from string, starts []record.StartBranch, needs []string) (conflicts []string, unmerged bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, branchRowTimeout)
	defer cancel()
	github, repo, err := r.branchClient(ctx, issue)
	if err != nil {
		return nil, false, err
	}
	missing := func(start record.StartBranch, err error) error {
		return &needBranchMissing{branch: start.Branch, holds: start.Holds, merged: start.Branch == issue.DefaultBranch, ofTask: start.OfTask, err: err}
	}
	base := from
	if len(starts) > 0 {
		base = starts[0].Branch
	}
	if err := ghbranch.Create(ctx, github, repo, onto, base); err != nil {
		if len(starts) > 0 && githubAnswered(err, http.StatusNotFound) {
			return nil, false, missing(starts[0], err)
		}
		return nil, false, err
	}
	for _, start := range starts {
		message := fmt.Sprintf("%s: join the work of %s", issue.Key, idsNamed("task", start.Holds))
		if start.Branch == issue.DefaultBranch {
			message = fmt.Sprintf("%s: join %s, which holds the work of %s", issue.Key, start.Branch, idsNamed("task", start.Holds))
		}
		joined, err := ghbranch.Merge(ctx, github, repo, onto, start.Branch, message)
		if err != nil {
			if githubAnswered(err, http.StatusNotFound) && !mergeBaseMissing(err) {
				return nil, false, missing(start, err)
			}
			return nil, false, err
		}
		if joined == ghbranch.MergeConflict {
			conflicts = append(conflicts, start.Holds...)
		}
	}
	if onto == branch {
		return conflicts, false, nil
	}
	if err := ghbranch.Create(ctx, github, repo, branch, onto); err != nil {
		return nil, false, err
	}
	joined, err := ghbranch.Merge(ctx, github, repo, branch, onto, fmt.Sprintf("%s: join the work of %s", issue.Key, idsNamed("task", needs)))
	if err != nil {
		return nil, false, err
	}
	return conflicts, joined == ghbranch.MergeConflict, nil
}

// idsNamed is noun and the one id of ids, or noun's plural and each id, as the words of a join name
// the tasks or needs whose work a branch holds: `task b`, `tasks a, b`.
func idsNamed(noun string, ids []string) string {
	if len(ids) == 1 {
		return noun + " " + ids[0]
	}
	return noun + "s " + strings.Join(ids, ", ")
}

// needBranchMissing is a join that cannot end: GitHub has no branch, branch, that holds the work of
// holds: a need's own branch, or the default branch, which holds the work of each need that merged
// (merged), or, for a part, the branch of the task it is a part of, holds' one id (ofTask), so the
// row waits for that branch on its backoff (joinCannotEnd).
type needBranchMissing struct {
	branch string
	holds  []string
	merged bool
	ofTask bool
	err    error
}

func (e *needBranchMissing) Error() string {
	return fmt.Sprintf("GitHub has no branch %s%s: %v", e.branch, e.held("the"), e.err)
}

// held is what the missing branch holds, as the words that name it go on: ` of <whose> need <id>`
// for a need's own branch, `, which holds the work of <whose> need <id>` for the default branch,
// with `needs` and each id for several, and `, the branch of task <id>, whose part waits for it`
// for a part's task's branch.
func (e *needBranchMissing) held(whose string) string {
	switch {
	case e.ofTask:
		return fmt.Sprintf(", the branch of task %s, whose part waits for it", strings.Join(e.holds, ", "))
	case e.merged:
		return fmt.Sprintf(", which holds the work of %s %s", whose, idsNamed("need", e.holds))
	}
	return fmt.Sprintf(" of %s %s", whose, idsNamed("need", e.holds))
}

func (e *needBranchMissing) Unwrap() error { return e.err }

// githubAnswered says whether err carries GitHub's answer of status (a *githubrest.Answer).
func githubAnswered(err error, status int) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && answer.Status == status
}

// mergeBaseMissing says whether err is GitHub's 404 to a merge whose base, not its head, it lacks
// ("Base does not exist"): the task's own branch, which names no need.
func mergeBaseMissing(err error) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && strings.Contains(answer.Body, "Base does not exist")
}

// taskJoinToldPrefix begins the last error of a task_branch row whose join cannot end once its
// task's owner is told (joinCannotEnd), the one record that the notice was written, so the row's
// next 404 tells nobody again; a try that fails for another cause between two such answers
// replaces the last error, and the next 404 tells again.
const taskJoinToldPrefix = "the owner is told: "

// joinCannotEnd is the failure of the row of task of issue whose join waits for missing, a need's
// branch GitHub does not have: the row tries again on its backoff, the task's first step keeps its
// wait for the join, and at the row's first such answer the issue's architect, the task's owner, is
// told once, in a task-join-failed notice that names the task, the need and the branch.
func (r *outbox) joinCannotEnd(ctx context.Context, row record.OutboxRow, issue record.Issue, task record.PlanTask, missing *needBranchMissing) error {
	words := fmt.Sprintf("task %s of %s waits for the join of its needs: GitHub has no branch %s%s", task.Task, issue.Key, missing.branch, missing.held("its"))
	if !strings.HasPrefix(row.LastError, taskJoinToldPrefix) {
		notice := record.Notice{Kind: "task-join-failed", Role: claim.RoleArchitect, Reason: words + ". Push that branch again, or stop the task with stop_task."}
		if err := r.enqueueNotice(ctx, issue.Key, notice.Kind, notice); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s%s: %w", taskJoinToldPrefix, words, missing.err)
}
