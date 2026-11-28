package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// workHoldColumns is the columns of a work_holds row in WorkHold's order. work_hold_openings
// carries the same columns, and after them opened_at and opening.
const workHoldColumns = `project, tree, root_generation, step, artifact_id, version, closed_at`

// standingColumns and openedColumns are what a read of each table scans (scanWorkHold): a
// work_holds row, a hold that stands and names no opening, and a work_hold_openings row, a hold
// that an opening ended.
const (
	standingColumns = workHoldColumns + `, null::timestamptz as opened_at, ''::text as opening`
	openedColumns   = workHoldColumns + `, opened_at, opening`
)

// PutWorkHold writes tree's work hold in place of any the tree held: the close of a hold the boot
// applies again finds its own row and writes it whole. It deletes the record of the tree's last
// opening in the same statement, so a close after an opening makes the hold stand again. The
// table's checks refuse a hold that names no document, no step of the three or no version, so a
// record of a close that names nothing is refused here rather than read back by the boot or the
// state.
func (s *Postgres) PutWorkHold(ctx context.Context, tx pgx.Tx, hold WorkHold) error {
	if hold.RootGeneration > maxInt64 {
		return fmt.Errorf("record the work hold of %s: generation %d does not fit a bigint", hold.Tree, hold.RootGeneration)
	}
	if _, err := tx.Exec(ctx, `with opened as (delete from work_hold_openings where tree = $2)
		insert into work_holds (`+workHoldColumns+`) values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (tree) do update set project = excluded.project, root_generation = excluded.root_generation, step = excluded.step,
		artifact_id = excluded.artifact_id, version = excluded.version, closed_at = excluded.closed_at`,
		hold.Project, hold.Tree, int64(hold.RootGeneration), string(hold.Step), hold.ArtifactID, hold.Version, hold.ClosedAt); err != nil {
		return fmt.Errorf("record the work hold of %s at version %d of its %s document: %w", hold.Tree, hold.Version, hold.Step, err)
	}
	return nil
}

// OpenWorkHold deletes the record of tree's last opening first, so a tree whose work_holds row is
// gone keeps no record of an older opening. Then one statement deletes the row and writes what it
// named into work_hold_openings.
func (s *Postgres) OpenWorkHold(ctx context.Context, tx pgx.Tx, tree string, at time.Time, opening string) error {
	if _, err := tx.Exec(ctx, `delete from work_hold_openings where tree = $1`, tree); err != nil {
		return fmt.Errorf("delete the record of the last opening of the work hold of %s: %w", tree, err)
	}
	if _, err := tx.Exec(ctx, `with closed as (delete from work_holds where tree = $1 returning `+workHoldColumns+`)
		insert into work_hold_openings (`+openedColumns+`) select `+workHoldColumns+`, $2, $3 from closed`, tree, at, opening); err != nil {
		return fmt.Errorf("record the opening of the work hold of %s: %w", tree, err)
	}
	return nil
}

func (s *Postgres) TreeWorkHold(ctx context.Context, tx pgx.Tx, tree string) (*WorkHold, error) {
	hold, err := scanWorkHold(tx.QueryRow(ctx, `select `+standingColumns+` from work_holds where tree = $1
		union all select `+openedColumns+` from work_hold_openings where tree = $1
		order by opened_at nulls first limit 1`, tree))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the record of the work hold of %s: %w", tree, err)
	}
	return &hold, nil
}

func (s *Postgres) LockWorkHold(ctx context.Context, tx pgx.Tx, tree string) (*WorkHold, error) {
	return readWorkHold(ctx, tx, tree)
}

// readWorkHold is tree's hold that stands, read on tx with its work_holds row locked for update
// until tx ends, nil for none.
func readWorkHold(ctx context.Context, tx pgx.Tx, tree string) (*WorkHold, error) {
	hold, err := scanWorkHold(tx.QueryRow(ctx, `select `+standingColumns+` from work_holds where tree = $1 for update`, tree))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the work hold of %s: %w", tree, err)
	}
	return &hold, nil
}

func (s *Postgres) WorkHolds(ctx context.Context, tx pgx.Tx, project string) ([]WorkHold, error) {
	rows, err := tx.Query(ctx, `select `+standingColumns+` from work_holds where project = $1 order by tree`, project)
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
	if _, err := tx.Exec(ctx, `with opened as (delete from work_hold_openings where tree = $1)
		delete from work_holds where tree = $1`, tree); err != nil {
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

// StoredClaim is one claim as the store holds it on an issue (Store.StoredClaims): its token and
// the role it is on.
type StoredClaim struct {
	Token claim.Token
	Role  claim.Role
}

func (s *Postgres) StoredClaims(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]StoredClaim, error) {
	claims := map[string][]StoredClaim{}
	if len(issues) == 0 {
		return claims, nil
	}
	rows, err := tx.Query(ctx, `select issue, token, role from claims where issue = any($1) order by issue, token`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the claims of %d issues: %w", len(issues), err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue, token, role string
		if err := rows.Scan(&issue, &token, &role); err != nil {
			return nil, fmt.Errorf("read the claims of %d issues: %w", len(issues), err)
		}
		claims[issue] = append(claims[issue], StoredClaim{Token: claim.Token(token), Role: claim.Role(role)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of %d issues: %w", len(issues), err)
	}
	return claims, nil
}

// scanWorkHold reads one row of standingColumns or openedColumns. opened_at is null on a hold that
// stands, and leaves OpenedAt zero.
func scanWorkHold(row pgx.Row) (WorkHold, error) {
	var hold WorkHold
	var generation int64
	var step string
	var openedAt *time.Time
	if err := row.Scan(&hold.Project, &hold.Tree, &generation, &step, &hold.ArtifactID, &hold.Version, &hold.ClosedAt,
		&openedAt, &hold.Opening); err != nil {
		return WorkHold{}, err
	}
	hold.RootGeneration, hold.Step, hold.ClosedAt = uint64(generation), DesignStep(step), hold.ClosedAt.UTC()
	if openedAt != nil {
		hold.OpenedAt = openedAt.UTC()
	}
	return hold, nil
}
