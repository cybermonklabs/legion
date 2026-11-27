package plantedscope

import "context"

// plantedOwner stores a commit scope under the scope's key outside TransactWith: a scope whose
// hooks no TransactWith commits, which the key's guard must list. The guards parse this file; Go
// builds nothing under testdata.
func plantedOwner(ctx context.Context) context.Context {
	return context.WithValue(ctx, commitHooksKey{}, &commitScope{})
}

// scopeKey takes the key's type into a package-level alias, through which any function could
// store a scope.
type scopeKey = commitHooksKey

// plantedGrace marks a context as an outcome read's outside readOutcome, so its COMMIT would
// have no grace.
func plantedGrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCommitGrace{}, true)
}
