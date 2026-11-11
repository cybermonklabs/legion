package legionfacts

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Write upserts the rows and answers which issues it changed, by key, in the order Postgres
// wrote them. A row whose stored values already match is left alone: a sweep that finds nothing
// new rewrites nothing, and a check can count what moved.
//
// One statement, so a sweep of hundreds of issues is one round trip. The rows go in as arrays
// unnested into a row each, as the sampler's own write does (legion/store.go's
// RecordObservations), and the conflict's `do update` carries the whole row's comparison in its
// where clause, so Postgres decides what changed and `returning` answers exactly the rows it
// touched. updated_at moves only with a row that changed, because an unchanged row is never
// updated at all. The hours are compared as jsonb, so a Go encoding that ordered an object's keys
// differently from jsonb cannot read as a change.
//
// updated_at is the row's own UpdatedAt, the moment its facts were computed as of; a row that
// carries none takes the moment this write started. A waiting reason of "" is stored as null, the
// column's "waits on nothing": the rule never answers an empty reason, so the two cannot be
// confused.
func Write(ctx context.Context, q Querier, rows []Row) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	started := time.Now().UTC()
	project := rows[0].Project
	keys := make([]string, len(rows))
	openedAt := make([]time.Time, len(rows))
	closedAt := make([]*time.Time, len(rows))
	hoursByPhase := make([]string, len(rows))
	workHours := make([]float64, len(rows))
	reviewRounds := make([]int32, len(rows))
	rootKeys := make([]string, len(rows))
	treeDepth := make([]int32, len(rows))
	slotHours := make([]float64, len(rows))
	waitingReason := make([]string, len(rows))
	workFirstAt := make([]*time.Time, len(rows))
	workLastAt := make([]*time.Time, len(rows))
	updatedAt := make([]time.Time, len(rows))
	for index, row := range rows {
		if row.Project != project {
			return nil, fmt.Errorf("write the Legion facts: %s and %s are two projects in one write", project, row.Project)
		}
		hours, err := json.Marshal(storedHours(row.HoursByPhase))
		if err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", row.Issue, err)
		}
		keys[index] = row.Issue
		openedAt[index] = row.OpenedAt
		closedAt[index] = row.ClosedAt
		hoursByPhase[index] = string(hours)
		workHours[index] = row.WorkHours
		reviewRounds[index] = int32(row.ReviewRounds)
		rootKeys[index] = row.RootKey
		treeDepth[index] = int32(row.TreeDepth)
		slotHours[index] = row.SlotHours
		waitingReason[index] = row.WaitingReason
		workFirstAt[index] = row.WorkFirstAt
		workLastAt[index] = row.WorkLastAt
		updatedAt[index] = row.UpdatedAt
		if row.UpdatedAt.IsZero() {
			updatedAt[index] = started
		}
	}
	result, err := q.Query(ctx, `
		insert into issue_facts (
			project_key, issue_key, opened_at, closed_at, hours_by_phase, work_hours,
			review_rounds, root_key, tree_depth, slot_hours, waiting_reason,
			work_first_at, work_last_at, updated_at)
		select $1, given.issue_key, given.opened_at, given.closed_at, given.hours_by_phase::jsonb,
		       given.work_hours, given.review_rounds, given.root_key, given.tree_depth,
		       given.slot_hours, nullif(given.waiting_reason, ''), given.work_first_at,
		       given.work_last_at, given.updated_at
		from unnest($2::text[], $3::timestamptz[], $4::timestamptz[], $5::text[],
		            $6::double precision[], $7::integer[], $8::text[], $9::integer[],
		            $10::double precision[], $11::text[], $12::timestamptz[], $13::timestamptz[],
		            $14::timestamptz[])
			as given(issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
			         root_key, tree_depth, slot_hours, waiting_reason, work_first_at, work_last_at,
			         updated_at)
		on conflict (project_key, issue_key) do update set
			opened_at = excluded.opened_at,
			closed_at = excluded.closed_at,
			hours_by_phase = excluded.hours_by_phase,
			work_hours = excluded.work_hours,
			review_rounds = excluded.review_rounds,
			root_key = excluded.root_key,
			tree_depth = excluded.tree_depth,
			slot_hours = excluded.slot_hours,
			waiting_reason = excluded.waiting_reason,
			work_first_at = excluded.work_first_at,
			work_last_at = excluded.work_last_at,
			updated_at = excluded.updated_at
		where (issue_facts.opened_at, issue_facts.closed_at, issue_facts.hours_by_phase,
		       issue_facts.work_hours, issue_facts.review_rounds, issue_facts.root_key,
		       issue_facts.tree_depth, issue_facts.slot_hours, issue_facts.waiting_reason,
		       issue_facts.work_first_at, issue_facts.work_last_at)
		  is distinct from
		      (excluded.opened_at, excluded.closed_at, excluded.hours_by_phase,
		       excluded.work_hours, excluded.review_rounds, excluded.root_key,
		       excluded.tree_depth, excluded.slot_hours, excluded.waiting_reason,
		       excluded.work_first_at, excluded.work_last_at)
		returning issue_key
	`, project, keys, openedAt, closedAt, hoursByPhase, workHours, reviewRounds, rootKeys,
		treeDepth, slotHours, waitingReason, workFirstAt, workLastAt, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	changed := []string{}
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
		}
		changed = append(changed, key)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
	}
	return changed, nil
}

// storedHours is the hours an issue's row stores: a nil map is written as the empty object, which
// is what an issue whose work took no measured time holds, and never as the JSON null.
func storedHours(hours map[string]float64) map[string]float64 {
	if hours == nil {
		return map[string]float64{}
	}
	return hours
}

// Read answers the stored rows of a project, by issue key. It is the read every consumer of the
// facts makes: rows, and no computation.
func Read(ctx context.Context, q Querier, project string) (map[string]Row, error) {
	return readRows(ctx, q, project, nil)
}

// ReadIssues answers the stored rows of the named issues, by issue key. An issue with no stored
// row is left out of the answer rather than an error: a row is written the first time that
// issue's facts are computed.
func ReadIssues(ctx context.Context, q Querier, project string, keys []string) (map[string]Row, error) {
	if len(keys) == 0 {
		return map[string]Row{}, nil
	}
	return readRows(ctx, q, project, keys)
}

// readRows is the body the two reads share. A nil keys reads the whole project.
func readRows(ctx context.Context, q Querier, project string, keys []string) (map[string]Row, error) {
	where := ""
	args := []any{project}
	if keys != nil {
		where = " and issue_key = any($2)"
		args = append(args, keys)
	}
	result, err := q.Query(ctx, `
		select issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
		       root_key, tree_depth, slot_hours, coalesce(waiting_reason, ''),
		       work_first_at, work_last_at, updated_at
		from issue_facts
		where project_key = $1`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	rows := map[string]Row{}
	for result.Next() {
		row := Row{Project: project}
		// opened_at is a nullable column, which a row this package wrote always fills.
		var opened *time.Time
		var hours []byte
		if err := result.Scan(&row.Issue, &opened, &row.ClosedAt, &hours, &row.WorkHours,
			&row.ReviewRounds, &row.RootKey, &row.TreeDepth, &row.SlotHours, &row.WaitingReason,
			&row.WorkFirstAt, &row.WorkLastAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
		}
		if opened != nil {
			row.OpenedAt = *opened
		}
		if err := json.Unmarshal(hours, &row.HoursByPhase); err != nil {
			return nil, fmt.Errorf("read the Legion facts of %s: %w", row.Issue, err)
		}
		rows[row.Issue] = row
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	return rows, nil
}
