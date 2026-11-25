package delivery

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkedPullRequestTerms are the `with recursive` terms of the pull requests the external links
// of the issue $1 name in a repository the issue does not resolve to: linked_pulls (repo,
// number, url), one row per link of the form https://github.com/<owner>/<repo>/pull/<n>, the
// repository lowercased, as refs.ExternalRepo spells an issue's own repository, and under the
// canonical name it is called today (refs.CanonicalRepoSQL over renames).
//
// Such a pull request is one of the issue's heads when the results are read, as a pull request
// attributed to the issue is (Attributor.HeadAttributedTo, Attributor.HeadsAttributedTo). An
// issue whose work spans two repositories links the pull request it has in the second one, and
// the attribution can never give that pull request to it: an issue is held to the repository it
// resolves to (attributionHeld), and the pull request is in another. So the link stands in for
// the attribution there, and only there. A link to a pull request of the repository the issue
// resolves to adds nothing, since the attribution, of which that link is the first source,
// alone decides which of that repository's pull requests are the issue's. The terms hold the
// issue to a repository by the attribution's own rule, over the same rename pairs, so the two
// cannot disagree about which repository is the issue's.
//
// The link names the pull request and not its head: the head is the stored row's, matched by the
// repository and the number, so a push that moves the head moves the basis with it, as it does
// for an attributed pull request. A pull request that closed without a merge is no head, by the
// state of its row, exactly as an attributed one that closed without a merge is no head.
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
//
// linked_pulls is materialized: it is the handful of pull requests one issue links, read once,
// so a join of it to the heads compares each head with a stored name and number, rather than
// evaluating the link's pattern and the canonical name's expression again for every head it
// meets, as it would for the laid heads of a read-only caller's read (answeredHeadsRelation).
func linkedPullRequestTerms(renames refs.Renames) string {
	return `subjects as (select $1::text as key), ` + refs.IssueRepoTerms + `,
	linked_pulls as materialized (
		select ` + refs.CanonicalRepoSQL(renames, "link.repo") + ` as repo, link.number, link.url
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
		where not ` + attributionHeld(renames, "link.repo") + `
	)`
}

// linkedHeadJoin joins each linked_pulls row, l, to the stored head row of the pull request it
// names, h: the same number in the same repository, compared without case, since a row written
// from a `pull_request` envelope carries GitHub's own spelling of the repository and a link is
// read lowercased. Every read of the linked heads joins them so. In a read of the results path,
// pull_request_heads is the relation its WITH list names (headsStatement), so the linked rows a
// read-only caller reads carry its request's answers as the attributed rows do.
const linkedHeadJoin = `join pull_request_heads h on lower(h.repo) = lower(l.repo) and h.number = l.number`

// LinkedPullRequest is one pull request an issue's external links name in a repository the
// issue does not resolve to (linkedPullRequestTerms).
type LinkedPullRequest struct {
	Repo   string
	Number int
}

// linkedPullRequestsQuery reads linkedPullRequestTerms in the order the links are listed in.
// NewAttributor builds it once, as it builds every statement of the rename pairs.
func linkedPullRequestsQuery(renames refs.Renames) string {
	return `with recursive ` + linkedPullRequestTerms(renames) + `
	select repo, number from linked_pulls order by url`
}

// LinkedPullRequests is every pull request the external links of issueKey name in a repository
// the issue does not resolve to, in the order the issue lists its links. The result route reads
// it to say which repositories a result of the issue may name a commit of (api/results_linked.go,
// refuseUnknownCommit), so the route and the results read take one list.
func (a *Attributor) LinkedPullRequests(ctx context.Context, q refs.Queryer, issueKey string) ([]LinkedPullRequest, error) {
	rows, err := q.Query(ctx, a.linkedQuery, issueKey)
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

// headAttributedQuery is HeadAttributedTo's statement: its first arm the attributed rows, read
// through pull_request_heads_issue_head, and its second the linked ones, read through
// pull_request_heads_repo_head (`lower(repo), head_sha`).
func headAttributedQuery(renames refs.Renames) string {
	return `with recursive ` + linkedPullRequestTerms(renames) + `
	select exists (
		select 1 from pull_request_heads
		where issue_key = $1 and head_sha = $2 and state in ('open', 'merged')
	) or exists (
		select 1 from linked_pulls l
		` + linkedHeadJoin + `
		where h.head_sha = $2 and h.state in ('open', 'merged')
	)`
}

// HeadAttributedTo reports whether sha is the head of an open or merged pull request that is
// issueKey's: attributed to it, or linked from it in a repository it does not resolve to
// (linkedPullRequestTerms). It is the test a `before_merge` result's commit passes to stand at
// one of the heads its check is compared with. A closed-unmerged pull request's head proves
// nothing. A pull request attributed to another issue never answers for this one; the results
// read asks this about the check's own issue, so a child's pull request never answers for a
// check above it (api/results_comparison.go's comparisonReader).
func (a *Attributor) HeadAttributedTo(ctx context.Context, pool *store.Pool, issueKey, sha string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, a.headQuery, issueKey, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the pull request heads of %s at %s: %w", issueKey, sha, err)
	}
	return found, nil
}

// mergeInRead lists linked merged pull requests over the same stored or request-local heads.
func mergeInRead(renames refs.Renames) headsStatement {
	return newHeadsStatement(linkedPullRequestTerms(renames), mergeInQuery, 2, "")
}

var mergeInQuery = `
	select ` + mergedColumns + ` from (
		select distinct h.repo, h.number, h.base_ref, h.head_sha, h.merge_commit_sha,
			h.merged_at, h.attribution_rule
		from linked_pulls l
		` + linkedHeadJoin + `
		where lower(l.repo) = lower($2) and h.state = 'merged'
	) merged
	order by ` + newestMergeFirst

