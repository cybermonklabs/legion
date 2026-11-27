package delivery

import "context"

// UnownedIssue is one open P0 issue with neither a claim nor a route.
type UnownedIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// The reads of the open P0 issues with no owner. Each carries issues_open_p0's predicate as the
// migration spells it (20261117100010, `priority = 0 and status <> 'done'`), so the planner proves
// the index from the statement's own clauses and reads that index, which holds the open P0 issues
// alone, and no other row of issues, with the rest of the rule (no claim, no route) a filter over
// what it reads: one project's issues are one range of its key, every project's the whole of a
// small index. One statement for each case, rather than one whose condition holds for an empty $1
// or a matching project_key: under a generic plan, which knows no value of $1, that condition keys
// nothing, and the project read would walk every project's entries.
const (
	// openP0Predicate is issues_open_p0's predicate (migration 20261117100010), which every read of
	// the index repeats (here and openP0KeysQuery in store.go).
	openP0Predicate = `priority = 0 and status <> 'done'`

	// unownedP0Rule is the rule every read below holds an issue to: the index's predicate, no claim
	// and no route.
	unownedP0Rule = openP0Predicate + ` and claimed_by is null and route is null`

	unownedP0Query             = `select key, title from issues where ` + unownedP0Rule + ` order by project_key, number`
	projectUnownedP0Query      = `select key, title from issues where project_key = $1 and ` + unownedP0Rule + ` order by number`
	projectUnownedP0CountQuery = `select count(*) from issues where project_key = $1 and ` + unownedP0Rule
)

// ListUnownedP0 is the "P0 issues with no owner" KPI (CONTRACT.md "KPI targets"): every issue of
// project with priority 0, status other than done, claimed_by null and route null, in number
// order, or with project empty the same issues of every project, in project then number order
// (the prototype's collector/src/collector/dispatch.py:157-170 unowned_p0, without its fetch of
// three named projects). The delivery measures read every project's. It follows neither the
// measures' window nor their facets; how many there are is the length of the list. It reads in
// q, the pool or a transaction whose snapshot an answer reads.
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
// unlike ListUnownedP0's empty project, which lists every project's. The day-row refresher calls
// it (evals' readRefresherEvents): an event that moves an issue's repository or parent moves the
// repository each unowned P0 issue below it is counted under, so the refresher writes unowned-p0's
// rows of today again for that issue's project only where the project holds such an issue now,
// which needs the count alone; a project that holds none has every repository's count at 0
// already. The plan test of this package holds its statement to the index issues_open_p0
// (migration 20261117100010).
func CountUnownedP0(ctx context.Context, q Querier, project string) (int, error) {
	var count int
	err := q.QueryRow(ctx, projectUnownedP0CountQuery, project).Scan(&count)
	return count, err
}
