package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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

// A Create of a key whose last record is a delete marker that loses to a write of the key still
// being replicated, on a bucket of three copies, is a CAS conflict. nats.go v1.55.0's Create sends
// its write at revision 0, reads the key back on that write's refusal, and, finding the delete
// marker (nats.ErrKeyDeleted), sends the write again at the marker's revision and returns that
// write's error as it is (kv.go:737-738): neither nats.ErrKeyExists nor nats.ErrKeyRevisionMismatch
// wraps it, so the leader's 10164 is caught by IsCASConflict's code alone. The store's role claim
// takes this path whenever it writes at revision 0 over a deleted record. The store's interest
// write does not: it replaces a delete marker by an Update at the revision the stream's leader
// reports for the marker (store's createInterest), and nats.go names that Update's lost race
// nats.ErrKeyRevisionMismatch, which IsCASConflict matches by class.
//
// The read must be answered while the routes are frozen. A direct get, which every bucket of this
// build reads by, goes to whichever copy's queue subscriber the server picks, a remote one as
// readily as its own (client.go:5639-5697 in nats-server v2.15.1), and a remote one cannot answer
// under the hold. So this bucket's stream is set to take no direct gets: the read is then the
// stream leader's message get, answered by member 0, and the write path, which is all this row
// is about, is the same.
func TestACreateOverADeleteMarkerThatLosesToAReplicatingWriteIsACASConflict(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_replicated_marker_test"
	opened, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3, History: 5})
	if err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	undirected := info.Config
	undirected.AllowDirect = false
	if _, err := js.UpdateStream(&undirected); err != nil {
		t.Fatalf("take direct gets off the bucket's stream: %v", err)
	}
	cluster.MoveStreamLeader(t, "KV_"+bucket, 0)
	// No span of its own: an answer can come late on a loaded machine with nothing wrong.
	bucketJS, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream for the bucket: %v", err)
	}
	legacy, err := bucketJS.KeyValue(bucket)
	if err != nil {
		t.Fatalf("open the bucket with no direct gets: %v", err)
	}
	kv := NewKeyValue(legacy, opened.WatchHandle(), opened.Streams())
	if _, err := kv.Put("marked", []byte("v")); err != nil {
		t.Fatalf("put the key: %v", err)
	}
	if err := kv.Delete("marked"); err != nil {
		t.Fatalf("delete the key: %v", err)
	}
	cluster.AwaitReplicated(t, "KV_"+bucket)

	release := cluster.HoldReplicatingWrite(t, "KV_"+bucket, "$KV."+bucket+".marked")
	_, err = kv.Create("marked", []byte("x"))
	release()
	if !errors.Is(err, replicatedConflict) {
		t.Fatalf("the create = %v, want the replicated stream's wrong last sequence, 10164", err)
	}
	// The write again at the marker's revision is the one that answered: Create's own refusal
	// wraps nats.ErrKeyExists, and this error carries neither class.
	if errors.Is(err, nats.ErrKeyExists) || errors.Is(err, nats.ErrKeyRevisionMismatch) {
		t.Fatalf("the create = %v, want the raw answer of the write over the delete marker, not one nats.go named", err)
	}
	if !IsCASConflict(err) {
		t.Fatalf("IsCASConflict(%v) = false, want a conflict", err)
	}
	// The write the Create lost to rewrote the delete marker, so the key is still deleted: a read
	// answers no key, and the record it stands at is a delete marker.
	if entry, err := kv.Get("marked"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("read the key after the race = %v, %v; want nats.ErrKeyNotFound, the key still deleted", entry, err)
	}
	last, err := js.GetLastMsg("KV_"+bucket, "$KV."+bucket+".marked")
	if err != nil {
		t.Fatalf("read the key's last record: %v", err)
	}
	if op := last.Header.Get("KV-Operation"); op != "DEL" {
		t.Fatalf("the key's last record (revision %d) has KV-Operation %q, want DEL, a delete marker", last.Sequence, op)
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
	// No span of its own: an answer can come late on a loaded machine with nothing wrong.
	ctx := testwait.Context(t)
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
