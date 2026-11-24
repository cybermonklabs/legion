package record

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// EarlierRunOf says whether an earlier run of issue ran a step: the record holds a phases row of
// the issue of a role other than the architect's. A phase worker's row is written when its role
// first runs a step of the issue (PutPhase), and no new generation deletes it (ClearGeneration
// resets the row and keeps it), while the architect's row stands from the start of the issue's
// tree. Its one reader asks at a run's first step, which no worker of that run has reached, so such
// a row is an earlier run's. The issue's generation does not say it: admission records a root at
// generation 1, but the workflow records a child at its root's generation (enterChild), which can
// be any.
func (s *Postgres) EarlierRunOf(ctx context.Context, tx pgx.Tx, issue Issue) (bool, error) {
	var ran bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from phases where issue = $1 and role <> $2)`,
		issue.Key, string(claim.RoleArchitect)).Scan(&ran); err != nil {
		return false, fmt.Errorf("read whether an earlier run of %s ran a step: %w", issue.Key, err)
	}
	return ran, nil
}
