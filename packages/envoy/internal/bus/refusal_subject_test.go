package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// seedLookup is a connect's environment (WithEnvironment) that names seed as the nkey seed and
// nothing else, so rows that each connect as the user of a server of their own run in parallel,
// which t.Setenv forbids.
func seedLookup(seed string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == NkeySeedEnvVar && seed != "" {
			return seed, true
		}
		return "", false
	}
}

// deniedRelayedClient starts a server whose one user may publish to anything but deny
// (testnats.StartNkeyDenied), and returns a connection to it as its admin, who may do anything, and
// a client connected as that user through a relay that counts and drops the sends of dropped
// (unansweredRelay). The client is a bus client, so the server's refusals are recorded for it.
func deniedRelayedClient(t *testing.T, dropped string, deny ...string) (*nats.Conn, *Client, *unansweredRelay) {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyDenied(t, public, deny...)
	admin, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect as the server's admin: %v", err)
	}
	t.Cleanup(admin.Close)
	relay := startUnansweredRelayOn(t, uri, dropped)
	client, err := Connect([]string{relay.url}, WithEnvironment(seedLookup(seed)))
	if err != nil {
		t.Fatalf("connect as the denied user through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	return admin, client, relay
}

// refusalSeen is what refusalOfAnotherSubject answered, and when.
type refusalSeen struct {
	denial error
	at     time.Time
}

// requireUnansweredBeside runs call, named what, whose own request the relay drops, while the
// connection earns two refusals of subject, a request call does not send, once call's own request
// is on the wire (refusalOfAnotherSubject). It requires call to end with its own unanswered
// request, and not with that refusal, and the refusal to be left in the record. It also fails the
// row unless the refusal was recorded before call ended: one recorded after the call had read the
// record could not have answered it, and the row would hold nothing.
func requireUnansweredBeside(t *testing.T, relay *unansweredRelay, client *Client, what, subject string, call func() error) {
	t.Helper()
	owner := clientOwning(client.Conn)
	if owner == nil {
		t.Fatal("no client owns the connection, so nothing records the server's refusals")
	}
	seen := make(chan refusalSeen, 1)
	go func() {
		denial := refusalOfAnotherSubject(t, relay, owner, client.Conn, subject)
		seen <- refusalSeen{denial: denial, at: time.Now()}
	}()
	sentAt := time.Now()
	err := call()
	ended := time.Now()
	got := <-seen
	if errors.Is(err, ErrPublishDenied) {
		t.Fatalf("the %s, whose own request drew no answer, ended with %v, the refusal of %s, which it never sends; want its own unanswered request",
			what, err, subject)
	}
	if !unanswered(err) {
		t.Fatalf("the %s, whose own request drew no answer, = %v, want its own unanswered request", what, err)
	}
	if got.denial == nil || !got.at.Before(ended) {
		t.Fatalf("the connection recorded no refusal of %s before the %s ended (%v), so the %s met none and this row holds nothing",
			subject, what, got.denial, what)
	}
	if denial := owner.deniedPublish(subject, sentAt); denial == nil {
		t.Fatalf("the record holds no refusal of %s after the %s: the %s took it", subject, what, what)
	}
}

// readModeBucket has admin create bucket, with AllowDirect, as CreateKeyValue makes every bucket,
// when direct, and without it, as a stream made otherwise can be, writes key in it, and returns
// client's handle on it, opened as a rebuild opens one (OpenKeyValue), and the key's revision.
func readModeBucket(t *testing.T, admin *nats.Conn, client *Client, bucket, key string, direct bool) (KeyValue, uint64) {
	t.Helper()
	js, err := admin.JetStream()
	if err != nil {
		t.Fatalf("open the admin's JetStream: %v", err)
	}
	if direct {
		_, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	} else {
		_, err = js.AddStream(&nats.StreamConfig{
			Name:              "KV_" + bucket,
			Subjects:          []string{"$KV." + bucket + ".>"},
			MaxMsgsPerSubject: 1,
			Storage:           nats.MemoryStorage,
			AllowRollup:       true,
			DenyDelete:        true,
			Discard:           nats.DiscardNew,
		})
	}
	if err != nil {
		t.Fatalf("create the bucket (AllowDirect %t): %v", direct, err)
	}
	written, err := js.KeyValue(bucket)
	if err != nil {
		t.Fatalf("open the admin's handle on the bucket: %v", err)
	}
	revision, err := written.Put(key, []byte("value"))
	if err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
	clientJS, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open the client's JetStream: %v", err)
	}
	kv, err := OpenKeyValue(t.Context(), client.Conn, clientJS, bucket)
	if err != nil {
		t.Fatalf("open the client's handle on the bucket: %v", err)
	}
	return kv, revision
}

// A read of a key or of a revision takes only the refusal of the request its handle sends. nats.go
// sends a read through a handle on a stream that allows direct gets as a direct get, and through
// one on a stream that does not as a get from the stream's leader, never the other (readsDirect).
// So a read whose own request lost its answer, while the connection earned the refusal of the
// request the other kind of handle sends, reports its own silence and leaves that refusal in the
// record; and a read whose own request the grant refuses names that refusal.
//
// Each row starts a server of its own, whose user may publish to anything but the one subject the
// row refuses, and the server's admin makes the bucket there. The handle under test is the user's,
// through a relay that drops the one send of the handle's own request that a lost row drops; the
// relay's count of that request is the row's proof of which request the handle sent.
func TestAReadTakesOnlyTheRefusalOfTheRequestItsHandleSends(t *testing.T) {
	const bucket, key = "envoy_read_mode", "key"
	stream := "KV_" + bucket
	directByKey := jsAPI("DIRECT.GET", stream, "$KV."+bucket+"."+key)
	directBySequence := jsAPI("DIRECT.GET", stream)
	fromLeader := jsAPI("STREAM.MSG.GET", stream)
	get := func(kv KeyValue, _ uint64) (nats.KeyValueEntry, error) { return kv.Get(key) }
	getRevision := func(kv KeyValue, revision uint64) (nats.KeyValueEntry, error) { return kv.GetRevision(key, revision) }
	for _, read := range []struct {
		name       string
		direct     bool
		own, other string
		call       func(KeyValue, uint64) (nats.KeyValueEntry, error)
	}{
		{"direct Get", true, directByKey, fromLeader, get},
		{"direct GetRevision", true, directBySequence, fromLeader, getRevision},
		{"leader Get", false, fromLeader, directByKey, get},
		{"leader GetRevision", false, fromLeader, directBySequence, getRevision},
	} {
		t.Run(read.name+" lost beside a refusal of a request it does not send", func(t *testing.T) {
			t.Parallel()
			admin, client, relay := deniedRelayedClient(t, read.own, read.other)
			kv, revision := readModeBucket(t, admin, client, bucket, key, read.direct)
			// The grant covers the handle's own request, so a read the relay lets through is answered.
			if entry, err := read.call(kv, revision); err != nil || string(entry.Value()) != "value" {
				t.Fatalf("the %s the relay let through = %v, want the value written", read.name, err)
			}

			relay.arm(1)
			requireUnansweredBeside(t, relay, client, read.name, read.other, func() error {
				_, err := read.call(kv, revision)
				return err
			})
			if sends := relay.sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's read sends that request and no other", read.own, sends)
			}
		})
		t.Run(read.name+" whose own request is refused", func(t *testing.T) {
			t.Parallel()
			admin, client, relay := deniedRelayedClient(t, read.own, read.own)
			kv, revision := readModeBucket(t, admin, client, bucket, key, read.direct)

			relay.arm(0)
			started := time.Now()
			_, err := read.call(kv, revision)
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("the %s, whose own request %s the grant refuses, = %v (after %s), want ErrPublishDenied", read.name, read.own, err, took)
			}
			if want := `Permissions Violation for Publish to "` + read.own + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", read.name, err, want)
			}
			if sends := relay.sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's read sends that request", read.own, sends)
			}
			if bound := RequestTimeout + refusalAnswerBound; took > bound {
				t.Fatalf("the refused %s ended after %s, want within %s", read.name, took, bound)
			}
		})
	}
}

// A consumer's create takes only the refusal of the subject it sends. nats.go sends the create of
// a named consumer to one subject, chosen by the server's version and the consumer's filter
// (consumerCreateSubject); on this server, 2.15, that is the consumer's create, with the filter
// subject in it when the consumer has one other than ">". So a create whose own request lost its
// answer, while the connection earned the refusal of a sibling subject the create did not send,
// ends at its own wait and leaves that refusal in the record; and a create whose own subject the
// grant refuses ends at once naming that refusal. AddConsumer reads the consumer first, which the
// server answers ("stream not found": no stream is needed), and then creates it; UpdateConsumer
// only creates. Both go through the client's own JetStream context (readsRefusals).
func TestAConsumerCreateTakesOnlyTheRefusalOfTheSubjectItSends(t *testing.T) {
	const stream, name, filter = "ENVOY_CONSUMER_SUBJECT", "subject", "events.x"
	unfiltered := jsAPI("CONSUMER.CREATE", stream, name)
	filtered := jsAPI("CONSUMER.CREATE", stream, name, filter)
	durableCreate := jsAPI("CONSUMER.DURABLE.CREATE", stream, name)
	add := func(js nats.JetStreamContext, cfg *nats.ConsumerConfig) error {
		_, err := js.AddConsumer(stream, cfg)
		return err
	}
	update := func(js nats.JetStreamContext, cfg *nats.ConsumerConfig) error {
		_, err := js.UpdateConsumer(stream, cfg)
		return err
	}
	for _, create := range []struct {
		name         string
		call         func(nats.JetStreamContext, *nats.ConsumerConfig) error
		cfg          nats.ConsumerConfig
		own, sibling string
	}{
		{"AddConsumer of a durable", add, nats.ConsumerConfig{Durable: name, AckPolicy: nats.AckExplicitPolicy}, unfiltered, durableCreate},
		{"AddConsumer of a filtered durable", add, nats.ConsumerConfig{Durable: name, FilterSubject: filter, AckPolicy: nats.AckExplicitPolicy}, filtered, unfiltered},
		{"AddConsumer of a durable filtered on every subject", add, nats.ConsumerConfig{Durable: name, FilterSubject: ">", AckPolicy: nats.AckExplicitPolicy}, unfiltered, durableCreate},
		{"UpdateConsumer of a durable", update, nats.ConsumerConfig{Durable: name, AckPolicy: nats.AckExplicitPolicy}, unfiltered, durableCreate},
		{"UpdateConsumer of a named filtered consumer", update, nats.ConsumerConfig{Name: name, FilterSubject: filter, AckPolicy: nats.AckExplicitPolicy}, filtered, unfiltered},
	} {
		t.Run(create.name+" lost beside a refusal of a subject it does not send", func(t *testing.T) {
			t.Parallel()
			_, client, relay := deniedRelayedClient(t, create.own, create.sibling)
			cfg := create.cfg

			relay.arm(1)
			requireUnansweredBeside(t, relay, client, create.name, create.sibling, func() error { return create.call(client.JS(), &cfg) })
			if sends := relay.sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's create sends that subject and no other", create.own, sends)
			}
		})
		t.Run(create.name+" whose own subject is refused", func(t *testing.T) {
			t.Parallel()
			_, client, relay := deniedRelayedClient(t, create.own, create.own)
			cfg := create.cfg

			relay.arm(0)
			started := time.Now()
			err := create.call(client.JS(), &cfg)
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("the %s, whose subject %s the grant refuses, = %v (after %s), want ErrPublishDenied", create.name, create.own, err, took)
			}
			if want := `Permissions Violation for Publish to "` + create.own + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", create.name, err, want)
			}
			if sends := relay.sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's create sends that subject", create.own, sends)
			}
			if took > refusalAnswerBound {
				t.Fatalf("the refused %s ended after %s, want within %s", create.name, took, refusalAnswerBound)
			}
		})
	}
}

// A create on a connection that has not connected yet, a client's that connects in the background
// (WithConnectInBackground) to a server that is not there, reads no server version, which nats.go
// reads as an older server's (consumerCreateSubject). It sends nothing: a bus connection buffers
// nothing until it is connected, so nats.go fails the create at once with its own error, and the
// create neither waits for an answer nor takes a refusal.
func TestACreateBeforeTheFirstConnectEndsAtOnceWithItsOwnError(t *testing.T) {
	client, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(seedLookup("")), WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect in the background: %v", err)
	}
	t.Cleanup(client.Close)
	if version := client.Conn.ConnectedServerVersion(); version != "" {
		t.Fatalf("a connection that never connected reports server version %q, want none, and this row holds nothing", version)
	}
	cfg := nats.ConsumerConfig{Durable: "unconnected", AckPolicy: nats.AckExplicitPolicy}
	for name, call := range map[string]func() error{
		"AddConsumer":    func() error { _, err := client.JS().AddConsumer("ENVOY_UNCONNECTED", &cfg); return err },
		"UpdateConsumer": func() error { _, err := client.JS().UpdateConsumer("ENVOY_UNCONNECTED", &cfg); return err },
	} {
		started := time.Now()
		err := call()
		took := time.Since(started)
		if !errors.Is(err, nats.ErrReconnectBufExceeded) || errors.Is(err, ErrPublishDenied) {
			t.Errorf("%s before the first connect = %v, want nats.ErrReconnectBufExceeded: nothing is sent, so nothing is refused", name, err)
		}
		if took > refusalAnswerBound {
			t.Errorf("%s before the first connect ended after %s, want within %s: nats.go sends nothing and fails it at once", name, took, refusalAnswerBound)
		}
	}
}
