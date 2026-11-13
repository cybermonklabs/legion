package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// followHead is the per-issue read's check of pr's head against pull, GitHub's own read of it.
// The daemon otherwise moves a recorded head only on the pull_request synchronize webhook, and a
// webhook lost, or waiting behind hours of the GitHub
// consumer's backlog, leaves reviews and the CI verdict judged against an old head. When
// GitHub shows the pull request open at another head than the record holds now (the read looked
// its record up before it asked GitHub, and a webhook may have moved it since), it applies the
// intake.PullRequestSynchronized that webhook would have carried, with GitHub's updated_at as its
// clock, so the workflow decides it as it decides the webhook's (workflow's
// pullRequestSynchronized), behind the same fence: a read older than the record's clock moves
// nothing, and a webhook arriving later that is older than this read moves nothing either. The
// fence orders by GitHub's whole seconds, so a clock equal to the record's is not late. A record a
// webhook moved since the read looked it up is kept when this read is no newer than its clock: the
// read may predate that push within the same second, and following it would move the head back. A
// webhook of the same second arriving later runs as a redelivery does, and one applied between this
// function's read of the record and its apply is still undone by it - the record moves back to the
// head read here, judging it again, until the next read of the issue moves it forward.
//
// Before the synchronize it applies, oldest first, the pushes that took the branch to the new head,
// as their push webhooks would have carried them (headPushes): those webhooks wait in the same
// backlog, or were lost, and the workflow judges a move by the chain of its pushes
// (classify.AdvancePullRequestHead). When a recorded push already ends at the new head it applies
// none, and when no recorded push does they are the pushes GitHub's activity of the branch shows
// (lostPushes); a push that activity cannot be followed for is left to its webhook, as when a
// synchronize beats its push: a head with no known push is judged as such, and the push's own
// handler settles what only the push says, replaying the head run the head opened
// (classify.ApplyPush).
//
// It returns the record as the read goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability and review reads after it judge the
// head GitHub shows. On an error, or a record no longer open, it returns the record as it last
// read it, and the reads after it judge the pull request at that head; the next read of the issue
// follows it.
func (w *workflowRuntime) followHead(ctx context.Context, github githubrest.Client, pr record.PullRequest, pull githubPullRequest) (record.PullRequest, error) {
	if pull.State != "open" || pull.Head.SHA == "" || pull.Head.SHA == pr.HeadSHA {
		return pr, nil
	}
	current, err := w.openRecord(ctx, pr)
	if err != nil || current.HeadSHA == pull.Head.SHA {
		return current, err
	}
	if current.HeadSHA != pr.HeadSHA && !pull.UpdatedAt.After(current.HeadUpdatedAt) {
		return current, nil
	}
	pr = current
	pushes, err := w.headPushes(ctx, github, pr, pull.Head.SHA)
	if err != nil {
		return pr, err
	}
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt, "pushes", len(pushes))
	for _, push := range pushes {
		if _, err := intake.ApplyFact(ctx, w.pool, "github", push.eventID, push.fact, w.handlers...); err != nil {
			return pr, fmt.Errorf("apply the push to %s: %w", push.fact.After, err)
		}
	}
	eventID := fmt.Sprintf("head-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	fact := intake.PullRequestSynchronized{Repo: pr.Repo, Number: pr.Number, Branch: pr.Branch, HeadSHA: pull.Head.SHA, Body: pull.Body, UpdatedAt: pull.UpdatedAt}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return pr, fmt.Errorf("apply the synchronize to %s: %w", pull.Head.SHA, err)
	}
	return w.openRecord(ctx, pr)
}

// openRecord is pr's issue's pull request record as it stands now, which must still be pr's and
// open.
func (w *workflowRuntime) openRecord(ctx context.Context, pr record.PullRequest) (record.PullRequest, error) {
	var current *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		current, err = w.records.PullRequest(ctx, tx, pr.Issue)
		return err
	}); err != nil {
		return pr, fmt.Errorf("read the pull request record of %s: %w", pr.Issue, err)
	}
	if current == nil || current.State != record.PullRequestOpen || current.Number != pr.Number {
		return pr, fmt.Errorf("%s records pull request #%d open no longer", pr.Issue, pr.Number)
	}
	return *current, nil
}

// applyMergeability applies what GitHub's read of a pull request (pull, one GET /pulls/{number})
// says about merging its head into its base, for the per-issue read (readIssuePullRequest). Every
// read that GitHub answers applies an intake.PullRequestMergeability fact: GitHub's lazily
// computed mergeability, its own merge state for the head, the base it names - this read is the
// one that records the base, both for a pull request whose record has none and as the answer to a
// push onto its base, which GitHub recomputed the mergeability for and sent no event about - and
// the moment the answer was in hand (intake.PullRequestMergeability.ReadAt).
//
// A read that only confirms what is recorded applies the fact too, and then the read time is the
// only thing that moves, which is the point of applying it: a head read a minute ago and found
// `blocked` must read differently from a head nothing has read for a day, and the stamp
// is the only thing that tells them apart (record.PullRequest.StateReadAt). readAt is the moment
// the caller had GitHub's answer in hand, read by the caller straight after readPullRequest
// returned and before anything else it does with the answer: the per-issue read follows a head
// GitHub shows before it applies this (followHead), which makes more GitHub calls and waits on the
// fact lock, so a clock read here would date the answer to the end of that follow and make it
// look fresher than it is, which is the one thing the stamp exists not to do. It is spent twice,
// on the fact's ReadAt and on the event id, so the stamp recorded and the id the fact is applied
// under come from one reading rather than two. The confirming read is
// needed for its own sake as well: workflow's mergeability decides a CONFLICTING read every time
// it sees one, so a conflict recorded while the issue stood in a phase the conflict trigger has no
// row for is acted on once the issue reaches one that has, with no change in GitHub's answer to
// key off.
//
// What a confirming read costs is one fact and one round settlement each time: the merge-state
// ladder makes at most five reads per wait (mergeStateLadderReads), the review-stuck notice such a
// settlement would write is deduplicated by the round's own mark
// (record.ReviewDecision.ToldStuckHead), and a settlement that finds the round mergeable advances
// the issue, which is the right answer whenever it is true.
//
// The fact names the head GitHub answered for (`head.sha` of the same answer), which the engine
// records the verdict against only while the record still stands at it: this read and the merge
// state ladder's (daemon's readMergeState) both run outside the record's transaction, so a push
// can land between GitHub's answer and the apply.
// An apply that fails is logged, and the next read of the pull request applies it again.
func (w *workflowRuntime) applyMergeability(ctx context.Context, pr record.PullRequest, pull githubPullRequest, readAt time.Time) {
	base, mergeable, state := pull.Base.Ref, pull.mergeability(), pull.mergeState()
	eventID := fmt.Sprintf("mergeability:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, readAt.UnixNano())
	fact := intake.PullRequestMergeability{Repo: pr.Repo, Number: pr.Number, Base: base, HeadSHA: pull.Head.SHA,
		Mergeable: mergeable, MergeState: state, ReadAt: readAt}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply whether a pull request's head can be merged into its base", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
	}
}

// readPush is a push the per-issue read rebuilt from its reads of GitHub, with the event id it is
// applied under.
type readPush struct {
	eventID string
	fact    intake.Push
}

// headPushes is the pushes followHead applies, oldest first, before the synchronize that moves pr
// from its recorded head to head. When a recorded push already ends at head
// (classify.HeadPushKnown) there are none: the record knows the move. When none does, its webhook
// lost or waiting behind the GitHub consumer's backlog, they are the pushes GitHub's activity of
// the branch shows took it there (lostPushes); when that activity cannot be followed, which it
// logs, there are none either, and the pushes are left to their webhooks. A rate-limit answer, or
// ctx ending, is an error.
func (w *workflowRuntime) headPushes(ctx context.Context, github githubrest.Client, pr record.PullRequest, head string) ([]readPush, error) {
	if classify.HeadPushKnown(pr, head) {
		return nil, nil
	}
	pushes, unfollowed, err := w.lostPushes(ctx, github, pr, head)
	if err != nil || unfollowed == "" {
		return pushes, err
	}
	w.log.Warn("workflow: the read cannot rebuild from GitHub's activity the pushes that moved a pull request's head; it applies none of them, and the synchronize",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "branch", pr.Branch, "recorded", pr.HeadSHA, "head", head, "reason", unfollowed)
	return nil, nil
}

// lostPushes is the pushes that took pr's branch from the recorded head to head, oldest first, as
// their push webhooks would have carried them, less those the record holds already (its pushes and
// the moves it made): GitHub's activity of the branch (githubrest.Client.BranchActivity, its newest
// hundred entries) followed back from head to the recorded head (followActivity). Each is GitHub's
// push, force push or branch creation, with Before and After its two commits and Pusher its
// actor's login - an App's <slug>[bot], the form of the push webhook's pusher.name, by which the
// workflow tells the review App's pushes. Repo and Branch are the record's, the names the webhook's
// push carries. The workflow judges a move by the chain of its pushes, so one push from the
// recorded head to head would judge it wrong. Each push's event id names the project, the pull
// request and the push's two commits (pushReadEventID), so a later read skips it before any
// transaction, and the push webhook arriving later is the same push recorded again, which the
// record keeps as its move's push or a repeat of it and which changes no judgement
// (classify.ApplyPush).
//
// unfollowed says why the pushes cannot be rebuilt, and then there are none: the page does not
// lead back from head to the recorded head, an entry on the way is no push, or GitHub answered the
// read with an error. A rate-limit answer, or ctx ending, is an error: the read ends there.
func (w *workflowRuntime) lostPushes(ctx context.Context, github githubrest.Client, pr record.PullRequest, head string) ([]readPush, string, error) {
	read, cancel := context.WithTimeout(ctx, issueRead)
	activity, err := github.BranchActivity(read, pr.Branch)
	cancel()
	if err != nil {
		if endsRead(ctx, err) {
			return nil, "", fmt.Errorf("read the activity of %s: %w", pr.Branch, err)
		}
		return nil, fmt.Sprintf("the read of the branch's activity failed: %v", err), nil
	}
	chain, unfollowed := followActivity(activity, pr.HeadSHA, head)
	if unfollowed != "" {
		return nil, unfollowed, nil
	}
	recorded := map[[2]string]bool{}
	for _, p := range pr.Pushes {
		recorded[[2]string{p.Before, p.SHA}] = true
	}
	for _, m := range pr.Moves {
		recorded[[2]string{m.Before, m.SHA}] = true
	}
	var pushes []readPush
	for _, entry := range chain {
		if recorded[[2]string{entry.Before, entry.After}] {
			continue
		}
		pushes = append(pushes, readPush{
			eventID: w.pushReadEventID(pr, entry.Before, entry.After),
			fact:    intake.Push{Repo: pr.Repo, Branch: pr.Branch, Before: entry.Before, After: entry.After, Pusher: entry.Actor.Login},
		})
	}
	return pushes, "", nil
}

// endsRead says whether err, a read of GitHub that failed, ends the whole per-issue read: GitHub's
// rate limit, which every later read on the installation meets, or ctx ending.
func endsRead(ctx context.Context, err error) bool {
	var answer *githubrest.Answer
	return ctx.Err() != nil || (errors.As(err, &answer) && answer.RateLimited)
}

// pushActivity is the kinds of GitHub's activity of a branch that are its pushes, whose push
// webhook carries the same two commits: a push, a force push, and the push that created the branch.
var pushActivity = map[string]bool{"push": true, "force_push": true, "branch_creation": true}

// followActivity is the entries of activity, GitHub's activity of a branch newest first, that took
// the branch from from to to, oldest first: from the newest entry that left the branch at to, each
// older entry in turn, each of which left the branch at the commit the newer one replaced, until one
// replaced from. Every entry on the way must be a push (pushActivity). unfollowed says why the page
// holds no such chain: no entry left the branch at to, the entries break off, one is no push, or the
// page ends before from.
func followActivity(activity []githubrest.Activity, from, to string) ([]githubrest.Activity, string) {
	start := slices.IndexFunc(activity, func(entry githubrest.Activity) bool { return entry.After == to })
	if start < 0 {
		return nil, fmt.Sprintf("none of the %d newest entries of the branch's activity left it at %s", len(activity), to)
	}
	var chain []githubrest.Activity
	at := to
	for _, entry := range activity[start:] {
		switch {
		case entry.After != at:
			return nil, fmt.Sprintf("the branch's activity before it reached %s left it at %s", at, entry.After)
		case !pushActivity[entry.Type]:
			return nil, fmt.Sprintf("the branch's activity reached %s by a %s, not a push", at, entry.Type)
		}
		chain = append(chain, entry)
		if entry.Before == from {
			slices.Reverse(chain)
			return chain, ""
		}
		at = entry.Before
	}
	return nil, fmt.Sprintf("the %d newest entries of the branch's activity do not lead back from %s to the recorded head %s", len(activity), to, from)
}

// pushReadEventID is the event id of a push from before to after on pr's branch that a read of
// GitHub rebuilt (readEventID): it names the two commits and nothing of the read that rebuilt
// them, so the same push rebuilt by a later read is one fact.
func (w *workflowRuntime) pushReadEventID(pr record.PullRequest, before, after string) string {
	return readEventID("push-read", w.dispatchProject, pr.Repo, pr.Number, before+"..."+after)
}

// heardSpelling is repo as GitHub intake hears it, the spelling a webhook's fact names it by and a
// pull request is recorded and looked up under: a served repository as the mapping names it, and
// any other as its record keeps GitHub's spelling (heardRepositories).
func (w *workflowRuntime) heardSpelling(repo string) string {
	if w.repositories != nil {
		for _, served := range w.repositories.Accepted() {
			if strings.EqualFold(served.String(), repo) {
				return served.String()
			}
		}
	}
	return repo
}

// openPullRequestOn is the open pull request of repo on branch (GET
// /pulls?head=<owner>:<branch>&state=open), and whether there is one. The list's filter is not
// taken on trust: a listed pull request counts only when its head.ref is branch, the same identity
// the webhook path reads (intake's decodePullRequest takes head_ref alone), whichever repository
// the head lives in. The workflow keeps one pull request per branch and assumes one is open at a
// time, so of two it takes the first GitHub lists.
//
// The repository GitHub serves the recorded one under is what this reads, addresses and filters
// on (repositories.serving): after a transfer the record keeps the old name, and the head filter
// names an owner, so a listing made under the recorded name would ask the wrong installation for
// heads of an owner that holds none and find no pull request at all — which the caller reports as
// a pull request that has gone missing.
func (w *workflowRuntime) openPullRequestOn(ctx context.Context, repo, branch string) (githubPullRequest, bool, error) {
	recorded, err := ghrepo.Parse("the issue's repository", repo)
	if err != nil {
		return githubPullRequest{}, false, err
	}
	repository := w.repositories.serving(recorded)
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repository)
	if err != nil {
		return githubPullRequest{}, false, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	var pulls []githubPullRequest
	query := url.Values{"head": {repository.Owner() + ":" + branch}, "state": {"open"}}
	if err := github.Get(ctx, "/pulls?"+query.Encode(), &pulls); err != nil {
		return githubPullRequest{}, false, err
	}
	for _, pull := range pulls {
		if pull.Head.Ref == branch {
			return pull, true, nil
		}
	}
	return githubPullRequest{}, false, nil
}
