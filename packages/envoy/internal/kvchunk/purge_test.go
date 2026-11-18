package kvchunk_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
)

// malformedChunkKeys are keys under the chunk prefix that no write of this build makes, and that any
// writer of a bucket could get a watcher to deliver: wildcards a purge filter would widen to every
// chunk, a generation of the wrong length or alphabet, and an index that is not a plain decimal.
var malformedChunkKeys = []string{
	kvchunk.Prefix + ">",
	kvchunk.Prefix + "*.*",
	kvchunk.Prefix + strings.Repeat("A", 26) + ".*",
	kvchunk.Prefix + strings.Repeat("A", 26) + ".>",
	kvchunk.Prefix + "AAAA.0",
	kvchunk.Prefix + strings.Repeat("a", 26) + ".0",
	kvchunk.Prefix + strings.Repeat("A", 26),
	kvchunk.Prefix + strings.Repeat("A", 26) + ".01",
	kvchunk.Prefix + strings.Repeat("A", 26) + ".-1",
	kvchunk.Prefix + strings.Repeat("A", 26) + ".0.1",
}

// A chunk key becomes a purge filter only in the shape this build writes,
// `=chunk.<26-character base32 generation>.<decimal index>`: PurgeMarkers refuses any other key
// under the prefix before it sends a purge, so a wildcard key cannot widen a purge to every chunk
// subject of the bucket, and a refused key purges nothing. A well-formed key's marker is the
// control: it is purged.
func TestAMalformedChunkKeyIsRefusedAndPurgesNothing(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: "kvchunk_purge", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	record := []byte(`{"topics":["` + strings.Repeat("x", 3*kvchunktest.MaxPayload) + `"]}`)
	written, err := kvchunk.Write(t.Context(), kv, "live", record, nil, kvchunk.Layout{}, func(head []byte) (uint64, error) {
		return kv.Put("live", head)
	}, time.Now, slog.Default(), "record")
	if err != nil || !written.Layout.Chunked() {
		t.Fatalf("store a chunked record: %+v (%v)", written, err)
	}
	subjects := func() int {
		t.Helper()
		info, err := js.StreamInfo("KV_kvchunk_purge", &nats.StreamInfoRequest{SubjectsFilter: "$KV.kvchunk_purge." + kvchunk.Prefix + ">"})
		if err != nil {
			t.Fatalf("read the chunk subjects: %v", err)
		}
		return len(info.State.Subjects)
	}
	before := subjects()
	for _, key := range malformedChunkKeys {
		if purged, err := kvchunk.PurgeMarkers(t.Context(), kv, []string{key}); err == nil || purged != 0 {
			t.Fatalf("a purge of the malformed chunk key %q answered %d purged (%v), want it refused", key, purged, err)
		}
		if after := subjects(); after != before {
			t.Fatalf("a refused purge of %q changed the bucket's chunk subjects from %d to %d", key, before, after)
		}
	}

	// The control: the marker a well-formed chunk key's delete leaves is purged.
	replaced := written.Layout.Keys()[0]
	if err := kv.Delete(replaced); err != nil {
		t.Fatalf("delete a chunk: %v", err)
	}
	if purged, err := kvchunk.PurgeMarkers(t.Context(), kv, []string{replaced}); err != nil || purged != 1 {
		t.Fatalf("a purge of the well-formed chunk key %s answered %d purged (%v), want 1", replaced, purged, err)
	}
	if after := subjects(); after != before-1 {
		t.Fatalf("after the control purge the bucket holds %d chunk subjects, want %d", after, before-1)
	}
}
