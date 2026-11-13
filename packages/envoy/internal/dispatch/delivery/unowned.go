package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// UnownedIssue is one open P0 issue with neither a claim nor a route.
type UnownedIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// The two reads of ListUnownedP0. Each carries the rule as issues_unowned_p0's predicate spells
// it (migration 20261113082158), so the planner proves the index from the statement's own
// clauses and reads that index, which holds the unowned P0 issues alone, and no other row of
// issues: one project's issues are one range of its key, every project's the whole of a small
// index. One statement for each case, rather than one whose condition holds for an empty $1 or
// a matching project_key: under a generic plan, which knows no value of $1, that condition keys
// nothing, and the project read would walk every project's entries.
const (
	unownedP0Query = `
		select key, title from issues
		where priority = 0 and status <> 'done' and claimed_by is null and route is null
		order by project_key, number
	`
	projectUnownedP0Query = `
		select key, title from issues
		where project_key = $1 and priority = 0 and status <> 'done' and claimed_by is null and route is null
		order by number
	`
)

// ListUnownedP0 is the "P0 issues with no owner" KPI (CONTRACT.md "KPI targets"): every issue of
// project with priority 0, status other than done, claimed_by null and route null, in number
// order, or with project empty the same issues of every project, in project then number order
// (the prototype's collector/src/collector/dispatch.py:157-170 unowned_p0, without its fetch of
// three named projects). The delivery measures read every project's. It follows neither the
// measures' window nor their facets; how many there are is the length of the list.
func ListUnownedP0(ctx context.Context, pool *store.Pool, project string) ([]UnownedIssue, error) {
	query, arguments := unownedP0Query, []any{}
	if project != "" {
		query, arguments = projectUnownedP0Query, []any{project}
	}
	rows, err := pool.Query(ctx, query, arguments...)
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
