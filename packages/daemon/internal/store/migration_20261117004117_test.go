package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// step_starts holds one row for each place of a task where a step runs, which the record's id is
// built from (workflow's StepID): a step at its place from 1, a retro or a merge at 0, which no
// plan names, and a count that starts at 0 and only rises. The table refuses any other row itself,
// as the outbox refuses a kind it does not name, so no caller can write a place no step has; the
// places today's phases run at are taken.
func TestStepStartsRefusesAPlaceNoStepHas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	insert := func(task, kind string, place, starts int) error {
		_, err := store.Pool().Exec(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, run)
			values ('LEGION-208', $1, $2, $3, $4, '', 'ended')`, task, kind, place, starts)
		return err
	}
	for _, taken := range []struct {
		kind  string
		place int
	}{{"step", 1}, {"step", 5}, {"retro", 0}, {"merge", 0}} {
		if err := insert("default", taken.kind, taken.place, 0); err != nil {
			t.Fatalf("a row of %s at %d = %v; want it taken: today's phases run there", taken.kind, taken.place, err)
		}
	}
	for _, refused := range []struct {
		name          string
		task, kind    string
		place, starts int
	}{
		{"a kind no step has", "default", "review", 1, 0},
		{"a step at the place 0", "default", "step", 0, 0},
		{"a retro at a place of a plan", "default", "retro", 1, 0},
		{"a merge at a place of a plan", "default", "merge", 2, 0},
		{"a negative count", "default", "step", 2, -1},
		{"no task", "", "step", 3, 0},
	} {
		var pgErr *pgconn.PgError
		if err := insert(refused.task, refused.kind, refused.place, refused.starts); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("a row of %s = %v; want the table's check to refuse it (23514)", refused.name, err)
		}
	}
}
