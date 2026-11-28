package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// UnownedIssue is one open P0 issue with neither a claim nor a route.
type UnownedIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// The reads of the open P0 issues: the issues not done whose resolved priority is 0, the issue's
// own priority where it has one, else that of the nearest issue above it that has one, at any
// status of that issue and in any project, within refs.IssueDepthCap links (J1 of
// dispatch://INFRA-5793, the rule refs.IssuePriorityTerms climbs). They descend rather than climb:
// from every issue whose own priority is 0, through issues_parent_key, to its children whose own
// priority is null, their children whose own priority is null, and so on, at most
// refs.IssueDepthCap links, at any status, so each issue they reach takes its P0 from the issue
// they descended from, and they read the few issues with an own P0 and the issues under them where
// a climb would start from every issue. The seeds repeat the predicates of the two partial indexes
// that hold them as the migrations spell them, issues_open_p0 (20261117100010) for the open ones and
// issues_done_p0 (20261127112234) for the done ones, which pass P0 to an open child all the same, so
// the planner proves each index from the statement's own clauses and reads it whole, and no other
// row of issues, for its seeds. Both are read, so neither is left for a later release to drop. The
// status, the claim, the route and a project are each a filter over what the descent reaches. A
// read of one project starts from every project's P0 issues too: an issue takes the priority of an
// issue of another project above it, so one project's open P0 issues are not all under its own,
// and Postgres computes a recursive term whole, so the project filter never narrows the seeds.
const (
	// openP0Predicate is issues_open_p0's predicate (migration 20261117100010), which the
	// descent's open seeds repeat.
	openP0Predicate = `priority = 0 and status <> 'done'`

	// doneP0Predicate is issues_done_p0's predicate (migration 20261127112234), which the
	// descent's done seeds repeat.
	doneP0Predicate = `priority = 0 and status = 'done'`

	// openP0Rule keeps, of the issues the descent reaches, the ones not done: the open P0 issues.
	openP0Rule = `status <> 'done'`

	// unownedP0Rule is the rule every unowned read below holds an issue to: an open P0, with no
	// claim and no route.
	unownedP0Rule = openP0Rule + ` and claimed_by is null and route is null`
)

// openP0Descent is the term every read of the open P0 issues starts with (here and openP0KeysQuery
// in store.go): refs.IssueInheritingDescent, the one descent by the priority-resolution rule, from
// two seeds, the open and the done issues whose own priority is 0. p0 holds each of them at depth
// 0, and each issue under one of them that takes its P0, at its depth below it, with the columns
// its readers read. The two seeds hold no issue in common, so no row comes twice.
var openP0Descent = `with recursive ` + refs.IssueInheritingDescent("p0",
	`c.key, c.project_key, c.number, c.title, c.status, c.claimed_by, c.route`,
	openP0Predicate, doneP0Predicate) + `
	`

var (
	unownedP0Query             = openP0Descent + `select key, title from p0 where ` + unownedP0Rule + ` order by project_key, number`
	projectUnownedP0Query      = openP0Descent + `select key, title from p0 where project_key = $1 and ` + unownedP0Rule + ` order by number`
	projectUnownedP0CountQuery = openP0Descent + `select count(*) from p0 where project_key = $1 and ` + unownedP0Rule
)

// ListUnownedP0 is the "P0 issues with no owner" KPI (CONTRACT.md "KPI targets"): every issue of
// project not done whose resolved priority is 0, its own or the one it takes from the nearest
// issue above it that has one, with claimed_by null and route null, in number order, or with
// project empty the same issues of every project, in project then number order (the prototype's
// collector/src/collector/dispatch.py:157-170 unowned_p0, without its fetch of three named
// projects). An issue of project that takes its P0 from an issue of another project is project's.
// The delivery measures read every project's. It follows neither the measures' window nor their
// facets; how many there are is the length of the list. It reads in q, the pool or a transaction
// whose snapshot an answer reads.
func ListUnownedP0(ctx context.Context, q Querier, project string) ([]UnownedIssue, error) {
	query, arguments := unownedP0Query, []any{}
	if project != "" {
		query, arguments = projectUnownedP0Query, []any{project}
	}
	rows, err := q.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []UnownedIssue{}
	for rows.Next() {
		var issue UnownedIssue
		if err := rows.Scan(&issue.Key, &issue.Title); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// CountUnownedP0 is how many issues ListUnownedP0 lists for project, which names one project,
// counted by the statement rather than read. An empty project counts no issue and answers 0,
// unlike ListUnownedP0's empty project, which lists every project's. Nothing in this tree calls it
// yet: it is here because issues_open_p0's migration (20261117100010) and the plan test of this
// package name its statement.
func CountUnownedP0(ctx context.Context, q Querier, project string) (int, error) {
	var count int
	err := q.QueryRow(ctx, projectUnownedP0CountQuery, project).Scan(&count)
	return count, err
}
