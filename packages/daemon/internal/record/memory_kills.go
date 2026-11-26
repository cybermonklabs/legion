package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// KeepMemoryKill keeps report, one memory kill of an issue pod of project, as one row of
// memory_kills: its guard and sequence, which name the kill once, the moment the launcher's watch
// named it (the report's At, so a report a reconnected launcher sends late counts at its own
// moment), its cause and whether the process was of no role. A kill kept already, by its guard and
// sequence, stays as it was kept. It takes the wire's own frame, as runtime.MemoryGuardKilled does,
// rather than a copy of it in a shape of this package's.
//
// It then deletes every kill of project at or before now less signals.KillSpan: the kills signal
// of a moment counts the kills of the hour before it (signals.KillCount), and no read is of a
// moment before now, so no read counts those again, and the rows a read meets stay bounded by the
// kills of an hour rather than by the project's whole history. A report older than that is so
// deleted as it is kept.
//
// A kill it keeps is a write admission reads (RecordsWritten), so the fact's pass counts it; a
// kill kept already, and the deletion of kills no read counts, change nothing a pass reads.
func (s *Postgres) KeepMemoryKill(ctx context.Context, tx pgx.Tx, project string, report shimwire.LauncherMemoryKill, now time.Time) error {
	tag, err := tx.Exec(ctx, `insert into memory_kills (project, guard, sequence, at, cause, no_role) values ($1, $2, $3, $4, $5, $6)
		on conflict (project, guard, sequence) do nothing`,
		project, report.Guard, int64(report.Sequence), report.At, string(report.Cause), report.NoRole)
	if err != nil {
		return fmt.Errorf("keep the memory kill %s:%d of %s: %w", report.Guard, report.Sequence, project, err)
	}
	noteRowsWritten(ctx, tag)
	if _, err := tx.Exec(ctx, "delete from memory_kills where project = $1 and at <= $2", project, now.Add(-signals.KillSpan)); err != nil {
		return fmt.Errorf("forget the memory kills of %s an hour before %s: %w", project, now, err)
	}
	return nil
}

// MemoryKillsAfter is the moment of each kill of project kept later than after, oldest first, in
// UTC: the kills signal of a moment reads them with after an hour before that moment
// (signals.KillsAt, signals.KillsNext), and the index by project and time bounds the read by that
// hour.
func (s *Postgres) MemoryKillsAfter(ctx context.Context, tx pgx.Tx, project string, after time.Time) ([]time.Time, error) {
	rows, err := tx.Query(ctx, "select at from memory_kills where project = $1 and at > $2 order by at", project, after)
	if err != nil {
		return nil, fmt.Errorf("read the memory kills of %s after %s: %w", project, after, err)
	}
	kills, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var at time.Time
		err := row.Scan(&at)
		return at.UTC(), err
	})
	if err != nil {
		return nil, fmt.Errorf("read the memory kills of %s after %s: %w", project, after, err)
	}
	return kills, nil
}

// BeginSignalInput records at as the moment the daemon began keeping input, one input of a signal
// (a name of signals.Names()), for project, where no moment is recorded yet: a later call leaves
// the first moment as it stands, so each boot of a release that keeps the input may call it and
// the first one's moment is the one that stays. The moment it records is a write admission reads
// (RecordsWritten): it begins the span over which the signal is recorded.
func (s *Postgres) BeginSignalInput(ctx context.Context, tx pgx.Tx, project, input string, at time.Time) error {
	tag, err := tx.Exec(ctx, `insert into signal_inputs (project, input, kept_since) values ($1, $2, $3)
		on conflict (project, input) do nothing`, project, input, at)
	if err != nil {
		return fmt.Errorf("record that %s keeps %s from %s: %w", project, input, at, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// SignalInputSince is the moment the daemon began keeping input for project, in UTC, and whether
// one is recorded (BeginSignalInput).
func (s *Postgres) SignalInputSince(ctx context.Context, tx pgx.Tx, project, input string) (time.Time, bool, error) {
	var since time.Time
	err := tx.QueryRow(ctx, "select kept_since from signal_inputs where project = $1 and input = $2", project, input).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read since when %s keeps %s: %w", project, input, err)
	}
	return since.UTC(), true, nil
}
