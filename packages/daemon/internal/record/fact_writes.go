package record

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// issueWritesKey is the context key of the issues one transaction's writes touched.
type issueWritesKey struct{}

// issueWrites is each issue one transaction's writes touched, in the order of the first write of
// each, and whether a write changed a record of the project beside an issue's own (RecordsWritten).
type issueWrites struct {
	order []string
	seen  map[string]bool
	// records is whether a write changed a row the admission pass reads beside an issue's record,
	// pull request and hand-over fence: the copy of Dispatch's issue or of its claim, a root's ask,
	// a design gate or an issue's plan.
	records bool
}

// WithIssueWrites answers ctx in which the store keeps each issue a write touches — its record
// (PutIssue), its pull request (PutPullRequest) or its hand-over fence (PutHandover,
// DeleteHandover, DeleteHandoverAt), the inputs of the end of a step's run the store writes —
// which IssuesWritten answers, and whether a write changed any record of the project, which
// RecordsWritten answers. A ctx that keeps them already is answered as it is, so the writes of
// one transaction are kept once whoever asks for them.
func WithIssueWrites(ctx context.Context) context.Context {
	if _, ok := ctx.Value(issueWritesKey{}).(*issueWrites); ok {
		return ctx
	}
	return context.WithValue(ctx, issueWritesKey{}, &issueWrites{seen: map[string]bool{}})
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

// RecordsWritten says whether a write in ctx changed a record of the project, which is what a
// fact runs the admission pass for (admit's Admission.Apply): an issue's record, pull request or
// hand-over fence (IssuesWritten), or a row the pass reads beside them — the copy of what
// Dispatch says of an issue and of its claim (PutDispatchIssue, PutDispatchClaim), a root's ask
// (PutRootAsk), a design gate (PutGate, and a clear that deletes one) and an issue's plan, whose
// first step the pass starts (PutPlan, CarryPlan, DeletePlan, DeleteTasks, MoveTask, StopTask,
// PutReturnCounts). A write the row's own fence refuses changes nothing and is not kept. False
// for a ctx that keeps no writes (WithIssueWrites).
func RecordsWritten(ctx context.Context) bool {
	writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites)
	return ok && (len(writes.order) > 0 || writes.records)
}

// noteRecordWrite keeps that a write in ctx changed a record of the project beside an issue's own
// (RecordsWritten). A ctx that keeps no writes keeps nothing.
func noteRecordWrite(ctx context.Context) {
	if writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites); ok {
		writes.records = true
	}
}

// noteIssueWrite keeps key among the issues the writes of ctx touched, once per key, in the order
// of the transaction's first write of each. A ctx that keeps no writes keeps nothing.
func noteIssueWrite(ctx context.Context, key string) {
	writes, ok := ctx.Value(issueWritesKey{}).(*issueWrites)
	if !ok || writes.seen[key] {
		return
	}
	writes.seen[key] = true
	writes.order = append(writes.order, key)
}

// IssueStepRun is one place of an issue the store holds a row of starts for: the place, the
// latest start's count, agent, moment and run, the generation that start's kept write names, and
// that write (Kept), the record's identity, which the end of a run no step of its walk holds
// writes again (workflow's endOffWalk). Each writer of a row keeps the write with the count in its
// own transaction (RaiseStepStarts, OpenUncountedStep), so a committed row names one; the 0 that a
// row with none reads as is the read's default alone.
type IssueStepRun struct {
	Issue string
	Place StepPlace
	StepStart
	Generation uint64
	Kept       StepRecord
}

// StepRuns is every row of starts of each of issues, by issue and place, in one read of the
// table's key: what the end of a step's run reads of the runs of the issues one fact touched
// (workflow's settleRuns). No issues read nothing.
func (s *Postgres) StepRuns(ctx context.Context, tx pgx.Tx, issues []string) ([]IssueStepRun, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select issue, task, kind, place, starts, agent, started_at, run,
		coalesce((record->>'generation')::bigint, 0), record from step_starts where issue = any($1) order by issue, kind, place`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
	}
	defer rows.Close()
	var runs []IssueStepRun
	for rows.Next() {
		var run IssueStepRun
		var at *time.Time
		var kept []byte
		if err := rows.Scan(&run.Issue, &run.Place.Task, &run.Place.Kind, &run.Place.Place, &run.Starts, &run.Agent, &at, &run.Run, &run.Generation, &kept); err != nil {
			return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
		}
		if at != nil {
			run.StartedAt = at.UTC()
		}
		if kept != nil {
			if err := json.Unmarshal(kept, &run.Kept); err != nil {
				return nil, fmt.Errorf("read the kept start of %s %s/%s/%d: %w", run.Issue, run.Place.Task, run.Place.Kind, run.Place.Place, err)
			}
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the runs of %v: %w", issues, err)
	}
	return runs, nil
}
