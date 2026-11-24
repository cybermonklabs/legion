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
// for that bucket nor opens it, and reads the stream no further (addBucketStream). The member here
// answers the lookup as one that has not applied the other client's create does, and sends the
// create to the server, which answers it as the cluster does (behindMember). The control is
// CreateCopiedBucket with the same member, which waits the member out and opens the bucket. A
// member whose misses were not all taken would hold nothing: the open never met the member behind.
func TestCreateMissingCopiedBucketRefusesABucketItsCreateFindsInUse(t *testing.T) {
	stream := "KV_" + SessionBucket
	_, uri := testnats.Start(t)
	conn, js := serverJetStream(t, uri)
	config, _ := CopiedBucketConfig(SessionBucket, 1)
	// Another configuration: two values a key, where the session bucket keeps one. The server
	// answers a create equal to the stream it holds as made, and this one "already in use".
	config.History = 2
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket another client made: %v", err)
	}

	// The lookup.
	member := &behindMember{JetStreamContext: js}
	member.misses.Store(1)
	start := time.Now()
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, member, SessionBucket, 1)
	waited := time.Since(start)
	if !errors.Is(err, ErrBucketExists) || !errors.Is(err, nats.ErrStreamNameAlreadyInUse) || !strings.Contains(err.Error(), stream+" create") || kv.Opened() {
		t.Fatalf("a create-only open whose create is answered \"already in use\" = %v (handle opened: %t), want ErrBucketExists named for the create and no handle", err, kv.Opened())
	}
	if waited >= refusalWait {
		t.Fatalf("the refusal took %s, want it answered within %s rather than waited out", waited.Round(time.Millisecond), refusalWait)
	}
	if creates := member.created(); len(creates) != 1 || !errors.Is(creates[0], nats.ErrStreamNameAlreadyInUse) {
		t.Fatalf("the create-only open's creates were answered %v, want one answered %v", creates, nats.ErrStreamNameAlreadyInUse)
	}
	if left := member.misses.Load(); left != 0 {
		t.Fatalf("the member was behind for %d reads the create-only open never made, so this row holds nothing", left)
	}

	// The lookup, the create's read of the stream and the lookup after the create.
	control := &behindMember{JetStreamContext: js}
	control.misses.Store(3)
	kv, err = CreateCopiedBucket(t.Context(), conn, control, SessionBucket, 1)
	if err != nil || kv.Bucket() != SessionBucket {
		t.Fatalf("control: CreateCopiedBucket with the same member = %v, want the bucket opened", err)
	}
	if left := control.misses.Load(); left != 0 {
		t.Fatalf("control: the member was behind for %d reads CreateCopiedBucket never made", left)
	}
}

// serverBefore211 answers each read of a stream (StreamInfo) as a nats-server before 2.11 does,
// with the stream's configuration and no metadata of the server's own (_nats.ver, _nats.level,
// _nats.req.level), and passes every other request to its member.
type serverBefore211 struct{ *behindMember }

func (s serverBefore211) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := s.behindMember.StreamInfo(stream, opts...)
	if info != nil {
		info.Config.Metadata = nil
	}
	return info, err
}

// A create-only open updates no stream its create finds in use, whatever configuration the server
// reports for it. Another client made the session bucket, between the open's lookup (which the
// member misses, behindMember) and its create, with the discard policy and direct get of a KV made
// before nats-server 2.7.2 and 2.9, and the server answers each read of the stream with no metadata
// of its own, as one before 2.11 does (serverBefore211). The open answers ErrBucketExists named for
// the create, sends the stream its create alone and leaves the bucket as the other client made it.
// The control is the normal path's step on the same answers (createBucketStream), which reads the
// stream, finds it equal to its own but for those two settings, and updates it. On nats-server
// 2.15.1's own answers, which carry that metadata, the normal path sends no update either, so that
// alone kept a create-only open from updating the bucket at 6e52228ad6, whose create was the normal
// path's: the open there answered no error, having updated the bucket and opened it.
func TestCreateMissingCopiedBucketUpdatesNoStreamItsCreateFindsInUse(t *testing.T) {
	stream := "KV_" + SessionBucket
	_, uri := testnats.Start(t)
	conn, js := serverJetStream(t, uri)
	config, _ := CopiedBucketConfig(SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket another client made: %v", err)
	}
	made, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	legacy := made.Config
	legacy.Discard, legacy.AllowDirect = nats.DiscardOld, false
	if _, err := js.UpdateStream(&legacy); err != nil {
		t.Fatalf("give the bucket an old discard policy and no direct get: %v", err)
	}
	before, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	writes := testnats.WatchStreamWrites(t, uri)

	// The lookup.
	member := &behindMember{JetStreamContext: js}
	member.misses.Store(1)
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, serverBefore211{member}, SessionBucket, 1)
	if !errors.Is(err, ErrBucketExists) || !errors.Is(err, nats.ErrStreamNameAlreadyInUse) || !strings.Contains(err.Error(), stream+" create") || kv.Opened() {
		t.Fatalf("a create-only open whose create finds the stream in use, on a server answering no metadata = %v (handle opened: %t), want ErrBucketExists named for the create and no handle", err, kv.Opened())
	}
	if got := writes.Take(t); len(got) != 1 || got[0] != "$JS.API.STREAM.CREATE."+stream {
		t.Fatalf("the create-only open sent %v, want its create alone", got)
	}
	after, err := js.StreamInfo(stream)
	if err != nil || !reflect.DeepEqual(after.Config, before.Config) {
		t.Fatalf("the bucket after the create-only open: %v, %+v; want it as the other client made it, %+v", err, after, before)
	}
	if left := member.misses.Load(); left != 0 {
		t.Fatalf("the member was behind for %d reads the create-only open never made, so this row holds nothing", left)
	}

	// The control: the normal path's step on the same answers updates the stream.
	control := serverBefore211{&behindMember{JetStreamContext: js}}
	if err := createBucketStream(t.Context(), conn, control, &config); err != nil {
		t.Fatalf("control: the normal path's create on a server answering no metadata = %v, want the stream updated", err)
	}
	if got := writes.Take(t); len(got) != 2 || got[1] != "$JS.API.STREAM.UPDATE."+stream {
		t.Fatalf("control: the normal path's create sent %v, want its create and the update", got)
	}
	if updated, err := js.StreamInfo(stream); err != nil || updated.Config.Discard != nats.DiscardNew || !updated.Config.AllowDirect {
		t.Fatalf("control: the bucket after the normal path's create: %v, %+v; want discard new and direct get", err, updated)
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
// (awaitBucketApplied): a clustered create is answered once the metadata group commits it, and a
// member that has not applied it yet answers a read of the bucket's stream "stream not found"
// (bucketAppliedInterval). The relay passes the lookup, and sends the first lookup after the
// create to a stream that is not there, which the server answers as such a member does; the
// lookup is taken again and gets the bucket. Without the wait, the open fails on that first
// answer. A real cluster cannot stand in here: its member catches up before the lookup asks, run
// after run.
func TestCreateMissingCopiedBucketWaitsForTheMemberToApplyItsCreate(t *testing.T) {
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(SessionBucket))
	conn, js := relayedJetStream(t, relay)
	t.Cleanup(func() { _ = js.DeleteKeyValue(SessionBucket) })

	relay.misdirect(1, 1, openSubject("envoy_not_applied_yet"))
	kv, err := CreateMissingCopiedBucket(t.Context(), conn, js, SessionBucket, 1)
	if err != nil {
		t.Fatalf("a create-only open whose member first answered its lookup after the create \"stream not found\" = %v, want the bucket opened once the member has it", err)
	}
	// Four reads ride the bucket's STREAM.INFO up to here: the lookup, the lookup after the create
	// answered "stream not found", that lookup again, which found the bucket, and the handle open.
	// The stream read below is a fifth.
	if sends := relay.sent(); sends != 4 {
		t.Fatalf("reads of the bucket's stream sent = %d, want 4: the lookup, the lookup after the create answered as a member behind, that lookup again, and the handle open", sends)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the bucket's stream through the handle: %v", err)
	}
}

// Every bucket a listener copies has the stream its create makes (CopiedBucketStreamConfig), at
// any copies and on any server version, and a bucket no listener copies has none.
func TestEveryCopiedBucketHasAStreamConfig(t *testing.T) {
	for _, bucket := range CopiedBuckets() {
		for _, version := range []string{"2.6.2", "2.15.1"} {
			stream, ok := CopiedBucketStreamConfig(bucket, 3, version)
			if !ok || stream.Name != "KV_"+bucket || stream.Replicas != 3 {
				t.Errorf("CopiedBucketStreamConfig(%s, 3, %s) = %+v, %t; want the bucket's stream at three copies", bucket, version, stream, ok)
			}
		}
	}
	if stream, ok := CopiedBucketStreamConfig("envoy_not_copied", 1, "2.15.1"); ok {
		t.Errorf("CopiedBucketStreamConfig of a bucket no listener copies = %+v, want none", stream)
	}
}
