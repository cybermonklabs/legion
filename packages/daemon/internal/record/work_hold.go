package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// workHoldColumns is the columns of a work_holds row in WorkHold's order, as each read scans them.
const workHoldColumns = `project, tree, root_generation, step, artifact_id, version, closed_at`

// PutWorkHold writes tree's work hold in place of any the tree held: the close of a hold the boot
// applies again finds its own row and writes it whole. The table's checks refuse a hold that names
// no document, no step of the three or no version, so a record of a close that names nothing is
// refused here rather than read back by the boot or the state.
func (s *Postgres) PutWorkHold(ctx context.Context, tx pgx.Tx, hold WorkHold) error {
	if hold.RootGeneration > maxInt64 {
		return fmt.Errorf("record the work hold of %s: generation %d does not fit a bigint", hold.Tree, hold.RootGeneration)
	}
	if _, err := tx.Exec(ctx, `insert into work_holds (`+workHoldColumns+`) values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (tree) do update set project = excluded.project, root_generation = excluded.root_generation, step = excluded.step,
		artifact_id = excluded.artifact_id, version = excluded.version, closed_at = excluded.closed_at`,
		hold.Project, hold.Tree, int64(hold.RootGeneration), string(hold.Step), hold.ArtifactID, hold.Version, hold.ClosedAt); err != nil {
		return fmt.Errorf("record the work hold of %s at version %d of its %s document: %w", hold.Tree, hold.Version, hold.Step, err)
	}
	return nil
}

func (s *Postgres) WorkHold(ctx context.Context, tx pgx.Tx, tree string) (*WorkHold, error) {
	hold, err := scanWorkHold(tx.QueryRow(ctx, `select `+workHoldColumns+` from work_holds where tree = $1`, tree))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the work hold of %s: %w", tree, err)
	}
	return &hold, nil
}

func (s *Postgres) WorkHolds(ctx context.Context, tx pgx.Tx, project string) ([]WorkHold, error) {
	rows, err := tx.Query(ctx, `select `+workHoldColumns+` from work_holds where project = $1 order by tree`, project)
	if err != nil {
		return nil, fmt.Errorf("list the work holds of %s: %w", project, err)
	}
	defer rows.Close()
	var holds []WorkHold
	for rows.Next() {
		hold, err := scanWorkHold(rows)
		if err != nil {
			return nil, fmt.Errorf("list the work holds of %s: %w", project, err)
		}
		holds = append(holds, hold)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the work holds of %s: %w", project, err)
	}
	return holds, nil
}

func (s *Postgres) DeleteWorkHold(ctx context.Context, tx pgx.Tx, tree string) error {
	if _, err := tx.Exec(ctx, `delete from work_holds where tree = $1`, tree); err != nil {
		return fmt.Errorf("delete the work hold of %s: %w", tree, err)
	}
	return nil
}

func (s *Postgres) WorkHeldTrees(ctx context.Context, tx pgx.Tx, project string) (map[string]struct{}, error) {
	rows, err := tx.Query(ctx, `select distinct issues.tree from claims join issues on issues.key = claims.issue
		where issues.project = $1 and claims.state = $2 and claims.suspended_by = $3`,
		project, string(supervise.StateSuspended), string(supervise.SuspendedByHold))
	if err != nil {
		return nil, fmt.Errorf("list the trees of %s holding a claim the work hold stopped: %w", project, err)
	}
	defer rows.Close()
	trees := map[string]struct{}{}
	for rows.Next() {
		var tree string
		if err := rows.Scan(&tree); err != nil {
			return nil, fmt.Errorf("list the trees of %s holding a claim the work hold stopped: %w", project, err)
		}
		trees[tree] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the trees of %s holding a claim the work hold stopped: %w", project, err)
	}
	return trees, nil
}

// scanWorkHold reads one row of workHoldColumns.
func scanWorkHold(row pgx.Row) (WorkHold, error) {
	var hold WorkHold
	var generation int64
	var step string
	if err := row.Scan(&hold.Project, &hold.Tree, &generation, &step, &hold.ArtifactID, &hold.Version, &hold.ClosedAt); err != nil {
		return WorkHold{}, err
	}
	hold.RootGeneration, hold.Step, hold.ClosedAt = uint64(generation), DesignStep(step), hold.ClosedAt.UTC()
	return hold, nil
}
