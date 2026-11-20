package plantedscope

import "context"

// Each scope helper taken into a package variable, and called through it by plantedOwner, which
// the scope guards must list. They parse this file; Go builds nothing under testdata.
var (
	open = withCommitHooks
	read = outcomeReadContext
)

func plantedOwner(ctx context.Context) {
	_, _ = open(ctx, nil, nil)
	_ = read(ctx)
}
