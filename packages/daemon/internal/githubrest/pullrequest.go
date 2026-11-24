package githubrest

import (
	"context"
	"net/url"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// PullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in GET
// /pulls: its number, state (open or closed), page, body, base branch, head branch, commit and the
// repository the head lives in (nil when GitHub no longer has it, a deleted fork), when it last
// changed, whether and when it merged and its merge commit, and its nullable, lazily computed
// `mergeable`, which the list leaves out, as it leaves out `merged` (merged_at says it there).
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Mergeable      *bool      `json:"mergeable"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
}

// IsMerged says whether GitHub shows the pull request merged: `merged` on GET /pulls/{number}, or
// a `merged_at`, which the list carries too.
func (p PullRequest) IsMerged() bool {
	return p.Merged || p.MergedAt != nil
}

// HeadIn says whether the pull request's head lives in repository (owner/name, in any case, or
// the other name of a configured rename, renames): a branch of the repository itself, not of a
// fork, whose branch may carry any name.
func (p PullRequest) HeadIn(repository string, renames ghrepo.Renames) bool {
	return p.Head.Repo != nil && renames.Same(p.Head.Repo.FullName, repository)
}

// HeadRepository is the repository the pull request's head lives in, as GitHub names it
// (head.repo.full_name), and "" when GitHub no longer has it (a deleted fork).
func (p PullRequest) HeadRepository() string {
	if p.Head.Repo == nil {
		return ""
	}
	return p.Head.Repo.FullName
}

// OpenPullRequestOn is the open pull request on branch of the client's repository, repository
// (GET /pulls?head=<owner>:<branch>&state=open), and whether there is one. The list's filter
// is not taken on trust: a listed pull request counts only when its head is branch of repository
// itself (head.ref is branch, and HeadIn), as the webhook path counts one (intake's
// decodePullRequest reads head_ref and head_repo). The filter names an owner only, and the owner of
// a repository can hold a fork of it, under another name, whose branches may carry any name, an
// issue's or a task's among them; a fork GitHub no longer has names no repository at all. The
// workflow keeps one pull request per branch and assumes one is open at a time, so of two it takes
// the first GitHub lists.
func (c Client) OpenPullRequestOn(ctx context.Context, repository ghrepo.Repository, branch string) (PullRequest, bool, error) {
	var pulls []PullRequest
	query := url.Values{"head": {repository.Owner() + ":" + branch}, "state": {"open"}}
	if err := c.Get(ctx, "/pulls?"+query.Encode(), &pulls); err != nil {
		return PullRequest{}, false, err
	}
	for _, pull := range pulls {
		if pull.Head.Ref == branch && pull.HeadIn(repository.String(), ghrepo.Renames{}) {
			return pull, true, nil
		}
	}
	return PullRequest{}, false, nil
}
