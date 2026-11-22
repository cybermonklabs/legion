package kvwatch_test

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A read barrier (bus.BarrierKey) is no record of the bucket's, and no cache is handed one: not one
// a scan streams past at a read's start, and not one a later read writes while the copy follows the
// bucket. The copy keeps no row and no fence for it, and the warm-up counts it as no marker. Every
// copy a store keeps - the interest, role claim, session, known session and CI caches - is fed
// through these hooks alone.
func TestAWatcherHandsNoCacheTheReadBarrier(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "before")
	writeBarrier(t, kv)

	logger, records := jsonLogger(t)
	into := &recordingCache{seen: newSeen()}
	w := kvwatch.New("interest registry", kv, into, kvwatch.WithLogger(logger), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", func() bool { return len(into.completions()) == 1 })
	if line := record(t, records(), "interest registry cache warm-up"); line["entries"] != float64(1) || line["delete_markers"] != float64(0) {
		t.Errorf("the warm-up over a bucket holding one key and a barrier logged entries=%v delete_markers=%v, want 1 and 0: %v",
			line["entries"], line["delete_markers"], line)
	}

	writeBarrier(t, kv)
	putKeys(t, kv, "after")
	eventually(t, "the copy to take the key written after the barrier", func() bool { return into.has("after") })
	if applied := into.appliedKeys(); slices.Contains(applied, bus.BarrierKey) {
		t.Errorf("the cache was handed %v, the barrier among them", applied)
	}
	if fence := into.fenceOf(w, bus.BarrierKey); fence != 0 || into.has(bus.BarrierKey) {
		t.Fatalf("the copy holds the barrier: row %v, fence %d", into.has(bus.BarrierKey), fence)
	}
}

// No scan hands its visitor a read barrier or counts it: not the scan of one pattern
// (kvwatch.ScanExistingKeys: the interest marker collector, the role claim restore and its revision
// read) and not the scan of several (kvwatch.ScanFilteredKeys).
func TestScansHandNoVisitorTheReadBarrier(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "live")
	writeBarrier(t, kv)
	for name, scan := range map[string]func(visit func(kvwatch.Entry) error) (kvwatch.KeyScan, error){
		"ScanExistingKeys": func(visit func(kvwatch.Entry) error) (kvwatch.KeyScan, error) {
			return kvwatch.ScanExistingKeys(context.Background(), kv, jetstream.AllKeys, "test scan", 10*time.Second, slog.Default(), visit)
		},
		"ScanFilteredKeys": func(visit func(kvwatch.Entry) error) (kvwatch.KeyScan, error) {
			return kvwatch.ScanFilteredKeys(kv, []string{"*"}, "test scan", 10*time.Second, slog.Default(), visit)
		},
	} {
		var visited []string
		got, err := scan(func(entry kvwatch.Entry) error {
			visited = append(visited, entry.Key)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := (kvwatch.KeyScan{Puts: 1}); got != want || !slices.Equal(visited, []string{"live"}) {
			t.Errorf("%s over one key and a barrier = %+v visiting %v, want %+v visiting [live]", name, got, visited, want)
		}
	}
}

// writeBarrier writes a read barrier into the bucket kv is a handle on, as every read does
// (bus.KeyValue.WriteBarrier).
func writeBarrier(t *testing.T, kv bus.KeyValue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := kv.WriteBarrier(ctx); err != nil {
		t.Fatalf("write a read barrier: %v", err)
	}
}

// recordingCache is a seen that records the key of every record it is handed.
type recordingCache struct {
	*seen
	mu      sync.Mutex
	applied []string
}

func (c *recordingCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.mu.Lock()
	c.applied = append(c.applied, entry.Key)
	c.mu.Unlock()
	c.seen.Apply(entry, read)
}

func (c *recordingCache) appliedKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.applied)
}
