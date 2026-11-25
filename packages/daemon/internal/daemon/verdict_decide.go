package daemon

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// The kinds of head a write of legion-verdict names in its log line: a legion/<KEY> head of this
// daemon's project, and a head no Legion issue owns.
const (
	headLegion  = "legion"
	headOutside = "outside"
)

// atHead is decided, the decision of pull request number at decided.head, made over every other
// open pull request at that head as well (spec R4). A check run belongs to a commit, not to a pull
// request, so the one legion-verdict at a head is every open pull request's there:
//   - failure where a refusal of any of them stands, since a refusal at a head beats every approval
//     at that head;
//   - where a Legion issue owns the head, as one of them is a legion/<KEY> head or a task branch of
//     whatever project, success only where each such pull request's steps all passed there, since
//     legion-verdict is success at such a head only then;
//   - at a head no Legion issue owns, success where the verdict of any of them stands;
//   - and nothing otherwise.
//
// GitHub lists the pull requests of a commit (GET /commits/{sha}/pulls), and only the open ones whose
// head is that commit are decided, each by decideVerdict; pulls is the write's one read of that list
// (headPulls). A failure needs no other read, and the callers ask for none then. The reason of a
// failure for a commit with no reader, and the commits it names, stay only where the head's
// conclusion is the pull request's own.
func (w *workflowRuntime) atHead(ctx context.Context, entry config.VerdictRepository, repository ghrepo.Repository, number int, decided verdictDecision,
	pulls *headPulls) (verdictDecision, error) {
	if decided.head == currentHead {
		return decided, nil
	}
	others, err := w.openPullRequestsAt(ctx, repository, decided.head, pulls)
	if err != nil {
		return verdictDecision{}, err
	}
	at := []verdictDecision{decided}
	for _, other := range others {
		if other == number {
			continue
		}
		each, err := w.decideVerdict(ctx, entry, repository, other, decided.head, pulls)
		if err != nil {
			return verdictDecision{}, err
		}
		at = append(at, each)
	}
	combined := decided
	if combined.conclusion = headConclusion(at); combined.conclusion != decided.conclusion {
		combined.reason, combined.noReader = "", nil
	}
	return combined, nil
}

// headConclusion is legion-verdict's conclusion at one head from the decisions of the open pull
// requests there (atHead).
func headConclusion(at []verdictDecision) verdict.Conclusion {
	legion, legionPassed, outsidePassed := false, true, false
	for _, decided := range at {
		switch {
		case decided.conclusion == verdict.Failure:
			return verdict.Failure
		case decided.kind == headLegion:
			legion, legionPassed = true, legionPassed && decided.conclusion == verdict.Success
		default:
			outsidePassed = outsidePassed || decided.conclusion == verdict.Success
		}
	}
	if legion && legionPassed || !legion && outsidePassed {
		return verdict.Success
	}
	return verdict.None
}

// openPullRequestsAt is the number of each open pull request of repository whose head is head
// (pullRequestsAt).
func (w *workflowRuntime) openPullRequestsAt(ctx context.Context, repository ghrepo.Repository, head string, pulls *headPulls) ([]int, error) {
	listed, err := w.pullRequestsAt(ctx, repository, head, pulls)
	if err != nil {
		return nil, err
	}
	open := make([]int, 0, len(listed))
	for _, pull := range listed {
		open = append(open, pull.Number)
	}
	return open, nil
}

// headPulls is one write's read of the open pull requests at the head it decides, of the one
// repository it writes on (pullRequestsAt). The branch a Legion head's change is compared with
// (compareBase) and the pull requests decided with the head (atHead) come from that one list, so a
// write reads it once. The zero value has read nothing. A read GitHub did not answer is kept
// nowhere, and the write's next asker reads again.
type headPulls struct {
	head  string
	pulls []githubrest.PullRequest
	read  bool
}

// pullRequestsAt is each open pull request of repository whose head is head, with the branch it
// merges into, as GitHub lists the pull requests of that commit with the daemon's read token (GET
// /commits/{sha}/pulls), read once in the write pulls is of.
func (w *workflowRuntime) pullRequestsAt(ctx context.Context, repository ghrepo.Repository, head string, pulls *headPulls) ([]githubrest.PullRequest, error) {
	if pulls.read && pulls.head == head {
		return pulls.pulls, nil
	}
	serving := w.repositories.serving(repository)
	client, err := readClient(ctx, w.tokens, w.githubAPI, serving)
	if err != nil {
		return nil, fmt.Errorf("mint the read token for %s: %w", serving, err)
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	listed, err := githubrest.GetPages[githubrest.PullRequest](read, client, "/commits/"+head+"/pulls")
	if err != nil {
		return nil, fmt.Errorf("list the pull requests of %s at %s: %w", serving, head, err)
	}
	var open []githubrest.PullRequest
	for _, pull := range listed {
		if pull.State == "open" && pull.Head.SHA == head {
			open = append(open, pull)
		}
	}
	pulls.head, pulls.pulls, pulls.read = head, open, true
	return open, nil
}

// verdictDecision is what decideVerdict decided of a head: the head, legion-verdict's conclusion
// there, on a Legion head the reason of a failure no check and no step refused (verdict.Reason, which
// the run's output names) with each commit of the change that has no reader there, the kind of head
// (headLegion, headOutside) and, on a Legion head of this project, the issue that owns it.
type verdictDecision struct {
	head       string
	conclusion verdict.Conclusion
	reason     verdict.Reason
	noReader   []string
	kind       string
	issue      string
}

// currentHead, as decideVerdict's head, asks it to decide the pull request at the head it stands at
// now, which a merge group's write does (writeMergeGroup): the group's event names the pull request
// and its own commit, not the head.
const currentHead = ""

// decideVerdict decides head of pull request number of repository, entry being the repository's
// verdict entry, or, for currentHead, the head the pull request stands at now: the record's, or the
// one GitHub shows. A pull request the project's records hold is a Legion head of the issue that
// owns it, decided from the steps of its task's plan at the head and the reviews delivered at the
// head (legionInputs), and, where every step passed there, from the readers of the change's commits
// (readCommits), read after the transaction so it holds no connection over a GitHub call. A
// legion/<KEY> head of another project's issue is a Legion head no verdict stands at: this daemon
// holds no step of that issue (otherProjectsHead). Every other pull request is an outside one,
// decided from its reviews and, where the entry takes them, its side reviewer's verdict
// (outsideInputs). A pull request no record holds yet - one opened before the daemon heard its
// repository - is read from GitHub once (GET /pulls/{number}): one GitHub shows closed merges
// nothing and has no verdict, as a closed Legion pull request is written on by nobody (workflow's
// askVerdictWrite); a legion/<KEY> head holds no verdict here, of this project's issue since no
// record holds it and of another project's as above; and any other is recorded as an outside pull
// request at the head GitHub shows and decided as one. pulls is the write's one read of the open pull
// requests at the head (headPulls).
func (w *workflowRuntime) decideVerdict(ctx context.Context, entry config.VerdictRepository, repository ghrepo.Repository, number int, head string,
	pulls *headPulls) (verdictDecision, error) {
	if other := w.verdicts.otherIssue(repository.String(), number); other != "" {
		return w.otherProjectsHead(repository, number, other, head), nil
	}
	var decided verdictDecision
	var legion *legionHead
	known, other := false, ""
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := ghrepo.Under(w.renames(), repository.String(), func(name string) (*record.PullRequest, error) {
			return w.records.PullRequestByNumber(ctx, tx, name, number)
		})
		if err != nil {
			return err
		}
		if pr != nil {
			known = true
			if head == currentHead {
				head = pr.HeadSHA
			}
			issue, err := w.records.Issue(ctx, tx, pr.Issue)
			if err != nil || issue == nil {
				other = pr.Issue
				return err
			}
			read, err := w.legionInputs(ctx, tx, entry, *issue, *pr, head)
			legion = &read
			decided = verdictDecision{head: head, kind: headLegion, issue: issue.Key}
			return err
		}
		outside, found, err := store.OutsidePullRequestTx(ctx, tx, repository, number)
		if err != nil || !found {
			return err
		}
		known = true
		if head == currentHead {
			head = outside.Head
		}
		in, err := outsideInputs(ctx, tx, entry, repository, number, head)
		decided.conclusion, _ = verdict.Decide(in)
		decided.head, decided.kind = head, headOutside
		return err
	}); err != nil {
		return decided, err
	}
	switch {
	case other != "":
		return w.otherProjectsHead(repository, number, other, head), nil
	case legion != nil:
		w.readCommits(ctx, repository, number, legion, pulls)
		decided.conclusion, decided.reason = verdict.Decide(legion.in)
		if decided.reason == verdict.NoReader {
			decided.noReader = verdict.CommitsWithNoReader(legion.in)
		}
		return decided, nil
	case known:
		return decided, nil
	}
	pull, err := w.readVerdictPullRequest(ctx, repository, number)
	if err != nil {
		return verdictDecision{}, err
	}
	if head == currentHead {
		head = pull.Head.SHA
	}
	if pull.State != "open" {
		return verdictDecision{head: head, kind: headOutside}, nil
	}
	if key, issueBranch := claim.BranchIssue(pull.Head.Ref); issueBranch {
		if !claim.KeyOfProject(w.dispatchProject, key) {
			return w.otherProjectsHead(repository, number, key, head), nil
		}
		// A head of this project's issue that no record holds has no verdict here.
		return verdictDecision{head: head, kind: headLegion}, nil
	}
	err = store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := store.RecordOutsidePullRequestTx(ctx, tx, store.OutsidePullRequest{Repository: repository, Number: number,
			Head: pull.Head.SHA, UpdatedAt: pull.UpdatedAt, RecordedAt: w.clockNow().UTC()}); err != nil {
			return err
		}
		in, err := outsideInputs(ctx, tx, entry, repository, number, head)
		decided.conclusion, _ = verdict.Decide(in)
		decided.head, decided.kind = head, headOutside
		return err
	})
	return decided, err
}

// otherProjectsHead is the decision of head of pull request number of repository, whose head branch
// is issue's, another project's: a Legion head where no verdict stands, since this daemon holds no
// step of that issue (spec R4), so the writer writes no success there and failure over each run and
// status of the one App it did not write (R19). It is logged once per pull request.
func (w *workflowRuntime) otherProjectsHead(repository ghrepo.Repository, number int, issue, head string) verdictDecision {
	w.noteOtherProject(repository.String(), number, issue)
	return verdictDecision{head: head, kind: headLegion}
}

// legionHead is what decideVerdict reads of a Legion head in its transaction (legionInputs): the
// inputs verdict.Decide reads, all but the change's commits, which readCommits reads from GitHub
// after the transaction, so it holds no connection over a GitHub call; the base branch the record
// holds for the pull request, "" where it holds none; and the issue's default branch.
type legionHead struct {
	in            verdict.Inputs
	recordedBase  string
	defaultBranch string
}

// legionInputs is what the record holds of head of issue's pull request pr: the steps of the task
// its run walks (workflow.Engine.VerdictSteps), by #4611's step type; the states Dispatch published
// of the task's checks at the head (record.HeadStates), which each must read yes there whatever steps
// the plan names, and which every testing step reads; for each review step, the verdict at the head
// of its step (store.StepVerdict, workflow's recordVerdict); the sessions the daemon records for the
// claim of each review step's agent that approved the head, and of each testing step's agent that
// its step's record names (store.ClaimSessionNamesTx), by which those agents read the change's
// commits (verdict.Step.Sessions); and every review of the pull request delivered by webhook
// (store.VerdictReview), with entry's ci_reviewer, whose request for changes refuses a Legion head
// too (spec R4). Each is read at head alone, so a pass at an earlier commit counts at no later head.
func (w *workflowRuntime) legionInputs(ctx context.Context, tx pgx.Tx, entry config.VerdictRepository, issue record.Issue, pr record.PullRequest, head string) (legionHead, error) {
	repository, err := ghrepo.Parse("the repository of "+issue.Key+"'s pull request", pr.Repo)
	if err != nil {
		return legionHead{}, err
	}
	steps, err := w.engine.VerdictSteps(ctx, tx, issue)
	if err != nil {
		return legionHead{}, err
	}
	read := legionHead{in: verdict.Inputs{Head: head, Legion: true, Steps: make([]verdict.Step, 0, len(steps)), CIReviewer: entry.CIReviewer},
		recordedBase: pr.Base, defaultBranch: issue.DefaultBranch}
	for _, step := range steps {
		read.in.Steps = append(read.in.Steps, verdict.Step{Type: step.Type})
		reader := step.Agent
		switch step.Type {
		case planwords.TypeReview:
			given, found, err := store.StepVerdictTx(ctx, tx, repository, pr.Number, head, step.Place.Task, step.Place.Place)
			if err != nil {
				return legionHead{}, err
			}
			if !found || given.Issue != issue.Key {
				continue
			}
			read.in.Steps[len(read.in.Steps)-1].Verdict = given.Verdict
			if given.Verdict != verdict.ReviewerApprove {
				continue
			}
			reader = claim.Token(given.Session)
		case planwords.TypeTesting:
		default:
			continue
		}
		if reader == "" {
			continue
		}
		names, err := store.ClaimSessionNamesTx(ctx, tx, reader)
		if err != nil {
			return legionHead{}, err
		}
		read.in.Steps[len(read.in.Steps)-1].Sessions = names
	}
	held, err := w.records.HeadStates(ctx, tx, issue.Key, pr.Repo, head)
	if err != nil {
		return legionHead{}, err
	}
	read.in.Checks = checksAt(held)
	read.in.Reviews, err = reviewsOf(ctx, tx, repository, pr.Number)
	return read, err
}

// checksAt is each check of the head's set and the state the daemon holds of it there, "" for
// none, in the set's order: nil where no set is held at the head.
func checksAt(held record.HeadStates) []verdict.Check {
	if held.Checks == nil {
		return nil
	}
	checks := make([]verdict.Check, 0, len(held.Checks))
	for _, check := range held.Checks {
		checks = append(checks, verdict.Check{Name: check, State: held.States[check].State})
	}
	return checks
}

// changeUnread is what the writer logs when it cannot read the change's commits, and so finds no
// reader of any.
const changeUnread = "verdict: the change's commits could not be read, so no commit has a reader"

// readCommits reads the change's commits of read's head where they decide anything: each check of
// the task reads yes and every review step approved there (verdict.StepsPassed). The change is the
// compare of the head with the branch the pull request merges into (compareBase). Each commit's
// makers are the sessions its Omp-Session lines name (ompSessions). Where the commits cannot be read
// the change is unread, and no commit has a reader (verdict.Inputs.Unread); the line it logs says
// why.
func (w *workflowRuntime) readCommits(ctx context.Context, repository ghrepo.Repository, number int, read *legionHead, pulls *headPulls) {
	if !verdict.StepsPassed(read.in) {
		return
	}
	base, err := w.compareBase(ctx, repository, number, read, pulls)
	var commits []verdict.Commit
	if err == nil {
		commits, err = w.changeCommits(ctx, repository, base, read.in.Head)
	}
	if err != nil {
		read.in.Unread = true
		w.log.Warn(changeUnread, "repository", repository.String(), "pull_request", number, "head", read.in.Head, "error", err)
		return
	}
	read.in.Commits = commits
}

// compareBase is the branch pull request number merges into, with which its head's change is
// compared: the base GitHub lists for it among the open pull requests at the head (pullRequestsAt,
// the write's one read of them, which atHead reads too), so a child issue's change leaves out the
// commits its parent's branch holds; where GitHub lists no such pull request there, the base the
// record holds; and only where neither names one, the issue's default branch. Where GitHub's list
// cannot be read and the record holds no base, the base is unknown, and the change is no reading
// (an error).
func (w *workflowRuntime) compareBase(ctx context.Context, repository ghrepo.Repository, number int, read *legionHead, at *headPulls) (string, error) {
	pulls, err := w.pullRequestsAt(ctx, repository, read.in.Head, at)
	if err != nil {
		if read.recordedBase != "" {
			return read.recordedBase, nil
		}
		return "", fmt.Errorf("the branch pull request %d merges into: %w", number, err)
	}
	for _, pull := range pulls {
		if pull.Number == number && pull.Base.Ref != "" {
			return pull.Base.Ref, nil
		}
	}
	if read.recordedBase != "" {
		return read.recordedBase, nil
	}
	return read.defaultBranch, nil
}

// listedCommit is one commit of the change as GitHub's compare lists it: its id and its message.
type listedCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// changeCommits is each commit of the change, in its order, with the sessions its Omp-Session lines
// name: GitHub's compare of base and head (GET /compare/BASE...HEAD) with the daemon's read token,
// a hundred commits to a page (githubrest.GetListPages) and every page, which must hold head. A
// pull request's own list of commits stops at 250 (spec R4), and the compare's pages do not. A
// compare GitHub refuses, or one that does not hold head, is no reading of the change, and an error.
func (w *workflowRuntime) changeCommits(ctx context.Context, repository ghrepo.Repository, base, head string) ([]verdict.Commit, error) {
	serving := w.repositories.serving(repository)
	if base == "" {
		return nil, fmt.Errorf("no base branch is known of %s's pull request at %s", serving, head)
	}
	client, err := readClient(ctx, w.tokens, w.githubAPI, serving)
	if err != nil {
		return nil, fmt.Errorf("mint the read token for %s: %w", serving, err)
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	listed, err := githubrest.GetListPages[listedCommit](read, client, "/compare/"+base+"..."+head, "commits")
	if err != nil {
		return nil, fmt.Errorf("compare %s...%s of %s: %w", base, head, serving, err)
	}
	if !slices.ContainsFunc(listed, func(c listedCommit) bool { return c.SHA == head }) {
		return nil, fmt.Errorf("the %d commits of the compare %s...%s of %s do not hold %s", len(listed), base, head, serving, head)
	}
	commits := make([]verdict.Commit, 0, len(listed))
	for _, c := range listed {
		commits = append(commits, verdict.Commit{SHA: c.SHA, Makers: ompSessions(c.Commit.Message)})
	}
	return commits, nil
}

// ompSessionLine is a commit message's Omp-Session trailer line, as `legion git` writes it
// (cmd/legion's gitTrailers): the key in any case, as git compares trailer keys, and the session
// id.
var ompSessionLine = regexp.MustCompile(`(?im)^omp-session:[ \t]*(\S+)[ \t\r]*$`)

// ompSessions is every session id an Omp-Session line of message names, wherever it stands in the
// message: a squash or a cherry-pick can carry a commit's trailer into the body of another, and a
// line read so makes that session a maker of the other commit too, which takes a reader away and
// never adds one (R4's fourth departure).
func ompSessions(message string) []string {
	var sessions []string
	for _, match := range ompSessionLine.FindAllStringSubmatch(message, -1) {
		sessions = append(sessions, match[1])
	}
	return sessions
}

// outsideInputs is what the daemon records of head of an outside pull request: its reviews, and,
// where entry takes side reviews, the verdict the controller relayed from its side reviewer at the
// head (a store.Verdict of no issue).
func outsideInputs(ctx context.Context, tx pgx.Tx, entry config.VerdictRepository, repository ghrepo.Repository, number int, head string) (verdict.Inputs, error) {
	in := verdict.Inputs{Head: head, CIReviewer: entry.CIReviewer, SideReviews: entry.SideReviews}
	if entry.SideReviews {
		given, found, err := store.VerdictTx(ctx, tx, repository, number, head)
		if err != nil {
			return verdict.Inputs{}, err
		}
		// A side verdict stands only while its session is the reviewer the controller names at the
		// head now: a later naming of another session withdraws it.
		named, current, err := store.SideReviewerTx(ctx, tx, repository, number, head)
		if err != nil {
			return verdict.Inputs{}, err
		}
		if found && given.Issue == "" && current && named.Session == given.Session {
			in.Side = given.Verdict
		}
	}
	var err error
	in.Reviews, err = reviewsOf(ctx, tx, repository, number)
	return in, err
}

// reviewsOf is every review of the pull request the daemon recorded, in the order it recorded them,
// as a verdict reads them: each with GitHub's id and submission time, by which verdict.Decide takes
// each author's last.
func reviewsOf(ctx context.Context, tx pgx.Tx, repository ghrepo.Repository, number int) ([]verdict.Review, error) {
	recorded, err := store.VerdictReviewsTx(ctx, tx, repository, number)
	if err != nil {
		return nil, err
	}
	reviews := make([]verdict.Review, 0, len(recorded))
	for _, r := range recorded {
		reviews = append(reviews, verdict.Review{Head: r.Head, Author: r.Author, Person: r.AuthorCanWrite, State: r.State,
			GitHubID: r.GitHubID, SubmittedAt: r.SubmittedAt})
	}
	return reviews, nil
}

// readVerdictPullRequest reads pull request number of repository from GitHub with the daemon's
// read token (GET /pulls/{number}).
func (w *workflowRuntime) readVerdictPullRequest(ctx context.Context, repository ghrepo.Repository, number int) (githubrest.PullRequest, error) {
	serving := w.repositories.serving(repository)
	client, err := readClient(ctx, w.tokens, w.githubAPI, serving)
	if err != nil {
		return githubrest.PullRequest{}, fmt.Errorf("mint the read token for %s: %w", serving, err)
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	var pull githubrest.PullRequest
	if err := client.Get(read, "/pulls/"+strconv.Itoa(number), &pull); err != nil {
		return githubrest.PullRequest{}, fmt.Errorf("read pull request %s#%d: %w", serving, number, err)
	}
	return pull, nil
}

// renames is the deployment's repository renames, none for a runtime a test builds with no
// repositories.
func (w *workflowRuntime) renames() ghrepo.Renames {
	if w.repositories == nil {
		return ghrepo.Renames{}
	}
	return w.repositories.renames
}

// noteOtherProject logs, once per pull request, that its head is another project's issue branch,
// where no verdict stands since this daemon holds no step of that issue (otherProjectsHead), and
// keeps that issue for the pull request, so the next decision of it reads nothing.
func (w *workflowRuntime) noteOtherProject(repository string, number int, issue string) {
	if w.verdicts.markForeign(repository, number, issue) {
		w.log.Info(otherProjectsHead, "repository", repository, "pull_request", number, "issue", issue, "project", w.dispatchProject)
	}
}
