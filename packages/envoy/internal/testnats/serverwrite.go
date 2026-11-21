package testnats

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// The server writes a KV record itself when a request names that record's subject as its reply
// subject: the server publishes its answer there, and the bucket's stream stores whatever is
// published to its subjects. A user who may send the request needs no grant to the bucket's
// subjects to write one this way. These helpers make such a write for a test, in the request
// shapes a measurement of nats-server 2.15.3 found stored: a direct get's copy of another key's
// record, a JetStream API answer, and a pull consumer's status answer, which carries no data.

// KVSubject is the subject key's records are stored under in the bucket named bucket.
func KVSubject(bucket, key string) string { return "$KV." + bucket + "." + key }

// DirectGetLast is the request for key's newest record in bucket, answered by a direct get: the
// answer is that record's value with its headers, and the headers a direct get adds name the
// record's own stream sequence, which a copy of it stored elsewhere still carries.
func DirectGetLast(bucket, key string) string {
	return "$JS.API.DIRECT.GET.KV_" + bucket + "." + KVSubject(bucket, key)
}

// StreamInfo is the request for stream's info, answered with a JSON document.
func StreamInfo(stream string) string { return "$JS.API.STREAM.INFO." + stream }

// NoWaitPull creates a pull consumer on bucket's stream that holds no message, and returns the
// request and its data that ask it for one message without waiting: the server answers it with a
// 404 status that carries no data.
func NoWaitPull(t testing.TB, conn *natsgo.Conn, bucket string) (string, []byte) {
	t.Helper()
	js, err := conn.JetStream()
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
// server's own answer is stored as key's newest record. It waits until the bucket's stream holds a
// message on that subject newer than the one it held before, and returns that message's stream
// sequence. Both are read from the stream's leader (a stream message get), never with a direct
// get: a direct get of a copied record reports the sequence its copied headers name, the source
// record's, and not the one the copy is stored at.
func ServerWritesKey(t testing.TB, conn *natsgo.Conn, bucket, key, request string, data []byte) uint64 {
	t.Helper()
	before := LastSequence(t, conn, bucket, key)
	if err := conn.PublishRequest(request, KVSubject(bucket, key), data); err != nil {
		t.Fatalf("send %s answering to %s: %v", request, KVSubject(bucket, key), err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush %s: %v", request, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if stored := LastSequence(t, conn, bucket, key); stored > before {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server's answer to %s was not stored under %s within 10 s (newest sequence %d)", request, KVSubject(bucket, key), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// RepublishesKey stores body as key's newest record in bucket the way a RePublish does: a stream
// of its own, named after key, RePublishes the message it stores on a subject of its own to key's
// subject, so the server stores body there with that stream's Nats-Stream, Nats-Subject and
// Nats-Sequence headers, the last naming that stream's sequence (1) and not the one the bucket
// stores it at. Any user that may create a stream and publish to it can write one this way. It
// waits until the bucket's stream holds a message on key's subject newer than the one it held
// before, read from the stream's leader, and returns that message's stream sequence.
func RepublishesKey(t testing.TB, conn *natsgo.Conn, bucket, key string, body []byte) uint64 {
	t.Helper()
	js, err := conn.JetStream()
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		if stored := LastSequence(t, conn, bucket, key); stored > before {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's RePublish was not stored under %s within 10 s (newest sequence %d)", stream, KVSubject(bucket, key), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LastSequence is the stream sequence of the newest message bucket's stream holds for key, read
// from the stream's leader, and 0 when it holds none.
func LastSequence(t testing.TB, conn *natsgo.Conn, bucket, key string) uint64 {
	t.Helper()
	js, err := conn.JetStream()
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
