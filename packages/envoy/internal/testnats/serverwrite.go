package testnats

import (
	"errors"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The server writes a KV record itself when a request names that record's subject as its reply
// subject: the server publishes its answer there, and the bucket's stream stores whatever is
// published to its subjects. A user who may send the request needs no grant to the bucket's
// subjects to write one this way. These helpers make such a write for a test: a direct get's copy
// of another key's record (ServerCopiesKey), a JetStream API answer, and a pull consumer's status
// answer, which carries no data. Measured against the hub image, nats:2.15.1-alpine, whose server
// is nats-server v2.15.1, the version packages/envoy/go.mod builds the embedded test server from.

// KVSubject is the subject key's records are stored under in the bucket named bucket.
func KVSubject(bucket, key string) string { return "$KV." + bucket + "." + key }

// ServerCopiesKey makes the server store the newest record of srcKey in srcBucket under dstKey in
// dstBucket, the forge a client with a direct get and no write grant makes: it sends a direct get
// of srcKey whose reply names dstKey's subject, so the server publishes srcKey's value, with the
// headers a direct get adds (its source stream, subject and sequence, which a copy stored elsewhere
// still carries), to dstKey's subject, where dstBucket's stream stores it. It returns the stream
// sequence the copy is stored at (ServerWritesKey), dstBucket's own and not the source sequence the
// copied headers name.
//
// It asks through the direct get's body form, $JS.API.DIRECT.GET.KV_<srcBucket> with
// {"last_by_subj":"<srcKey's subject>"}, not the subject-encoded form
// $JS.API.DIRECT.GET.KV_<srcBucket>.<srcKey's subject>: on nats-server v2.15.1 the server answers
// the subject-encoded form to the requester alone, so a copy aimed at a subject of the source's own
// stream is never stored (a cross-stream copy of it is), while it publishes the body form's answer
// to the reply subject, which the stream stores either way.
func ServerCopiesKey(t testing.TB, conn *natsgo.Conn, dstBucket, dstKey, srcBucket, srcKey string) uint64 {
	t.Helper()
	request := "$JS.API.DIRECT.GET.KV_" + srcBucket
	data := []byte(`{"last_by_subj":"` + KVSubject(srcBucket, srcKey) + `"}`)
	return ServerWritesKey(t, conn, dstBucket, dstKey, request, data)
}

// ServerCopyRefused sends the direct get ServerCopiesKey sends - a copy of srcKey's record onto
// dstKey, its reply dstKey's subject - for a copy the server refuses to store, and fails the test
// unless dstBucket's stream holds no newer message on dstKey's subject once it has taken the copy.
// A copy of a record this build wrote (bound to its key by a conditional write) carries that
// write's stored Nats-Expected-Last-Subject-Sequence, so the server stores it only where dstKey's
// subject holds that many messages; onto a key that holds another session's record it is refused,
// and a direct-get copy can never overwrite a different occupied key this way. It returns dstKey's
// unchanged newest sequence.
//
// The stream's verdict is read with no span. A subscriber of dstKey's subject receives the server's
// copy, which the stream's subscription took in the same publish; then a publish to the same subject
// that the stream must refuse (an expected last subject sequence it cannot hold) is answered only
// once the stream has worked every message queued before it, the copy among them, and stores
// nothing. A wait of 2 s (before) could pass with a wrongly stored copy still on its way on a loaded
// machine, where ServerWritesKey's own copies take longer. That order holds on a stream of one copy,
// whose server works its subject's messages one at a time and refuses the marker with the wrong last
// sequence (10071) only after the copy; the leader of a stream of more copies can answer 10164 while
// the copy is still being stored. So the helper requires dstBucket's stream to hold one copy, and the
// marker's refusal to be 10071.
func ServerCopyRefused(t testing.TB, conn *natsgo.Conn, dstBucket, dstKey, srcBucket, srcKey string) uint64 {
	t.Helper()
	before := LastSequence(t, conn, dstBucket, dstKey)
	request := "$JS.API.DIRECT.GET.KV_" + srcBucket
	data := []byte(`{"last_by_subj":"` + KVSubject(srcBucket, srcKey) + `"}`)
	copies, err := conn.SubscribeSync(KVSubject(dstBucket, dstKey))
	if err != nil {
		t.Fatalf("subscribe to %s: %v", KVSubject(dstBucket, dstKey), err)
	}
	defer func() { _ = copies.Unsubscribe() }()
	if err := conn.PublishRequest(request, KVSubject(dstBucket, dstKey), data); err != nil {
		t.Fatalf("send %s answering to %s: %v", request, KVSubject(dstBucket, dstKey), err)
	}
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush %s: %v", request, err)
	}
	if _, err := copies.NextMsgWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("the server's copy of %s onto %s never reached its subject: %v", KVSubject(srcBucket, srcKey), KVSubject(dstBucket, dstKey), err)
	}
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if info, err := js.StreamInfo("KV_" + dstBucket); err != nil {
		t.Fatalf("read the stream of %s: %v", dstBucket, err)
	} else if info.Config.Replicas > 1 {
		t.Fatalf("ServerCopyRefused proves its order on a stream of one copy; KV_%s has %d", dstBucket, info.Config.Replicas)
	}
	marker := natsgo.ExpectLastSequencePerSubject(before + 1<<40)
	var api *natsgo.APIError
	if _, err := js.Publish(KVSubject(dstBucket, dstKey), nil, marker); err == nil {
		t.Fatalf("the stream stored a marker on %s that expects a sequence it cannot hold", KVSubject(dstBucket, dstKey))
	} else if !errors.As(err, &api) || api.ErrorCode != natsgo.JSErrCodeStreamWrongLastSequence {
		t.Fatalf("the marker on %s after the copy: %v, want the stream's wrong-last-sequence refusal (10071)", KVSubject(dstBucket, dstKey), err)
	}
	if stored := LastSequence(t, conn, dstBucket, dstKey); stored != before {
		t.Fatalf("the server stored a copy of %s under %s at sequence %d, want it refused and %s left at %d",
			KVSubject(srcBucket, srcKey), KVSubject(dstBucket, dstKey), stored, KVSubject(dstBucket, dstKey), before)
	}
	return before
}

// StreamInfo is the request for stream's info, answered with a JSON document.
func StreamInfo(stream string) string { return "$JS.API.STREAM.INFO." + stream }

// NoWaitPull creates a pull consumer on bucket's stream that holds no message, and returns the
// request and its data that ask it for one message without waiting: the server answers it with a
// 404 status that carries no data.
func NoWaitPull(t testing.TB, conn *natsgo.Conn, bucket string) (string, []byte) {
	t.Helper()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	const consumer = "server-write-pull"
	if _, err := js.AddConsumer("KV_"+bucket, &natsgo.ConsumerConfig{
		Durable:       consumer,
		AckPolicy:     natsgo.AckExplicitPolicy,
		FilterSubject: KVSubject(bucket, "server-write-pull-matches-nothing"),
	}); err != nil {
		t.Fatalf("create a pull consumer on KV_%s: %v", bucket, err)
	}
	return "$JS.API.CONSUMER.MSG.NEXT.KV_" + bucket + "." + consumer, []byte(`{"batch":1,"no_wait":true}`)
}

// ServerWritesKey sends request with data, its reply subject key's subject in bucket, so the
// server's own answer is stored as key's newest record. It waits through the shared wait
// (awaitStored) until the bucket's stream holds a message on that subject newer than the one it
// held before, and returns that message's stream sequence. Both are read from the stream's leader
// (a stream message get), never with a direct get: a direct get of a copied record reports the
// sequence its copied headers name, the source record's, and not the one the copy is stored at.
func ServerWritesKey(t testing.TB, conn *natsgo.Conn, bucket, key, request string, data []byte) uint64 {
	t.Helper()
	before := LastSequence(t, conn, bucket, key)
	if err := conn.PublishRequest(request, KVSubject(bucket, key), data); err != nil {
		t.Fatalf("send %s answering to %s: %v", request, KVSubject(bucket, key), err)
	}
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush %s: %v", request, err)
	}
	return awaitStored(t, fmt.Sprintf("the server's answer to %s to be stored under %s past sequence %d", request, KVSubject(bucket, key), before),
		conn, bucket, key, before)
}

// RepublishesKey stores body as key's newest record in bucket the way a RePublish does: a stream
// of its own, named after key, RePublishes the message it stores on a subject of its own to key's
// subject, so the server stores body there with that stream's Nats-Stream, Nats-Subject and
// Nats-Sequence headers, the last naming that stream's sequence (1) and not the one the bucket
// stores it at. Any user that may create a stream and publish to it can write one this way. It
// waits through the shared wait (awaitStored) until the bucket's stream holds a message on key's
// subject newer than the one it held before, read from the stream's leader, and returns that
// message's stream sequence.
func RepublishesKey(t testing.TB, conn *natsgo.Conn, bucket, key string, body []byte) uint64 {
	t.Helper()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	stream, subject := "REPUBLISH_"+bucket+"_"+key, "republish."+bucket+"."+key
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{subject}, Storage: natsgo.MemoryStorage,
		RePublish: &natsgo.RePublish{Source: subject, Destination: KVSubject(bucket, key)}}); err != nil {
		t.Fatalf("create the republishing stream %s: %v", stream, err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(stream) })
	before := LastSequence(t, conn, bucket, key)
	if _, err := js.Publish(subject, body); err != nil {
		t.Fatalf("publish the body to %s: %v", subject, err)
	}
	return awaitStored(t, fmt.Sprintf("%s's RePublish to be stored under %s past sequence %d", stream, KVSubject(bucket, key), before),
		conn, bucket, key, before)
}

// awaitStored waits through the shared wait (testwait.Until) until bucket's stream holds a message
// on key's subject newer than before, read from the stream's leader (LastSequence), and returns that
// message's stream sequence. what names the write the wait is for, and its last reading is the
// newest sequence it read.
func awaitStored(t testing.TB, what string, conn *natsgo.Conn, bucket, key string, before uint64) uint64 {
	t.Helper()
	return uint64(testwait.Until(t, what, func() (newestSequence, bool) {
		stored := LastSequence(t, conn, bucket, key)
		return newestSequence(stored), stored > before
	}))
}

// newestSequence is the newest stream sequence a wait read for a key, which its failure prints.
type newestSequence uint64

func (s newestSequence) String() string { return fmt.Sprintf("newest sequence %d", uint64(s)) }

// LastSequence is the stream sequence of the newest message bucket's stream holds for key, read
// from the stream's leader, and 0 when it holds none. The read waits as long as MaxWait's, since one
// cut at nats.go's own 5 s would fail the test on a loaded machine.
func LastSequence(t testing.TB, conn *natsgo.Conn, bucket, key string) uint64 {
	t.Helper()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	message, err := js.GetLastMsg("KV_"+bucket, KVSubject(bucket, key))
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the newest message of %s: %v", KVSubject(bucket, key), err)
	}
	return message.Sequence
}
