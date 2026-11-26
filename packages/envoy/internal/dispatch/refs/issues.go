package refs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// IssueDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (the api package's refuseParentTooDeep), so it bounds
// every recursive issue walk — the ancestor walk below, the children subtree walk, the
// components and repository walks that climb a whole read's chains together, the architecture
// tree's containment walks, and the legion-label walk — so the queries terminate even if a raced
// reparent ever commits a cycle. api's parentDepthCap is this number, and the walks it builds as
// SQL text splice it in.
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
// a write - those four and the scope a result's check resolves in - and a rule that skipped an
// issue it may not see would let a cycle through that issue, route an event short of its root, or
// leave an ancestor out of work above an issue in work. The status change's plan moves each
// ancestor that is not in work into work, so that the parent status rule (R24) holds. It names
// those ancestors to the caller who asked to move them: its dry run and its answer list them, and
// its 403 PROJECT_SCOPE names one that is in a project the caller's credential may not write. The
// near-copy gate reads the chain as its caller sees it (IssueAncestorsAsSeen), since it runs before
// the create knows its parent is one the caller sees.
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

// IssueAncestorsAsSeen is IssueAncestors for a write that names key on behalf of who before it
// knows who may see it: the near-copy gate of a create with a parent, which runs before the
// create's transaction. An issue who may not see answers [], as an issue no row has does, in the
// same one statement, so a parent of a project who may not see costs the rows a parent nobody
// holds costs. Only the walk's first issue is judged: a parent and the issues above it are of one
// project, since a parent names an issue of its child's project and no route moves an issue
// between projects.
func IssueAncestorsAsSeen(ctx context.Context, q Queryer, who model.Actor, key string) ([]string, error) {
	ancestors := []string{}
	if key == "" {
		return ancestors, nil
	}
	if err := q.QueryRow(ctx, `
		with recursive ancestors as (
			select p.key, p.parent_key, 1 as depth
			from issues i join issues p on p.key = i.parent_key
			where i.key = $1 and `+VisibleKind("issue", "i.key", "$3")+`
			union
			select i.key, i.parent_key, a.depth + 1
			from issues i join ancestors a on i.key = a.parent_key
			where a.depth < $2
		)
		select coalesce(array_agg(key order by depth), '{}') from ancestors
	`, key, IssueDepthCap, VisibilityArg(who)).Scan(&ancestors); err != nil {
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
// its answer list them, and its 403 PROJECT_SCOPE names one that is in a project the caller's
// credential may not write.
//
// The walk carries each issue's project and number, which its order needs, from the row it reads
// already, rather than join its keys back to issues: the planner has no estimate of a recursive
// CTE's size, so such a join hashes the whole table (api's issueChildrenStatement says what it
// cost there). The two columns follow from the key, so `union` removes the rows it removes
// without them.
func IssueDescendantRows(ctx context.Context, q Queryer, key string) ([]IssueDescendant, error) {
	if key == "" {
		return []IssueDescendant{}, nil
	}
	rows, err := q.Query(ctx, `
		with recursive descendants as (
			select c.key, c.parent_key, c.status, c.project_key, c.number, 1 as depth
			from issues c where c.parent_key = $1
			union
			select c.key, c.parent_key, c.status, c.project_key, c.number, d.depth + 1
			from issues c join descendants d on c.parent_key = d.key
			where d.depth < $2
		)
		select key, parent_key, status, depth, project_key from descendants
		order by depth, project_key, number
	`, key, IssueDepthCap)
	if err != nil {
		return nil, fmt.Errorf("read the descendants of %s: %w", key, err)
	}
	descendants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[IssueDescendant])
	if err != nil {
		return nil, fmt.Errorf("read the descendants of %s: %w", key, err)
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
// through these terms (api's issue reads), IssueRepository resolves one key through them, and
// delivery's attribution splices them over the candidate keys of a pull request to hold its
// issue to the repository it is in, so the results read and the pull-request head attribution
// can never disagree with the issue page.
var IssueRepoTerms = `repo_chain as (
		select s.key as start, i.project_key, i.parent_key, i.repo, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, c.project_key, p.parent_key, p.repo, c.depth + 1
		from repo_chain c join issues p on p.key = c.parent_key and p.project_key = c.project_key
		where c.repo is null and c.depth < ` + issueDepthCapSQL + `
	), repo as (
		select c.start as key,
		       max(c.repo) as owned,
		       bool_or(c.depth = ` + issueDepthCapSQL + ` and c.repo is null and c.parent_key is not null) as too_deep
		from repo_chain c
		group by c.start
	)`

// IssueRepoJoin attaches IssueRepoTerms' answer to the issue aliased `i` in the enclosing query,
// with the project row of every project those issues belong to, for its default repository. One
// row per issue: a key that is no issue has no chain and no row, which the columns below read as
// the default alone.
const IssueRepoJoin = `left join repo on repo.key = i.key
	left join projects repo_default on repo_default.key = i.project_key`

// projectDefaultRepoSQL is the default repository of the project projectExpr names, read as
// IssueRepoJoin reads it (projects.default_repo), null where it has none: the repository an issue
// of that project resolves to when its chain names none. RepositoryClause's index path
// (repositorySeen) reads it, so the default comes from the column IssueRepoJoin reads.
func projectDefaultRepoSQL(projectExpr string) string {
	return `(select repo_default.default_repo from projects repo_default where repo_default.key = (` + projectExpr + `))`
}

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
