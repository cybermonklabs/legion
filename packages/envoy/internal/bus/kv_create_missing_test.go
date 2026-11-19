package bus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// serverJetStream is a connection to uri and a JetStream context on it, opened with the MaxWait the
// listener and Dispatch open theirs with.
func serverJetStream(t *testing.T, uri string) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream on %s: %v", uri, err)
	}
	return conn, js
}

// A create-only open creates a missing copied bucket with the configuration CreateCopiedBucket
// creates it with, and answers with a handle that reads and writes its keys. Each runs on a server
// of its own, so the two streams are compared whole.
func TestCreateMissingCopiedBucketCreatesWhatCreateCopiedBucketCreates(t *testing.T) {
	stream := "KV_" + SessionBucket
	streamOf := func(create func(context.Context, *nats.Conn, nats.JetStreamContext, string, int) (KeyValue, error)) (nats.StreamConfig, KeyValue) {
		_, uri := testnats.Start(t)
		conn, js := serverJetStream(t, uri)
		kv, err := create(t.Context(), conn, js, SessionBucket, 1)
		if err != nil {
			t.Fatalf("create %s: %v", SessionBucket, err)
		}
		info, err := js.StreamInfo(stream)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		return info.Config, kv
	}
	want, _ := streamOf(CreateCopiedBucket)
	got, kv := streamOf(CreateMissingCopiedBucket)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateMissingCopiedBucket made %+v\nCreateCopiedBucket makes %+v", got, want)
	}
	if _, err := kv.Put("session-1", []byte("held")); err != nil {
		t.Fatalf("write a key through the handle: %v", err)
	}
	if entry, err := kv.Get("session-1"); err != nil || string(entry.Value()) != "held" {
		t.Fatalf("read the key back through the handle: %v, %v", entry, err)
	}
}

// A create-only open of a bucket its lookup finds answers ErrBucketExists, named for the lookup, and
// leaves the bucket as it is: on a three-member cluster, a bucket of one copy opened at three is
// neither raised nor written at all. The control is CreateCopiedBucket on the same bucket, which
// raises it to three with a stream update, so the reading and the watch both see a change when there
// is one.
func TestCreateMissingCopiedBucketLeavesABucketItFindsAsItIs(t *testing.T) {
	stream := "KV_" + SessionBucket
	cluster := testnats.StartCluster(t, 3)
	uri := cluster.Servers[0].URL()
	conn, js := serverJetStream(t, uri)
	config, _ := CopiedBucketConfig(SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket at one copy: %v", err)
	}
	before, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	writes := testnats.WatchStreamWrites(t, uri, "DELETE", "PURGE")

	_, err = CreateMissingCopiedBucket(t.Context(), conn, js, SessionBucket, 3)
	if !errors.Is(err, ErrBucketExists) || !strings.Contains(err.Error(), stream+" STREAM.INFO") {
		t.Fatalf("a create-only open of a bucket its lookup finds = %v, want ErrBucketExists named for the lookup", err)
	}
	if got := writes.Take(t); len(got) != 0 {
		t.Fatalf("the create-only open wrote %v", got)
	}
	after, err := js.StreamInfo(stream)
	if err != nil || after.Config.Replicas != 1 || !after.Created.Equal(before.Created) || !reflect.DeepEqual(after.Config, before.Config) {
		t.Fatalf("the bucket after the create-only open: %v, %+v; want it as it was, %+v", err, after, before)
	}

	if _, err := CreateCopiedBucket(t.Context(), conn, js, SessionBucket, 3); err != nil {
		t.Fatalf("control: CreateCopiedBucket of the bucket at three copies: %v", err)
	}
	if got := writes.Take(t); len(got) != 1 || got[0] != "$JS.API.STREAM.UPDATE."+stream {
		t.Fatalf("control: CreateCopiedBucket wrote %v, want the update that raises the copies", got)
	}
	if raised, err := js.StreamInfo(stream); err != nil || raised.Config.Replicas != 3 {
		t.Fatalf("control: the bucket after CreateCopiedBucket: %v, %+v; want three copies", err, raised)
	}
}

// A create-only open whose create the server answers "stream name already in use" - another client
// made the bucket between its lookup and its create, with another configuration - answers
// ErrBucketExists named for the create, at once, with no handle: it neither waits a member out
// for that bucket nor opens it. The member here answers the lookup as one that has not applied the
// other client's create does, and the create as the cluster does (behindMember). The control is
// CreateCopiedBucket with the same member, which waits the member out and opens the bucket.
func TestCreateMissingCopiedBucketRefusesABucketItsCreateFindsInUse(t *testing.T) {
	stream := "KV_" + SessionBucket
	_, uri := testnats.Start(t)
	conn, js := serverJetStream(t, uri)
	config, _ := CopiedBucketConfig(SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket another client made: %v", err)
	}

	start := time.Now()
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, &behindMember{JetStreamContext: js, misses: 1}, SessionBucket, 1)
	waited := time.Since(start)
	if !errors.Is(err, ErrBucketExists) || !errors.Is(err, nats.ErrStreamNameAlreadyInUse) || !strings.Contains(err.Error(), stream+" create") || kv.Opened() {
		t.Fatalf("a create-only open whose create is answered \"already in use\" = %v (handle opened: %t), want ErrBucketExists named for the create and no handle", err, kv.Opened())
	}
	if waited >= refusalWait {
		t.Fatalf("the refusal took %s, want it answered within %s rather than waited out", waited.Round(time.Millisecond), refusalWait)
	}

	kv, err = CreateCopiedBucket(t.Context(), conn, &behindMember{JetStreamContext: js, misses: 1}, SessionBucket, 1)
	if err != nil || kv.Bucket() != SessionBucket {
		t.Fatalf("control: CreateCopiedBucket with the same member = %v, want the bucket opened", err)
	}
}

// A create-only open whose create draws no answer sends it again and creates the bucket, as
// CreateCopiedBucket does (TestAnUnansweredBucketCreateIsSentAgainAndTheStartGoesOn): the first
// create never reaches the server, so the bucket is the second create's, and the handle on it reads
// the bucket's stream.
func TestAnUnansweredCreateOfAMissingCopiedBucketIsSentAgain(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), createSubject(SessionBucket))
	conn, js := relayedJetStream(t, relay)
	t.Cleanup(func() { _ = js.DeleteKeyValue(SessionBucket) })

	relay.arm(1)
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, js, SessionBucket, 1)
	if err != nil {
		t.Fatalf("a create-only open whose first create drew no answer = %v, want the bucket created", err)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle: %v", err)
	}
	if sends := relay.sent(); sends != 2 {
		t.Fatalf("creates sent = %d, want 2: the lost one and the one that made the bucket", sends)
	}
}

// A create-only open answers with a handle only once the member answering has applied its create
// (wrapKeyValue, awaitBucketApplied): a clustered create is answered once the metadata group
// commits it, and a member that has not applied it yet answers a read of the bucket's stream
// "stream not found" (bucketAppliedInterval). The relay passes the lookup, and sends the first
// handle open after the create to a stream that is not there, which the server answers as such a
// member does; the open reads again and gets the bucket. Without the wait, the open fails on that
// first answer. A real cluster cannot stand in here: its member catches up before the handle open
// asks, run after run.
func TestCreateMissingCopiedBucketWaitsForTheMemberToApplyItsCreate(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(SessionBucket))
	conn, js := relayedJetStream(t, relay)
	t.Cleanup(func() { _ = js.DeleteKeyValue(SessionBucket) })

	relay.misdirect(1, 1, openSubject("envoy_not_applied_yet"))
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, js, SessionBucket, 1)
	if err != nil {
		t.Fatalf("a create-only open whose member first answered its handle open \"stream not found\" = %v, want the bucket opened once the member has it", err)
	}
	// Three reads ride the bucket's STREAM.INFO up to here: the lookup, the handle open answered
	// "stream not found", and the handle open that found the bucket. The stream read below is a
	// fourth.
	if sends := relay.sent(); sends != 3 {
		t.Fatalf("reads of the bucket's stream sent = %d, want 3: the lookup, the open answered as a member behind, and the open again", sends)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle: %v", err)
	}
}
