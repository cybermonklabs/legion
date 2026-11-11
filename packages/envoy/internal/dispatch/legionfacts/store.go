package legionfacts

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ProjectWrite is one project's computed facts and the set the pass that computed them stated:
// what WriteProject writes in one call. The project stands beside the rows rather than being
// read out of them, because a project whose rows are all stale answers no row at all and still
// owes its deletion.
//
// Stated is the set of keys the pass computed over, inside which it may delete a stale row, and
// Whole says the pass read the project whole (ComputeProject), whose stated set is every issue
// the project has and whose Stated is therefore empty.
type ProjectWrite struct {
	Project string
	Rows    []Row
	Stated  []string
	Whole   bool
}

// WriteProject writes one project's computed facts: it upserts the rows the computation answered
// (upsert) and deletes the stored rows inside the stated set the computation answered none for
// (deleteStaleRows, or deleteStaleProjectRows for a pass that read the project whole). It answers
// every key whose stored row moved, the keys it deleted after the keys it wrote, because a row
// that went is a change of the stored facts as much as a row that moved.
//
// A stored row means Legion worked the issue (dispatch://INFRA-2915), and an issue loses its
// phase work as well as gains it: a session's newest title decides which issue that session's
// events work (legion.PhaseWork), so a role session that writes next on another issue under
// that issue's title takes the work with it, and the issue it left carries none. The deletion
// is what takes that issue's row away; an upsert alone leaves it standing for ever.
//
// It is the one write of the facts: the consumer's pass makes it for each project of the pass,
// inside the transaction that moves the cursors (consumer.go's writePass), and a caller that
// holds one project's rows makes it alone.
func WriteProject(ctx context.Context, q Querier, write ProjectWrite) ([]string, error) {
	written, err := upsert(ctx, q, write.Project, write.Rows)
	if err != nil {
		return nil, err
	}
	kept := computedKeys(write.Rows)
	deleted := []string{}
	if write.Whole {
		deleted, err = deleteStaleProjectRows(ctx, q, write.Project, kept)
	} else {
		deleted, err = deleteStaleRows(ctx, q, write.Project, write.Stated, kept)
	}
	if err != nil {
		return nil, err
	}
	return append(written, deleted...), nil
}

// computedKeys is the issue of each row: the keys the computation answered a row for, which the
// deletion keeps.
func computedKeys(rows []Row) []string {
	keys := make([]string, len(rows))
	for index, row := range rows {
		keys[index] = row.Issue
	}
	return keys
}

// upsert writes the rows of the project and answers which issues it changed, by key, in the
// order Postgres wrote them. It deletes nothing, so its caller takes the stale rows away
// (WriteProject). A row whose stored values already match is left alone: a sweep that finds
// nothing new rewrites nothing, and a check can count what moved.
//
// The project is the caller's and not the first row's: a write of no row still belongs to a
// project, and the loop below refuses a row of any other project, so the rows and the project
// this writes into cannot disagree.
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
func upsert(ctx context.Context, q Querier, project string, rows []Row) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	started := time.Now().UTC()
	keys := computedKeys(rows)
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

// The rule both deletions below keep, and the one that matters more than the deletion itself: a
// pass deletes only inside the set of keys it computed over, and never the row of an issue it
// did not compute. A pass states that set before it writes - the keys its feeds named, their
// ancestors and the trees a moved parent link touched (consumer.go's rowsToCompute), or every
// issue of the project where it read the project whole - and a key of that set its computation
// answered no row for is a row to delete. A stored row outside the stated set is a row this
// pass knows nothing of: the pass that states it computes it, and until then it stands.

// deleteStaleRows deletes the stored rows of the stated keys that computed holds no row for, and
// answers the keys it deleted. It is the deletion of a pass that computed a set of keys: stated
// is the set the pass considered, computed is the keys its computation answered rows for, and no
// row outside stated is read at all.
func deleteStaleRows(ctx context.Context, q Querier, project string, stated, computed []string) ([]string, error) {
	if len(stated) == 0 {
		return nil, nil
	}
	return deleteRows(ctx, q, project, `
		delete from issue_facts
		where project_key = $1 and issue_key = any($2) and not (issue_key = any($3))
		returning issue_key
	`, project, stated, computed)
}

// deleteStaleProjectRows deletes the stored rows of the project that computed holds no row for,
// and answers the keys it deleted. It is the deletion of a pass that computed the project whole
// (ComputeProject), whose stated set is every issue the project has, because that is the
// population such a computation reads (metrics.ReadProject). A stored row whose key names no
// issue of the project was therefore computed by nothing, and this deletion leaves it where it
// stands.
func deleteStaleProjectRows(ctx context.Context, q Querier, project string, computed []string) ([]string, error) {
	return deleteRows(ctx, q, project, `
		delete from issue_facts
		where project_key = $1 and not (issue_key = any($2))
		  and exists (select 1 from issues
		              where issues.project_key = $1 and issues.key = issue_facts.issue_key)
		returning issue_key
	`, project, computed)
}

// deleteRows runs one of the two deletions and answers the keys it deleted, in the order
// Postgres deleted them. Each row it deletes logs a delivery change of kind issue_facts carrying
// that row's day span, which the table's own trigger writes on a delete as it does on a write
// (migration 20261111093100), so every reader of the facts is told the row went.
func deleteRows(ctx context.Context, q Querier, project, statement string, args ...any) ([]string, error) {
	result, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	deleted := []string{}
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
		}
		deleted = append(deleted, key)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
	}
	return deleted, nil
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
