package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) Handover(ctx context.Context, tx pgx.Tx, issue string) (*Handover, error) {
	var handover Handover
	err := tx.QueryRow(ctx, `select issue, tree, linger_generation, cancelled from handovers where issue = $1`, issue).Scan(
		&handover.Issue, &handover.Tree, &handover.Generation, &handover.Cancelled,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hand-over of %s: %w", issue, err)
	}
	return &handover, nil
}

// PutHandover writes issue's hand-over fence. The fence is an input of the end of a merge's run
// (workflow's mergeCanLand), so each of its writes keeps the issue among those its transaction
// touched (noteIssueWrite), and the settle of a transaction that settles reads its runs.
func (s *Postgres) PutHandover(ctx context.Context, tx pgx.Tx, handover Handover) error {
	if handover.Issue == "" || handover.Tree == "" || handover.Generation == 0 {
		return errors.New("hand-over requires issue, tree and linger generation")
	}
	if handover.Generation > maxInt64 {
		return fmt.Errorf("put hand-over of %s: generation %d does not fit a bigint", handover.Issue, handover.Generation)
	}
	noteIssueWrite(ctx, handover.Issue)
	_, err := tx.Exec(ctx, `insert into handovers (issue, tree, linger_generation, cancelled) values ($1, $2, $3, false)
		on conflict (issue) do update set tree = excluded.tree, linger_generation = excluded.linger_generation, cancelled = false`,
		handover.Issue, handover.Tree, int64(handover.Generation))
	if err != nil {
		return fmt.Errorf("put hand-over of %s: %w", handover.Issue, err)
	}
	return nil
}

// DeleteHandover deletes issue's hand-over fence, keeping the issue among those its transaction
// touched (PutHandover says why).
func (s *Postgres) DeleteHandover(ctx context.Context, tx pgx.Tx, issue string) error {
	noteIssueWrite(ctx, issue)
	if _, err := tx.Exec(ctx, `delete from handovers where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete hand-over of %s: %w", issue, err)
	}
	return nil
}

// DeleteHandoverAt deletes issue's hand-over fence while it names tree and generation, keeping the
// issue among those its transaction touched (PutHandover says why).
func (s *Postgres) DeleteHandoverAt(ctx context.Context, tx pgx.Tx, issue, tree string, generation uint64) error {
	if generation > maxInt64 {
		return fmt.Errorf("delete hand-over of %s: generation %d does not fit a bigint", issue, generation)
	}
	noteIssueWrite(ctx, issue)
	if _, err := tx.Exec(ctx, `delete from handovers where issue = $1 and tree = $2 and linger_generation = $3`,
		issue, tree, int64(generation)); err != nil {
		return fmt.Errorf("delete hand-over of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) CancelTreeHandovers(ctx context.Context, tx pgx.Tx, tree string, generation uint64) error {
	if generation > maxInt64 {
		return fmt.Errorf("cancel hand-overs of %s: generation %d does not fit a bigint", tree, generation)
	}
	if _, err := tx.Exec(ctx, `update handovers set cancelled = true where tree = $1 and linger_generation = $2`, tree, int64(generation)); err != nil {
		return fmt.Errorf("cancel hand-overs of %s: %w", tree, err)
	}
	return nil
}

func (s *Postgres) HandedOverRoot(ctx context.Context, tx pgx.Tx, issue string) (*HandedOverRoot, error) {
	root := HandedOverRoot{Issue: issue}
	var generation int64
	err := tx.QueryRow(ctx, `select generation, gate_carried, from_root from handed_over_roots where issue = $1`, issue).Scan(
		&generation, &root.GateCarried, &root.FromRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read handed-over root %s: %w", issue, err)
	}
	root.Generation = uint64(generation)
	return &root, nil
}

func (s *Postgres) PutHandedOverRoot(ctx context.Context, tx pgx.Tx, root HandedOverRoot) error {
	if root.Generation == 0 || root.Generation > maxInt64 {
		return fmt.Errorf("put handed-over root %s: generation %d is out of range", root.Issue, root.Generation)
	}
	if _, err := tx.Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried, from_root) values ($1, $2, $3, $4)
		on conflict (issue) do update set generation = excluded.generation, gate_carried = excluded.gate_carried, from_root = excluded.from_root`,
		root.Issue, int64(root.Generation), root.GateCarried, root.FromRoot); err != nil {
		return fmt.Errorf("put handed-over root %s: %w", root.Issue, err)
	}
	return nil
}
