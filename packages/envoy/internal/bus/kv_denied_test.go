package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A start whose lookup of a bucket the server refuses under the connection's grant ends at that
// one send, with an error naming the refusal and the subject it was refused to, rather than
// sending the lookup again: a request a grant does not cover is refused again however often it is
// sent. The server answers such a request with nothing and reports the refusal on the connection
// (recordPublishDenial), so without that record the step would read the silence as a member still
// working and send the lookup again until the start's hang guard passed, ending with a bare
// "KV_envoy_denied_lookup STREAM.INFO: ... context deadline exceeded".
//
// The relay counts the sends of the lookup's subject, which is the count this row is about; it
// drops none, so every send it counts reached the server and was refused there.
func TestADeniedBucketLookupIsNotSentAgain(t *testing.T) {
	const bucket = "envoy_denied_lookup"
	seed, public := testnats.User(t)
	// Every subject a start of this bucket publishes to except its lookup's, so the refusal the
	// row reads is the one of that subject and of nothing else.
	grant := testnats.StartNkeyGranted(t, public,
		accountInfoSubject(), createSubject(bucket), "$JS.API.STREAM.UPDATE.KV_"+bucket, "_INBOX.>")
	t.Setenv("NATS_NKEY_SEED", seed)
	relay := startUnansweredRelayOn(t, grant.URL, openSubject(bucket))
	// The call under test reads the record of the refusal through the client that owns the
	// connection (clientOwning), so the connection is a client's and not a bare one.
	conn, js := relayedJetStream(t, relay)

	relay.arm(0)
	started := time.Now()
	_, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	took := time.Since(started)

	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a start whose bucket lookup the server refused = %v, want ErrPublishDenied", err)
	}
	if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+openSubject(bucket)+`"`) {
		t.Fatalf("the refused lookup was reported as %q, which does not carry the server's violation of %s",
			err, openSubject(bucket))
	}
	if sends := relay.sent(); sends != 1 {
		t.Fatalf("lookups sent = %d, want 1: a request the server refused is not sent again", sends)
	}
	if took > refusalAnswerBound {
		t.Fatalf("the refused lookup ended after %s, want within %s: the refusal answers the send as it arrives, not at the end of the send's own %s wait",
			took, refusalAnswerBound, RequestTimeout)
	}
}

// refusalAnswerBound is how long a request the server refuses may take to end with that refusal.
// The refusal is one round trip, which takes milliseconds; a request that read the refusal only
// once its own wait ran out took RequestTimeout, ten seconds, so a bound of three seconds tells
// the two apart on a loaded machine.
const refusalAnswerBound = 3 * time.Second

// grantedClient is a client of a single server whose one user may publish only to allow, the
// connection a start opens its buckets on (a client's, so the server's refusals are recorded).
func grantedClient(t *testing.T, allow ...string) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, append([]string{"_INBOX.>"}, allow...)...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{grant.URL})
	if err != nil {
		t.Fatalf("connect the granted client: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return client.Conn, js
}

// A start that raises a bucket the grant does not let it update ends at once, with the server's
// refusal of the update and the setting it asked for, rather than after the update's own ten
// second wait with a bare "context deadline exceeded" that names neither.
func TestARaiseTheGrantRefusesEndsAtOnceNamingTheRefusal(t *testing.T) {
	const bucket = "envoy_denied_raise"
	stream := "KV_" + bucket
	conn, js := grantedClient(t, accountInfoSubject(), openSubject(bucket), createSubject(bucket))
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	started := time.Now()
	_, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, Replicas: 3})
	took := time.Since(started)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a raise the grant refuses = %v (after %s), want ErrPublishDenied", err, took)
	}
	for _, want := range []string{"raise " + stream + ": " + ReplicasEnvVar + "=3: ", `Permissions Violation for Publish to "$JS.API.STREAM.UPDATE.` + stream + `"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refused raise = %v, want it to name %q", err, want)
		}
	}
	if took > refusalAnswerBound {
		t.Fatalf("the refused raise ended after %s, want within %s", took, refusalAnswerBound)
	}
}

// A start that creates a bucket the grant does not let it create ends at once, with the server's
// refusal of the create, rather than after the create's own wait.
func TestACreateTheGrantRefusesEndsAtOnceNamingTheRefusal(t *testing.T) {
	const bucket = "envoy_denied_create"
	conn, js := grantedClient(t, accountInfoSubject(), openSubject(bucket))
	started := time.Now()
	_, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	took := time.Since(started)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a create the grant refuses = %v (after %s), want ErrPublishDenied", err, took)
	}
	if want := `Permissions Violation for Publish to "` + createSubject(bucket) + `"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("the refused create = %v, want it to name %q", err, want)
	}
	if took > refusalAnswerBound {
		t.Fatalf("the refused create ended after %s, want within %s", took, refusalAnswerBound)
	}
}

// A write of a key, and a watch of the bucket, that the grant refuses end at once with the
// server's refusal, as every request the bus makes of a bucket does (refusedOn), rather than after
// the write's own wait as a write nothing answered.
func TestAKeyWriteAndAWatchTheGrantRefusesEndAtOnceNamingTheRefusal(t *testing.T) {
	const bucket = "envoy_denied_write"
	stream := "KV_" + bucket
	conn, js := grantedClient(t, accountInfoSubject(), openSubject(bucket), createSubject(bucket))
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	for _, row := range []struct {
		name    string
		refused string
		call    func() error
	}{
		{"put", "$KV." + bucket + ".key", func() error { _, err := kv.Put("key", []byte("value")); return err }},
		{"watch", jsAPI("CONSUMER.CREATE", stream), func() error {
			watcher, err := kv.WatchKeys(t.Context(), "key")
			if err == nil {
				_ = watcher.Stop()
			}
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			started := time.Now()
			err := row.call()
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("a %s the grant refuses = %v (after %s), want ErrPublishDenied", row.name, err, took)
			}
			if want := `Permissions Violation for Publish to "` + row.refused; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", row.name, err, want)
			}
			if took > refusalAnswerBound {
				t.Fatalf("the refused %s ended after %s, want within %s", row.name, took, refusalAnswerBound)
			}
		})
	}
}

// A client that owns the notification stream, on a grant that does not let it read the stream or
// does not let it create the stream, fails its start at once with the server's refusal, rather than
// sending the request until the start's hang guard passes and ending with an error that names the
// guard and not the grant.
func TestAStreamEnsureTheGrantRefusesEndsAtOnceNamingTheRefusal(t *testing.T) {
	for _, row := range []struct {
		name    string
		allow   []string
		refused string
	}{
		{"read", []string{jsAPI("INFO"), jsAPI("STREAM.CREATE", Stream)}, jsAPI("STREAM.INFO", Stream)},
		{"create", []string{jsAPI("INFO"), jsAPI("STREAM.INFO", Stream)}, jsAPI("STREAM.CREATE", Stream)},
	} {
		t.Run(row.name, func(t *testing.T) {
			seed, public := testnats.User(t)
			grant := testnats.StartNkeyGranted(t, public, append([]string{"_INBOX.>"}, row.allow...)...)
			t.Setenv("NATS_NKEY_SEED", seed)
			started := time.Now()
			client, err := ConnectOwningStream([]string{grant.URL})
			took := time.Since(started)
			if err == nil {
				client.Close()
				t.Fatal("a client whose grant refuses the stream's ensure started")
			}
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("the refused ensure = %v (after %s), want ErrPublishDenied", err, took)
			}
			if want := `Permissions Violation for Publish to "` + row.refused + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused ensure = %v, want it to name %q", err, want)
			}
			if took > refusalAnswerBound {
				t.Fatalf("the refused ensure ended after %s, want within %s", took, refusalAnswerBound)
			}
		})
	}
}

// A start whose lookup of a bucket draws no answer is not ended by the server's refusal of a
// subject that lookup never published to: a refusal of another subject is not this send's
// answer, so the lookup is sent again, and the start ends at its hang guard. refusedSend reads
// the record under the subjects the step publishes to, and that list is what keeps the two apart:
// a read of whatever refusal the connection holds would end this start at its first send with
// another subject's violation, and every other row of this package would still pass, since no
// other one records a refusal the step under it did not earn.
//
// The grant covers every subject a start of this bucket publishes to, so nothing the call sends
// is refused, and the relay drops every send of the lookup, so the call draws no answer at all.
// The refusal the row plants beside it is of the delete of the bucket's stream, which no step of
// EnsureKeyValue publishes to. The row reads that record itself before it reads the call's
// error, because a row that never saw the refusal recorded would pass whatever refusedSend read.
func TestARefusalOfAnotherSubjectDoesNotAnswerAnUnansweredLookup(t *testing.T) {
	const bucket = "envoy_denied_other_subject"
	// The delete of the bucket's own stream: a subject no step of this call publishes to, and
	// one no other connection on this server earns a refusal of, since it names this bucket.
	deleteSubject := "$JS.API.STREAM.DELETE.KV_" + bucket
	lookup := "KV_" + bucket + " STREAM.INFO"
	seed, public := testnats.User(t)
	// Every subject a start of this bucket publishes to, so the only refusal the connection can
	// record is the one the row plants.
	grant := testnats.StartNkeyGranted(t, public, openSubject(bucket), createSubject(bucket),
		accountInfoSubject(), "$JS.API.STREAM.UPDATE.KV_"+bucket, "_INBOX.>")
	t.Setenv("NATS_NKEY_SEED", seed)
	relay := startUnansweredRelayOn(t, grant.URL, openSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	owner := clientOwning(conn)
	if owner == nil {
		t.Fatal("no client owns the connection the start looks the bucket up on, so nothing records the server's refusals")
	}

	// Every send of the lookup is dropped, so the step sends until its hang guard passes: each
	// send a read waited for up to its read limit (readAgain), sent again after it.
	relay.arm(1 << 30)
	recorded := make(chan error, 1)
	go func() { recorded <- refusalOfAnotherSubject(t, relay, owner, conn, deleteSubject) }()
	_, err := ensureKeyValue(context.Background(), refusalGuard, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	denial := <-recorded

	if denial == nil {
		t.Fatalf("the connection recorded no refusal of %s while the lookup was in flight, so the start met none and this row holds nothing",
			deleteSubject)
	}
	if errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a start whose lookup nothing answered ended after %d sends with %v, want no ErrPublishDenied: the refusal beside it, %v, is of a subject the lookup never published to",
			relay.sent(), err, denial)
	}
	if err == nil || !strings.Contains(err.Error(), lookup) || !errors.Is(err, errHangGuard) {
		t.Fatalf("a start whose lookup nothing answered = %v, want %q and the start's hang guard: a send another subject's refusal does not answer ends on the wait it held",
			err, lookup)
	}
	if sends := relay.sent(); sends < 2 {
		t.Fatalf("lookups sent = %d, want at least 2: a send a refusal of another subject does not answer is sent again", sends)
	}
	if strings.Contains(err.Error(), deleteSubject) {
		t.Fatalf("the start's error %q carries %s, a subject no step of it publishes to", err, deleteSubject)
	}
}

// refusalGuard is the hang guard of the row above: long enough for the lookup to be sent several
// times, each up to its read limit, and for the refusal of another subject to be recorded while
// one of them is in flight.
const refusalGuard = RequestTimeout + 5*time.Second

// refusalRecordWait and refusalRecordPoll bound the waits the row above makes on the relay's
// count and on the connection's record, and are waitUntil's bound and poll. That helper itself
// does not serve here: these waits run on a goroutine beside the call under test, where t.Fatalf
// is the test goroutine's to call. The call lasts refusalGuard, fifteen seconds, so a bound of
// five seconds ends a wait whose refusal never came while the call is still running, and a poll
// of 20 ms costs nothing beside a wait of ten seconds.
const (
	refusalRecordWait = 5 * time.Second
	refusalRecordPoll = 20 * time.Millisecond
)

// awaitRefusal polls done until it reports true and reports whether it did before
// refusalRecordWait ran out. Both readings it serves are written by another goroutine: the relay
// counts a send on the connection it filters, and the record is written where nats.go hands the
// server's report over (onAsyncError).
func awaitRefusal(done func() bool) bool {
	deadline := time.Now().Add(refusalRecordWait)
	for {
		if done() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(refusalRecordPoll)
	}
}

// refusalOfAnotherSubject has the connection earn the server's refusal of subject, which its
// grant does not cover, while the step under test waits for an answer, and returns the refusal
// the client recorded of it, nil when none was recorded in time. Its caller reads that answer
// before it judges the step: a refusal that was never recorded is one the step could not have
// read either.
func refusalOfAnotherSubject(t *testing.T, relay *unansweredRelay, owner *Client, conn *nats.Conn, subject string) error {
	t.Helper()
	// The refusal is published once the step's first send is on the wire, so it is recorded at
	// or after the moment that send began and deniedPublish's sentAt guard cannot skip it as an
	// earlier publish's.
	if !awaitRefusal(func() bool { return relay.sent() >= 1 }) {
		return nil
	}
	sentAt := time.Now()
	// Two publishes, because the read below takes the violation it reads out of the record
	// (deniedPublish): one violation is this row's proof that the refusal was recorded, and the
	// other is left in the record, where a send reading a refusal of any subject rather than of
	// its own finds it. With one, the row would prove the record and leave nothing to catch that
	// reading.
	for range 2 {
		if err := conn.Publish(subject, nil); err != nil {
			t.Errorf("publish to %s, which the connection's grant does not cover: %v", subject, err)
			return nil
		}
	}
	if err := conn.Flush(); err != nil {
		t.Errorf("flush the publishes the server refuses: %v", err)
		return nil
	}
	var denial error
	awaitRefusal(func() bool {
		denial = owner.deniedPublish(subject, sentAt)
		return denial != nil
	})
	return denial
}
