package record

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// BranchRefusal is the refusal for good of an issue_branch row (IssueBranch): GitHub, or the
// deployment's rules, refused the create or the move of the issue's branch in a way the same write
// meets again, so the row was finished rather than retried (internal/daemon's permanentRefusal).
// Generation is the generation the row served, Row the row's id, Write what the row did
// (BranchWriteCreate or BranchWriteMove), Kind the refusal's kind (ghbranch.RefusalKind), Reason
// the row's error as the row kept it (storetext.FarSideError), and RefusedAt when it was refused.
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
// (Store.RefuseBranch).
const BranchRefusedNotice NoticeKind = "branch-refused"

func (s *Postgres) RefuseBranch(ctx context.Context, tx pgx.Tx, refusal BranchRefusal) (bool, error) {
	return false, nil
}

func (s *Postgres) ClearBranchRefusals(ctx context.Context, tx pgx.Tx, issue string) error {
	return nil
}

func (s *Postgres) BranchRefusals(ctx context.Context, tx pgx.Tx, project string) ([]BranchRefusal, error) {
	return nil, nil
}
