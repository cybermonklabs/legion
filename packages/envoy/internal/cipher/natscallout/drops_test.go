package natscallout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A drop's detail is built only while none waits for the counter: add asks for it while no detail
// of its reason is held, and not once one is, so a flood of drops builds one string a reason a
// tick rather than one a drop. Every drop is counted all the same.
func TestADropAsksForItsDetailOnlyWhenNoneWaitsToBeLogged(t *testing.T) {
	d := &drops{}
	require.True(t, d.add(dropHubGaveUp), "the first drop was not asked for its detail")
	d.note(dropHubGaveUp, "the first")
	for range 3 {
		require.False(t, d.add(dropHubGaveUp), "a drop was asked for its detail while one waited to be logged")
	}
	require.True(t, d.add(dropUnopened), "another reason's first drop was not asked for its detail")
	d.note(dropUnopened, "another reason's first")
	require.EqualValues(t, 4, d.pending[dropHubGaveUp].Load(), "a drop was not counted")
	require.Equal(t, "the first", *d.first[dropHubGaveUp].Swap(nil), "the detail logged is not the first drop's")
	require.Equal(t, "another reason's first", *d.first[dropUnopened].Swap(nil), "a reason's first detail was lost behind another reason's")
	require.True(t, d.add(dropHubGaveUp), "the first drop after a log line was not asked for its detail")
}
