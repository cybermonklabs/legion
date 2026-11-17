package kvchunk_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
)

// A damaged chunked record is answered with no layout, so no store deletes the chunks its manifest
// names: they were never proved to be its own, and a manifest any writer of the bucket can store
// may name another record's. Read answers it so on every damage it can meet - a chunk missing at
// the manifest's own revision, and chunks that join into another record than the manifest names -
// and a record it reads whole is the control, answered with its layout.
func TestReadAnswersADamagedRecordWithNoLayout(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: "kvchunk_read", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	record := []byte(`{"topics":["` + strings.Repeat("x", 3*kvchunktest.MaxPayload) + `"]}`)
	write := func(key string) kvchunk.Written {
		t.Helper()
		written, err := kvchunk.Write(t.Context(), kv, key, record, kvchunk.Layout{}, func(head []byte) (uint64, error) {
			return kv.Put(key, head)
		}, time.Now, slog.Default(), "record")
		if err != nil || !written.Stored || !written.Layout.Chunked() {
			t.Fatalf("write %s: %+v (%v), want a chunked record stored", key, written, err)
		}
		return written
	}
	read := func(key string) ([]byte, kvchunk.Layout, error) {
		t.Helper()
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		value, layout, _, err := kvchunk.Read(context.Background(), kv, key, entry)
		return value, layout, err
	}

	whole := write("whole")
	value, layout, err := read("whole")
	if err != nil || !slices.Equal(value, record) || layout != whole.Layout {
		t.Fatalf("a whole chunked record reads %d bytes with layout %+v (%v), want the record and %+v", len(value), layout, err, whole.Layout)
	}

	missing := write("missing")
	if err := kv.Delete(missing.Layout.Keys()[1]); err != nil {
		t.Fatalf("delete a chunk: %v", err)
	}
	changed := write("changed")
	chunk, err := kv.Get(changed.Layout.Keys()[0])
	if err != nil {
		t.Fatalf("read a chunk: %v", err)
	}
	altered := slices.Clone(chunk.Value())
	altered[0] ^= 1
	if _, err := kv.Put(changed.Layout.Keys()[0], altered); err != nil {
		t.Fatalf("rewrite a chunk: %v", err)
	}
	for _, key := range []string{"missing", "changed"} {
		if _, layout, err := read(key); !errors.Is(err, kvchunk.ErrDamaged) || layout != (kvchunk.Layout{}) {
			t.Fatalf("a record with a %s chunk reads layout %+v (%v), want no layout and ErrDamaged", key, layout, err)
		}
	}
}
