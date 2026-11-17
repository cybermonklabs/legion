package kvchunk

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// A chunk key's marker is due for its purge once MarkerGrace has passed since it was first
// delivered, and a put on the same key afterwards - a write that stored a value there again -
// takes the key off the list, so no purge takes a value the bucket holds. A marker delivered again
// by a later read keeps its first moment. The control is a key whose marker no put follows, which
// stays due.
func TestAChunkMarkerAPutFollowsIsNotDueForItsPurge(t *testing.T) {
	markers := NewMarkers()
	start := time.Now()
	for _, key := range []string{Prefix + "A.0", Prefix + "B.0"} {
		markers.Delivered(kvwatch.Entry{Key: key, Op: kvwatch.OpDelete}, start)
	}
	markers.Delivered(kvwatch.Entry{Key: Prefix + "A.0", Op: kvwatch.OpDelete}, start.Add(MarkerGrace))
	markers.Delivered(kvwatch.Entry{Key: Prefix + "B.0", Op: kvwatch.OpPut}, start.Add(time.Second))
	// A store asks for the markers due at now as Due(now - MarkerGrace).
	dueAt := func(now time.Time) []string { return markers.Due(now.Add(-MarkerGrace)) }
	if due := dueAt(start.Add(MarkerGrace - time.Second)); len(due) != 0 {
		t.Fatalf("inside the grace the markers due are %v, want none", due)
	}
	if due := dueAt(start.Add(MarkerGrace)); !slices.Equal(due, []string{Prefix + "A.0"}) {
		t.Fatalf("past the grace of the first delivery the markers due are %v, want %s alone", due, Prefix+"A.0")
	}
}
