package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// replicatedConflict is the code a write that loses to a write of its key still being replicated
// carries on a stream of more than one copy (testnats.Cluster.HoldReplicatingWrite). The rows below
// check it first: a write that met the 10071 a stream of one copy answers, which nats.ErrKeyExists
// matches by code, proves nothing about a replicated bucket.
var replicatedConflict = &nats.APIError{ErrorCode: nats.JSErrCodeStreamWrongLastSequenceConstant}

// Each conditional write of a bucket of three copies that loses to a write of its key still being
// replicated is answered 10164, not the 10071 a bucket of one copy answers, and IsCASConflict counts
// it a conflict all the same: a Create of the key, an Update at its revision, and a Delete and a
// Purge at its revision (nats.LastRevision). nats.ErrKeyExists matches 10071 alone, so it misses the
// Update, the Delete and the Purge; nats.go v1.55.0 names those nats.ErrKeyRevisionMismatch, and
// wraps a Create's 10164 in nats.ErrKeyExists itself.
func TestAConditionalWriteThatLosesToAReplicatingWriteIsACASConflict(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_replicated_conflict_test"
	opened, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3, History: 5})
	if err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	cluster.MoveStreamLeader(t, "KV_"+bucket, 0)
	// A Create that lost reads the key back to tell a deleted key from one that is there, and
	// under the hold that read can reach a copy only over a frozen route: a short wait ends it,
	// and the Create answers its conflict all the same.
	shortJS, err := conn.JetStream(nats.MaxWait(500 * time.Millisecond))
	if err != nil {
		t.Fatalf("open JetStream with a short wait: %v", err)
	}
	legacy, err := shortJS.KeyValue(bucket)
	if err != nil {
		t.Fatalf("open the bucket with a short wait: %v", err)
	}
	kv := NewKeyValue(legacy, opened.WatchHandle(), opened.Streams())
	for _, row := range []struct {
		name  string
		key   string
		write func(key string, revision uint64) error
		// keyExists is what errors.Is(err, nats.ErrKeyExists) answers of the write's error.
		keyExists bool
	}{
		{"a create of the key", "create", func(key string, _ uint64) error {
			_, err := kv.Create(key, []byte("x"))
			return err
		}, true},
		{"an update at the key's revision", "update", func(key string, revision uint64) error {
			_, err := kv.Update(key, []byte("x"), revision)
			return err
		}, false},
		{"a delete at the key's revision", "delete", func(key string, revision uint64) error {
			return kv.Delete(key, nats.LastRevision(revision))
		}, false},
		{"a purge at the key's revision", "purge", func(key string, revision uint64) error {
			return kv.Purge(key, nats.LastRevision(revision))
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			revision, err := kv.Put(row.key, []byte("v"))
			if err != nil {
				t.Fatalf("put %s: %v", row.key, err)
			}
			release := cluster.HoldReplicatingWrite(t, "KV_"+bucket, "$KV."+bucket+"."+row.key)
			err = row.write(row.key, revision)
			release()
			if !errors.Is(err, replicatedConflict) {
				t.Fatalf("the write = %v, want the replicated stream's wrong last sequence, 10164", err)
			}
			if !IsCASConflict(err) {
				t.Fatalf("IsCASConflict(%v) = false, want a conflict", err)
			}
			if got := errors.Is(err, nats.ErrKeyExists); got != row.keyExists {
				t.Fatalf("errors.Is(%v, nats.ErrKeyExists) = %t, want %t", err, got, row.keyExists)
			}
		})
	}
}

// IsCASConflict answers for nothing but a lost conditional write: no error, a key that is not
// there, a stream that is not there, and a request nobody answered are no conflicts.
func TestIsCASConflictAnswersNoOtherError(t *testing.T) {
	for _, err := range []error{nil, nats.ErrKeyNotFound, nats.ErrStreamNotFound, nats.ErrTimeout, errors.New("nats: no responders available for request")} {
		if IsCASConflict(err) {
			t.Errorf("IsCASConflict(%v) = true, want false", err)
		}
	}
}

// A cursor write over a sequence that loses to a write of its subject still being replicated, on a
// stream of three copies, is ErrCursorMoved: the leader answers it 10164, not the 10071 a stream of
// one copy answers, and a lost cursor is a lost cursor whichever it answers. Only 10071 was
// ErrCursorMoved, so a kept role drain that lost its cursor to another listener's write on a
// replicated stream read the loss as a failed write.
func TestACursorWriteThatLosesToAReplicatingWriteIsCursorMoved(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client, err := ConnectOwningStream([]string{cluster.Servers[0].URL()}, WithReplicas(3))
	if err != nil {
		t.Fatalf("start on member %s: %v", cluster.Name(0), err)
	}
	t.Cleanup(client.Close)
	cluster.MoveStreamLeader(t, Stream, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	subject := KeptRoleCursorSubject("replicated-cursor")
	first, err := client.PublishCursorAt(ctx, subject, []byte(`{"kept":1}`), 0)
	if err != nil {
		t.Fatalf("the first cursor write: %v", err)
	}

	release := cluster.HoldReplicatingWrite(t, Stream, subject)
	_, err = client.PublishCursorAt(ctx, subject, []byte(`{"kept":2}`), first)
	release()
	if !errors.Is(err, replicatedConflict) {
		t.Fatalf("the cursor write = %v, want the replicated stream's wrong last sequence, 10164", err)
	}
	if !errors.Is(err, ErrCursorMoved) {
		t.Fatalf("the cursor write = %v, want an error wrapping ErrCursorMoved", err)
	}
}
