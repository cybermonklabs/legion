package codesurvival

// reads_test.go holds the reads of one repository's stored results that the tests compare with
// what survival.Measure answered: the changes and the pairs of a set of days, each read through
// the readers the run and the items route read the store by (ChangesOn, PairsOf).

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
)

// Changes is every change of repo on days, in the order the commits were made: what a run
// measured, as survival.Measure answered it, read through ChangesOn.
func Changes(ctx context.Context, db Querier, repo string, days []time.Time) ([]survival.Change, error) {
	repos := make([]string, len(days))
	for index := range repos {
		repos[index] = repo
	}
	stored, err := ChangesOn(ctx, db, repos, days)
	if err != nil {
		return nil, err
	}
	changes := make([]survival.Change, len(stored))
	for index, change := range stored {
		changes[index] = change.Change
	}
	return changes, nil
}

// Pairs is every pair of repo whose adder is a change on days, in the order of the adders' commits
// and then of the removers' SHAs: what a run measured, as survival.Measure answered it, read
// through PairsOf of the changes Changes reads.
func Pairs(ctx context.Context, db Querier, repo string, days []time.Time) ([]survival.Pair, error) {
	changes, err := Changes(ctx, db, repo, days)
	if err != nil {
		return nil, err
	}
	repos, adders, order := make([]string, len(changes)), make([]string, len(changes)), make(map[string]int, len(changes))
	for index, change := range changes {
		repos[index], adders[index], order[change.SHA] = repo, change.SHA, index
	}
	stored, err := PairsOf(ctx, db, repos, adders)
	if err != nil {
		return nil, err
	}
	// PairsOf answers an adder's pairs in the order of the removers' SHAs, which a stable sort by
	// the adders' commits keeps.
	slices.SortStableFunc(stored, func(a, b fold.Pair) int { return cmp.Compare(order[a.AdderSHA], order[b.AdderSHA]) })
	pairs := make([]survival.Pair, len(stored))
	for index, pair := range stored {
		pairs[index] = pair.Pair
	}
	return pairs, nil
}
