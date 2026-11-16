package cistore

import (
	"bytes"
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

// A head's CI record this listener last wrote (State.Writer) and the copy held when the bucket was
// deleted and created again empty is in the new bucket once the read that finds it completes, byte
// for byte as it was held, and a record another listener last wrote is left to that listener. The
// control runs the same schedule with the write-back left out, and the record is missing.
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
			s.owner = "m1"
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

			own := State{Owner: "o", Repo: "r", Number: "1", SHA: "own", LastEventAt: time.Now().UnixMilli(), Generation: 3, Writer: "m1"}
			foreign := State{Owner: "o", Repo: "r", Number: "1", SHA: "foreign", LastEventAt: time.Now().UnixMilli(), Writer: "m2"}
			raws := map[string][]byte{}
			for _, st := range []State{own, foreign} {
				raw, err := json.Marshal(st)
				if err != nil {
					t.Fatalf("encode %s: %v", st.SHA, err)
				}
				raws[st.SHA] = raw
				if _, err := base.Put(Key(st.Owner, st.Repo, st.Number, st.SHA), raw); err != nil {
					t.Fatalf("put %s: %v", st.SHA, err)
				}
			}
			waitCached(t, s, Key("o", "r", "1", "own"), func(st State) bool { return st.Generation == 3 })
			waitCached(t, s, Key("o", "r", "1", "foreign"), func(State) bool { return true })

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

			entry, err := recreated.Get(Key("o", "r", "1", "own"))
			if !reassert {
				if !errors.Is(err, natsgo.ErrKeyNotFound) {
					t.Fatalf("with no write-back the new bucket's record of the own head = %v, want none", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the new bucket's record of the own head: %v, want it written back", err)
			}
			if !bytes.Equal(entry.Value(), raws["own"]) {
				t.Fatalf("the written-back record = %s, want the held record byte for byte: %s", entry.Value(), raws["own"])
			}
			if _, err := recreated.Get(Key("o", "r", "1", "foreign")); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the new bucket's record of the foreign head = %v, want none: another listener wrote it", err)
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
