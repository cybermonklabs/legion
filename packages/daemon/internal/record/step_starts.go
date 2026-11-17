package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StepPlace is one place of a task where a step runs: the task, the kind of step (step, retro or
// merge) and its place in the task, from 1 for a step and 0 for a retro or a merge, which no plan
// names (workflow's stepOf).
type StepPlace struct {
	Task  string
	Kind  string
	Place int
}

// StepStart is the count of starts of one place of an issue, the agent and the moment its latest
// start wrote, and whether that start's run has ended: what the end of the step running there
// computes the record's id from and writes again (workflow's endStep), so the start and the end
// agree whichever reaches Dispatch first. The zero StepStart is a place no start of this release
// has raised: a step that was running when the release deployed. StartedAt is zero where the
// moment is unknown: an end of such a step whose role's entry recorded none.
type StepStart struct {
	Starts    int64
	Agent     string
	StartedAt time.Time
	Ended     bool
}

// RaiseStepStarts counts one more start of place on issue, keeps the agent and the moment that
// start writes, clears the mark that the run before it ended, and answers the count with that
// start in it. It is called in the transaction that writes the start's record.
func (s *Postgres) RaiseStepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, agent string, at time.Time) (int64, error) {
	var starts int64
	if err := tx.QueryRow(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, started_at, ended) values ($1, $2, $3, $4, 1, $5, $6, false)
		on conflict (issue, task, kind, place) do update set starts = step_starts.starts + 1, agent = excluded.agent, started_at = excluded.started_at, ended = false
		returning starts`, issue, place.Task, place.Kind, place.Place, agent, at).Scan(&starts); err != nil {
		return 0, fmt.Errorf("count a start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return starts, nil
}

// StepStarts is the count of starts of place on issue, with the agent, the moment and the end mark
// of the latest, unchanged: the zero StepStart where no start was counted and no end written.
func (s *Postgres) StepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace) (StepStart, error) {
	start, err := scanStepStart(tx.QueryRow(ctx, `select starts, agent, started_at, ended from step_starts
		where issue = $1 and task = $2 and kind = $3 and place = $4`, issue, place.Task, place.Kind, place.Place))
	if errors.Is(err, pgx.ErrNoRows) {
		return StepStart{}, nil
	}
	if err != nil {
		return StepStart{}, fmt.Errorf("read the starts of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return start, nil
}

// EndStepStart marks the run of the latest start of place on issue ended, in the transaction that
// writes that run's end, and answers that start, marked, and true. It answers false, and marks
// nothing, where no counted start's run is open: its end was written already (the linger's before
// the hand-over's, or a second end one fact writes), or no start of this release was counted
// (EndUncountedStep). The update locks the row, so two transactions that end one run at one time
// end it once: the second reads the mark the first committed.
func (s *Postgres) EndStepStart(ctx context.Context, tx pgx.Tx, issue string, place StepPlace) (StepStart, bool, error) {
	start, err := scanStepStart(tx.QueryRow(ctx, `update step_starts set ended = true
		where issue = $1 and task = $2 and kind = $3 and place = $4 and not ended
		returning starts, agent, started_at, ended`, issue, place.Task, place.Kind, place.Place))
	if errors.Is(err, pgx.ErrNoRows) {
		return StepStart{}, false, nil
	}
	if err != nil {
		return StepStart{}, false, fmt.Errorf("end the latest start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return start, true, nil
}

// EndUncountedStep records the end of the run at place on issue that no start of this release
// counted, a step that started under the release before: the row at the count 0, with the agent
// and the moment its start would have named (startedAt zero where unknown), marked ended, so the
// next start counts 1. It answers false, and writes nothing, where the place has a row already,
// whose latest run has ended (EndStepStart found none open), so no run gets a second end.
func (s *Postgres) EndUncountedStep(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, agent string, startedAt time.Time) (bool, error) {
	var at *time.Time
	if !startedAt.IsZero() {
		at = &startedAt
	}
	tag, err := tx.Exec(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, started_at, ended) values ($1, $2, $3, $4, 0, $5, $6, true)
		on conflict (issue, task, kind, place) do nothing`, issue, place.Task, place.Kind, place.Place, agent, at)
	if err != nil {
		return false, fmt.Errorf("end the uncounted step of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return tag.RowsAffected() == 1, nil
}

// scanStepStart reads one step_starts row's starts, agent, started_at and ended.
func scanStepStart(row pgx.Row) (StepStart, error) {
	var start StepStart
	var at *time.Time
	if err := row.Scan(&start.Starts, &start.Agent, &at, &start.Ended); err != nil {
		return StepStart{}, err
	}
	if at != nil {
		start.StartedAt = at.UTC()
	}
	return start, nil
}
