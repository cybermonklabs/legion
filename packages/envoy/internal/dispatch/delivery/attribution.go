package delivery

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/citebody"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's issue is named by the first of its five ranked sources that names an issue
// Dispatch holds: of the issues that source names, the first that resolves to the repository the
// pull request is in. The walk stops at that source whatever repository its issues resolve to, so
// where it names only issues of another repository, or of no repository at all, the pull request
// is attributed to nobody, with the source "none", and no later source is consulted. A key that
// names no issue Dispatch holds names nothing, and the walk goes on to the next source. Every
// candidate of one source ranks above any of the next (attributionCandidates):
//
//  1. the issue whose external link is the pull request's own URL;
//  2. the issue whose external link is a GitHub issue the body cites, a full
//     github.com/<owner>/<repo>/issues/<n> URL or a bare #N in the pull request's own repository,
//     the first such citation in body order;
//  3. a key in its head branch name, whatever its case;
//  4. a key in its title or body, title first;
//  5. a key in its commit messages, in commit order.
//
// Sources 4 and 5 each read their `Dispatch:` trailer lines first (dispatchTrailer): a key on such
// a line ranks above every key the same text only mentions, as a source of its own. A trailer is
// the pull request's own statement of its issue, so where a trailer names an issue the walk stops
// there, and a key the body or the commits only mention is never reached: a body whose trailer
// names an issue of another repository is credited to nobody, not to an issue of this repository
// its prose mentions, and a merge commit's prose does not outrank the last commit's trailer
// (dispatch://INFRA-3992/ask/27e8000f-5a0b-417b-a855-f824dc7054c5). Trailer keys keep the order
// they were read in, the commits' in commit order.
//
// The prototype's collector/attribution.py tried the title and the body first, and ranks 1 to 3
// below them. R8 of INFRA-1522 moved them above: a branch name and a link are acts that name the
// issue — someone branched for it, someone linked it — while a key in a title or a body is a
// mention, which a pull request that reverts, follows or merely cites another issue's work
// carries as readily as one that does that work. The commit messages stay last: they are the
// weakest mention of all, and the only source that costs a GitHub call to read (attributeHead).
//
// The walk stops at the first source that names an issue because a later source is weaker
// evidence about the same pull request, never evidence that the pull request belongs to another
// issue. A pull request whose body declares an issue of another repository is that issue's work,
// and rules 1 to 8, which passed such an issue over for the next candidate, gave the pull request
// to whatever issue of its own repository a later source happened to mention: a merge commit's
// prose naming an unrelated issue was enough (dispatch://INFRA-3992/artifact/attribution-fallthrough-md).
// An issue of another repository claims a pull request through its external link instead
// (linked_pull_requests.go).
//
// The holding to the repository is the server's one repository-resolution rule, spliced into the
// same statement as the candidates (refs.IssueRepoTerms, attributionHeld), so what an
// attribution believes an issue's repository is can never disagree with the issue page.
//
// Sources 2, 3, 4 and 5 read what GitHub says about the pull request, which does not change once
// it has merged, so those inputs (AttributionInputs) are read from GitHub once and stored on its
// row. Sources 1 and 2 also read Dispatch's external links, which change whenever someone links an
// issue, every source reads whether the issue it names exists, and the holding reads where that
// issue sits — a reparent, or a change of its project's default repository, moves it — so a
// stored row's issue is resolved again from the database alone (AttributePullRequests), a batch of
// rows a pass, the rows checked longest ago first: an issue that links a pull request tomorrow
// credits it within one walk of the table, with no GitHub call. Nothing in the schema says which
// rows a change could affect (a link carries no time, and adding or removing one leaves its
// issue's updated_at alone), so the walk visits them all in turn.

// AttributionRuleVersion names the rule this build judges a pull request by: the first source, of
// the five above, that names an issue, and of that source's issues the first whose issue resolves
// to the repository the pull request is in. pull_request_heads.attribution_rule records the rule
// that last judged a row, and the reconcile re-judges a merged row once when the rule that judged
// it is older than this one (ListStalePullRequestHeads, pull_request_heads.go). 0 is what a row
// written before that column has: the rule that took the first candidate outright, whatever
// repository its issue resolved to. 1 held every candidate to the pull request's repository and
// ranked the title and the body first. 2 is the order above, with the two links and the head
// branch over the title and the body. 6 is the rule of 2, with each merged row's merge commit and
// merge time stored: raising it offers each merged row stored before once more, and that refresh
// stores both from GitHub's answer (RefreshPullRequestHead, headRow). It is 6, not 3, because
// cybermonklabs/legion#4230 numbers its own rules 3 to 5.
// 8 is the rule of 6 with a tie between candidates broken by issue key in byte order
// (collate "C"). It is 8, not 7, because cybermonklabs/legion#4230 numbers its own rule 7.
// 10 is the rule of 8 with the walk stopped at the first source that names an issue (above): where
// that source names no issue of the pull request's repository, nobody is credited rather than an
// issue a later source names, and the commit messages are read only where no earlier source names
// an issue; a `Dispatch:` trailer line ranks above every key its own source only mentions. It is
// 10, not 9, because cybermonklabs/legion#4230 numbers its own rule 9.
//
// Raising this is how a later change to the attribution, or to what a merged row stores, reaches
// the merged rows already stored. Nothing else revisits one — an open row comes round at every
// reconcile pass of one process, each starting at most five minutes after the one before it
// ended (PullRequestHeadStaleAfter), but a merged row is read again only by that arm — so a
// change made without raising it leaves every pull request merged before it judged by the rule
// of its day for good.
const AttributionRuleVersion = 10

// attributionSource names the source a pull request's issue came from.
type attributionSource string

const (
	sourceTitleBody       attributionSource = "title_body"
	sourceExternalLink    attributionSource = "external_link"
	sourceGitHubIssueLink attributionSource = "github_issue_link"
	sourceBranch          attributionSource = "branch"
	sourceCommitMessage   attributionSource = "commit_message"
	sourceNone            attributionSource = "none"
)

// AttributionInputs is what the attribution reads from GitHub, as stored on the pull request's
// row: the issue keys its title and body name, the GitHub issue URLs its body cites, the keys its
// head branch names and the keys its commit messages name, each deduplicated in first-seen order,
// and of those the keys the body's and the commits' `Dispatch:` trailer lines name
// (dispatchTrailer). Nil on a DeliveryPullRequest means they have not been read; a row stored
// before the trailer keys were holds nil trailer keys, which the attribution reads as no trailer.
type AttributionInputs struct {
	TitleKeys         []string
	CitedIssues       []string
	BranchKeys        []string
	CommitKeys        []string
	BodyTrailerKeys   []string
	CommitTrailerKeys []string
}

// dispatchTrailer matches a `Dispatch:` trailer line, whatever its case and its indentation, and
// captures what follows the colon. The colon must be followed by a space or a tab, so a line that
// starts with a dispatch:// reference is no trailer.
var dispatchTrailer = regexp.MustCompile(`(?im)^[ \t]*dispatch:[ \t]+(.*)$`)

// trailerKeys is every key text's `Dispatch:` trailer lines name, deduplicated in first-seen order.
func trailerKeys(text string) []string {
	keys := []string{}
	for _, line := range dispatchTrailer.FindAllStringSubmatch(text, -1) {
		keys = appendUnique(keys, issueKeyCandidate.FindAllString(line[1], -1)...)
	}
	return keys
}

// commitTrailerKeys is trailerKeys over each commit message below its subject line, in commit
// order: a subject is no trailer, whatever word it begins with, so a commit titled "dispatch: …"
// does not declare the issue its subject mentions.
func commitTrailerKeys(messages []string) []string {
	keys := []string{}
	for _, message := range messages {
		_, below, _ := strings.Cut(message, "\n")
		keys = appendUnique(keys, trailerKeys(below)...)
	}
	return keys
}

// attributionFacts is what GitHub says about one pull request that the attribution reads.
type attributionFacts struct {
	Repo           string
	URL            string
	Title          string
	Body           string
	HeadRef        string
	CommitMessages []string
}

// githubIssuePath matches the path a citation carries, `/<owner>/<repo>/issues/<n>`, and no host
// at all; githubBareReference matches a bare #N of two to six digits (the prototype's reference
// shape), which GitHub reads as an issue or pull request of the same repository. A # after a word
// character, `&` (an HTML character reference), `/` (a URL fragment) or another # (a heading) is
// not a reference.
//
// The host is no part of the pattern. A pattern that matched `github.com` would have to say where
// a host ends, and an expression matching a host name unanchored is itself the kind of defect
// this file was changed to fix. citesGitHubIssue decides instead, from what stands before the
// path, and net/url reads the host.
var (
	githubIssuePath     = regexp.MustCompile(`/([\w.-]+)/([\w.-]+)/issues/([1-9][0-9]*)\b`)
	githubBareReference = regexp.MustCompile(`(?:^|[^\w&/#])#([1-9][0-9]{1,5})\b`)
)

const (
	// maxHostName is the longest run of host bytes read before the path: a DNS name is at most
	// 253 bytes (RFC 1035), so a longer run is no host. It bounds the digits of a port too,
	// since the digits a writer puts between a colon and the path name no host: a port no URL
	// may carry is still github.com, and measuring those digits against 65535 would only lose
	// citations to a typo.
	maxHostName = 253
	// citationReach is the most of a body before an issue path that citesGitHubIssue reads: up to
	// maxHostName digits of a port, the colon before them, and up to maxHostName bytes of the run
	// before that (authorityBefore). citedIssues hands each decision no more of the body than
	// that and the path itself, so citesGitHubIssue, as it is or as a later change makes it, reads
	// no more than citationReach bytes before a path however long the body is.
	citationReach = 2*maxHostName + 1
)

// citesGitHubIssue says whether the issue path running from pathAt to the end of text belongs
// to a URL whose host is github.com. text is the part of a body that ends where the path does and
// starts at most citationReach bytes before the path (citedIssues). It asks one question, and
// it is the question the alert this file was changed for asks: what is the host of the URL this
// path hangs off?
//
// authorityBefore answers it by reading the run of host bytes immediately before the path, and
// that read is what refuses most of the names that look like github.com: `@` and `%` are no host
// bytes, so `github.com@evil.test` and a percent-escaped name end the run elsewhere, and a run
// that is longer than github.com — `evilgithub.com`, `www.github.com`, `my-github.com`,
// `Seegithub.com` — is simply another name; userinfo stops the run at its `@` the same way, so
// `https://user@github.com/…` reads the host after it. net/url then separates a port from that
// run and says what host it names, and the comparison refuses every name but github.com, however
// the body spells its case. A github.com URL whose path is not an issue's is refused by the same
// read: the bytes before `/main/docs/issues/12` in a blob URL are `blob`, not a host. The path
// comparison states what that leaves true, that the authority runs to where the match begins.
//
// What stands further left decides nothing. The default branch reads less than this — the bytes
// `github.com/` wherever they fall — and credits every lower-case, portless shape a reader
// writes; a rule for the bytes before the host credited some of those shapes and dropped others,
// which is why there is none. So a citation inside another URL's path, query or fragment is
// credited here as it is there, and what this change does to attribution is to refuse a host
// that is not github.com and to credit a host spelled in any case or carrying a port.
func citesGitHubIssue(text string, pathAt int) bool {
	host := authorityBefore(text, pathAt)
	if host < 0 {
		return false
	}
	parsed, err := url.Parse("//" + text[host:])
	return err == nil && strings.EqualFold(parsed.Hostname(), "github.com") &&
		parsed.EscapedPath() == text[pathAt:]
}

// authorityBefore is where the authority owning the path at pathAt begins, or -1 where no host
// stands before the path at all. It reads the port's digits and then the run of host bytes, each
// bounded by a host name's length, so it reads no byte more than citationReach before the path.
func authorityBefore(body string, pathAt int) int {
	at := pathAt
	digits := at
	for digits > 0 && at-digits < maxHostName && isDigitByte(body[digits-1]) {
		digits--
	}
	if digits < at && digits > 0 && body[digits-1] == ':' {
		at = digits - 1 // a port, which belongs to the authority
	}
	start := at
	for start > 0 && at-start < maxHostName && isHostByte(body[start-1]) {
		start--
	}
	if start == at || at-start >= maxHostName {
		return -1 // no host before the path, or a run too long to be one
	}
	return start
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

func isHostByte(b byte) bool {
	return isDigitByte(b) || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '.' || b == '-'
}

func (a *Attributor) attributionInputsFrom(facts attributionFacts) AttributionInputs {
	commits := strings.Join(facts.CommitMessages, "\n")
	return AttributionInputs{
		TitleKeys:         uniqueKeys(facts.Title + "\n" + facts.Body),
		CitedIssues:       citedIssues(a.renames, facts.Repo, citebody.New(facts.Body, nil)),
		BranchKeys:        uniqueKeys(strings.ToUpper(facts.HeadRef)),
		CommitKeys:        uniqueKeys(commits),
		BodyTrailerKeys:   trailerKeys(facts.Body),
		CommitTrailerKeys: commitTrailerKeys(facts.CommitMessages),
	}
}

func uniqueKeys(text string) []string {
	return appendUnique([]string{}, issueKeyCandidate.FindAllString(text, -1)...)
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// citedIssues is every GitHub issue body cites, as https://github.com/<owner>/<repo>/issues/<n>,
// in the order the citations appear; a bare #N names an issue of repo. Each URL is under its
// repository's new name where a rename pair renames it away (refs.Renames.CanonicalURL), as every
// stored URL is, so a citation of an old name and one of the new name are one URL here, kept once.
// It reaches the body only through citebody's counted reads, so the cost tests can count every
// byte it reads (citebody.Reads); its production caller wraps the body with no counter, which
// counts nothing. Each path's decision gets the body from citationReach bytes before the path to
// the path's end, the most it reads, so its cost is bounded per path whatever the body holds
// before it.
func citedIssues(renames refs.Renames, repo string, body citebody.Body) []string {
	type citation struct {
		at  int
		url string
	}
	citations := []citation{}
	for _, match := range body.Find(githubIssuePath) {
		from := max(0, match[0]-citationReach)
		if !citesGitHubIssue(body.Span(from, match[1]), match[0]-from) {
			continue
		}
		citations = append(citations, citation{match[0], "https://github.com/" + body.Span(match[2], match[3]) + "/" + body.Span(match[4], match[5]) + "/issues/" + body.Span(match[6], match[7])})
	}
	for _, match := range body.Find(githubBareReference) {
		citations = append(citations, citation{match[2], "https://github.com/" + repo + "/issues/" + body.Span(match[2], match[3])})
	}
	slices.SortStableFunc(citations, func(a, b citation) int { return cmp.Compare(a.at, b.at) })
	urls := []string{}
	for _, c := range citations {
		urls = appendUnique(urls, renames.CanonicalURL(c.url))
	}
	return urls
}

// attributionColumns are the SQL expressions attributionCandidates reads a pull request's URL
// and its six stored inputs from: bound parameters for one pull request
// (resolveAttributionCandidates), or a stored row's own columns (attributionRowColumns, which
// AttributePullRequests and ResolvedCandidates read). Named, so the callers cannot hand the six
// text[] inputs over in another order. The repository the candidates are held to is not read
// here: it reaches the SQL through attributionHeld alone, from attributionOneRepo or the batch's
// own c.pr_repo.
type attributionColumns struct {
	URL, TitleKeys, CitedIssues, BranchKeys, CommitKeys, BodyTrailerKeys, CommitTrailerKeys string
}

// attributionCandidateBound is how many ranked candidates one attribution carries out of the
// union. Every candidate costs a repository resolution now — a walk up its parent chain and its
// project's default repository (refs.IssueRepoTerms) — and a title and a body together can name
// many keys: a Legion pull request names one or two, while a body listing a tree's issues can
// name a dozen, so 25 is room for the shapes that occur and a bound on the work a pathological
// body can ask for. The bound drops only candidates ranked below it, so it decides an attribution
// alone only where the first source that names an issue names more than 25 and none of the first
// 25 is held to the pull request's repository.
const attributionCandidateBound = 25

// attributionCandidates is the issues a pull request's five sources name, best first: the
// ranking is the file's, every candidate of a source before any of the next, and within a
// source the order the keys were read in. The issues one place names together — the issues whose
// link is the pull request's URL, or those whose link is the GitHub issue one citation names —
// rank by key in byte order (`collate "C"`), and so do the statements that order the candidates
// again, so two databases holding the same links rank them alike whatever order the links were
// stored in: the first held candidate of the first source is the pull request's issue, and
// ResolvedCandidates returns the whole ranked list. Each candidate says whether it is of that
// first source (first_source): its rank is the best rank of the answer, read with a window over
// the whole union (`min(s.rank) over ()`), which the bound never drops since it keeps the best
// ranks. It is decided here, once, so every reader of the list picks within the same source. The
// union's arms are written in that ranked order, so the text reads as the list at the top of this
// file reads. The two link sources compare URLs lowercased on both sides: GitHub's owner and
// repository names are case-insensitive, its answers spell them as their owners do
// (`Acme/Widgets`), and Dispatch stores an issue's GitHub link lowercased (refs.ExternalRepo) or
// as a person typed it (issue_external_links_url_lower serves the comparison).
//
// Each link source compares a link with every spelling of the URL it looks for: the URL itself,
// and the URL under the other name of each rename pair that names it (refs.URLSpellingsSQL). So
// a link stored under a repository's old name still credits a pull request on the new name, and
// a link stored under the new name credits one Dispatch stored under the old name. The
// comparison is `lower(l.url) = any(…)`, which issue_external_links_url_lower still serves.
func attributionCandidates(renames refs.Renames, c attributionColumns) string {
	return `select s.key, s.source, s.rank, s.n, s.rank = min(s.rank) over () as first_source from (
		select l.issue_key as key, '` + string(sourceExternalLink) + `' as source, 0 as rank, 0 as n
			from issue_external_links l where lower(l.url) = any(` + refs.URLSpellingsSQL(renames, c.URL) + `)
		union all
		select l.issue_key, '` + string(sourceGitHubIssueLink) + `', 1, c.n
			from unnest(` + c.CitedIssues + `) with ordinality c(url, n)
			join issue_external_links l on lower(l.url) = any(` + refs.URLSpellingsSQL(renames, "c.url") + `)
		union all
		select b.key, '` + string(sourceBranch) + `', 2, b.n
			from unnest(` + c.BranchKeys + `) with ordinality b(key, n) join issues i on i.key = b.key
		union all
		select t.key, '` + string(sourceTitleBody) + `', 3, t.n
			from unnest(` + c.BodyTrailerKeys + `) with ordinality t(key, n) join issues i on i.key = t.key
		union all
		select t.key, '` + string(sourceTitleBody) + `', 4, t.n
			from unnest(` + c.TitleKeys + `) with ordinality t(key, n) join issues i on i.key = t.key
		union all
		select m.key, '` + string(sourceCommitMessage) + `', 5, m.n
			from unnest(` + c.CommitTrailerKeys + `) with ordinality m(key, n) join issues i on i.key = m.key
		union all
		select m.key, '` + string(sourceCommitMessage) + `', 6, m.n
			from unnest(` + c.CommitKeys + `) with ordinality m(key, n) join issues i on i.key = m.key
	) s order by s.rank, s.n, s.key collate "C" limit ` + strconv.Itoa(attributionCandidateBound)
}

// attributionHeld is the condition a candidate of the first source passes to be the pull
// request's issue: it resolves, through the server's one repository-resolution rule
// (refs.IssueRepoTerms, joined by refs.IssueRepoJoin in the enclosing statement), to the
// repository the expression repo reads. It is never null — a candidate whose issue row is
// missing, and one whose parent chain ran out of depth, is simply not held — so a caller may read
// it as a plain boolean and a `where` over it drops exactly the candidates that are not held. The
// comparison is lowercased on both sides because GitHub spells an owner and a repository as their
// owners do (`Acme/Widgets`), and a stored repository may be spelled either way.
//
// Both sides go through the server's one repository-renaming rule first
// (refs.CanonicalRepoSQL over renames), so one repository under two names is one repository
// here: an issue recorded under a name that was renamed away is held to a pull request on the
// new name, and an issue recorded under the new name to a pull request Dispatch stored under the
// old one. A repository with no rename pair is its own canonical name, so it is compared by that
// name alone.
func attributionHeld(renames refs.Renames, repo string) string {
	return `coalesce(not ` + refs.IssueRepoTooDeep +
		` and lower(` + refs.CanonicalRepoSQL(renames, refs.IssueRepoResolved) + `) = lower(` + refs.CanonicalRepoSQL(renames, repo) + `), false)`
}

// attributionOneColumns are the bound parameters one pull request's candidates are read from:
// its URL, then its first four inputs; attributionOneRepo, the sixth parameter, is the repository
// the pull request is in, which attributionHeld reads; the seventh and the eighth are the two
// trailer inputs. resolveAttributionCandidates binds the eight in that order.
var attributionOneColumns = attributionColumns{
	URL: "$1", TitleKeys: "$2::text[]", CitedIssues: "$3::text[]",
	BranchKeys: "$4::text[]", CommitKeys: "$5::text[]",
	BodyTrailerKeys: "$7::text[]", CommitTrailerKeys: "$8::text[]",
}

const attributionOneRepo = "$6"

// attributionSubjects is the `subjects` term the three statements below splice: the distinct
// keys their candidates name, which is the set refs.IssueRepoTerms walks. One text, spliced
// three times, because the statements must ask the walk about exactly the same keys — a term
// that differed between them would resolve one statement's candidates and not another's. `where
// key is not null` because the two statements over stored rows read their candidates through a
// `left join lateral`, which answers one null-keyed row for a pull request whose five sources
// name nothing at all.
const attributionSubjects = `subjects as (
	select distinct key from candidates where key is not null
)`

// attributionCandidatesQuery is the one statement that resolves a pull request's candidates from
// stored inputs: every candidate the five sources name, best first, each marked whether it is of
// the first source that names an issue and whether it is held to the pull request's own
// repository. The repository rule is spliced in over the candidate keys the way
// refs.IssueRepository splices it over one key, so one walk resolves them all together. `left
// join issues`, not `join`: a candidate whose issue row has gone stays in the answer, unheld, so
// a caller can name the candidates it passed over. NewAttributor builds the text once, rather
// than each read: the concatenation is work a read would repeat for a statement that never
// differs within a process, and pgx caches a prepared plan per statement text.
func attributionCandidatesQuery(renames refs.Renames) string {
	return `with recursive candidates as (
	` + attributionCandidates(renames, attributionOneColumns) + `
), ` + attributionSubjects + `, ` + refs.IssueRepoTerms + `
select c.key, c.source, c.first_source, ` + attributionHeld(renames, attributionOneRepo) + ` as held
from candidates c
	left join issues i on i.key = c.key
	` + refs.IssueRepoJoin + `
order by c.rank, c.n, c.key collate "C"`
}

// attributionRowColumns are a stored row's own columns, aliased src, which the two statements over
// stored rows read the candidates from (attributionBatchQuery, resolvedCandidatesQuery).
var attributionRowColumns = attributionColumns{
	URL: "src.url", TitleKeys: "src.attribution_title_keys", CitedIssues: "src.attribution_cited_issues",
	BranchKeys: "src.attribution_branch_keys", CommitKeys: "src.attribution_commit_keys",
	BodyTrailerKeys: "src.attribution_body_trailer_keys", CommitTrailerKeys: "src.attribution_commit_trailer_keys",
}

// attributionRowTerms is the terms the two statements over stored rows splice after their batch
// (attributionBatchQuery, resolvedCandidatesQuery): `candidates`, each batch row's (src) candidates
// read from its own columns (attributionRowColumns), with the row's repository and number
// (pr_repo, pr_number), then attributionSubjects and the repository walk over their keys
// (refs.IssueRepoTerms). One text, for attributionSubjects' reason: the two statements must resolve
// the same candidates alike.
func attributionRowTerms(renames refs.Renames) string {
	return `candidates as (
			select src.repo as pr_repo, src.number as pr_number, f.key, f.rank, f.n, f.first_source
			from batch src
			left join lateral (` + attributionCandidates(renames, attributionRowColumns) + `) f on true
		), ` + attributionSubjects + `, ` + refs.IssueRepoTerms
}

// attributionBatchQuery is the statement AttributePullRequests resolves a whole batch of stored
// rows with: the same five sources, the same first source and the same holding as
// attributionCandidatesQuery above, read from each row's own columns instead of bound
// parameters, with the batch's size the one parameter. NewAttributor builds it once, for that
// statement's reason: the concatenation never differs between passes, and pgx caches a prepared
// plan per statement text.
func attributionBatchQuery(renames refs.Renames) string {
	return `
		with recursive batch as (
			select repo, number, url, issue_key, attribution_title_keys, attribution_cited_issues,
				attribution_branch_keys, attribution_commit_keys, attribution_body_trailer_keys,
				attribution_commit_trailer_keys
			from delivery_pull_requests
			where attribution_title_keys is not null
			order by attribution_checked_at nulls first, repo, number
			limit $1
		), ` + attributionRowTerms(renames) + `, held as (
			select c.pr_repo, c.pr_number, c.key, c.rank, c.n
			from candidates c
				left join issues i on i.key = c.key
				` + refs.IssueRepoJoin + `
			where c.first_source and ` + attributionHeld(renames, "c.pr_repo") + `
		), picked as (
			select distinct on (pr_repo, pr_number) pr_repo, pr_number, key
			from held order by pr_repo, pr_number, rank, n, key collate "C"
		), resolved as (
			select src.repo, src.number, p.key, src.issue_key is distinct from p.key as changed
			from batch src
			left join picked p on p.pr_repo = src.repo and p.pr_number = src.number
		), written as (
			update delivery_pull_requests pr
			set issue_key = resolved.key,
				updated_at = case when resolved.changed then now() else pr.updated_at end,
				attribution_checked_at = now()
			from resolved
			where pr.repo = resolved.repo and pr.number = resolved.number
			returning resolved.changed
		)
		select count(*) filter (where changed) from written
	`
}

// resolvedCandidatesQuery is the statement ResolvedCandidates reads named stored rows' candidates
// with: the same five sources over each row's own columns as attributionBatchQuery, of the rows
// of the pull requests its two arrays name (repositories, then numbers) whose attribution inputs
// are stored, and the same repository walk over every key they name. Where the batch holds each
// candidate to the live default of its issue's project, this answers what that holding is made
// of, so a reader can hold the candidates to the defaults it reads by: for each candidate, in rank
// order, its issue's project, the repository its own chain names (repo.owned of
// refs.IssueRepoTerms) under its canonical name, whether that repository is the pull request's,
// compared as attributionHeld compares them (both canonical, without case), whether the walk ran
// out of depth, and whether the candidate is of the first source that names an issue, the one
// source a pick is made in. A row whose five sources name nothing answers one row of no key, the
// lateral's null row, so a reader can tell a row whose inputs name no issue from one whose inputs
// are not stored, which answers no row. NewAttributor builds it once, for the batch's reason.
func resolvedCandidatesQuery(renames refs.Renames) string {
	owned := refs.CanonicalRepoSQL(renames, "repo.owned")
	return `
		with recursive batch as (
			select pr.repo, pr.number, pr.url, pr.attribution_title_keys, pr.attribution_cited_issues,
				pr.attribution_branch_keys, pr.attribution_commit_keys, pr.attribution_body_trailer_keys,
				pr.attribution_commit_trailer_keys
			from unnest($1::text[], $2::int[]) as given(repo, number)
			join delivery_pull_requests pr on pr.repo = given.repo and pr.number = given.number
			where pr.attribution_title_keys is not null
		), ` + attributionRowTerms(renames) + `
		select c.pr_repo, c.pr_number, c.key, i.project_key, coalesce(` + owned + `, ''),
			coalesce(lower(` + owned + `) = lower(` + refs.CanonicalRepoSQL(renames, "c.pr_repo") + `), false),
			` + refs.IssueRepoTooDeep + `, coalesce(c.first_source, false)
		from candidates c
			left join issues i on i.key = c.key
			` + refs.IssueRepoJoin + `
		order by c.pr_repo, c.pr_number, c.rank, c.n, c.key collate "C"`
}

// Attributor attributes pull requests to issues by the rename pairs this process read at boot
// (refs.Renames), keys every pull_request_heads row it writes by the canonical name of the row's
// repository (canonical), and reads the heads an issue's results are compared with, its linked
// pull requests' included (linked_pull_requests.go), by the same pairs. NewAttributor builds its
// statements once from those pairs. cmd/dispatch's main builds one Attributor and gives it to
// the delivery reconcile (NewReconcile) and the intakes (StartIntakes). The API builds its own
// from the same pairs (api.NewDeps). No package variable holds the pairs, so a test builds an
// Attributor from pairs of its own.
type Attributor struct {
	renames         refs.Renames
	candidatesQuery string
	batchQuery      string
	resolvedQuery   string
	linkedQuery     string
	headQuery       string
	headsRead       headsStatement
	mergeInRead     headsStatement
}

// NewAttributor is the Attributor of renames.
func NewAttributor(renames refs.Renames) *Attributor {
	return &Attributor{
		renames:         renames,
		candidatesQuery: attributionCandidatesQuery(renames),
		batchQuery:      attributionBatchQuery(renames),
		resolvedQuery:   resolvedCandidatesQuery(renames),
		linkedQuery:     linkedPullRequestsQuery(renames),
		headQuery:       headAttributedQuery(renames),
		headsRead:       headsAttributedRead(renames),
		mergeInRead:     mergeInRead(renames),
	}
}

// canonical is repo under the name it is called today (refs.Renames.Canonical).
func (a *Attributor) canonical(repo string) string {
	return a.renames.Canonical(repo)
}

// attributionCandidate is one issue a pull request's sources name: the key, the source that
// named it, whether that source is the first that names an issue (first_source of
// attributionCandidates), and whether the issue resolves to the repository the pull request is
// in. Only a held candidate of the first source may be the pull request's issue; the candidates
// of that source above it are what a caller names when it reports what it passed over
// (headAttribution).
type attributionCandidate struct {
	key    string
	source attributionSource
	first  bool
	held   bool
}

// resolveAttributionCandidates is every issue the stored inputs of the pull request at url, in
// repo, name, best first, each marked of the first source or not and held or not: the whole
// ranked list, so a caller that must skip a candidate can take the next rather than make a second
// read.
func (a *Attributor) resolveAttributionCandidates(ctx context.Context, pool *store.Pool, repo, url string, inputs AttributionInputs) ([]attributionCandidate, error) {
	rows, err := pool.Query(ctx, a.candidatesQuery,
		url, inputs.TitleKeys, inputs.CitedIssues, inputs.BranchKeys, inputs.CommitKeys, repo,
		inputs.BodyTrailerKeys, inputs.CommitTrailerKeys)
	if err != nil {
		return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
	}
	defer rows.Close()
	var candidates []attributionCandidate
	for rows.Next() {
		var candidate attributionCandidate
		if err := rows.Scan(&candidate.key, &candidate.source, &candidate.first, &candidate.held); err != nil {
			return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
	}
	return candidates, nil
}

// resolveStoredIssueKey is the issue a pull request's stored inputs name: the first candidate of
// the first source that names an issue held to repo, the repository the pull request is in, and
// nobody with the source "none" when that source names none (attributedCandidate). Its callers
// hand over the repository they already hold — completePullRequest its repoFull,
// readAttributionInputs its row's own repo.
func (a *Attributor) resolveStoredIssueKey(ctx context.Context, pool *store.Pool, repo, url string, inputs AttributionInputs) (*string, attributionSource, error) {
	candidates, err := a.resolveAttributionCandidates(ctx, pool, repo, url, inputs)
	if err != nil {
		return nil, sourceNone, err
	}
	if i := attributedCandidate(candidates); i >= 0 {
		key := candidates[i].key
		return &key, candidates[i].source, nil
	}
	return nil, sourceNone, nil
}

// attributedCandidate is the index of the pull request's issue in its ranked list, -1 when it has
// none: the first candidate of the first source that names an issue that is held to the pull
// request's own repository. A later source is never reached by a pick, since the first source's
// candidates rank above every other. It is the pick made over one pull request's ranked list,
// which both readers of that list share — resolveStoredIssueKey and headAttribution
// (pull_request_heads.go), the second of which also needs the index to name what it passed over
// — so the two cannot come to answer one list differently.
//
// The pick is made in two places, not one: a whole batch of stored rows is picked for in SQL
// instead, by attributionBatchQuery's `distinct on (pr_repo, pr_number) … order by … rank, n, key`
// over its `held` term, which keeps a row's first-source candidates that are held, since
// resolving a batch row by row would pay for a repository walk per row (AttributePullRequests).
// The two are one rule because they read one ranked list, one first source and one holding:
// attributionCandidates ranks and marks the first source, attributionHeld holds,
// attributionSubjects names the keys the walk resolves, and all three are spliced into both
// statements.
func attributedCandidate(candidates []attributionCandidate) int {
	return slices.IndexFunc(candidates, func(candidate attributionCandidate) bool {
		return candidate.first && candidate.held
	})
}

// AttributionResolveBatch is how many stored rows one reconcile pass resolves again
// (AttributePullRequests): a 28-day population is about 4,000 rows, so a pass costs a bounded
// few milliseconds however large the table grows, and the walk comes round to every row in a
// handful of passes at today's size.
const AttributionResolveBatch = 1000

// AttributePullRequests resolves again, from the database alone, the issue of up to limit stored
// pull requests whose inputs have been read, those checked longest ago first, and writes each
// one that changed: an issue that links a pull request, or cites one of its GitHub issues,
// credits it here; one that no longer does, and one that has moved to another repository,
// uncredits it. Every row it reads is marked checked now, so the next pass takes the rows after
// it. Answers how many rows it changed.
//
// One statement and one set-based update, whatever the batch's size: the candidates of every row
// are collected first, then one repository walk resolves all of their issues together
// (refs.IssueRepoTerms over the batch's whole candidate list), then each row takes its own best
// held candidate. Resolving inside the lateral instead would pay for a recursive walk per row —
// AttributionResolveBatch of them a pass.
func (a *Attributor) AttributePullRequests(ctx context.Context, pool *store.Pool, limit int) (int64, error) {
	var changed int64
	err := pool.QueryRow(ctx, a.batchQuery, limit).Scan(&changed)
	if err != nil {
		return 0, fmt.Errorf("attribute stored pull requests: %w", err)
	}
	return changed, nil
}

// PullRequestKey names one stored pull request: its repository and its number.
type PullRequestKey struct {
	Repo   string
	Number int
}

// ResolvedCandidate is one issue a stored pull request's sources name, as ResolvedCandidates
// answers it: the issue's key and its project ("" where no issue row holds the key); the
// repository the issue's own chain names (repo.owned of refs.IssueRepoTerms: itself or the
// nearest ancestor of its project that names one), under its canonical name, "" where none does,
// so the issue resolves to its project's default repository; whether that repository is the pull
// request's, as attributionHeld compares them; whether the chain's walk ran out of depth, which
// every reader of a resolved repository refuses; and whether the candidate is of the first source
// that names an issue, the only source a pick takes an issue from (attributedCandidate).
type ResolvedCandidate struct {
	Key, Project, Owned             string
	OwnedHere, TooDeep, FirstSource bool
}

// ResolvedCandidates reads through q, from the database alone, the ranked candidates of each of
// pulls whose attribution inputs are stored (resolvedCandidatesQuery), by pull request: every
// candidate its five sources name, best first, with what its holding is made of
// (ResolvedCandidate). A pull request whose inputs name nothing has no candidate, and one whose
// inputs are not stored, or that no row holds, is not in the answer. AttributePullRequests picks
// an issue using the project's current default repository. ResolvedCandidates returns the
// information a caller needs to make the pick against a different set of project defaults,
// without rewriting stored attribution. One statement, whatever the number of pull requests:
// one walk resolves every candidate's issue together, as the batch's does.
func (a *Attributor) ResolvedCandidates(ctx context.Context, q Querier, pulls []PullRequestKey) (map[PullRequestKey][]ResolvedCandidate, error) {
	resolved := map[PullRequestKey][]ResolvedCandidate{}
	if len(pulls) == 0 {
		return resolved, nil
	}
	repos, numbers := make([]string, 0, len(pulls)), make([]int32, 0, len(pulls))
	named := make(map[PullRequestKey]bool, len(pulls))
	for _, pull := range pulls {
		if !named[pull] {
			named[pull] = true
			repos, numbers = append(repos, pull.Repo), append(numbers, int32(pull.Number))
		}
	}
	rows, err := q.Query(ctx, a.resolvedQuery, repos, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the candidates of %d stored pull requests: %w", len(repos), err)
	}
	defer rows.Close()
	for rows.Next() {
		var pull PullRequestKey
		var key, project *string
		var candidate ResolvedCandidate
		if err := rows.Scan(&pull.Repo, &pull.Number, &key, &project, &candidate.Owned, &candidate.OwnedHere, &candidate.TooDeep, &candidate.FirstSource); err != nil {
			return nil, fmt.Errorf("read the candidates of %d stored pull requests: %w", len(repos), err)
		}
		if key == nil {
			resolved[pull] = nil
			continue
		}
		candidate.Key = *key
		if project != nil {
			candidate.Project = *project
		}
		resolved[pull] = append(resolved[pull], candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the candidates of %d stored pull requests: %w", len(repos), err)
	}
	return resolved, nil
}
