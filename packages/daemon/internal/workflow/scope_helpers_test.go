package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// firstScope is the first scope of issue's run read in tx (Engine.scopes): the issue itself for a
// run with no plan rows, and the plan's one task for a plan of one. A test that names no task
// reads it for an issue whose plan holds one task or none.
func firstScope(ctx context.Context, tx pgx.Tx, e *Engine, issue record.Issue) (TaskScope, error) {
	scopes, err := e.scopes(ctx, tx, issue)
	if err != nil {
		return TaskScope{}, err
	}
	if len(scopes) == 0 {
		return TaskScope{}, fmt.Errorf("read %s at generation %d: its plan holds no task", issue.Key, issue.Generation)
	}
	return scopes[0], nil
}
