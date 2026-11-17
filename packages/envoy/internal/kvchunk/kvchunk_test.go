package kvchunk

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
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

// A record that fits one message is stored whole on a server of any max payload, even one too
// small to leave a chunk room, as every earlier build stores it; only a larger record is refused
// there. The control is a server whose max payload leaves a chunk room, where the larger record is
// laid out in chunks.
func TestARecordThatFitsIsStoredWholeWhateverTheMaxPayload(t *testing.T) {
	small := []byte(`{"session_id":"s"}`)
	for _, maxPayload := range []int{2048, 2389, 2390, 4096} {
		value, layout, chunks, err := layOut(small, maxPayload)
		if err != nil || layout.Chunked() || len(chunks) != 0 || string(value) != string(small) {
			t.Fatalf("a record of %d bytes under a max payload of %d is laid out as %q in %d chunks (%v), want it whole", len(small), maxPayload, value, len(chunks), err)
		}
	}
	large := make([]byte, 2000)
	if _, _, _, err := layOut(large, 2048); !errors.Is(err, bus.ErrTooLarge) {
		t.Fatalf("a record of 2000 bytes under a max payload of 2048 answered %v, want bus.ErrTooLarge", err)
	}
	if _, layout, chunks, err := layOut(large, 1600+HeaderBytes); err != nil || !layout.Chunked() || len(chunks) < 2 {
		t.Fatalf("a record of 2000 bytes under a max payload of %d is %d chunks (%v), want two or more", 1600+HeaderBytes, len(chunks), err)
	}
}
