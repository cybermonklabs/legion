package consumernames

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// No call writes a record over the bucket's sentinel. RecordLastFloor checks its key as every other
// entry point does (Names.checkKey), so a key no family writes (SentinelKey, one holding a '.') is
// ErrInvalidKey before anything is read; and the KV store refuses the sentinel's key as a record's
// key on its own, in Read and Write, so no caller of the Store reaches it either. The control is
// RecordLastFloor of a session's own current, which records its floor. Before, RecordLastFloor read
// the sentinel as an empty record at its revision and wrote a record over it, after which every
// State of the bucket was ErrMalformedSentinel.
func TestNoCallWritesARecordOverTheSentinel(t *testing.T) {
	ctx := context.Background()
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, Boundary: 1})
	names := live.names("p1")
	for _, key := range []string{SentinelKey, "ses.A"} {
		if err := names.RecordLastFloor(ctx, key, "", 5, "names-stream"); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("RecordLastFloor(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if _, _, err := live.store.Read(ctx, SentinelKey); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("KVStore.Read(%s) = %v, want ErrInvalidKey", SentinelKey, err)
	}
	if _, err := live.store.Write(ctx, SentinelKey, Record{}, 0); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("KVStore.Write(%s) = %v, want ErrInvalidKey", SentinelKey, err)
	}
	if state, err := live.store.State(ctx); err != nil || state.Cutover != CutoverDone {
		t.Fatalf("the sentinel after the calls = %+v, %v; want it as written", state, err)
	}
	// Control: a session's own current records its floor.
	if _, err := live.store.Write(ctx, "ses-1", Record{Current: "session~A", Start: 3}, 0); err != nil {
		t.Fatal(err)
	}
	if err := names.RecordLastFloor(ctx, "ses-1", "session~A", 5, "names-stream"); err != nil {
		t.Fatalf("control: RecordLastFloor of ses-1's current = %v", err)
	}
	if record, _, err := live.store.Read(ctx, "ses-1"); err != nil || record.LastFloor == nil || record.LastFloor.Floor != 5 {
		t.Fatalf("control: ses-1's record after RecordLastFloor = %+v, %v; want last floor 5", record, err)
	}
}
