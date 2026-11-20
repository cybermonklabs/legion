package plantedscope

import "context"

// plantedOwner opens a commit scope outside TransactWith and marks an outcome read outside
// readOutcome, which the scope guards must list. They parse this file; Go builds nothing under
// testdata.
func plantedOwner(ctx context.Context) {
	_, _ = withCommitHooks(ctx, nil, nil)
	_ = outcomeReadContext(ctx)
}
