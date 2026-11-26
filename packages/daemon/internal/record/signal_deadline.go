package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// SignalDeadline is a project's one deadline of admission by signals as committed: Due, the moment
// a signal with a limit would next cross it with no event, where Set says there is one, and Fence,
// which rises by one at each change of the deadline, so a firing of an earlier deadline is told
// from the newest one (admit's deadlineFires). The zero value is a project no pass gave a deadline.
type SignalDeadline struct {
	Due   time.Time
	Set   bool
	Fence int64
}

// SignalDeadline is the deadline committed for project, the zero deadline where none was written.
func (s *Postgres) SignalDeadline(ctx context.Context, tx pgx.Tx, project string) (SignalDeadline, error) {
	var deadline SignalDeadline
	var due *time.Time
	err := tx.QueryRow(ctx, "select due, fence from signal_deadlines where project = $1", project).Scan(&due, &deadline.Fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignalDeadline{}, nil
	}
	if err != nil {
		return SignalDeadline{}, fmt.Errorf("read the signal deadline of %s: %w", project, err)
	}
	if due != nil {
		deadline.Due, deadline.Set = due.UTC(), true
	}
	return deadline, nil
}

// PutSignalDeadline writes deadline as project's, in the fact's own transaction. It keeps no mark
// of a write admission reads (RecordsWritten): the deadline is what a pass computed, not an input
// of one.
func (s *Postgres) PutSignalDeadline(ctx context.Context, tx pgx.Tx, project string, deadline SignalDeadline) error {
	var due *time.Time
	if deadline.Set {
		at := deadline.Due.UTC()
		due = &at
	}
	if _, err := tx.Exec(ctx, `insert into signal_deadlines (project, due, fence) values ($1, $2, $3)
		on conflict (project) do update set due = excluded.due, fence = excluded.fence`, project, due, deadline.Fence); err != nil {
		return fmt.Errorf("write the signal deadline of %s: %w", project, err)
	}
	return nil
}

// SignalPassMark is what the last committed admission pass recorded of one series of admission by
// signals: whether it held admission (false while admission by signals is off), and the last value
// a pass recorded of it by the steps of S6, where Written says a pass recorded one (Value unrecorded
// for a value the daemon could not compute).
type SignalPassMark struct {
	Holds   bool
	Written bool
	Value   signals.Value
}

// SignalPassMarks is each series' mark of project, by its name of signals.Names(): a series no pass
// marked has none, which holds nothing and has no written value.
func (s *Postgres) SignalPassMarks(ctx context.Context, tx pgx.Tx, project string) (map[string]SignalPassMark, error) {
	rows, err := tx.Query(ctx, "select series, holds, written, recorded, value from signal_pass_marks where project = $1", project)
	if err != nil {
		return nil, fmt.Errorf("read the signal marks of %s: %w", project, err)
	}
	marks := make(map[string]SignalPassMark)
	var series string
	var mark SignalPassMark
	if _, err := pgx.ForEachRow(rows, []any{&series, &mark.Holds, &mark.Written, &mark.Value.Recorded, &mark.Value.Value}, func() error {
		marks[series] = mark
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the signal marks of %s: %w", project, err)
	}
	return marks, nil
}

// KeepSignalPassMarks writes marks, the marks of the series a pass changed, by series name, as
// project's, in the pass's own transaction. Like PutSignalDeadline it keeps no mark of a write
// admission reads: it is what a pass recorded, not an input of one.
func (s *Postgres) KeepSignalPassMarks(ctx context.Context, tx pgx.Tx, project string, marks map[string]SignalPassMark) error {
	if len(marks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for series, mark := range marks {
		batch.Queue(`insert into signal_pass_marks (project, series, holds, written, recorded, value) values ($1, $2, $3, $4, $5, $6)
			on conflict (project, series) do update set holds = excluded.holds, written = excluded.written,
				recorded = excluded.recorded, value = excluded.value`,
			project, series, mark.Holds, mark.Written, mark.Value.Recorded, mark.Value.Value)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("keep the signal marks of %s: %w", project, err)
	}
	return nil
}
