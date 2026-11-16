package cistore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// withoutReassert is the store's cache with the write-back left out: a read of a recreated bucket
// begins exactly as the store's own does (beginRead) and writes nothing back. It is the control of
// the test below.
type withoutReassert struct{ ciCache }

func (c withoutReassert) BeginRead(read kvwatch.Read) { c.s.beginRead(read) }

// A head's CI record the copy held when the bucket was deleted and created again empty is in the
// new bucket once the read that finds it completes, encoded as it was held (a record with no schema
// stamp keeps none), and a record whose last event is older than the new bucket's TTL, which the
// deleted bucket had already expired, is left out. The control runs the same schedule with the
// write-back left out, and the record is missing.
func TestARecreatedCIBucketGetsBackTheRecordsTheCopyHeld(t *testing.T) {
	for _, reassert := range []bool{true, false} {
		name := "write-back"
		if !reassert {
			name = "control: no write-back"
		}
		t.Run(name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			s := openStore(t, conn)
			t.Cleanup(s.StopWatch)
			var cache kvwatch.Cache = ciCache{s}
			if !reassert {
				cache = withoutReassert{ciCache{s}}
			}
			base := s.watcher.KV()
			s.watcher.Stop()
			s.watcher = kvwatch.New("cistore", base, cache, kvwatch.WithStreamProbeInterval(0))
			s.watcher.Start()
			waitFollowing(t, s, 0)

			fresh := State{Owner: "o", Repo: "r", Number: "1", SHA: "fresh", LastEventAt: time.Now().UnixMilli(), Generation: 3}
			aged := State{Owner: "o", Repo: "r", Number: "1", SHA: "aged", LastEventAt: time.Now().Add(-2 * time.Hour).UnixMilli()}
			for _, st := range []State{fresh, aged} {
				raw, err := json.Marshal(st)
				if err != nil {
					t.Fatalf("encode %s: %v", st.SHA, err)
				}
				if _, err := base.Put(Key(st.Owner, st.Repo, st.Number, st.SHA), raw); err != nil {
					t.Fatalf("put %s: %v", st.SHA, err)
				}
			}
			waitCached(t, s, Key("o", "r", "1", "fresh"), func(st State) bool { return st.Generation == 3 })
			waitCached(t, s, Key("o", "r", "1", "aged"), func(State) bool { return true })

			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			if err := js.DeleteKeyValue(bus.CIStateBucket); err != nil {
				t.Fatalf("delete the CI bucket: %v", err)
			}
			recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, TTL: time.Hour, Storage: natsgo.FileStorage})
			if err := s.Rewatch(conn); err != nil {
				t.Fatalf("Rewatch: %v", err)
			}
			waitFollowing(t, s, 1)

			entry, err := recreated.Get(Key("o", "r", "1", "fresh"))
			if !reassert {
				if !errors.Is(err, natsgo.ErrKeyNotFound) {
					t.Fatalf("with no write-back the new bucket's record of the fresh head = %v, want none", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the new bucket's record of the fresh head: %v, want it written back", err)
			}
			var got State
			if err := json.Unmarshal(entry.Value(), &got); err != nil || got.Generation != 3 || got.Schema != 0 {
				t.Fatalf("the written-back record = %+v, %v; want generation 3 and no schema stamp, as held", got, err)
			}
			if _, err := recreated.Get(Key("o", "r", "1", "aged")); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the new bucket's record of the aged head = %v, want none: its last event is past the TTL", err)
			}
		})
	}
}

// waitFollowing waits until the store's watcher follows its bucket at epoch.
func waitFollowing(t *testing.T, s *Store, epoch uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.watcher.Epoch() != epoch || !s.watcher.Following() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the watcher to follow epoch %d", epoch)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
