package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// OpenUncountedRuns is the boot's one pass over the runs no start of this release counted. Each
// issue of the project that stands in a step whose runs are recorded, whose place has no row in the
// store — a run the release before this one started — or a row the roll forward of a rollback
// marked uncounted — a run an earlier release started after the rollback — and whose run is open
// now (uncountedRunOpen: this release's end rule does not hold it over, it is not the operator's,
// and its generation started a run) gets an open row there (record.Store.OpenUncountedStep), with
// the agent and the moment its start would have named (upgradedStart). So an issue out of Legion's
// work, and a root that waits for the promotion that starts it, get none. It queues no record: the
// run reaches Dispatch with its end,
// as an uncounted run's end does (endStep). From then on it is an open run like any other: the
// settle ends it (settleRuns), and the record names its agent (record.RunsAStepOn), so a fact that
// agent's request applies waits for the agent's stream first. A place whose row is open or ended
// is a run a start counted, or one a boot before this one opened, and stays as it stands, so a
// boot that runs again opens nothing. It answers how many runs it opened. The daemon runs it at
// each boot, before any fact applies (internal/daemon's openUncountedRuns).
func (e *Engine) OpenUncountedRuns(ctx context.Context, tx pgx.Tx) (int, error) {
	issues, err := e.store.LiveIssues(ctx, tx, e.cfg.Project)
	if err != nil {
		return 0, err
	}
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	stored, err := e.store.StepRuns(ctx, tx, keys)
	if err != nil {
		return 0, err
	}
	counted := make(map[runAt]bool, len(stored))
	for _, row := range stored {
		if row.Run != record.RunUncounted {
			counted[runAt{issue: row.Issue, place: row.Place}] = true
		}
	}
	opened := 0
	for _, issue := range issues {
		step, ok := standingStep(issue)
		if !ok || counted[runAt{issue: issue.Key, place: step.place}] {
			continue
		}
		_, open, err := e.uncountedRunOpen(ctx, tx, issue)
		if err != nil {
			return 0, err
		}
		if !open {
			continue
		}
		agent, startedAt, err := e.upgradedStart(ctx, tx, issue, step.at)
		if err != nil {
			return 0, err
		}
		first, err := e.store.OpenUncountedStep(ctx, tx, issue.Key, step.place, func(starts int64) record.StepRecord {
			write := stepWrite(issue, step.place, step.role, step.typ, string(agent), starts)
			if !startedAt.IsZero() {
				write.StartedAt = new(startedAt)
			}
			return write
		})
		if err != nil {
			return 0, err
		}
		// The pass read this place's row as missing or uncounted in this transaction, under the
		// lock every fact applies under, so the store opens it: a refusal is a broken read.
		if !first {
			return 0, fmt.Errorf("open the uncounted run of %s at %+v: the store holds it counted, which this pass read as not", issue.Key, step.place)
		}
		opened++
	}
	return opened, nil
}
