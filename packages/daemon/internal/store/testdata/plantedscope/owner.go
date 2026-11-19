package plantedscope

import "context"

// plantedOwner opens a commit scope outside Transact, which the scope guard must list.
// TestTheScopeGuardFindsAPlantedOpener parses this file; Go builds nothing under testdata.
func plantedOwner(ctx context.Context) { _, _ = withCommitHooks(ctx) }
