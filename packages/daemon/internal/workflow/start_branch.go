package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// StartBranch is the branch start names (record.SuperviseRequest.Branch) by the one rule the queue
// names every start's branch by (startBranch), over rows, the issue's phases rows as tx holds them.
// The daemon's news-only start reads it (its settleNews): that start carries no task, so the queue
// (queuedRow) never sees it, and it names its phase, so its delivery adopts the working copy of the
// branch it names.
func StartBranch(ctx context.Context, tx pgx.Tx, store record.Store, issue record.Issue, start record.SuperviseRequest, rows []record.PhaseRow) (string, error) {
	return startBranch(ctx, tx, store, issue, start, rows)
}
