package metrics

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// plantedStoredColumn is the control: a reader of the stored column that the census must name.
func plantedStoredColumn(ctx context.Context, pool *store.Pool, repo string, number int) (*string, error) {
	var issue *string
	err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&issue)
	return issue, err
}
