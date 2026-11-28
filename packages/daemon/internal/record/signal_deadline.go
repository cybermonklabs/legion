package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
