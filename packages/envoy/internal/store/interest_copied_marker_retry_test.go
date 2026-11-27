package store

import (
	"context"
	"sync"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// countedUpdates counts the conditional writes the registry sends through it, with the revision
// each was fenced on.
type countedUpdates struct {
	natsgo.KeyValue

	mu   sync.Mutex
	last []uint64
}

func (kv *countedUpdates) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.mu.Lock()
	kv.last = append(kv.last, last)
	kv.mu.Unlock()
	return kv.KeyValue.Update(key, value, last)
}

func (kv *countedUpdates) fences() []uint64 {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return append([]uint64(nil), kv.last...)
}

// An interest write meets a delete marker the server copied onto the session's key from another
// key's. Every write of a row is one Update at the revision of the key's newest entry its read
// found (putInterestAt), and a direct get reports the copy at its source's sequence, the
// Nats-Sequence header the copy carries first, so the first write is refused. The write made again
// reads the key from the stream's leader (readLeader: UpsertWithin's retry, and
// PlanInterestAfterConflict for the planned write), which reports the sequence the marker is stored
// at, and that one retry lands: the row is the session's own, under its own key, and no third write
// is sent.
//
// Control: with the retry reading the bucket by a direct get as the first try does (readBucket in
// place of readLeader in UpsertWithin's retry and in PlanInterestAfterConflict), every retry is
// fenced on the copy's reported revision again and refused, and the merging write ends with
// ErrInterestTopicsNotMerged, and the planned write with ErrInterestPlanConflict, after its
// attempts.
func TestAnInterestWriteOverACopiedMarkerLandsOnOneRetryFromTheLeader(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	ctx := context.Background()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.InterestBucket, err)
	}
	counted := &countedUpdates{KeyValue: kv}
	useKV(t, reg, counted)
	for _, write := range rowWrites {
		t.Run(write.name, func(t *testing.T) {
			victim := "ses_copied_marker_retry_" + write.suffix
			source := victim + "_source"
			if _, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic}); err != nil {
				t.Fatalf("register %s: %v", victim, err)
			}
			putInterest(t, kv, Interest{SessionID: source, MachineID: "shared-a", Topics: []string{bindingPubTopic}})
			if err := kv.Delete(source); err != nil {
				t.Fatalf("delete %s: %v", source, err)
			}
			reported := testnats.LastSequence(t, conn, bus.InterestBucket, source)
			stored := testnats.ServerCopiesKey(t, conn, bus.InterestBucket, victim, bus.InterestBucket, source)
			if stored == reported {
				t.Fatalf("the copy of %s's marker is stored at its source's sequence %d: the row shows nothing to correct", source, reported)
			}
			awaitInterestApplied(t, reg, victim, stored)

			before := len(counted.fences())
			row, err := write.write(ctx, reg, Interest{SessionID: victim, MachineID: "edge-a", Dir: "/priv"}, []string{bindingPrivTopic})
			if err != nil {
				t.Fatalf("%s for %s over the marker copied at %d: %v (writes fenced on %v)", write.name, victim, stored, err, counted.fences()[before:])
			}
			requireOwnRow(t, row, victim, write.name)
			fences := counted.fences()[before:]
			if len(fences) != 2 || fences[1] != stored {
				t.Fatalf("%s for %s sent writes fenced on %v, want two: one the bucket refused, then one at the marker's own sequence %d read from the stream's leader",
					write.name, victim, fences, stored)
			}
			own, revision, err := reg.RefreshAt(ctx, victim)
			if err != nil || revision <= stored {
				t.Fatalf("the bucket's row of %s is %+v at revision %d (%v), want one written after the marker copied at %d", victim, own, revision, err, stored)
			}
			requireOwnRow(t, own, victim, "the bucket's row of "+victim)
		})
	}
}
