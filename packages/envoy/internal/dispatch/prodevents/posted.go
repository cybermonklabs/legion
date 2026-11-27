package prodevents

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// posted.go is the record's reads for the delivery evals of a project with an events source
// (evals' deploy_repos.go, dispatch://INFRA-978 R1 and R12): its posted deploys, the rows of kind
// deploy and source api, which a system posted to the project's route. The GitHub adapter's deploy
// rows, source github, are a repository's runs, which a source repository's rows count already.

// postedEnd is the end of the window PostedDeploysFrom reads: later than any deploy finishes.
var postedEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// FirstPostedAt reads, in q, the moment the first posted deploy of each of projects finished, in
// the order of projects, nil for a project that holds none; none where projects is empty. One
// statement probes, for each project, the low end of production_events_posted, (project_key,
// finished_at) where kind is deploy and source api (migration 20261123002035), so it reads one row
// of the record for each project whatever its length.
func FirstPostedAt(ctx context.Context, q delivery.Querier, projects []string) ([]*time.Time, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select given.at, first.finished_at
		from unnest($1::text[]) with ordinality as given(project_key, at)
		cross join lateral (
			select finished_at from production_events
			where project_key = given.project_key and kind = '`+KindDeploy+`' and source = '`+SourceAPI+`'
			  and finished_at is not null
			order by finished_at
			limit 1
		) first
		order by given.at
	`, projects)
	if err != nil {
		return nil, fmt.Errorf("read the first posted deploys of %d projects: %w", len(projects), err)
	}
	firsts := make([]*time.Time, len(projects))
	var at int64
	var first time.Time
	if _, err := pgx.ForEachRow(rows, []any{&at, &first}, func() error {
		firsts[at-1] = new(first)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the first posted deploys of %d projects: %w", len(projects), err)
	}
	return firsts, nil
}

// PostedDeploy is what the delivery evals read of a posted deploy: its row id, the moment it
// finished, its repository ("" for none), and the pull requests it names (`owner/repo#n`), in the
// order it names them.
type PostedDeploy struct {
	ID           string
	FinishedAt   time.Time
	Repo         string
	PullRequests []string
}

// PostedDeploysFrom reads, in q, project's posted deploys that finished from from on, newest finish
// first (ties by id, the greater first): one range of production_events_posted, (project_key,
// finished_at) where kind is deploy and source api (migration 20261123002035).
func PostedDeploysFrom(ctx context.Context, q delivery.Querier, project string, from time.Time) ([]PostedDeploy, error) {
	rows, err := q.Query(ctx, `
		select id::text, finished_at, repo, pull_requests from production_events
		where project_key = $1 and kind = '`+KindDeploy+`' and source = '`+SourceAPI+`'
		  and finished_at >= $2 and finished_at < $3
		order by finished_at desc, id desc
	`, project, from, postedEnd)
	if err != nil {
		return nil, fmt.Errorf("read the posted deploys of %s: %w", project, err)
	}
	defer rows.Close()
	var deploys []PostedDeploy
	for rows.Next() {
		var one PostedDeploy
		if err := rows.Scan(&one.ID, &one.FinishedAt, &one.Repo, &one.PullRequests); err != nil {
			return nil, fmt.Errorf("read the posted deploys of %s: %w", project, err)
		}
		deploys = append(deploys, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the posted deploys of %s: %w", project, err)
	}
	return deploys, nil
}

// ChangedRow is what a reader of the record's changes reads of a row one names: whether it is a
// posted deploy (kind deploy, source api), and the pull requests it names.
type ChangedRow struct {
	Posted       bool
	PullRequests []string
}

// ChangedRows reads, in q, the rows of project's record ids names, by each id as given: one
// statement, a probe of the table's key for each. An id the record does not hold, or one that is
// no uuid, is absent.
func ChangedRows(ctx context.Context, q delivery.Querier, project string, ids []string) (map[string]ChangedRow, error) {
	canonical := make(map[string]string, len(ids))
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err == nil {
			canonical[id] = parsed.String()
			keys = append(keys, parsed.String())
		}
	}
	held := map[string]ChangedRow{}
	if len(keys) == 0 {
		return held, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, kind = '`+KindDeploy+`' and source = '`+SourceAPI+`', pull_requests
		from production_events
		where project_key = $1 and id = any($2::uuid[])
	`, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the %d production events of %s the page names: %w", len(keys), project, err)
	}
	defer rows.Close()
	found := map[string]ChangedRow{}
	for rows.Next() {
		var id string
		var row ChangedRow
		if err := rows.Scan(&id, &row.Posted, &row.PullRequests); err != nil {
			return nil, fmt.Errorf("read the %d production events of %s the page names: %w", len(keys), project, err)
		}
		found[id] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the %d production events of %s the page names: %w", len(keys), project, err)
	}
	for given, key := range canonical {
		if row, ok := found[key]; ok {
			held[given] = row
		}
	}
	return held, nil
}
