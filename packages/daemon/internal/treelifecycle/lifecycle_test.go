package treelifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The tree's own refusals of a claim's work are each one CheckWork or BindWork answers, read by
// one rule through whatever wraps them (Refuses): a reserved or confirmed cleanup, an absent
// lifecycle, and a stale or unbound epoch. Every other error of the check is the store's, and so
// is a refusal written as words alone. A lifecycle open under another authority is Open's answer,
// which no work check makes, so it is no refusal of a claim's work.
func TestTheTreesOwnRefusalsAreReadByOneRule(t *testing.T) {
	for _, tc := range []struct {
		err  error
		tree bool
	}{
		{fmt.Errorf("launch claim c: bind work of tree T: %w", ErrCleanupReserved), true},
		{fmt.Errorf("launch claim c: %w", ErrLifecycleClosed), true},
		{fmt.Errorf("launch claim c: %w", ErrLifecycleAbsent), true},
		{fmt.Errorf("launch claim c: %w", ErrEpochStale), true},
		{fmt.Errorf("launch claim c: %w", ErrEpochUnbound), true},
		{fmt.Errorf("launch claim c: %w", ErrAuthorityHeld), false},
		{errors.New("the store did not answer the launch check"), false},
		{fmt.Errorf("launch claim c: serialize tree lifecycle: %w", context.DeadlineExceeded), false},
		{errors.New("launch claim c: bound epoch 1 is stale; current is 2"), false},
	} {
		if got := Refuses(tc.err); got != tc.tree {
			t.Errorf("Refuses(%v) = %t, want %t", tc.err, got, tc.tree)
		}
	}
}
