package delivery

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// linkedPullRequestTerms are the `with recursive` terms of the pull requests the external links
// of the issue $1 name in a repository the issue does not resolve to: linked_pulls (repo,
// number, url), one row per link of the form https://github.com/<owner>/<repo>/pull/<n>, the
// repository lowercased, as refs.ExternalRepo spells a mapping, and under the canonical name
// it is called today (refs.CanonicalRepoSQL).
//
// The record route reads them to say which repositories a result of the issue may name a commit
// of (LinkedPullRequests, api/results_linked.go's refuseUnknownCommit): an issue whose work spans
// two repositories links the pull request it has in the second one, and a result at that pull
// request's head names a commit of the second repository. They are read from the links
// themselves, since the route asks GitHub about each pull request they name, and Dispatch may hold
// no row of it yet. The results read takes the same pull request from the statement instead
// (HeadsAttributedTo, BaseBranchMergedIn): a link of a pull request's own URL gives that pull
// request to the issue in any repository (attributedTerms), so it is one of the issue's heads
// once Dispatch holds its row, and the timeline names the same issue for it. A link to a pull
// request of the repository the issue resolves to names nothing here, since that repository is
// asked about the commit before any link is read. The terms hold the issue to a repository by the
// attribution's own rule, so the two cannot disagree about which repository is the issue's.
//
// The owner and the name are held to the patterns of GitHub's names that refs reads a name with
// (refs.GitHubOwnerPattern, refs.GitHubNamePattern): an owner of a letter or digit and then up to
// 38 letters, digits, `-` and `_`, as a managed user's login (`mona-cat_octo`) is, and a name of
// letters, digits, `.`, `-` and `_`. A name of `.` or `..` alone is no repository: the reads
// build `/repos/{owner}/{repo}/...` from the two parts, and url.PathEscape keeps a dot, so a link
// naming either would send a read up the path (`/repos/../x/installation`) rather than to a
// repository. The issue PATCH takes any absolute HTTP(S) URL as a link (api/issue_refs.go,
// validateExternalURL), so such a URL is still a link of the issue and is simply no pull request
// here, as api's parseSourceRepo refuses the same two names for a source. The rest of the rule,
// the name's 100 characters and a name that is empty, `.` or `..` once a trailing `.git` is
// trimmed, is held where a read puts the name into a GitHub path (refs.SplitGitHubRepo).
//
// The number is bounded to nine digits so that the cast cannot fail on a link a person typed.
var linkedPullRequestTerms = `subjects as (select $1::text as key), ` + refs.IssueRepoTerms + `,
	linked_pulls as (
		select ` + refs.CanonicalRepoSQL("link.repo") + ` as repo, link.number, link.url
		from (
			select parsed.url, lower(parsed.part[1] || '/' || parsed.part[2]) as repo, parsed.part[3]::int as number
			from (
				select l.url, regexp_match(l.url, '^https://github\.com/(` + refs.GitHubOwnerPattern + `)/(` + refs.GitHubNamePattern + `)/pull/([1-9][0-9]{0,8})$') as part
				from issue_external_links l where l.issue_key = $1
			) parsed
			where parsed.part is not null and parsed.part[2] not in ('.', '..')
		) link
		join issues i on i.key = $1
		` + refs.IssueRepoJoin + `
		where not ` + attributionHeld("link.repo") + `
	)`

// LinkedPullRequest is one pull request an issue's external links name in a repository the
// issue does not resolve to (linkedPullRequestTerms).
type LinkedPullRequest struct {
	Repo   string
	Number int
}

// linkedPullRequestsQuery reads linkedPullRequestTerms in the order the links are listed in.
var linkedPullRequestsQuery = `with recursive ` + linkedPullRequestTerms + `
	select repo, number from linked_pulls order by url`

// LinkedPullRequests is every pull request the external links of issueKey name in a repository
// the issue does not resolve to, in the order the issue lists its links. The result route reads
// it to say which repositories a result of the issue may name a commit of (api/results_linked.go,
// refuseUnknownCommit).
func LinkedPullRequests(ctx context.Context, q refs.Queryer, issueKey string) ([]LinkedPullRequest, error) {
	rows, err := q.Query(ctx, linkedPullRequestsQuery, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
	}
	defer rows.Close()
	var linked []LinkedPullRequest
	for rows.Next() {
		var pull LinkedPullRequest
		if err := rows.Scan(&pull.Repo, &pull.Number); err != nil {
			return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
		}
		linked = append(linked, pull)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pull requests %s links: %w", issueKey, err)
	}
	return linked, nil
}
