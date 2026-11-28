package graphstore

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
)

// TestGraphstoretestBaseAttributesAreTheStoresOwn holds graphstoretest's copy of the base
// attributes to the set IsBaseAttribute reads (baseAttributes), element for element, so a base
// attribute added here or taken out is added to or taken out of the list the tests outside this
// package read.
func TestGraphstoretestBaseAttributesAreTheStoresOwn(t *testing.T) {
	if copied := graphstoretest.BaseAttributes(); !slices.Equal(copied, baseAttributes[:]) {
		t.Errorf("graphstoretest.BaseAttributes is %q and the base attributes IsBaseAttribute reads are %q: change the copy with the set", copied, baseAttributes)
	}
}

// TestGraphstoretestVisibilitiesAreTheStoresOwn holds the visibilities graphstoretest.VisibilityFor
// answers to VisibilityProject and VisibilityAuthor: each word of this package is one it accepts,
// and what it answers for each is that word.
func TestGraphstoretestVisibilitiesAreTheStoresOwn(t *testing.T) {
	for _, row := range []struct {
		requested string
		proves    bool
		want      string
	}{
		{"", false, VisibilityProject},
		{VisibilityProject, false, VisibilityProject},
		{VisibilityAuthor, true, VisibilityAuthor},
	} {
		if visibility, err := graphstoretest.VisibilityFor(row.requested, row.proves); err != nil || visibility != row.want {
			t.Errorf("graphstoretest.VisibilityFor(%q, %t) answers %q (err %v), want %q: change its visibilities with this package's", row.requested, row.proves, visibility, err, row.want)
		}
	}
}
