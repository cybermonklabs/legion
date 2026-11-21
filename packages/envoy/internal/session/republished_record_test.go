package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A RePublish of another stream stores, at a session's key, a body that reads as the session's own
// record - its key, a machine, a time, its projects and the client instance a conditional delete
// names - with that stream's headers, so a direct get reports it at that stream's sequence, the
// control, and a delete fenced there is refused. After that refusal the conditional delete and the
// plain delete each read the key from the stream's leader, find a message the server wrote, and
// delete it at the sequence it is stored at (bus.KeyValue.DeleteServerWritten). The leader's read
// the edge confirms a session by (StoredByWriter) reads the session's own record as a writer's and
// the republished one as none.
func TestADeleteOverARepublishedRecordDeletesItAtItsTrueSequence(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	ctx := context.Background()
	kv, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	for _, tc := range []struct {
		victim string
		delete func(victim string) error
	}{
		{"ses_republished_deleted_if", func(victim string) error {
			outcome, err := r.DeleteIf(ctx, victim, DeleteConditions{ClientInstanceID: "instance-victim"})
			if err == nil && outcome != EntryDeleted {
				return errors.New(outcome.String())
			}
			return err
		}},
		{"ses_republished_deleted", func(victim string) error { return r.Delete(ctx, victim) }},
	} {
		if err := r.Put(tc.victim, SessionEntry{MachineID: "edge-a", ClientInstanceID: "instance-victim", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", tc.victim, err)
		}
		if own, err := r.StoredByWriter(ctx, tc.victim); err != nil || !own {
			t.Fatalf("StoredByWriter(%s) over its own registration = %t, %v; want true", tc.victim, own, err)
		}
		body, err := json.Marshal(map[string]any{"session_id": tc.victim, "machine_id": "edge-a", "updated_at": time.Now().UnixMilli(),
			"projects": []string{"PRIV"}, "client_instance_id": "instance-victim"})
		if err != nil {
			t.Fatalf("encode the republished record: %v", err)
		}
		stored := testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, tc.victim, body)
		if entry, err := kv.Get(tc.victim); err != nil || entry.Revision() == stored {
			t.Fatalf("a direct get of %s answers revision %v (%v), stored at %d: want another stream's sequence, or the control shows nothing to correct", tc.victim, entry, err, stored)
		}
		if own, err := r.StoredByWriter(ctx, tc.victim); err != nil || own {
			t.Fatalf("StoredByWriter(%s) over the republished record = %t, %v; want false", tc.victim, own, err)
		}
		if err := tc.delete(tc.victim); err != nil {
			t.Fatalf("delete %s over the record republished at %d: %v", tc.victim, stored, err)
		}
		if _, err := kv.Get(tc.victim); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("Get(%s) after the delete = %v, want %v", tc.victim, err, nats.ErrKeyNotFound)
		}
	}
}
