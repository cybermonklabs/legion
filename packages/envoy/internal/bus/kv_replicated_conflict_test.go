package bus

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The control of TestConcurrentCompareAndSwapsOnAReplicatedBucketAreErrKeyExists: a
// compare-and-swap that lost to a write that had already landed, on the same replicated bucket, is
// refused with code 10071, which nats.go names nats.ErrKeyExists for an Update and a conditional
// Delete alike, and the same calls at the revision the bucket holds land. It passes with or without
// the handle's naming of code 10164, so a failure of the concurrent case is that code's and not
// the bucket's.
func TestALostCompareAndSwapOnAReplicatedBucketIsErrKeyExists(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client, err := Connect(cluster.URLs())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	legacy := cluster.CreateKeyValue(t, client.Conn, &nats.KeyValueConfig{Bucket: "kv_replicated_cas", Replicas: 3, Storage: nats.MemoryStorage})
	kv, err := WrapKeyValue(client.Conn, legacy)
	if err != nil {
		t.Fatalf("wrap the bucket: %v", err)
	}
	first, err := kv.Put("cas", []byte("one"))
	if err != nil {
		t.Fatalf("seed the key: %v", err)
	}
	second, err := kv.Update("cas", []byte("two"), first)
	if err != nil {
		t.Fatalf("the control update at the revision the bucket holds: %v", err)
	}

	_, err = kv.Update("cas", []byte("three"), first)
	if !errors.Is(err, nats.ErrKeyExists) {
		t.Fatalf("an update at a revision another write replaced answered %v, want nats.ErrKeyExists", err)
	}
	t.Logf("the lost update: %v", err)
	if err := kv.Delete("cas", nats.LastRevision(first)); !errors.Is(err, nats.ErrKeyExists) {
		t.Fatalf("a delete at a revision another write replaced answered %v, want nats.ErrKeyExists", err)
	}
	if err := kv.Delete("cas", nats.LastRevision(second)); err != nil {
		t.Fatalf("the control delete at the revision the bucket holds: %v", err)
	}
}

// Two compare-and-swaps of one key at one revision in flight together on a replicated bucket: the
// one the stream's leader proposes second is refused with code 10164 while the first is still being
// applied (nats-server's jetstream_batching.go), which nats.go names ErrKeyRevisionMismatch and not
// ErrKeyExists. Each round sends two at once; every refusal of either code must be ErrKeyExists,
// and the run must have met at least one 10164, or it proved nothing about that code.
func TestConcurrentCompareAndSwapsOnAReplicatedBucketAreErrKeyExists(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client, err := Connect(cluster.URLs())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	legacy := cluster.CreateKeyValue(t, client.Conn, &nats.KeyValueConfig{Bucket: "kv_replicated_race", Replicas: 3, Storage: nats.MemoryStorage})
	kv, err := WrapKeyValue(client.Conn, legacy)
	if err != nil {
		t.Fatalf("wrap the bucket: %v", err)
	}
	revision, err := kv.Put("cas", []byte("0"))
	if err != nil {
		t.Fatalf("seed the key: %v", err)
	}
	constant := 0
	for round := range 200 {
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := kv.Update("cas", []byte("x"), revision)
				errs <- err
			}()
		}
		for range 2 {
			err := <-errs
			if err == nil {
				continue
			}
			var apiErr *nats.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequenceConstant {
				constant++
			}
			if !errors.Is(err, nats.ErrKeyExists) {
				t.Fatalf("round %d: a lost compare-and-swap answered %v, want nats.ErrKeyExists", round, err)
			}
		}
		entry, err := kv.Get("cas")
		if err != nil {
			t.Fatalf("read the key: %v", err)
		}
		revision = entry.Revision()
	}
	t.Logf("%d refusals carried code 10164", constant)
	if constant == 0 {
		t.Fatal("no refusal carried code 10164, so this run proved nothing about it")
	}
}
