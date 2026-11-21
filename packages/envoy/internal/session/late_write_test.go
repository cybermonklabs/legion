package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// lateSessionWrite runs late, once, before the first write of key through it (Put, Create or
// Update) reaches the bucket - what another writer does between a registration's read and its
// write - and sends on answers what the bucket answered each write of key, so a write its caller
// abandoned is seen too. late's own writes go through the raw handle and are not hooked. Create
// is hooked whole: nats.go's Create updates through its own handle, not through this one.
type lateSessionWrite struct {
	natsgo.KeyValue

	key     string
	late    func()
	writes  atomic.Int32
	answers chan error
}

func (kv *lateSessionWrite) Put(key string, value []byte) (uint64, error) {
	return kv.wrote(key, func() (uint64, error) { return kv.KeyValue.Put(key, value) })
}

func (kv *lateSessionWrite) Create(key string, value []byte) (uint64, error) {
	return kv.wrote(key, func() (uint64, error) { return kv.KeyValue.Create(key, value) })
}

func (kv *lateSessionWrite) Update(key string, value []byte, revision uint64) (uint64, error) {
	return kv.wrote(key, func() (uint64, error) { return kv.KeyValue.Update(key, value, revision) })
}

func (kv *lateSessionWrite) wrote(key string, write func() (uint64, error)) (uint64, error) {
	if key != kv.key {
		return write()
	}
	if kv.writes.Add(1) == 1 {
		kv.late()
	}
	revision, err := write()
	kv.answers <- err
	return revision, err
}

// A registration's write of a session the bucket held no entry for, which its caller abandons
// (the request it answers ended: bus.BoundedOn) and which another writer's entry and its delete
// (a deregistration, a reap) overtake before it lands, is refused over that delete and writes
// nothing more, so the delete stands. The caller's retry reads the bucket again and writes its
// entry over the delete it then reads (write, readStored): each write lands only over the very
// entry its own read found.
//
// Before the write went out as an Update at the revision its read answered, a read that found no
// entry wrote through Create, and nats.go's Create, refused over the delete marker, reads the key
// again and updates at that marker's revision (nats.go@v1.55.0 kv.go:729-739): the abandoned
// write landed over the delete and brought back a registration that had ended after its caller
// was told it was not written. A write its caller waits for cannot show that: refused, write
// reads the delete and writes the same entry over it itself, the very writes Create makes.
func TestALateSessionWriteIsRefusedOverADeleteMadeAfterItsRead(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	const sessionID = "ses_late_entry"
	raw := reg.watcher.KV().Legacy()
	ctx, abandon := context.WithCancel(context.Background())
	defer abandon()
	abandoned := make(chan struct{})
	late := &lateSessionWrite{KeyValue: raw, key: sessionID, answers: make(chan error, 2*writeAttempts), late: func() {
		abandon()
		<-abandoned
		if _, err := raw.Put(sessionID, []byte(`{"machine_id":"another-host"}`)); err != nil {
			t.Errorf("another writer stores %s: %v", sessionID, err)
		}
		if err := raw.Delete(sessionID); err != nil {
			t.Errorf("another writer deletes %s: %v", sessionID, err)
		}
	}}
	useKV(t, reg, late)

	err = reg.PutWithin(ctx, sessionID, SessionEntry{MachineID: "example-host", Dir: "/late"})
	close(abandoned)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("register %s answered %v, want its caller's end (context.Canceled): the row needs a write its caller abandoned before it reached the bucket", sessionID, err)
	}
	select {
	case err := <-late.answers:
		if !errors.Is(err, natsgo.ErrKeyExists) {
			t.Fatalf("the abandoned write planned on no entry answered %v after another writer's entry and delete, want the bucket's refusal (nats.ErrKeyExists): it landed over the delete made after its read", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned write never reached the bucket, so nothing overtook it")
	}
	// A write made again after that refusal would follow two reads of a local bucket, well
	// inside this wait.
	select {
	case err := <-late.answers:
		t.Fatalf("the abandoned write of %s, refused, wrote again (answered %v): it landed over the delete made after its read", sessionID, err)
	case <-time.After(500 * time.Millisecond):
	}
	if stored, err := raw.Get(sessionID); err == nil {
		t.Fatalf("the bucket holds %s for %s at revision %d after the abandoned write, want the delete made after its read", stored.Value(), sessionID, stored.Revision())
	} else if !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read %s after the abandoned write: %v", sessionID, err)
	}

	if err := reg.Put(sessionID, SessionEntry{MachineID: "example-host", Dir: "/late"}); err != nil {
		t.Fatalf("register %s again over the delete: %v", sessionID, err)
	}
	stored, err := raw.Get(sessionID)
	if err != nil {
		t.Fatalf("the bucket holds no entry of %s after the registration made again over the delete: %v", sessionID, err)
	}
	entry, err := decodeSessionEntry(stored.Value())
	if err != nil || entry.MachineID != "example-host" {
		t.Fatalf("the bucket holds %s for %s (%v), want the registration's own entry", stored.Value(), sessionID, err)
	}
}
