package delivery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// linkedPullRequestTerms are the `with recursive` terms of the pull requests the external links
// of the issue $1 name in a repository the issue does not resolve to: linked_pulls (repo,
// number, url), one row per link of the form https://github.com/<owner>/<repo>/pull/<n>, the
// repository lowercased, as refs.ExternalRepo spells an issue's own repository, and under the
// canonical name it is called today (refs.CanonicalRepoSQL over renames).
//
// The record route reads them to say which repositories a result of the issue may name a commit
// of (Attributor.LinkedPullRequests, api/results_linked.go's refuseUnknownCommit): an issue whose
// work spans two repositories links the pull request it has in the second one, and a result at
// that pull request's head names a commit of the second repository. They are read from the links
// themselves, since the route asks GitHub about each pull request they name, and Dispatch may hold
// no row of it yet. The results read takes no head from them. A pull request is one of the issue's
// heads only where the attribution gives it the issue (Attributor.HeadAttributedTo,
// Attributor.HeadsAttributedTo): a link of a pull request's own URL gives it to the issue in any
// repository (attributionHeldFor), but not a pull request whose head is in a fork, which anyone
// may open at any commit (headAttribution), and only once its row is judged by that rule, which a
// change of the links does at once from the facts stored on the row (Attributor.LinksChanged). The
// terms name the repository of such a head (AttributedHead.LinkedRepo) and the merge
// Attributor.BaseBranchMergedIn walks, which is why they are read beside the attribution there. A
// link to a pull request of the repository the issue resolves to names nothing here, since that
// repository is asked about the commit before any link is read. The terms hold the issue to a
// repository by the attribution's own rule, over the same rename pairs, so the two cannot disagree
// about which repository is the issue's.
//
// The link names the pull request and not its head: a head is the stored row's, matched by the
// repository and the number, so a push that moves the head moves the basis with it.
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
// refuseUnknownCommit).
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

// HeadOfChangedLink is one stored pull request a change of an issue's links judged again
// (Attributor.LinksChanged): the key of its row, its URL, the issue it held before the change and
// the issue it holds after it, each nil for nobody, and the source that names the issue after it
// ("none" for nobody).
type HeadOfChangedLink struct {
	Repo   string
	Number int
	URL    string
	From   *string
	To     *string
	Source string
}

// AttributionEvents is the issue.pull_request_attribution_changed events the change appends for
// this pull request: one on the issue it left and one on the issue it reached, as a head write
// appends them for the same move (attributionMoveEvents), and none where it kept its issue.
func (head HeadOfChangedLink) AttributionEvents() []model.Event {
	if sameText(head.From, head.To) {
		return nil
	}
	return attributionMoveEvents(head.Repo, head.Number, head.URL, head.From, head.To, head.Source)
}

// linksChangedRowsQuery is LinksChanged's read of the rows a change of the links of the issue $1
// names, $2 the lowercased URLs of the links it added or removed, each under every name a rename
// pair gives its repository (refs.Renames.URLSpellings), and $3 whether it removed a link to a
// GitHub issue (githubIssueLink): each row whose own URL is one of those links, each row whose
// stored citations (attribution_cited_issues) name one, and, where a GitHub-issue link went, each
// row the issue holds by a citation that stores none, since such a row cannot say which citation
// that was. A row the build before this one wrote during a roll can store a URL under the name its
// repository was renamed away from until the boot's pass rewrites it, so the spellings find it as
// the attribution's link sources do (attributionCandidates). Each row is locked before it is
// read, so a writer of the same row waits for this transaction rather than having its issue read
// before its write and overwritten after it. No index serves `lower(url)` or the citations: a
// change of an issue's links is a person's edit, rare beside the reads of the table, and one scan
// of a row per pull request Dispatch has seen costs that edit a few milliseconds.
var linksChangedRowsQuery = `
	select p.repo, p.number, p.url, p.issue_key, p.attribution_source,
		p.attribution_title_keys, p.attribution_cited_issues, p.attribution_branch_keys,
		p.attribution_commit_keys, p.head_in_repo
	from pull_request_heads p
	where lower(p.url) = any($2::text[])
		or exists (
			select 1 from unnest(p.attribution_cited_issues) c(url) where lower(c.url) = any($2::text[])
		)
		or ($3::boolean and p.issue_key = $1 and p.attribution_source = '` + string(sourceGitHubIssueLink) + `'
			and p.attribution_cited_issues is null)
	order by p.repo, p.number
	for no key update`

// githubIssueLink is a link to a GitHub issue in the one form a citation the attribution reads
// takes (citedIssues: https://github.com/<owner>/<repo>/issues/<n>), lowercased, as the
// attribution compares a link with a citation.
var githubIssueLink = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/issues/[0-9]+$`)

// linksOfIssueQuery is every link of the issue $1, lowercased: the links after the change, which
// LinksChanged tells a removed link from an added one by.
const linksOfIssueQuery = `select lower(url) from issue_external_links where issue_key = $1`

// linksChangedWrite is LinksChanged's write of one row's judgement.
const linksChangedWrite = `
	update pull_request_heads set issue_key = $3, attribution_source = $4, attribution_rule = $5
	where repo = $1 and number = $2`

// linkChangeQuerier is the surface LinksChanged runs on: the transaction that writes the links,
// or a pool in a test.
type linkChangeQuerier interface {
	refs.Queryer
	refs.Execer
}

// changedLinkRow is one row linksChangedRowsQuery read: its key, its URL, its issue and source,
// and the facts a writer stored with them (PullRequestHead.Inputs), each nil where none did.
type changedLinkRow struct {
	repo, url                                  string
	number                                     int
	issue                                      *string
	source                                     string
	titleKeys, citedIssues, branchKeys, commit []string
	headInRepo                                 *bool
}

// LinksChanged is what a change of issueKey's external links does to the stored pull requests
// those links decide the issue of, urls being the URLs of the links it added or removed, run in
// the transaction that writes the links, after the write (api/issue_patch.go). It judges each such
// pull request again from what Dispatch stored of it, with no read of GitHub, writes the issue the
// judgement gives, and answers each row it judged, for the caller to append the moves
// (HeadOfChangedLink.AttributionEvents). A link a person makes by hand is answered from what
// Dispatch holds, as the results route answers it.
//
// Two sources of the attribution are links of the issue (attributionCandidates): first, the link
// of a pull request's own URL, held in any repository (attributionHeldFor, R19 of the spec of
// several open pull requests), so a pull request is the issue's while the issue links it; second,
// the link of a GitHub issue the pull request's body cites. Each writer that read a pull request
// from GitHub stored the inputs of every source and whether its head is in its own repository
// (UpsertPullRequestHead), so the judgement here is headAttribution's over the stored inputs and
// the links as this transaction leaves them: the first held candidate, and nobody for a head in a
// fork, linked or not. A link added gives the issue every pull request whose URL it is or whose
// body cites it, where nothing ranked above holds it; a link removed takes from the issue only the
// pull requests no remaining link, citation, branch, title or commit message holds to it.
//
// Two kinds of row the stored facts cannot judge whole, and each is given attribution_rule 0, a
// rule older than every rule that judges a row now, so the reconcile judges it on its own pass:
// the merged arm takes a merged row of an older rule (ListStalePullRequestHeads), and an open row
// is due again once its last read is PullRequestHeadStaleAfter old, whatever its rule, and is
// tried within the bound that function gives. One is a row with stored inputs whose commit
// messages no writer read (they are read only when no earlier source names an issue) and no other
// source of which holds it now: it is nobody's until that read, since the link that gave it is
// gone. The other is a row written before the inputs were stored: a link of
// its own URL removed, or a GitHub-issue link removed where it was the issue's by a citation,
// takes it from the issue, and a link added leaves it as it is. Rule 4 has the reconcile store
// the inputs of every such row once (AttributionRuleVersion).
//
// The rows are locked in key order while this transaction holds the issue's owner lock; a head
// writer locks the row and then the owners it appends to (UpsertPullRequestHead), so the two can
// meet in a deadlock, which Postgres breaks by refusing one of them: the PATCH answers it as a
// server error and writes nothing, and the head writer's caller writes the row again.
func (a *Attributor) LinksChanged(ctx context.Context, q linkChangeQuerier, issueKey string, urls []string) ([]HeadOfChangedLink, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	lowered := make([]string, 0, len(urls))
	spellings := make([]string, 0, len(urls))
	for _, url := range urls {
		lowered = append(lowered, strings.ToLower(url))
		for _, spelling := range a.renames.URLSpellings(url) {
			spellings = append(spellings, strings.ToLower(spelling))
		}
	}
	links, err := q.Query(ctx, linksOfIssueQuery, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	linked, err := pgx.CollectRows(links, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issueKey, err)
	}
	removedIssueLink := slices.ContainsFunc(lowered, func(url string) bool {
		return githubIssueLink.MatchString(url) && !slices.Contains(linked, url)
	})
	read, err := q.Query(ctx, linksChangedRowsQuery, issueKey, spellings, removedIssueLink)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests whose links of %s changed: %w", issueKey, err)
	}
	rows, err := pgx.CollectRows(read, func(row pgx.CollectableRow) (changedLinkRow, error) {
		var r changedLinkRow
		err := row.Scan(&r.repo, &r.number, &r.url, &r.issue, &r.source,
			&r.titleKeys, &r.citedIssues, &r.branchKeys, &r.commit, &r.headInRepo)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the pull requests whose links of %s changed: %w", issueKey, err)
	}
	changed := make([]HeadOfChangedLink, 0, len(rows))
	for _, row := range rows {
		to, source, rule, err := a.judgeChangedLinkRow(ctx, q, issueKey, row, linked, removedIssueLink)
		if err != nil {
			return nil, err
		}
		if _, err := q.Exec(ctx, linksChangedWrite, row.repo, row.number, to, source, rule); err != nil {
			return nil, fmt.Errorf("judge again %s#%d, whose link of %s changed: %w", row.repo, row.number, issueKey, err)
		}
		changed = append(changed, HeadOfChangedLink{
			Repo: row.repo, Number: row.number, URL: row.url, From: row.issue, To: to, Source: source,
		})
	}
	return changed, nil
}

// judgeChangedLinkRow is the issue, the source and the rule LinksChanged writes for one row
// (LinksChanged's doc comment): headAttribution's judgement over the row's stored inputs, read
// through q so the candidates read the links this transaction wrote, and rule 0 for a row those
// inputs cannot judge whole. A link of the row's own URL counts under any name a rename pair gives
// its repository (refs.Renames.URLSpellings), as the attribution compares it.
func (a *Attributor) judgeChangedLinkRow(
	ctx context.Context, q refs.Queryer, issueKey string, row changedLinkRow, linked []string, removedIssueLink bool,
) (*string, string, int, error) {
	none := string(sourceNone)
	if row.titleKeys == nil || row.citedIssues == nil || row.branchKeys == nil || row.headInRepo == nil {
		ownLinked := slices.ContainsFunc(a.renames.URLSpellings(row.url), func(spelling string) bool {
			return slices.Contains(linked, strings.ToLower(spelling))
		})
		gone := row.issue != nil && *row.issue == issueKey &&
			(row.source == string(sourceExternalLink) && !ownLinked ||
				row.source == string(sourceGitHubIssueLink) && removedIssueLink)
		if gone {
			return nil, none, 0, nil
		}
		return row.issue, row.source, 0, nil
	}
	if !*row.headInRepo {
		return nil, none, AttributionRuleVersion, nil
	}
	candidates, err := a.resolveAttributionCandidates(ctx, q, row.repo, row.url, AttributionInputs{
		TitleKeys: row.titleKeys, CitedIssues: row.citedIssues, BranchKeys: row.branchKeys, CommitKeys: row.commit,
	})
	if err != nil {
		return nil, none, 0, err
	}
	if i := firstHeldCandidate(candidates); i >= 0 {
		key := candidates[i].key
		return &key, string(candidates[i].source), AttributionRuleVersion, nil
	}
	if row.commit == nil {
		return nil, none, 0, nil
	}
	return nil, none, AttributionRuleVersion, nil
}

// baseBranchMergedInQuery is BaseBranchMergedIn's statement after its WITH list: the merged rows
// attributed to the issue that its links name in a repository it does not resolve to, held to one
// repository, in BaseBranchMergedFor's order.
var baseBranchMergedInQuery = `
	select h.base_ref from linked_pulls l
	` + linkedHeadJoin + `
	where h.issue_key = $1 and lower(l.repo) = lower($2) and h.state = 'merged'
	order by h.merged_at desc nulls last, h.number desc
	limit 1`

// baseBranchMergedInRead is baseBranchMergedInQuery whole, for both callers, its linked pull
// requests read over renames (linkedPullRequestTerms). NewAttributor builds it once from its
// rename pairs.
func baseBranchMergedInRead(renames refs.Renames) headsStatement {
	return newHeadsStatement(linkedPullRequestTerms(renames), baseBranchMergedInQuery, 2, "")
}

// BaseBranchMergedIn is BaseBranchMergedFor for a repository issueKey does not resolve to: the
// branch the merged pull request that the issue links by its URL in repoFull merged into, and ""
// where Dispatch holds no merge of such a pull request that the attribution gives the issue. An
// issue whose work spans two repositories links the pull request it has in the second one, and
// the attribution gives it that pull request (attributionHeldFor), unless its head is in a fork
// (headAttribution). The results read measures an `after_merge` result whose commit the second
// repository holds against the branch answered here, in that repository (api/results_linked.go,
// linkedBranchBases), and BaseBranchMergedFor answers the repository the issue resolves to.
//
// The rows are ordered as BaseBranchMergedFor orders them, and the empty answer means what its
// empty answer means: no branch to measure against. A link to a pull request of the
// repository the issue resolves to names nothing here, so a repoFull the issue resolves to
// answers "". repoFull is compared without case with the canonical name each link is read under,
// as every other comparison of a repository here is.
//
// The linked rows are read through pull_request_heads_repo_head's leading column, one
// repository's rows for each linked pull request, as HeadsAttributedTo's are. answers is the
// caller's request's, as HeadsAttributedTo takes them.
func (a *Attributor) BaseBranchMergedIn(ctx context.Context, q refs.Queryer, issueKey, repoFull string, answers HeadAnswers) (string, error) {
	query, args := a.mergedInRead.query(answers, issueKey, repoFull)
	var branch string
	err := q.QueryRow(ctx, query, args...).Scan(&branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the merged linked pull request base branch of %s in %s: %w", issueKey, repoFull, err)
	}
	return branch, nil
}
