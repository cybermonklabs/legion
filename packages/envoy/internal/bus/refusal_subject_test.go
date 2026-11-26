package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// testnats.SeedOnly, the one seed lookup of the module's tests, names the seed by a string of its
// own, because testnats cannot import this package. It must answer under the variable a connect
// reads (NkeySeedEnvVar), and an empty seed is none, as an unset variable is.
func TestTheTestsSeedLookupAnswersTheBusSeedVariable(t *testing.T) {
	t.Parallel()
	if seed, ok := testnats.SeedOnly("SUAEXAMPLE")(NkeySeedEnvVar); !ok || seed != "SUAEXAMPLE" {
		t.Fatalf("testnats.SeedOnly answered %q, %v under %s, want the seed it holds", seed, ok, NkeySeedEnvVar)
	}
	if _, ok := testnats.SeedOnly("")(NkeySeedEnvVar); ok {
		t.Fatalf("testnats.SeedOnly answered an empty seed under %s, want none", NkeySeedEnvVar)
	}
	if _, ok := testnats.SeedOnly("SUAEXAMPLE")(nkeySeedFileVariable); ok {
		t.Fatalf("testnats.SeedOnly answered under %s, want the seed variable alone", nkeySeedFileVariable)
	}
}

// deniedRelayedClient starts a server whose one user may publish to anything but deny
// (testnats.StartNkeyDenied), and returns a connection to it as its admin, who may do anything, and
// a client connected as that user through a relay that counts and drops the sends of dropped
// (testnats.UnansweredRelay). The client is a bus client, so the server's refusals are recorded
// for it.
func deniedRelayedClient(t *testing.T, dropped string, deny ...string) (*nats.Conn, *Client, *testnats.UnansweredRelay) {
	t.Helper()
	return deniedRelayedClientWith(t, nil, dropped, deny...)
}

// deniedRelayedClientWith is deniedRelayedClient whose client connects with options too, such as
// the stream message reads an edge listener's client takes (WithStreamMessageReads).
func deniedRelayedClientWith(t *testing.T, options []ConnectOption, dropped string, deny ...string) (*nats.Conn, *Client, *testnats.UnansweredRelay) {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyDenied(t, public, deny...)
	admin, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect as the server's admin: %v", err)
	}
	t.Cleanup(admin.Close)
	relay := testnats.StartUnansweredRelayOn(t, uri, dropped)
	client, err := Connect([]string{relay.URL()}, append([]ConnectOption{WithEnvironment(testnats.SeedOnly(seed))}, options...)...)
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

// besideRefusal runs call while the connection earns two refusals of subject, a request call does
// not send at that moment, once call's own request is on the wire (refusalOfAnotherSubject), and
// then closes release, when it is not nil, so a request the relay holds goes on (HoldNext). It
// returns call's error, and requireLeft, which the row runs once it has judged that error, named
// what: it fails the row unless the refusal was recorded before call ended, since one recorded
// after the call had read the record could not have answered it and the row would hold nothing,
// and unless the refusal is still in the record for the request that earned it.
func besideRefusal(t *testing.T, relay *testnats.UnansweredRelay, client *Client, subject string, release chan struct{}, call func() error) (requireLeft func(what string), err error) {
	t.Helper()
	owner := clientOwning(client.Conn)
	if owner == nil {
		t.Fatal("no client owns the connection, so nothing records the server's refusals")
	}
	seen := make(chan refusalSeen, 1)
	go func() {
		denial := refusalOfAnotherSubject(t, relay, owner, client.Conn, subject)
		seen <- refusalSeen{denial: denial, at: time.Now()}
		if release != nil {
			close(release)
		}
	}()
	sentAt := time.Now()
	err = call()
	ended := time.Now()
	got := <-seen
	return func(what string) {
		t.Helper()
		if got.denial == nil || !got.at.Before(ended) {
			t.Fatalf("the connection recorded no refusal of %s before the %s ended (%v), so the %s met none and this row holds nothing",
				subject, what, got.denial, what)
		}
		if denial := owner.deniedPublish(subject, sentAt); denial == nil {
			t.Fatalf("the record holds no refusal of %s after the %s: the %s took it", subject, what, what)
		}
	}, err
}

// requireUnansweredBeside runs call, named what, whose own request the relay drops, beside two
// refusals of subject, a request call does not send then (besideRefusal). It requires call to end
// with its own unanswered request, and not with that refusal, and the refusal to be left in the
// record.
func requireUnansweredBeside(t *testing.T, relay *testnats.UnansweredRelay, client *Client, what, subject string, call func() error) {
	t.Helper()
	requireLeft, err := besideRefusal(t, relay, client, subject, nil, call)
	if errors.Is(err, ErrPublishDenied) {
		t.Fatalf("the %s, whose own request drew no answer, ended with %v, the refusal of %s, which it did not send then; want its own unanswered request",
			what, err, subject)
	}
	if !unanswered(err) {
		t.Fatalf("the %s, whose own request drew no answer, = %v, want its own unanswered request", what, err)
	}
	requireLeft(what)
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
// A handle of a client that reads with stream message gets (WithStreamMessageReads), an edge
// listener's, sends every read as a get from the stream's leader, on a stream that allows direct
// gets too (streamMessageReads). So a read whose own request lost its answer, while the connection
// earned the refusal of the request the other kind of handle sends, reports its own silence and
// leaves that refusal in the record; and a read whose own request the grant refuses names that
// refusal, ErrPublishDenied with the server's text, once its request has waited out
// RequestTimeout (10 s): nats-server drops a refused request unanswered and reports the refusal
// only on the connection, so the read does not fail at once. What the handle's kind decides is
// which refusal the read names after that wait, its own or none (a deadline).
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
	messageReads := []ConnectOption{WithStreamMessageReads()}
	for _, read := range []struct {
		name       string
		direct     bool
		own, other string
		call       func(KeyValue, uint64) (nats.KeyValueEntry, error)
		client     []ConnectOption
	}{
		{"direct Get", true, directByKey, fromLeader, get, nil},
		{"direct GetRevision", true, directBySequence, fromLeader, getRevision, nil},
		{"leader Get", false, fromLeader, directByKey, get, nil},
		{"leader GetRevision", false, fromLeader, directBySequence, getRevision, nil},
		{"stream message Get", true, fromLeader, directByKey, get, messageReads},
		{"stream message GetRevision", true, fromLeader, directBySequence, getRevision, messageReads},
	} {
		t.Run(read.name+" lost beside a refusal of a request it does not send", func(t *testing.T) {
			t.Parallel()
			admin, client, relay := deniedRelayedClientWith(t, read.client, read.own, read.other)
			kv, revision := readModeBucket(t, admin, client, bucket, key, read.direct)
			// The grant covers the handle's own request, so a read the relay lets through is answered.
			if entry, err := read.call(kv, revision); err != nil || string(entry.Value()) != "value" {
				t.Fatalf("the %s the relay let through = %v, want the value written", read.name, err)
			}

			relay.Arm(1)
			requireUnansweredBeside(t, relay, client, read.name, read.other, func() error {
				_, err := read.call(kv, revision)
				return err
			})
			if sends := relay.Sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's read sends that request and no other", read.own, sends)
			}
		})
		t.Run(read.name+" whose own request is refused", func(t *testing.T) {
			t.Parallel()
			admin, client, relay := deniedRelayedClientWith(t, read.client, read.own, read.own)
			kv, revision := readModeBucket(t, admin, client, bucket, key, read.direct)

			relay.Arm(0)
			started := time.Now()
			_, err := read.call(kv, revision)
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("the %s, whose own request %s the grant refuses, = %v (after %s), want ErrPublishDenied", read.name, read.own, err, took)
			}
			if want := `Permissions Violation for Publish to "` + read.own + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", read.name, err, want)
			}
			if sends := relay.Sent(); sends != 1 {
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
//
// A filter that ends in a full wildcard, as the Dispatch intake's `notifications.github.>` does,
// puts that wildcard at the end of the create's subject, and the subject is still matched as it is
// sent: the refusal of a narrower create of the same consumer is another request's.
func TestAConsumerCreateTakesOnlyTheRefusalOfTheSubjectItSends(t *testing.T) {
	const stream, name, filter, wide = "ENVOY_CONSUMER_SUBJECT", "subject", "events.x", "notifications.github.>"
	unfiltered := jsAPI("CONSUMER.CREATE", stream, name)
	filtered := jsAPI("CONSUMER.CREATE", stream, name, filter)
	durableCreate := jsAPI("CONSUMER.DURABLE.CREATE", stream, name)
	wideFiltered := jsAPI("CONSUMER.CREATE", stream, name, wide)
	narrower := jsAPI("CONSUMER.CREATE", stream, name, "notifications.github.x")
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
		{"AddConsumer of a durable filtered on a full wildcard", add, nats.ConsumerConfig{Durable: name, FilterSubject: wide, AckPolicy: nats.AckExplicitPolicy}, wideFiltered, narrower},
		{"UpdateConsumer of a durable filtered on a full wildcard", update, nats.ConsumerConfig{Durable: name, FilterSubject: wide, AckPolicy: nats.AckExplicitPolicy}, wideFiltered, narrower},
	} {
		t.Run(create.name+" lost beside a refusal of a subject it does not send", func(t *testing.T) {
			t.Parallel()
			_, client, relay := deniedRelayedClient(t, create.own, create.sibling)
			cfg := create.cfg

			relay.Arm(1)
			requireUnansweredBeside(t, relay, client, create.name, create.sibling, func() error { return create.call(client.JS(), &cfg) })
			if sends := relay.Sent(); sends != 1 {
				t.Fatalf("sends of %s = %d, want 1: the row's create sends that subject and no other", create.own, sends)
			}
		})
		t.Run(create.name+" whose own subject is refused", func(t *testing.T) {
			t.Parallel()
			_, client, relay := deniedRelayedClient(t, create.own, create.own)
			cfg := create.cfg

			relay.Arm(0)
			started := time.Now()
			err := create.call(client.JS(), &cfg)
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("the %s, whose subject %s the grant refuses, = %v (after %s), want ErrPublishDenied", create.name, create.own, err, took)
			}
			if want := `Permissions Violation for Publish to "` + create.own + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", create.name, err, want)
			}
			if sends := relay.Sent(); sends != 1 {
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
	client, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(testnats.SeedOnly("")), WithConnectInBackground())
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

// AddConsumer of a named consumer takes only the refusal of the request it is on. nats.go's
// AddConsumer reads the consumer and creates it only when the read finds no consumer
// (nats.go@v1.55.0 jsm.go:447-471), so a refusal of the create does not answer the call while its
// read is in flight, nor once the read has found the consumer and no create is sent. The grant of
// each row refuses the consumer's create, and the row has the connection earn that refusal, as
// another request of the same consumer would, while the call's read waits: once with the read
// lost, which the call answers with its own silence, and once with the read held and then
// answered over a consumer the server's admin made, which the call answers with that consumer.
func TestAnAddConsumerTakesOnlyTheRefusalOfTheRequestItIsOn(t *testing.T) {
	t.Parallel()
	const stream, name = "ENVOY_ADD_CONSUMER_STEP", "step"
	read := jsAPI("CONSUMER.INFO", stream, name)
	create := jsAPI("CONSUMER.CREATE", stream, name)
	config := nats.ConsumerConfig{Durable: name, AckPolicy: nats.AckExplicitPolicy}
	t.Run("its read lost beside a refusal of its create", func(t *testing.T) {
		t.Parallel()
		_, client, relay := deniedRelayedClient(t, read, create)
		cfg := config

		relay.Arm(1)
		requireUnansweredBeside(t, relay, client, "AddConsumer", create, func() error {
			_, err := client.JS().AddConsumer(stream, &cfg)
			return err
		})
		if sends := relay.Sent(); sends != 1 {
			t.Fatalf("reads of the consumer = %d, want 1: a read lost is the call's answer", sends)
		}
	})
	t.Run("its read held over a found consumer beside a refusal of its create", func(t *testing.T) {
		t.Parallel()
		admin, client, relay := deniedRelayedClient(t, read, create)
		js, err := admin.JetStream()
		if err != nil {
			t.Fatalf("open the admin's JetStream: %v", err)
		}
		if _, err := js.AddStream(&nats.StreamConfig{Name: stream, Subjects: []string{"add.consumer.step.>"}, Storage: nats.MemoryStorage}); err != nil {
			t.Fatalf("the admin creates the stream: %v", err)
		}
		cfg := config
		if _, err := js.AddConsumer(stream, &cfg); err != nil {
			t.Fatalf("the admin creates the consumer: %v", err)
		}

		release := make(chan struct{})
		relay.HoldNext(release)
		requireLeft, err := besideRefusal(t, relay, client, create, release, func() error {
			_, err := client.JS().AddConsumer(stream, &cfg)
			return err
		})
		if err != nil {
			t.Fatalf("an AddConsumer whose read found the consumer = %v, want the consumer: the refusal of %s is of a create it never sent", err, create)
		}
		requireLeft("AddConsumer")
	})
}

// A start's create of a bucket takes only the refusal of the request it is on. nats.go's
// CreateKeyValue asks the account for its JetStream info, then creates the bucket's stream, and on
// "stream name already in use" reads the stream and may update it (nats.go@v1.55.0 kv.go:422-551);
// EnsureKeyValue makes those requests one at a time (createBucketStream). The grant of each row
// refuses the update of the bucket's stream, and the row has the connection earn that refusal, as
// a raise of the same bucket would, while the start's info request, or its stream's create, has
// lost its answer. The start sends its create again, as it sends any request the server left
// unanswered, and goes on.
func TestABucketCreateTakesOnlyTheRefusalOfTheStepItIsOn(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, bucket string
		lost         func(bucket string) string
	}{
		{"its account info lost", "envoy_create_step_info", func(string) string { return accountInfoSubject() }},
		{"its stream's create lost", "envoy_create_step_create", createSubject},
	} {
		t.Run(row.name+" beside a refusal of its stream's update", func(t *testing.T) {
			t.Parallel()
			update := updateSubject(row.bucket)
			_, client, relay := deniedRelayedClient(t, row.lost(row.bucket), update)
			js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
			if err != nil {
				t.Fatalf("open JetStream: %v", err)
			}

			relay.Arm(1)
			requireLeft, err := besideRefusal(t, relay, client, update, nil, func() error {
				_, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: row.bucket, Storage: nats.MemoryStorage})
				return err
			})
			if err != nil {
				t.Fatalf("a start whose create lost one answer, beside a refusal of %s, which that request is not = %v, want the start to send its create again and go on", update, err)
			}
			requireLeft("start")
			if sends := relay.Sent(); sends != 2 {
				t.Fatalf("sends of %s = %d, want 2: the lost one and the one sent again", row.lost(row.bucket), sends)
			}
		})
	}
}
