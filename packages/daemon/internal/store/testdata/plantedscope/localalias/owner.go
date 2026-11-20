package plantedscope

import "context"

// plantedOwner takes each scope helper into a local variable and calls it through that, which
// the scope guards must list. They parse this file; Go builds nothing under testdata.
func plantedOwner(ctx context.Context) {
	open := withCommitHooks
	_, _ = open(ctx, nil, nil)
	read := outcomeReadContext
	_ = read(ctx)
}
