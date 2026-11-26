package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// verdictSweepInterval is how often the writers' sweep runs after the one at boot
// (tickVerdictSweep). It repairs what a lost event or a lost write left, so it has only to come
// well inside the merge queue's 180-minute timeout, and each run costs a read of every open pull
// request of each verdict repository (spec, "The writers' sweep").
const verdictSweepInterval = 10 * time.Minute

// openPullRequestsQuery reads one page of a repository's open pull requests, a hundred at most:
// each one's number, head branch, head commit and GitHub's clock of its last change and, where
// $reviews asks for them, each author's last opinionated review - an approval or a request for
// changes - with its id, author, state, body and submission time. That is the review
// verdict.Decide reads as an author's word: a comment left after it decides nothing and hides
// nothing, where latestReviews would list the comment in its place. A review's id is
// fullDatabaseId, a string, which GitHub's schema gives in place of the deprecated databaseId.
// GraphQL names a bot by its login without the [bot] REST ends it with, and by its type.
const openPullRequestsQuery = `query($owner:String!,$name:String!,$after:String,$reviews:Boolean!){
repository(owner:$owner,name:$name){pullRequests(states:OPEN,first:100,after:$after){
pageInfo{hasNextPage endCursor}
nodes{number headRefName headRefOid updatedAt latestOpinionatedReviews(first:100)@include(if:$reviews){
nodes{fullDatabaseId author{__typename login}state body submittedAt}}}}}}`

// ciReviewHead is the line a CI reviewer's review body names the head it read with:
// `<!-- ci-review-head <commit> -->`.
var ciReviewHead = regexp.MustCompile(`<!--\s*ci-review-head\s+(\S+)\s*-->`)

// mergeGroupRef is a merge queue's group branch, gh-readonly-queue/<base>/pr-<number>-<base sha>,
// with the number of the pull request whose group it is; the base may itself hold slashes.
var mergeGroupRef = regexp.MustCompile(`^refs/heads/gh-readonly-queue/.+/pr-([1-9][0-9]*)-[0-9a-f]+$`)

// sweptPullRequest is one open pull request as openPullRequestsQuery lists it.
type sweptPullRequest struct {
	Number                   int       `json:"number"`
	HeadRefName              string    `json:"headRefName"`
	HeadRefOid               string    `json:"headRefOid"`
	UpdatedAt                time.Time `json:"updatedAt"`
	LatestOpinionatedReviews struct {
		Nodes []listedReview `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
}

// listedReview is one author's last opinionated review of a pull request as openPullRequestsQuery
// lists it, its id as GitHub's schema spells it, a decimal string. A deleted account's review has
// no author.
type listedReview struct {
	FullDatabaseID string `json:"fullDatabaseId"`
	Author         *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	SubmittedAt *time.Time `json:"submittedAt"`
}

// groupRef is one of a merge queue's group branches as GitHub lists the repository's refs (GET
// /git/matching-refs/heads/gh-readonly-queue/): its name, and the group's commit it points at.
type groupRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// tickVerdictSweep runs the writers' sweep (sweepVerdicts) once as the runtime starts and then
// every verdictSweepInterval, until ctx ends. The pass at boot is what repairs a restart: every
// write a fact asked for is held in memory until the writer makes it (verdictWriter), and so is the
// merge-state read the writer asks for after a pass that wrote a success (writeVerdictOver), which
// is why the pass at boot alone also asks for a read of each issue whose success stands. A runtime
// whose configuration names no verdict repository sweeps nothing.
func (w *workflowRuntime) tickVerdictSweep(ctx context.Context) {
	if w.verdicts == nil || len(w.verdicts.entries) == 0 {
		return
	}
	w.sweepVerdicts(ctx, true)
	ticker := time.NewTicker(verdictSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweepVerdicts(ctx, false)
		}
	}
}

// sweepVerdicts is one pass of the writers' sweep over each verdict repository (sweepRepository),
// boot being whether it is the pass at boot. A repository whose pass fails is logged, and the next
// pass reads it again.
func (w *workflowRuntime) sweepVerdicts(ctx context.Context, boot bool) {
	for _, entry := range w.verdicts.entries {
		if err := w.sweepRepository(ctx, entry, boot); err != nil && ctx.Err() == nil {
			w.log.Warn("verdict: the writers' sweep of a verdict repository failed; its next pass reads it again",
				"repository", entry.Repository.String(), "error", err)
		}
	}
}

// sweepRepository sweeps one verdict repository (spec R6, R19, "The writers' sweep"). It reads the
// repository's open pull requests with one GraphQL query for each hundred, with each author's last
// opinionated review where the entry names a ci_reviewer, and records what a webhook may have lost
// of a pull request on a branch that is no issue's (recordSwept): its head, and the CI reviewer's
// review of that head. It then queues for the writer a write on each open head, that may put the
// daemon's own conclusion over another writer's run or status (overForeign): the writer decides the
// head from the record, creates the run a lost or failed write left unwritten, turns each run of the
// App there to the head's conclusion, and writes that conclusion - failure where none stands, and
// while no decision stands the conclusion undecidedOver gives - over each status of the one App its
// record does not hold. A legion/<KEY> head of another project's issue is swept too, as a head no
// verdict stands at (spec R4, R19). A head the list shows no other open pull request at is written
// alone, with no read of the pull requests at its commit
// (atHead). Once the list is read, the writer numbers the pass and forgets the count of sweeps with
// no answer of every head of the repository the list does not hold (sweepBegun), so a head counts
// once a pass and a head that closed or moved on keeps no count in memory. Last, it reads the merge
// queue's group branches (gh-readonly-queue/) and queues a write on each group's commit
// (queueMergeGroupWrite); a branch whose name gives no pull request is logged and written on by
// nobody, since no pull request's verdict can decide its group. Every write is made by the writer's
// one goroutine (writeVerdicts), so a write the sweep queues runs beside no other write, and decides
// its head from the record as it stands when the write is made.
//
// At boot it also asks for a read of the merge state of each issue of this project whose success,
// as this daemon recorded writing it, stands at its pull request's head (readStandingSuccess): a
// restart loses the read the writer asks for after it writes a success, and GitHub's answer to the
// reads before that may have been a stale `blocked` that holds the round.
//
// A daemon in token mode mints no verdict token, so no read here could lead to a write: it reads
// nothing, and says so once, as the writer does. A daemon that holds no App id reads nothing either,
// since its writer writes nothing (writeDecided), and the ERROR where the writer is built says why.
func (w *workflowRuntime) sweepRepository(ctx context.Context, entry config.VerdictRepository, boot bool) error {
	if w.verdicts.appID == 0 {
		return nil
	}
	serving := w.repositories.serving(entry.Repository)
	if _, err := w.tokens.VerdictToken(ctx, serving.Owner()); errors.Is(err, appauth.ErrNoVerdictToken) {
		w.verdicts.tokenMode.Do(func() { w.log.Warn(verdictTokenMode, "repository", entry.Repository.String()) })
		return nil
	} else if err != nil {
		return fmt.Errorf("mint the verdict token for %s: %w", serving, err)
	}
	lease, err := w.tokens.ReadToken(ctx, serving.Owner())
	if err != nil {
		return fmt.Errorf("mint the read token for %s: %w", serving, err)
	}
	pulls, err := w.openPullRequests(ctx, lease.Token, serving, entry.CIReviewer != "")
	if err != nil {
		return err
	}
	atHead := map[string]int{}
	for _, pull := range pulls {
		atHead[pull.HeadRefOid]++
	}
	w.verdicts.sweepBegun(entry.Repository.String(), atHead)
	for _, pull := range pulls {
		key, issueBranch := claim.BranchIssue(pull.HeadRefName)
		ours := issueBranch && claim.KeyOfProject(w.dispatchProject, key)
		switch {
		case issueBranch && !ours:
			w.noteOtherProject(entry.Repository.String(), pull.Number, key)
		case !issueBranch:
			if err := w.recordSwept(ctx, entry, pull); err != nil {
				return err
			}
		}
		w.verdicts.add(verdictTarget{repository: entry.Repository, number: pull.Number, head: pull.HeadRefOid, overForeign: true,
			alone: atHead[pull.HeadRefOid] == 1})
		if boot && ours {
			if err := w.readStandingSuccess(ctx, entry.Repository, pull.HeadRefOid, key); err != nil {
				return err
			}
		}
	}
	client := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, serving)}
	read, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	refs, err := githubrest.GetPages[groupRef](read, client, "/git/matching-refs/heads/gh-readonly-queue/")
	if err != nil {
		return fmt.Errorf("list the merge queue's group branches of %s: %w", serving, err)
	}
	for _, ref := range refs {
		match := mergeGroupRef.FindStringSubmatch(ref.Ref)
		var number int
		if match != nil {
			number, err = strconv.Atoi(match[1])
		}
		if match == nil || err != nil {
			w.log.Warn(unnamedGroupBranch, "repository", entry.Repository.String(), "ref", ref.Ref, "group", ref.Object.SHA)
			continue
		}
		w.queueMergeGroupWrite(entry.Repository, number, ref.Object.SHA, true)
	}
	return nil
}

// unnamedGroupBranch is what the sweep logs of a gh-readonly-queue/ branch whose name is not a
// group's of one pull request (mergeGroupRef): no pull request's verdict decides its group, so no
// legion-verdict is written on its commit, and the queue holds that group until its timeout.
const unnamedGroupBranch = "verdict: a merge queue's group branch names no pull request, so legion-verdict is not written on its commit"

// readStandingSuccess asks for a read of issue's merge state (queueMergeState) where the newest
// check run or status this daemon recorded writing on head of repository concluded success
// (wroteSuccess): the pass at boot's stand-in for the read a restart lost, which a write that
// changes nothing does not ask for (writeVerdictOver). A head whose success the boot pass's own
// write creates or restores asks for its read there instead.
func (w *workflowRuntime) readStandingSuccess(ctx context.Context, repository ghrepo.Repository, head, issue string) error {
	standing, err := w.wroteSuccess(ctx, verdictTarget{repository: repository, head: head})
	if err != nil {
		return fmt.Errorf("read whether the success written on %s at %s stands: %w", repository, head, err)
	}
	if standing {
		w.queueMergeState(issue)
	}
	return nil
}

// openPullRequests is every open pull request of repository, read a page of a hundred at a time
// (openPullRequestsQuery) with token, with each author's last opinionated review where reviews is
// set.
func (w *workflowRuntime) openPullRequests(ctx context.Context, token string, repository ghrepo.Repository, reviews bool) ([]sweptPullRequest, error) {
	var pulls []sweptPullRequest
	var after *string
	for {
		var page struct {
			Repository *struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []sweptPullRequest `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		read, cancel := context.WithTimeout(ctx, issueRead)
		err := githubrest.GraphQL(read, w.githubAPI, token, openPullRequestsQuery,
			map[string]any{"owner": repository.Owner(), "name": repository.Name(), "after": after, "reviews": reviews}, &page)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("read the open pull requests of %s: %w", repository, err)
		}
		if page.Repository == nil {
			return nil, fmt.Errorf("read the open pull requests of %s: GitHub answered no repository", repository)
		}
		listed := page.Repository.PullRequests
		pulls = append(pulls, listed.Nodes...)
		if !listed.PageInfo.HasNextPage {
			return pulls, nil
		}
		cursor := listed.PageInfo.EndCursor
		after = &cursor
	}
}

// recordSwept records what the sweep read of an open pull request on a branch that is no issue's,
// where the record lacks it: the head GitHub lists, with GitHub's clock of the pull request, as a
// sighting of the outside pull request when its newest sighting stands at another head or there is
// none, and the CI reviewer's listed review of that head (ciReviewerReview), once. A listed review
// whose id does not read is logged and counts for nothing. A person's listed review is recorded
// nowhere: GitHub lists a review at the pull request's current head whatever head it read, so only
// a webhook's delivery tells which head a person approved (spec R4).
func (w *workflowRuntime) recordSwept(ctx context.Context, entry config.VerdictRepository, pull sweptPullRequest) error {
	now := w.clockNow().UTC()
	review, counted, err := ciReviewerReview(entry, pull)
	if err != nil {
		w.log.Warn("verdict: the sweep could not read the id of the CI reviewer's listed review, so that review counts for nothing",
			"repository", entry.Repository.String(), "pull_request", pull.Number, "error", err)
	}
	return store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		sighted, found, err := store.OutsidePullRequestTx(ctx, tx, entry.Repository, pull.Number)
		if err != nil {
			return err
		}
		if !found || sighted.Head != pull.HeadRefOid {
			if _, err := store.RecordOutsidePullRequestTx(ctx, tx, store.OutsidePullRequest{Repository: entry.Repository, Number: pull.Number,
				Head: pull.HeadRefOid, UpdatedAt: pull.UpdatedAt, RecordedAt: now}); err != nil {
				return err
			}
		}
		if !counted {
			return nil
		}
		recorded, err := store.VerdictReviewsTx(ctx, tx, entry.Repository, pull.Number)
		if err != nil {
			return err
		}
		for _, r := range recorded {
			if r.GitHubID == review.GitHubID && r.Head == review.Head && r.State == review.State {
				return nil
			}
		}
		review.RecordedAt = now
		_, err = store.RecordVerdictReviewTx(ctx, tx, review)
		return err
	})
}

// ciReviewerReview is the last opinionated review of pull by entry's ci_reviewer, as a verdict
// reads it, and whether one counts: an approval or a request for changes whose body's
// ci-review-head line names the pull request's current head (spec R4). Its id is GitHub's
// fullDatabaseId read whole (strconv.ParseInt); one that does not read counts for nothing, and the
// error says why. The review is recorded as no person's, which the CI reviewer, a bot, is not.
func ciReviewerReview(entry config.VerdictRepository, pull sweptPullRequest) (store.VerdictReview, bool, error) {
	if entry.CIReviewer == "" {
		return store.VerdictReview{}, false, nil
	}
	for _, listed := range pull.LatestOpinionatedReviews.Nodes {
		if listed.Author == nil {
			continue
		}
		login := listed.Author.Login
		if listed.Author.Typename == "Bot" {
			login += "[bot]"
		}
		state := strings.ToLower(listed.State)
		if !strings.EqualFold(login, entry.CIReviewer) || state != verdict.StateApproved && state != verdict.StateChangesRequested ||
			reviewedHead(listed.Body) != pull.HeadRefOid {
			continue
		}
		id, err := strconv.ParseInt(listed.FullDatabaseID, 10, 64)
		if err != nil {
			return store.VerdictReview{}, false, fmt.Errorf("the fullDatabaseId %q of %s's review: %w", listed.FullDatabaseID, login, err)
		}
		review := store.VerdictReview{Repository: entry.Repository, Number: pull.Number, GitHubID: id, Head: pull.HeadRefOid,
			Author: login, State: state}
		if listed.SubmittedAt != nil {
			review.SubmittedAt = *listed.SubmittedAt
		}
		return review, true, nil
	}
	return store.VerdictReview{}, false, nil
}

// reviewedHead is the head a CI reviewer's review body names on its last ci-review-head line, ""
// for a body that names none.
func reviewedHead(body string) string {
	matches := ciReviewHead.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}
