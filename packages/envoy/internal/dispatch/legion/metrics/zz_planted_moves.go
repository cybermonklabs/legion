package metrics

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// plantedLastMove is a planted reader of a pull request's issue: the issue the last queued move of
// the pull request reached, a value a pass stored, not the statement's answer at this read.
func plantedLastMove(ctx context.Context, pool *store.Pool, repo string, number int) (*string, error) {
	var reached *string
	err := pool.QueryRow(ctx, `select to_key from pull_request_attribution_moves where repo = $1 and number = $2`, repo, number).Scan(&reached)
	return reached, err
}
