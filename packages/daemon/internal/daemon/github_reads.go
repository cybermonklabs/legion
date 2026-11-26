package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// logReadFailure logs err, a GitHub read of the per-issue read that failed, at error level with
// attributes, then err and GitHub's HTTP status when err carries one: as limited when the answer
// was GitHub's rate limit, and as failed otherwise. It returns true for the rate limit: every
// further read on the same installation meets the same limit, and the panes share it, so the
// caller must stop the whole read there.
func (w *workflowRuntime) logReadFailure(err error, limited, failed string, attributes ...any) bool {
	attributes = append(attributes, "error", err)
	var answer *githubrest.Answer
	if errors.As(err, &answer) {
		attributes = append(attributes, "status", answer.Status)
	}
	if answer != nil && answer.RateLimited {
		w.log.Error(limited, attributes...)
		return true
	}
	w.log.Error(failed, attributes...)
	return false
}

// readEventID is the event id, under source "github", of a fact a read of GitHub builds about
// pull request number of repo: kind, the Dispatch project, the pull request, and observed, what
// the read saw - a review's id, GitHub's submitted_at, the two heads of a compare. It holds no
// read time, no boot and nothing of who made the read, so one observation is one fact however
// many reads, boots, daemons or outbox rows see it, and unapplied finds it applied before any
// transaction. The per-issue read and the outbox row that make the same read of GitHub share
// this id, which is what keeps them from applying one observation twice.
func readEventID(kind, project, repo string, number int, observed ...string) string {
	return fmt.Sprintf("%s:%s:%s#%d:%s", kind, project, repo, number, strings.Join(observed, ":"))
}

// clock is GitHub's time t as an event id spells it.
func clock(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// unapplied says whether processed_events holds no eventID under source "github"
// (intake.Applied), read with no transaction and no fact lock: a fact an earlier read, another
// boot or a webhook applied already costs that one read and no wait behind the GitHub consumer's
// facts. A failed read is logged and says false, leaving the fact to a later read, as a failed
// apply would.
func (w *workflowRuntime) unapplied(ctx context.Context, eventID string) bool {
	applied, err := intake.Applied(ctx, w.pool, "github", []string{eventID})
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("read whether a fact read from GitHub is applied; a later read applies it", "event", eventID, "error", err)
		}
		return false
	}
	return !applied[eventID]
}

// githubPullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in
// GET /pulls: its number, state (open or closed), page, body, base branch and the base commit the
// answer names (`base.sha`, which the mergeability fact carries for the log of the read,
// intake.PullRequestMergeability.BaseSHA), head branch and commit, when it last changed, its
// nullable, lazily computed `mergeable`, which the list leaves out, and `mergeable_state`,
// GitHub's own merge state for the head, which the list leaves out too. Merged
// and MergeCommitSHA are whether GitHub merged it and the commit the merge made, which the
// merge-queue read applies as the merge (outbox's mergeQueueRead): GitHub answers a merged pull
// request `closed`, so State alone cannot tell a merge from a close. The list leaves Merged out,
// and only the read of one pull request names it. RequestedReviewers and RequestedTeams are the
// review requests GitHub holds open on the pull request, each one person, App or team asked for a
// review not given yet (reviewsRequested); a list GitHub left out or answered null decodes to nil.
// batch is set only on a pull request a base's GraphQL batch answered (graphQLPull), which counts
// the requests and reads the head's rollup in its own query, so neither is rebuilt into the REST
// lists.
type githubPullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Base    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Mergeable          *bool      `json:"mergeable"`
	MergeableState     string     `json:"mergeable_state"`
	Merged             bool       `json:"merged"`
	MergeCommitSHA     string     `json:"merge_commit_sha"`
	RequestedReviewers []struct{} `json:"requested_reviewers"`
	RequestedTeams     []struct{} `json:"requested_teams"`
	batch              *batchRead
}

// batchRead is what a base's GraphQL batch read of a pull request beside its merge state: the
// count of its open review requests, nil where GitHub answered none, and its head's rollup.
type batchRead struct {
	reviews *int
	checks  record.HeadChecks
}

// reviewsRequested is how many review requests GitHub holds open on the pull request, people and
// Apps and teams alike, which the record keeps beside the merge state
// (record.PullRequest.ReviewsRequested): the batch's own count, or the two REST lists counted, and
// nil where the answer held neither list, which the round reads as no count at all.
func (p githubPullRequest) reviewsRequested() *int {
	if p.batch != nil {
		return p.batch.reviews
	}
	if p.RequestedReviewers == nil && p.RequestedTeams == nil {
		return nil
	}
	count := len(p.RequestedReviewers) + len(p.RequestedTeams)
	return &count
}

// mergeability is the pull request's `mergeable` as the mergeability fact reads it: null
// is record.MergeabilityUnknown, GitHub not having computed it yet; true is
// record.MergeabilityMergeable; false is record.MergeabilityConflicting, a head that conflicts
// with its base, which a second read confirms before the workflow decides whether to merge the
// base forward (workflow's mergeability), and which the read carries the base's rules for
// (applyMergeability).
func (p githubPullRequest) mergeability() record.Mergeability {
	switch {
	case p.Mergeable == nil:
		return record.MergeabilityUnknown
	case *p.Mergeable:
		return record.MergeabilityMergeable
	default:
		return record.MergeabilityConflicting
	}
}

// mergeState is the pull request's `mergeable_state` as the record keeps it: GitHub's own merge
// state for the head, in GitHub's spelling, which is what an approval standing on that head
// waits for (workflow's reviewRound).
// An answer with no `mergeable_state` at all - a list entry, which leaves it out - is
// record.MergeStateUnread, since the read found none, and a spelling outside the eight GitHub
// documents is record.MergeStateUnknown, the one state that already means "GitHub has not said
// this head can be merged": the record's values are then exactly the set the state view and the
// contract name, and an unrecognised state is never read as one the issue may advance on
// (record.MergeState.Mergeable).
func (p githubPullRequest) mergeState() record.MergeState {
	switch state := record.MergeState(p.MergeableState); state {
	case record.MergeStateUnread, record.MergeStateClean, record.MergeStateUnstable, record.MergeStateHasHooks,
		record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft:
		return state
	default:
		return record.MergeStateUnknown
	}
}

// readPullRequest reads pr from one GET /pulls/{number}: the base the mergeability is recorded
// against, that mergeability itself, and the head and its clock followHead compares with the
// record, in the single read they share. It also returns the githubrest.Client its own token mint
// built, so the per-issue read's further reads of the same pull request reuse it rather than
// minting a second token.
//
// The read is conditional (githubrest.Client.ETags, the runtime's etags): it carries the ETag of
// GitHub's last answer for the same pull request, and GitHub's 304, which costs the installation's
// rate limit nothing, answers that same pull request again. The caller applies it as the answer it
// is, so a 304 leaves the record holding what it held and only moves the time it was read
// (applyMergeability). The client it returns sends its further reads whole.
//
// The read is made of the repository GitHub serves the recorded one under (repositories.serving),
// as every GitHub call the daemon makes for a recorded tree is: a record keeps the name it was
// written under until this release's one-time rename pass writes a pair's `to` over it
// (renamePass), and after a transfer GitHub holds the repository under the other name, on the
// other owner's installation. Read under the recorded name, this would mint on the installation
// the repository has left and address a repository GitHub no longer answers for.
func (w *workflowRuntime) readPullRequest(ctx context.Context, pr record.PullRequest) (githubrest.Client, githubPullRequest, error) {
	recorded, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	repository := w.repositories.serving(recorded)
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repository)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	var pull githubPullRequest
	if err := github.WithETags(w.etags).Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	return github, pull, nil
}

// etagBytes bounds the answer bodies the runtime keeps for its conditional reads (etags): about a
// thousand pull requests of the size GitHub usually answers, and every base's rules beside them.
const etagBytes = 16 << 20

// implementClient is the client for repository's REST API on the implement App's installation
// token for that repository's owner account: the App every read of an issue's own repository is
// made as, the per-issue reads' and the outbox rows' alike. The token names no repository, so one
// mint serves every repository of the owner the App is installed on. The mint failure is returned
// bare, so each caller says in its own words what it was minting the token for. Its Scope
// (implementScope) names that App, its write permissions and the owner, which the conditional
// reads key what they keep by (githubrest.Client.Scope).
func implementClient(ctx context.Context, tokens appauth.Tokens, githubAPI string, repository ghrepo.Repository) (githubrest.Client, error) {
	lease, err := tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		return githubrest.Client{}, err
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(githubAPI, repository), Scope: implementScope(repository)}, nil
}

// implementScope is the Scope of the implement App's client of repository (implementClient): the
// App, its write permissions and repository's owner, with no secret, so it is known before a token
// is minted, as the installation's GraphQL gate is found by it (queryChunk).
func implementScope(repository ghrepo.Repository) string {
	return "implement write " + strings.ToLower(repository.Owner())
}
