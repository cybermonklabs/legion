package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// UnownedIssue is one open P0 issue with neither a claim nor a route.
type UnownedIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ListUnownedP0 is the "P0 issues with no owner" KPI (CONTRACT.md "KPI targets"): every issue with
// priority 0, status other than done, claimed_by null and route null, across every project the
// viewer sees (access.ProjectClause over the visibility argument viewer, refs.VisibilityArg), in
// project then number order (the prototype's collector/src/collector/dispatch.py:157-170
// unowned_p0, without its fetch of three named projects). It follows neither the measures' window
// nor their facets.
func ListUnownedP0(ctx context.Context, pool *store.Pool, viewer any) ([]UnownedIssue, error) {
	rows, err := pool.Query(ctx, `
		select key, title from issues
		where priority = 0 and status <> 'done' and claimed_by is null and route is null
			and `+access.ProjectClause("project_key", "$1")+`
		order by project_key, number
	`, viewer)
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
