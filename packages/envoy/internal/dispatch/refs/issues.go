package refs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// IssueDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (the api package's refuseParentTooDeep), so it bounds
// every recursive issue walk — the ancestor walk below, the children subtree walk and its
// inheriting form, the components, repository and priority walks that climb a whole read's
// chains together, the architecture tree's containment walks, and the legion-label walk — so the
// queries terminate even if a raced reparent ever commits a cycle. api's parentDepthCap is this
// number, and the walks it builds as SQL text splice it in.
const IssueDepthCap = 32

// issueDepthCapSQL is IssueDepthCap spliced into the walks built as SQL text.
var issueDepthCapSQL = strconv.Itoa(IssueDepthCap)

// IssueAncestors is the parent chain of the issue key, parent first up to the root, read through
// q: the issues above it on `child_of`, which is issues.parent_key (the graph_edges view's
// child_of arm). A root issue, an issue no row has, and the empty key answer []. This is the
// server's one recursive ancestor walk: an issue event's ancestors (api's issueAncestors), a
// reparent's cycle refusal (api's validateIssueParent, which reads it from the proposed parent),
// a block pointer's ancestor rule (docs' refuseBlockPointers) and the plan of a status change that
// passes include_parent (api's readStatusChange) are all this query.
//
// It binds no visibility predicate and takes no acting person, because every caller is a rule of
// a write - those four, the near-copy gate's exclusion of the new issue's chain and the scope a
// result's check resolves in - and a rule that skipped an issue it may not see would let a cycle
// through that issue, route an event short of its root, or leave an ancestor out of work above an
// issue in work. The status change's plan moves each ancestor that is not in work into work, so
// that the parent status rule (R24) holds. It names those ancestors to the caller who asked to
// move them: its dry run and its answer list them. It leaves out an ancestor in a private project
// that the caller's credential does not name (access.CredentialCovers), and each ancestor above
// it, which the plan reaches only through it: it does not move those ancestors and names them
// nowhere, and the parent status rule still judges the private one.
func IssueAncestors(ctx context.Context, q Queryer, key string) ([]string, error) {
	ancestors := []string{}
	if key == "" {
		return ancestors, nil
	}
	if err := q.QueryRow(ctx, `
		with recursive ancestors as (
			select p.key, p.parent_key, 1 as depth
			from issues i join issues p on p.key = i.parent_key where i.key = $1
			union
			select i.key, i.parent_key, a.depth + 1
			from issues i join ancestors a on i.key = a.parent_key
			where a.depth < $2
		)
		select coalesce(array_agg(key order by depth), '{}') from ancestors
	`, key, IssueDepthCap).Scan(&ancestors); err != nil {
		return nil, fmt.Errorf("read the ancestors of %s: %w", key, err)
	}
	return ancestors, nil
}

// IssueDescendants is every issue below the issue key on `child_of`, at any status, read through
// q: its children, their children, and so on, bounded by IssueDepthCap like the ancestor walk
// above, so the query terminates even if a raced reparent ever commits a cycle. The key itself is
// not in the answer. An issue with no child, an issue no row has, and the empty key answer [].
// This is the server's one recursive descendant walk over whole subtrees that answers a reader:
// api's coverage read is this query. IssueDescendantRows is the same walk for a rule of a write,
// which answers no reader and so binds no visibility predicate. api's issueChildrenStatement is a
// different read — one row per direct child, with a rollup over that child's own subtree — and
// stays its own.
//
// It answers for who: an issue who may not see is not in the answer, and the walk does not pass
// below it, so neither it nor the issues it holds are reached through it (VisibleKind).
func IssueDescendants(ctx context.Context, q Queryer, who model.Actor, key string) ([]string, error) {
	descendants := []string{}
	if key == "" {
		return descendants, nil
	}
	if err := q.QueryRow(ctx, `
		with recursive descendants as (
			select c.key, 1 as depth from issues c where c.parent_key = $1
			  and `+VisibleKind("issue", "c.key", "$3")+`
			union
			select c.key, d.depth + 1
			from issues c join descendants d on c.parent_key = d.key
			where d.depth < $2 and `+VisibleKind("issue", "c.key", "$3")+`
		)
		select coalesce(array_agg(key order by key), '{}') from descendants
	`, key, IssueDepthCap, VisibilityArg(who)).Scan(&descendants); err != nil {
		return nil, fmt.Errorf("read the descendants of %s: %w", key, err)
	}
	return descendants, nil
}

// IssueDescendant is an issue below another one on `child_of`, as IssueDescendantRows reads it:
// its key, its parent, its status, its depth, 1 for a child of the issue the walk starts at, and
// its project, which may differ from its parent's.
type IssueDescendant struct {
	Key     string
	Parent  string
	Status  string
	Depth   int
	Project string
}

// IssueDescendantRows is every issue below the issue key on `child_of`, at any status, read
// through q: its children, their children, and so on, bounded by IssueDepthCap like the ancestor
// walk above, so the query terminates even if a raced reparent ever commits a cycle. Each row
// holds the issue's parent, its status, its depth and its project. The rows come nearest first,
// and at one depth in the order of their projects and numbers. The key itself is not in the
// answer. An issue with no child, an issue no row has, and the empty key answer []. The read of a
// status change's plan (api's readStatusChange) and the check of a move's subtree (api's
// refuseSpecWriterOfMovedSubtree, spec_writer.go) are this query.
//
// It binds no visibility predicate and takes no acting person, for the reason IssueAncestors
// gives. Every caller is a rule of a write: the plan of a status change, which moves or unlinks
// every open issue under the parent so that the parent status rule (R24) holds, and a plan that
// skipped an issue it may not see would leave that issue open under a done parent; and the check
// of a move, which must see every issue of the moved subtree that Legion runs. The plan names the
// issues it will move or unlink to the caller who asked to move or unlink them: its dry run and
// its answer list them. It leaves out an issue in a private project that the caller's credential
// does not name (access.CredentialCovers), and each issue below it, which the plan reaches only
// through it: it does not move or unlink those issues and names them nowhere, and the parent
// status rule still judges the private one.
//
// The walk carries each issue's project and number, which its order needs, from the row it reads
// already, rather than join its keys back to issues: the planner has no estimate of a recursive
// CTE's size, so such a join hashes the whole table (api's issueChildrenStatement says what it
// cost there). The two columns follow from the key, so `union` removes the rows it removes
// without them.
func IssueDescendantRows(ctx context.Context, q Queryer, key string) ([]IssueDescendant, error) {
	return readIssueDescendantRows(ctx, q, key, issueDescendantRowsStatement, "")
}

// IssueInheritingDescendantRows is every issue below the issue key on `child_of` that takes its
// priority from it: the children whose own priority is null, their children whose own priority is
// null, and so on, at any status and in any project, read through q. The descent stops below an
// issue that has a priority of its own, and that issue is not in the answer either: it resolves to
// its own priority, and every issue under it to that one or to one nearer, whatever the key's
// issue holds. So each issue of the answer resolves to what the key's issue passes down, its own
// priority with its key, else the priority and the source it resolves to itself
// (IssuePriorityTerms), and a write that moves what it passes down moves the resolved priority or
// its source of every one of them and of no other issue below it. It is IssueInheritingDescent
// from the key's issue, in IssueDescendantRows' rows and order: each issue's parent, its status,
// its depth and its project, nearest first, so each issue's parent comes before it and its
// ancestors follow from the rows before it and the key's own. It is bounded by IssueDepthCap, as
// that walk is, so the query terminates even if a raced reparent ever commits a cycle; an issue
// deeper than that is one IssuePriorityTerms' climb never reaches the key's issue from, so the
// bound leaves out no issue that takes its priority from it. The key itself is not in the answer.
// An issue with no such child, an issue no row has, and the empty key answer [].
//
// It binds no visibility predicate and takes no acting person, for the reason IssueAncestors
// gives: it is a rule of a write, which must reach every issue whose resolved priority the write
// moves, seen or not, and it answers no reader. Each of those issues takes its priority from the
// key's issue whatever project it is in, and a set that skipped one its writer may not see would
// leave that issue's resolved priority, to every reader that keeps one from the issue's events, as
// it was before the write. It names none of them to the writer.
func IssueInheritingDescendantRows(ctx context.Context, q Queryer, key string) ([]IssueDescendant, error) {
	return readIssueDescendantRows(ctx, q, key, issueInheritingDescendantRowsStatement, " that take its priority")
}

// IssueInheritingDescent is the recursive term, named term, of the descent from seeds to every
// issue under them that takes its priority from them, at any status and in any project: the
// children whose own priority is null, their children whose own priority is null, and so on. It
// stops below an issue that has a priority of its own and leaves that issue out, since it resolves
// to its own (J1 of dispatch://INFRA-5793), and it goes at most IssueDepthCap links down, the
// farthest IssuePriorityTerms' climb takes a priority, which bounds the walk whatever the rows hold.
// Each of seeds is a condition on the issue c, and the term holds each issue that meets one at
// depth 0, and each issue under one at its depth below it, with c.key, then extraColumns, more
// columns of c where it names any, then its depth. c.key is always the term's first column: the
// recursive arm joins each issue to its parent by it (c.parent_key = d.key), so the builder writes
// it itself and no caller can leave it out. Each seed is a select of its own, so a seed that
// repeats a partial index's predicate lets the planner read that index whole for it, and the
// issues under the seeds are read through issues_parent_key. It is a `union all`: an issue is
// reached only through its one parent, one link deeper than that parent, so the term holds no row
// twice unless two seeds hold one issue.
//
// It is the one descent by the priority-resolution rule: IssueInheritingDescendantRows descends
// through it from one issue, and delivery's openP0Descent from every issue whose own priority is 0,
// so the stop and the cap of a write's events and of the open P0 reads are one text.
func IssueInheritingDescent(term, extraColumns string, seeds ...string) string {
	columns := `c.key`
	if extraColumns != "" {
		columns += `, ` + extraColumns
	}
	arms := make([]string, 0, len(seeds)+1)
	for _, seed := range seeds {
		arms = append(arms, `select `+columns+`, 0 as depth from issues c where `+seed)
	}
	arms = append(arms, `select `+columns+`, d.depth + 1
		from `+term+` d join issues c on c.parent_key = d.key
		where c.priority is null and d.depth < `+issueDepthCapSQL)
	return term + ` as (
		` + strings.Join(arms, `
		union all
		`) + `
	)`
}

// issueDescendantColumns are the columns of the issue c that the walks of IssueDescendantRows and
// IssueInheritingDescendantRows carry after its key, c.key, which IssueInheritingDescent writes
// first itself and IssueDescendantRows' walk writes before them; and issueDescendantRowsAnswer the
// select both answer their rows by, in IssueDescendant's order, nearest first and at one depth in
// the order of their projects and numbers, so the two answer the same rows the same way and differ
// only in where the walk stops. The answer leaves out depth 0, the issue IssueInheritingDescent
// starts from.
const (
	issueDescendantColumns    = `c.parent_key, c.status, c.project_key, c.number`
	issueDescendantRowsAnswer = `
		select key, parent_key, status, depth, project_key from descendants
		where depth > 0
		order by depth, project_key, number
	`
)

// The statements of IssueDescendantRows and IssueInheritingDescendantRows, $1 the key each starts
// at, built once, at start. IssueDescendantRows' walk starts at the key's children, at depth 1, and
// passes below every issue; IssueInheritingDescendantRows' is IssueInheritingDescent from the
// key's own issue, at depth 0.
var (
	issueDescendantRowsStatement = `
		with recursive descendants as (
			select c.key, ` + issueDescendantColumns + `, 1 as depth
			from issues c where c.parent_key = $1
			union
			select c.key, ` + issueDescendantColumns + `, d.depth + 1
			from issues c join descendants d on c.parent_key = d.key
			where d.depth < ` + issueDepthCapSQL + `
		)` + issueDescendantRowsAnswer
	issueInheritingDescendantRowsStatement = `
		with recursive ` + IssueInheritingDescent("descendants", issueDescendantColumns, "c.key = $1") +
		issueDescendantRowsAnswer
)

// readIssueDescendantRows reads statement, the statement of IssueDescendantRows or of
// IssueInheritingDescendantRows, from the issue key through q, and answers its rows; the empty key
// answers [] and reads nothing. A failure names the descendants of key, followed by what, which
// says which of them the walk keeps ("" for every one).
func readIssueDescendantRows(ctx context.Context, q Queryer, key, statement, what string) ([]IssueDescendant, error) {
	if key == "" {
		return []IssueDescendant{}, nil
	}
	rows, err := q.Query(ctx, statement, key)
	if err != nil {
		return nil, fmt.Errorf("read the descendants of %s%s: %w", key, what, err)
	}
	descendants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[IssueDescendant])
	if err != nil {
		return nil, fmt.Errorf("read the descendants of %s%s: %w", key, what, err)
	}
	return descendants, nil
}

// IssueRepoTerms resolve the repository of every issue of `subjects`, the one-column term of
// issue keys each statement that splices these defines before them (api's issueComponentsTerms
// reads the same one): the nearest issue of its own project on its parent chain (itself first)
// whose own `repo` is set, else the project's default repository (projects.default_repo), else
// null. The walk carries the subject's project and climbs only to a parent of that project, so
// it ends at the first ancestor of another project as at a root: one project's deployment works
// each issue, and a parent of another project passes on no repository. One walk climbs every
// subject's chain together and one join reads the defaults of every project at once, so a read
// of many issues pays one set of joins rather than a walk and a default lookup per row. A chain
// stops climbing at the first issue that names a repository or at IssueDepthCap links up, which
// writes never let a chain pass (api's refuseParentTooDeep): at most IssueDepthCap+1 rows per
// subject, at most one of them naming a repository, which is why `max` over a chain is the
// nearest repository and needs no order. A walk that reaches the cap with links still above it
// sets `too_deep`, and the reader of these columns refuses it rather than answer with the
// default (api's repoScan.resolve, IssueRepository below). Such a chain is a loop, or one a
// direct database write made, or one an earlier release's API built: migration 0113 refuses to
// apply while one exists, but an earlier release left serving after it, against the rollout
// rule (cmd/dispatch/README.md), can build one again. A paged listing resolves only the issues
// of its page, so such a chain fails the page it is on and no other (api/issue_queries.go,
// issueSummaryQuery).
//
// This is the server's one repository-resolution rule. The issue page resolves a page of issues
// through these terms (api's issue reads), the listing through IssueRepoTermsOn, the same answer
// read from the one walk its other values share, IssueRepository resolves one key through them,
// and delivery's attribution splices them over the candidate keys of a pull request to hold its
// issue to the repository it is in, so the results read and the pull-request head attribution
// can never disagree with the issue page.
var IssueRepoTerms = `repo_chain as (
		select s.key as start, i.project_key, i.parent_key, i.repo, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, c.project_key, p.parent_key, p.repo, c.depth + 1
		from repo_chain c join issues p on p.key = c.parent_key and p.project_key = c.project_key
		where c.repo is null and c.depth < ` + issueDepthCapSQL + `
	), ` + issueRepoAnswer

// IssueRepoTermsOn are IssueRepoTerms over chain, an IssueChainTerm the statement defines before
// them, rather than over a walk of their own: repo_chain keeps the rows of chain that
// IssueRepoTerms' walk makes (repo_open), with that walk's columns, so `repo` answers for each
// subject what IssueRepoTerms answer, by the same text (issueRepoAnswer).
func IssueRepoTermsOn(chain string) string {
	return `repo_chain as (
		select c.start, c.project_key, c.parent_key, c.repo, c.depth
		from ` + chain + ` c
		where c.repo_open
	), ` + issueRepoAnswer
}

// issueRepoAnswer is the term `repo` of IssueRepoTerms and IssueRepoTermsOn: each subject's
// nearest repository and whether its walk ran out of depth, over the rows of repo_chain.
var issueRepoAnswer = `repo as (
		select c.start as key,
		       max(c.repo) as owned,
		       bool_or(c.depth = ` + issueDepthCapSQL + ` and c.repo is null and c.parent_key is not null) as too_deep
		from repo_chain c
		group by c.start
	)`

// IssueChainTerm is the term named chain: one walk up the parent chain of every issue that from
// names, which the statement's readers of those chains read in place of a walk each. from is a
// FROM list that names those issues, the subjects, as `i` with at least their key, parent_key,
// project_key, priority and repo: the issues of a one-column term of keys the statement defines
// before it (`subjects s join issues i on i.key = s.key`), or a term of issue rows it already
// holds, which the walk then starts from without reading the issues table for them. The listing
// resolves its rows' components, repository, priority and `advances` relations from one such term
// (api's issueSummaryQuery), so it reads the issues table for one walk rather than for four. Each
// row is one issue of a subject's chain: start, the subject; key, parent_key, priority and repo,
// that issue's own; project_key, the subject's project, carried up as IssueRepoTerms' walk
// carries it; depth, its links above the subject; and three flags, each saying whether one of the
// walks the term replaces makes the row:
//
//   - in_project: every issue from the subject up to this one is of the subject's project, the
//     rows of the components walk (api's issueComponentsTerms);
//   - repo_open: in_project, and no issue below this one on the chain names a repository, the
//     rows of IssueRepoTerms' walk (IssueRepoTermsOn);
//   - priority_open: no issue below this one on the chain has a priority of its own, the rows of
//     IssuePriorityTerms' walk (IssuePriorityTermsOn).
//
// Each flag is its walk's step condition carried one link up, so a reader that keeps the rows a
// flag holds reads exactly the rows that walk makes, with the same columns and depths. The walk
// itself stops at no flag: it climbs into a parent of any project and stops only IssueDepthCap
// links up, as far as a reader of every issue above climbs (api's issueAdvancesTerms), and is
// bounded by IssueDepthCap like every walk of the chain, so it terminates even if a raced reparent
// ever commits a cycle. A row carries its depth, so no two rows of one chain are equal and the
// walk is a `union all`.
func IssueChainTerm(chain, from string) string {
	return chain + ` as (
		select i.key as start, i.key, i.parent_key, i.project_key, i.priority, i.repo,
		       true as in_project, true as repo_open, true as priority_open, 0 as depth
		from ` + from + `
		union all
		select c.start, p.key, p.parent_key, c.project_key, p.priority, p.repo,
		       c.in_project and p.project_key = c.project_key,
		       c.repo_open and c.repo is null and p.project_key = c.project_key,
		       c.priority_open and c.priority is null,
		       c.depth + 1
		from ` + chain + ` c join issues p on p.key = c.parent_key
		where c.depth < ` + issueDepthCapSQL + `
	)`
}

// IssueRepoJoin attaches IssueRepoTerms' answer to the issue aliased `i` in the enclosing query,
// with the project row of every project those issues belong to, for its default repository. One
// row per issue: a key that is no issue has no chain and no row, which the columns below read as
// the default alone.
const IssueRepoJoin = `left join repo on repo.key = i.key
	left join projects repo_default on repo_default.key = i.project_key`

// IssueRepoResolved is the repository an issue resolves to, over IssueRepoJoin: the nearest one
// its chain names, else its project's default repository, else null. A statement that holds an
// issue to a repository rather than reading one — delivery's attributionHeld, which keeps a
// pull request's issue to the repository the pull request is in — compares against this
// expression, so it can never be a second copy of the rule drifting from this one.
const IssueRepoResolved = `coalesce(repo.owned, repo_default.default_repo)`

// IssueRepoTooDeep reports whether the walk ran out of depth with links still above it, which
// every reader of the resolved repository refuses rather than answer with the project's default
// (api's repoScan.resolve, IssueRepository below, delivery's attributionHeld).
const IssueRepoTooDeep = `coalesce(repo.too_deep, false)`

// IssueRepoColumns are the repository columns every issue read scans, in the order api's
// repoScan and IssueRepository below take them: the issue's own, the nearest one its chain names
// else its project's default, and whether the walk ran out of depth.
const IssueRepoColumns = `i.repo, ` + IssueRepoResolved + `, ` + IssueRepoTooDeep

// issueRepositoryQuery is IssueRepoTerms over a subjects term of one key, the shape api's
// issueRowStatement gives the issue page's single-key read. The text is built once, at
// start, rather than on every read: the concatenation is work a request would repeat for a
// statement that never differs, and pgx caches a prepared plan per statement text.
var issueRepositoryQuery = `
	with recursive subjects as (select $1::text as key), ` + IssueRepoTerms + `
	select ` + IssueRepoColumns + `
	from issues i ` + IssueRepoJoin + `
	where i.key = $1`

// IssueRepository is the repository the issue key resolves to, read through q: its own, the
// nearest one above it on `child_of` in its own project, or its project's default repository;
// nil when none resolves, and nil for an issue no row has and for the empty key. It runs
// IssueRepoTerms for one key, the terms the issue reads run for a whole page, so it answers
// exactly what the issue page shows; a chain the walk could not finish is an error rather than
// the project's default, which would be a wrong answer. The results route makes this read for
// the issue of a check.
func IssueRepository(ctx context.Context, q Queryer, key string) (*string, error) {
	if key == "" {
		return nil, nil
	}
	// own is the issue's own repository, which this read scans and does not answer: the columns
	// stay the issue reads' own set so the two cannot drift apart.
	var own, resolved *string
	var tooDeep bool
	if err := q.QueryRow(ctx, issueRepositoryQuery, key).Scan(&own, &resolved, &tooDeep); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve the repository of %s: %w", key, err)
	}
	if tooDeep {
		return nil, fmt.Errorf("resolve the repository of %s: its parent chain is deeper than %d links or loops", key, IssueDepthCap)
	}
	return resolved, nil
}

// IssuePriorityTerms resolve the priority of every issue of `subjects`, the one-column term of
// issue keys each statement that splices these defines before them, as IssueRepoTerms do: the
// issue's own priority where it has one, else that of the nearest issue above it on its parent
// chain that has one, in any project, else none (J1 of dispatch://INFRA-5793). The walk climbs
// every subject's chain together, itself first and then parent_key, as IssueRepoTerms' does, so
// a read of many issues pays one walk rather than one per row; but it climbs into a parent of any
// project, as api's issueAdvancesTerms does, since work under an issue of another project is that
// issue's work too. A chain stops climbing at the first issue whose own priority is set, or at
// IssueDepthCap links up, which writes never let a chain pass (api's refuseParentTooDeep): at most
// IssueDepthCap+1 rows per subject, at most one of them with a priority. A chain that reaches the
// cap with no priority found, a loop or one a direct database write made, resolves none: unlike
// the repository walk it marks nothing too deep and refuses nothing, since none is what an issue
// with no priority above it reads anyway, and a refusal would fail the whole read of every page
// that holds such an issue for want of a value that may be none.
//
// `priority_resolved` answers, for each subject that resolves a priority, that priority and
// `source`, the key of the issue it comes from where that is not the subject itself; a subject
// that resolves none has no row, which IssuePriorityJoin's left join reads as null in both
// (IssuePriorityColumns). This is the server's one priority-resolution rule, which a read takes
// one of two ways. The reads that climb, from an issue up to the priority it takes, splice these
// terms: the issue read resolves one issue through them (api's issueRowStatement), the listing
// its page through IssuePriorityTermsOn, the same answer read from the one walk its other values
// share (api's issueSummaryQuery), the p0 checks of a write one issue (api's
// issuePriorityStandingQuery), and every other read of issue rows the rows it reads
// (IssuePriorityJoin, IssuePriorityJoinOn); and the listing's priority filter climbs from its own
// candidates through the same walk (IssuePriorityTermsOver), so the rows a filter keeps and what
// they read can never disagree. The reads that descend, from the issues a priority comes from down
// to the issues that take it, go through IssueInheritingDescent, whose stop is this walk's, an own
// priority, and whose bound is this walk's cap: IssueInheritingDescendantRows, the issues whose
// resolved priority a write moves, and delivery's openP0Descent, the open P0 issues. No read
// writes the rule a third way.
//
// It binds no visibility predicate, as IssueRepoTerms binds none. No issue read of this release
// holds its reader to the projects it may see, so the priority of an issue above another is
// nothing the reads that splice these terms do not already answer of that issue: J1's rule that
// a reader who cannot see the project of the issue a priority comes from gets none is the read
// rule of dispatch://INFRA-3862, which no issue read binds yet, and whichever of the two merges
// second carries it. A rule of a write that reads these terms reads the chain whole, as J2 of
// dispatch://INFRA-5793 says the walks that decide whether a write makes work P0 do: a writer who
// cannot see a P0 issue's project still needs p0 to move or file work under it.
var IssuePriorityTerms = IssuePriorityTermsOver("subjects", "priority_resolved")

// IssuePriorityTermsOver is IssuePriorityTerms' walk over subjects, any one-column term of issue
// keys the statement defines before it, answering in the term named resolved, with its climb in
// resolved + "_chain", so one statement can resolve two sets of issues by the one rule: the
// listing resolves its page through IssuePriorityTermsOn and filters its candidates through these
// (api's issuePriorityFilterTerms).
func IssuePriorityTermsOver(subjects, resolved string) string {
	return resolved + `_chain as (
		select s.key as start, i.key, i.parent_key, i.priority, 0 as depth
		from ` + subjects + ` s join issues i on i.key = s.key
		union all
		select c.start, p.key, p.parent_key, p.priority, c.depth + 1
		from ` + resolved + `_chain c join issues p on p.key = c.parent_key
		where c.priority is null and c.depth < ` + issueDepthCapSQL + `
	), ` + issuePriorityAnswer(resolved)
}

// IssuePriorityTermsOn are IssuePriorityTerms over chain, an IssueChainTerm the statement defines
// before them, rather than over a walk of their own: priority_resolved_chain keeps the rows of
// chain that IssuePriorityTerms' walk makes (priority_open), with that walk's columns, so
// `priority_resolved` answers for each subject what IssuePriorityTerms answer, by the same text
// (issuePriorityAnswer), and IssuePriorityJoin reads it as it reads theirs.
func IssuePriorityTermsOn(chain string) string {
	return `priority_resolved_chain as (
		select c.start, c.key, c.parent_key, c.priority, c.depth
		from ` + chain + ` c
		where c.priority_open
	), ` + issuePriorityAnswer("priority_resolved")
}

// issuePriorityAnswer is the term named resolved of IssuePriorityTermsOver and
// IssuePriorityTermsOn: each subject's priority and its source, over the rows of resolved +
// "_chain".
func issuePriorityAnswer(resolved string) string {
	return resolved + ` as (
		select c.start as key, max(c.priority) as priority,
		       max(c.key) filter (where c.depth > 0) as source
		from ` + resolved + `_chain c
		where c.priority is not null
		group by c.start
	)`
}

// IssuePriorityJoinOn attaches IssuePriorityTerms' answer to the issue whose key is keyExpr, an
// expression over the enclosing query's rows: one row per issue, null for an issue that resolves
// no priority and for a key expression that is null. It is the one spelling of the join, so a
// statement that names its issue other than as `i` reaches the terms' answer through it, never by
// hand.
func IssuePriorityJoinOn(keyExpr string) string {
	return `left join priority_resolved on priority_resolved.key = ` + keyExpr
}

// IssuePriorityJoin is IssuePriorityJoinOn the issue aliased `i` in the enclosing query.
var IssuePriorityJoin = IssuePriorityJoinOn("i.key")

// IssuePriorityResolved is the priority the issue resolves to, over IssuePriorityJoin, null where
// it resolves none: the first of IssuePriorityColumns, alone, for a statement that orders or
// filters by the resolved priority or names the two columns apart (api's Inbox reads and open
// asks).
const IssuePriorityResolved = `priority_resolved.priority`

// IssuePrioritySource is the key of the issue IssuePriorityResolved comes from, over
// IssuePriorityJoin, null where it is the issue's own or there is none: the second of
// IssuePriorityColumns.
const IssuePrioritySource = `priority_resolved.source`

// IssuePriorityColumns are the priority columns every issue read scans beside the issue's own
// priority, over IssuePriorityJoin: the priority the issue resolves to, and the key of the issue
// it comes from, null where it is the issue's own; both null where it resolves none. They are the
// issue's resolved_priority and priority_from (model.Issue).
const IssuePriorityColumns = IssuePriorityResolved + `, ` + IssuePrioritySource
