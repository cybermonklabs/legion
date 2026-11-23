package record

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RunsAStepOn says whether the record names agent the agent of a run of a step on issue that is
// not ended: the latest start of a place of issue wrote agent (RaiseStepStarts), and the store
// does not hold that place's run ended (RunOpen, or RunUncounted after the roll forward of a
// rollback). A run no start of this release counted — one an earlier release started — names its
// agent here from the daemon's boot on, which gives it an open row (OpenUncountedStep).
func RunsAStepOn(ctx context.Context, tx pgx.Tx, agent, issue string) (bool, error) {
	var runs bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from step_starts where issue = $1 and agent = $2 and run <> $3)`,
		issue, agent, RunEnded).Scan(&runs); err != nil {
		return false, fmt.Errorf("read whether %s runs a step of %s: %w", agent, issue, err)
	}
	return runs, nil
}

// RunsAStep is RunsAStepOn over every issue: whether the record names agent the agent of a run
// that is not ended on any issue.
func RunsAStep(ctx context.Context, tx pgx.Tx, agent string) (bool, error) {
	var runs bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from step_starts where agent = $1 and run <> $2)`,
		agent, RunEnded).Scan(&runs); err != nil {
		return false, fmt.Errorf("read whether %s runs a step: %w", agent, err)
	}
	return runs, nil
}
