package record

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// issueWritesKey is the context key of the issues one transaction's writes touched.
type issueWritesKey struct{}

// issueWrites is each issue one transaction's writes touched, in the order of the first write of
// each, and its record as it stood before that write (nil for an issue that had none).
type issueWrites struct {
	order  []string
	before map[string]*Issue
}

// WithIssueWrites answers ctx in which the store keeps each issue a write touches — its record
// (PutIssue) or its pull request (PutPullRequest) — with the record as it stood before the first
// such write: IssuesWritten answers the keys, and IssueBefore the record of each. A ctx that keeps
// them already is answered as it is, so the writes of one transaction are kept once whoever asks
// for them.
func WithIssueWrites(ctx context.Context) context.Context {
	if _, ok := ctx.Value(issueWritesKey{}).(*issueWrites); ok {
		return ctx
	}
	return context.WithValue(ctx, issueWritesKey{}, &issueWrites{before: map[string]*Issue{}})
}

// IssuesWritten is the key of each issue a write in ctx touched, in the order of the first write of
// each: nothing for a ctx that keeps no writes (WithIssueWrites).
func IssuesWritten(ctx context.Context) []string {
	writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites)
	if !ok {
		return nil
	}
	return slices.Clone(writes.order)
}

// IssueBefore is the record of key as it stood before the first write in ctx that touched it, and
// whether one did.
func IssueBefore(ctx context.Context, key string) (*Issue, bool) {
	writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites)
	if !ok {
		return nil, false
	}
	before, written := writes.before[key]
	return before, written
}

// noteIssueWrite keeps key among the issues the writes of ctx touched, with its record as it
// stands now, before the write that calls it: once per key, so the record kept is the one before
// the transaction's first write of it. A ctx that keeps no writes keeps nothing, and reads nothing.
func (s *Postgres) noteIssueWrite(ctx context.Context, tx pgx.Tx, key string) error {
	writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites)
	if !ok {
		return nil
	}
	if _, seen := writes.before[key]; seen {
		return nil
	}
	before, err := s.Issue(ctx, tx, key)
	if err != nil {
		return fmt.Errorf("read %s before its write: %w", key, err)
	}
	writes.before[key] = before
	writes.order = append(writes.order, key)
	return nil
}

// IssueStepRun is one place of an issue the store holds a row of starts for: the place, the
// latest start's count, agent, moment and run, and the generation that start's kept write names
// (0 where the row keeps none: a row only an end of a run no start counted wrote).
type IssueStepRun struct {
	Issue string
	Place StepPlace
	StepStart
	Generation uint64
}

// StepRuns is every row of starts of each of issues, by issue and place, in one read of the
// table's key: what the end of a step's run reads of the runs of the issues one fact touched
// (workflow's settleRuns). No issues read nothing.
func (s *Postgres) StepRuns(ctx context.Context, tx pgx.Tx, issues []string) ([]IssueStepRun, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select issue, task, kind, place, starts, agent, started_at, run,
		coalesce((record->>'generation')::bigint, 0) from step_starts where issue = any($1) order by issue, kind, place`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
	}
	defer rows.Close()
	var runs []IssueStepRun
	for rows.Next() {
		var run IssueStepRun
		var at *time.Time
		if err := rows.Scan(&run.Issue, &run.Place.Task, &run.Place.Kind, &run.Place.Place, &run.Starts, &run.Agent, &at, &run.Run, &run.Generation); err != nil {
			return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
		}
		if at != nil {
			run.StartedAt = at.UTC()
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
	}
	return runs, nil
}
