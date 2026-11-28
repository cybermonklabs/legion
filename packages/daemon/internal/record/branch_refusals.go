package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BranchRefusal is the refusal for good of an issue_branch row (IssueBranch): GitHub, or the
// deployment's rules, refused the create or the move of the issue's branch in a way the same write
// meets again, so the row was finished rather than retried (internal/daemon's permanentRefusal).
// Generation is the generation the row served, Row the row's id, Write what the row did
// (BranchWriteCreate or BranchWriteMove), Kind the refusal's kind (ghbranch.RefusalKind), Reason
// the row's error as the row kept it (storetext.FarSideError), and RefusedAt when it was refused.
// A row of branch_refusals, keyed by its issue and generation.
type BranchRefusal struct {
	Issue      string
	Generation uint64
	Row        int64
	Write      string
	Kind       string
	Reason     string
	RefusedAt  time.Time
}

// The two writes of an issue's branch a BranchRefusal names: the create of a branch at the default
// branch, and the move of a next change's branch to the head of its base (IssueBranch.From).
const (
	BranchWriteCreate = "create"
	BranchWriteMove   = "move"
)

// BranchRefusedNotice is the architect's notice of a branch row refused for good: the branch, the
// write, the refusal's kind and the row's error, told once for each generation of the issue
// (Store.RefuseBranch). No earlier release decodes it, so the way back of migration
// 20261127215416 deletes its unsent rows.
const BranchRefusedNotice NoticeKind = "branch-refused"

// branchRefusalColumns is the columns of branch_refusals a read scans (scanBranchRefusal), in its
// order.
const branchRefusalColumns = `branch_refusals.issue, branch_refusals.generation, branch_refusals.row_id, branch_refusals.branch_write,
	branch_refusals.kind, branch_refusals.reason, branch_refusals.refused_at`

func (s *Postgres) RefuseBranch(ctx context.Context, tx pgx.Tx, refusal BranchRefusal) (bool, error) {
	switch {
	case refusal.Issue == "":
		return false, errors.New("record a refused branch: it names the issue whose branch it is")
	case refusal.Generation == 0 || refusal.Generation > maxInt64:
		return false, fmt.Errorf("record a refused branch of %s: generation %d is no generation a row serves", refusal.Issue, refusal.Generation)
	case refusal.Write != BranchWriteCreate && refusal.Write != BranchWriteMove:
		return false, fmt.Errorf("record a refused branch of %s: write %q is neither %s nor %s", refusal.Issue, refusal.Write, BranchWriteCreate, BranchWriteMove)
	case refusal.Kind == "" || refusal.RefusedAt.IsZero():
		return false, fmt.Errorf("record a refused branch of %s: it names the refusal's kind and when it came", refusal.Issue)
	}
	tag, err := tx.Exec(ctx, `insert into branch_refusals (issue, generation, row_id, branch_write, kind, reason, refused_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (issue, generation) do nothing`,
		refusal.Issue, int64(refusal.Generation), refusal.Row, refusal.Write, refusal.Kind, refusal.Reason, refusal.RefusedAt.UTC())
	if err != nil {
		return false, fmt.Errorf("record a refused branch of %s: %w", refusal.Issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) ClearBranchRefusals(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, `delete from branch_refusals where issue = $1`, issue); err != nil {
		return fmt.Errorf("clear the refused branches of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) BranchRefusals(ctx context.Context, tx pgx.Tx, project string) ([]BranchRefusal, error) {
	rows, err := tx.Query(ctx, `select `+branchRefusalColumns+` from branch_refusals
		join issues on issues.key = branch_refusals.issue and issues.generation = branch_refusals.generation
		where issues.project = $1
		order by branch_refusals.issue`, project)
	if err != nil {
		return nil, fmt.Errorf("list the refused branches of %s: %w", project, err)
	}
	defer rows.Close()
	var refusals []BranchRefusal
	for rows.Next() {
		refusal, err := scanBranchRefusal(rows)
		if err != nil {
			return nil, fmt.Errorf("read a refused branch of %s: %w", project, err)
		}
		refusals = append(refusals, refusal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the refused branches of %s: %w", project, err)
	}
	return refusals, nil
}

// scanBranchRefusal reads one row of branchRefusalColumns.
func scanBranchRefusal(row scanner) (BranchRefusal, error) {
	var refusal BranchRefusal
	var generation int64
	if err := row.Scan(&refusal.Issue, &generation, &refusal.Row, &refusal.Write, &refusal.Kind, &refusal.Reason, &refusal.RefusedAt); err != nil {
		return BranchRefusal{}, err
	}
	if generation < 1 {
		return BranchRefusal{}, fmt.Errorf("the refused branch of %s has generation %d", refusal.Issue, generation)
	}
	refusal.Generation, refusal.RefusedAt = uint64(generation), refusal.RefusedAt.UTC()
	return refusal, nil
}
