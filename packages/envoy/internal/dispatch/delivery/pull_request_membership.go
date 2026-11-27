package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// comparisonPage is how many commits fetchComparison asks GitHub for on each page of a comparison,
// GitHub's own maximum: a page that holds fewer is the last.
const comparisonPage = 100

// comparisonPageLimit bounds the body of one page of a comparison of two commits, in place of the
// client's generic megabyte: GitHub sends the comparison's changed files, up to 300 with their
// patches, on its first page, so the first page of a pull request that changes many files is far
// past a megabyte. Sixteen MiB is the bound githubapp gives the polled reads of a head's check
// runs, whose full page is about 13 MB. A page past it fails the read, which leaves its commits
// unknown.
const comparisonPageLimit = 16 << 20

// CommitMembership is what PullRequestMembership could tell about each commit it was asked about:
// Members are the commits of the pull request at the two revisions it was asked about, Outside the
// commits that are not, and Unknown each commit whose answer a read that failed withheld. Stale is
// whether GitHub's read of the pull request names another head or base revision than those two:
// GitHub has moved the pull request on where no write has stored it yet, and every commit is then
// unknown. Unreadable is whether GitHub cannot answer for the pull request at all and no retry
// changes that: the deployment has no App key, the App is installed on no installation covering
// the repository, or GitHub refuses the installation's token the repository (MembershipUnreadable);
// every commit is then unknown too. Complete is whether GitHub's comparison of the two revisions
// listed every commit between them; Pages, PullReads, Compares and CommitReads count the reads the
// answer cost: the pages of that comparison, the read of the pull request, the comparisons of one
// commit and the reads of a commit.
type CommitMembership struct {
	Members, Outside, Unknown               []string
	Stale, Unreadable, Complete             bool
	Pages, PullReads, Compares, CommitReads int
}

// MembershipUnreadable reports whether err, a failure of the token mint or of the read of the pull
// request itself, is a cause no retry fixes: no App key (githubapp.ErrNoAppKey), no installation
// covering the repository, or a token GitHub refuses the repository (RepositoryUnreadable). A 404 of
// the pull request is none of these: GitHub answers it for a pull request it has not caught up
// with as well as for one that is gone.
func MembershipUnreadable(err error) bool {
	return errors.Is(err, githubapp.ErrNoAppKey) || RepositoryUnreadable(err)
}

// PullRequestMembership tells, for each of candidates, whether it is a commit of pull request
// number of repoFull standing at head onto the base revision base, the two revisions Dispatch
// stores for it: a commit head holds and base does not, the set `git log base..head`. The check
// states settler asks it about the candidate former heads of an open pull request
// (api/check_states_former_heads.go, verifyFormerHeads).
//
// Every answer comes of reads that name those two commits, so it is about them by construction,
// whatever the pull request stands at while it is read: GitHub's comparison of base and head
// (GET /repos/{owner}/{repo}/compare/{base}...{head}, fetchComparison), which lists the commits
// head holds and base does not, oldest first, in pages of comparisonPage, at most
// maxCommitPages of them, and names how many it holds in all. A commit the comparison lists is a
// member. Where the pages list every commit it holds, a commit they do not name is outside, and so
// is every commit where the comparison of the two holds none. Where they list fewer, a commit they
// do not name is decided by two comparisons of its own, with head and then with base
// (githubapp.Client.CommitOnBranch, at the two commits): a commit head does not hold is outside,
// and one head holds is outside where base holds it too and a member where it does not. A
// comparison GitHub answers 403 or 404 for says nothing alone, since 404 is also its answer for a
// commit the repository does not have: the commit is then read (githubapp.Client.HasCommit), and
// only GitHub's `No commit found for SHA` puts it outside. No read by the pull request's number or
// by a branch's name decides a commit.
//
// The read of the pull request (GET /repos/{owner}/{repo}/pulls/{number}) comes first and only
// tells whether GitHub has moved the pull request on from those two revisions: where its head.sha
// or base.sha is another, the answer is Stale and every commit unknown, so the settler keeps the
// obligation that asked for the read and reads again once a write stores where the pull request
// went. A read of the pull request that fails or names no head or base revision, and a head or base
// Dispatch stores none of, leave every commit unknown too, and a token mint or a read of the pull
// request that fails for a cause no retry fixes (MembershipUnreadable) is Unreadable besides. A
// move after that read changes no answer, since the comparisons name their commits.
//
// Every other failure of a read leaves each commit that read would have decided in Unknown, and
// the first such failure is the error; the commits other reads decided keep their answers. A
// failed page of the comparison of base and head leaves every commit unknown.
func PullRequestMembership(ctx context.Context, client *githubapp.Client, repoFull string, number int, head, base string, candidates []string) (CommitMembership, error) {
	var answer CommitMembership
	unknown := func(err error) (CommitMembership, error) {
		answer.Unknown = append([]string(nil), candidates...)
		return answer, err
	}
	owner, repo, err := refs.SplitGitHubRepo(repoFull)
	if err != nil {
		return unknown(err)
	}
	if head == "" || base == "" {
		return unknown(fmt.Errorf("%s#%d names no stored head or base revision to compare", repoFull, number))
	}
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	current, err := token()
	if err != nil {
		answer.Unreadable = MembershipUnreadable(err)
		return unknown(fmt.Errorf("%w for %s: %w", ErrTokenMint, repoFull, err))
	}
	answer.PullReads++
	pull, err := fetchPullRequestPayload(ctx, client, current, owner, repo, number)
	if err != nil {
		answer.Unreadable = MembershipUnreadable(err)
		return unknown(err)
	}
	if pull.Head.SHA == "" || pull.Base.SHA == "" {
		return unknown(fmt.Errorf("the read of %s#%d names no head or base revision", repoFull, number))
	}
	if pull.Head.SHA != head || pull.Base.SHA != base {
		answer.Stale = true
		return unknown(nil)
	}
	commits, total, pages, err := fetchComparison(ctx, client, token, owner, repo, base, head)
	answer.Pages = pages
	if err != nil {
		return unknown(err)
	}
	listed := make(map[string]bool, len(commits))
	for _, sha := range commits {
		listed[sha] = true
	}
	answer.Complete = len(commits) >= total
	var unlisted []string
	for _, sha := range candidates {
		switch {
		case listed[sha]:
			answer.Members = append(answer.Members, sha)
		case answer.Complete:
			answer.Outside = append(answer.Outside, sha)
		default:
			unlisted = append(unlisted, sha)
		}
	}
	var failed error
	for _, sha := range unlisted {
		answer.Compares++
		onHead, err := client.CommitOnBranch(ctx, current, owner, repo, head, sha)
		if err != nil && errors.Is(err, githubapp.ErrRepositoryUnreadable) {
			answer.CommitReads++
			if held, heldErr := client.HasCommit(ctx, current, owner, repo, sha); heldErr == nil && !held {
				answer.Outside = append(answer.Outside, sha)
				continue
			}
		}
		if err != nil {
			answer.Unknown = append(answer.Unknown, sha)
			failed = cmp.Or(failed, fmt.Errorf("compare %s with the head %s of %s#%d: %w", sha, head, repoFull, number, err))
			continue
		}
		if !onHead {
			answer.Outside = append(answer.Outside, sha)
			continue
		}
		answer.Compares++
		onBase, err := client.CommitOnBranch(ctx, current, owner, repo, base, sha)
		switch {
		case err != nil:
			answer.Unknown = append(answer.Unknown, sha)
			failed = cmp.Or(failed, fmt.Errorf("compare %s with the base revision %s of %s#%d: %w", sha, base, repoFull, number, err))
		case onBase:
			answer.Outside = append(answer.Outside, sha)
		default:
			answer.Members = append(answer.Members, sha)
		}
	}
	return answer, failed
}

// comparisonPayload is one page of GET /repos/{owner}/{repo}/compare/{base}...{head}, limited to
// the fields fetchComparison reads: how many commits the comparison holds in all (total_commits),
// which GitHub names on every page, and the sha of each commit of the page.
type comparisonPayload struct {
	TotalCommits *int `json:"total_commits"`
	Commits      []struct {
		SHA string `json:"sha"`
	} `json:"commits"`
}

// fetchComparison reads GitHub's comparison of the commits base and head: the commits head holds
// and base does not, oldest first, pages of comparisonPage until it has read as many as the
// comparison holds or a page that is not full, at most maxCommitPages, asking token for each page's
// installation token. Each page is read within comparisonPageLimit and asked again as
// readGitHubPage asks a page again. It answers the commits of the pages it read, how many the
// comparison holds in all, and how many pages it asked for, the failed one included. A page that
// names no total, or another total than the page before, fails the read: every page is of the same
// two commits.
func fetchComparison(ctx context.Context, client *githubapp.Client, token func() (string, error), owner, repo, base, head string) ([]string, int, int, error) {
	var commits []string
	total := -1
	for page := 1; page <= maxCommitPages; page++ {
		current, err := token()
		if err != nil {
			return commits, total, page - 1, fmt.Errorf("mint installation token for %s/%s compare %s...%s page %d: %w", owner, repo, base, head, page, err)
		}
		comparePath := fmt.Sprintf("/repos/%s/%s/compare/%s...%s?per_page=%d&page=%d", url.PathEscape(owner), url.PathEscape(repo),
			url.PathEscape(base), url.PathEscape(head), comparisonPage, page)
		body, status, header, err := readGitHubPageWithin(ctx, client, current, comparePath, comparisonPageLimit)
		if err != nil {
			return commits, total, page, fmt.Errorf("compare %s...%s of %s/%s page %d: %w", base, head, owner, repo, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return commits, total, page, fmt.Errorf("compare %s...%s of %s/%s page %d: %w", base, head, owner, repo, page, err)
		}
		var read comparisonPayload
		if err := json.Unmarshal(body, &read); err != nil {
			return commits, total, page, fmt.Errorf("decode the comparison %s...%s of %s/%s page %d: %w", base, head, owner, repo, page, err)
		}
		if read.TotalCommits == nil || (total >= 0 && *read.TotalCommits != total) {
			return commits, total, page, fmt.Errorf("the comparison %s...%s of %s/%s names no total of its commits, or another total, on page %d", base, head, owner, repo, page)
		}
		total = *read.TotalCommits
		for _, commit := range read.Commits {
			commits = append(commits, commit.SHA)
		}
		if len(commits) >= total || len(read.Commits) < comparisonPage {
			return commits, total, page, nil
		}
	}
	return commits, total, maxCommitPages, nil
}
