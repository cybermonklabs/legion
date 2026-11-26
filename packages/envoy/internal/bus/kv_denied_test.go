package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
	_, err := EnsureKeyValue(countedReads(t.Context()), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
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

// A durable consumer's create and delete that the grant refuses end at once, naming the refusal,
// rather than after the request's own ten second wait with a bare "context deadline exceeded":
// the listener's start creates its durable, and Dispatch's take-over of a held durable deletes
// and creates one, through the client's own JetStream context (readsRefusals). The create's read
// of the consumer, which nats.go sends first, is granted, so the refusal the create ends on is its
// own; no stream is needed, since the read answers "stream not found" and nats.go goes on to the
// create.
func TestAConsumerCreateAndDeleteTheGrantRefusesEndAtOnceNamingTheRefusal(t *testing.T) {
	const stream, consumer = "ENVOY_DENIED_CONSUMERS", "denied"
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", jsAPI("CONSUMER.INFO", stream, consumer))
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{grant.URL})
	if err != nil {
		t.Fatalf("connect the granted client: %v", err)
	}
	t.Cleanup(client.Close)
	for _, row := range []struct {
		name, subject string
		call          func() error
	}{
		{"create", jsAPI("CONSUMER.CREATE", stream, consumer), func() error {
			_, err := client.JS().AddConsumer(stream, &nats.ConsumerConfig{Durable: consumer, AckPolicy: nats.AckExplicitPolicy})
			return err
		}},
		{"delete", jsAPI("CONSUMER.DELETE", stream, consumer), func() error { return client.JS().DeleteConsumer(stream, consumer) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			started := time.Now()
			err := row.call()
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("a %s the grant refuses = %v (after %s), want ErrPublishDenied", row.name, err, took)
			}
			if want := `Permissions Violation for Publish to "` + row.subject + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", row.name, err, want)
			}
			if took > refusalAnswerBound {
				t.Fatalf("the refused %s ended after %s, want within %s", row.name, took, refusalAnswerBound)
			}
		})
	}
}

// A watch of the bucket that the grant refuses ends at once with the server's refusal
// (watchRefused). A read and a write of a key the grant refuses name the refusal too, after their
// own wait: they are the calls the bus makes on every request it serves, so they read the record
// only when the server left them unanswered (refusedAfterOn), and a call the server answers costs
// what it cost before. At main each of them reported a bare timeout.
func TestAKeyReadWriteAndWatchTheGrantRefusesNameTheRefusal(t *testing.T) {
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
		bound   time.Duration
		call    func() error
	}{
		{"watch", jsAPI("CONSUMER.CREATE", stream), refusalAnswerBound, func() error {
			watcher, err := kv.WatchKeys(t.Context(), "key")
			if err == nil {
				_ = watcher.Stop()
			}
			return err
		}},
		{"put", "$KV." + bucket + ".key", RequestTimeout + refusalAnswerBound, func() error { _, err := kv.Put("key", []byte("value")); return err }},
		{"get", jsAPI("DIRECT.GET", stream, "$KV."+bucket+".key"), RequestTimeout + refusalAnswerBound, func() error { _, err := kv.Get("key"); return err }},
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
			if took > row.bound {
				t.Fatalf("the refused %s ended after %s, want within %s", row.name, took, row.bound)
			}
		})
	}
}

// refusingGetLegacy is a legacy handle whose Get earns the server's refusal of subject, a request
// the Get itself never sends, as a read by revision running beside it does when the grant does not
// cover a direct get by sequence, and then draws no answer of its own, as a read the server lost
// does. It answers only once owner's record holds that refusal (holdsRefusal), read without taking
// it out, so the Get's own read of the record, after the answer, is the one that could take it. It
// waits for that record for wait, which the row sets to the test binary's deadline less
// refusalRecordSpare (testwait.Remaining), so a record a loaded machine writes late is still read.
type refusingGetLegacy struct {
	nats.KeyValue
	conn    *nats.Conn
	owner   *Client
	subject string
	wait    time.Duration
}

func (l refusingGetLegacy) Get(key string) (nats.KeyValueEntry, error) {
	sentAt := time.Now()
	if err := l.conn.Publish(l.subject, nil); err != nil {
		return nil, err
	}
	if err := l.conn.Flush(); err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(l.wait); !l.owner.holdsRefusal(l.subject, sentAt); {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the record held no refusal of %s %s before the test binary's deadline", l.subject, refusalRecordSpare)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, nats.ErrTimeout
}

// holdsRefusal reports whether c's record holds a refusal of subject recorded at or after sentAt,
// and leaves it there, unlike deniedPublish, which takes the refusal it reports out.
func (c *Client) holdsRefusal(subject string, sentAt time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, denial := range c.deniedPublishes[subject] {
		if !denial.at.Before(sentAt) {
			return true
		}
	}
	return false
}

// A read of a key takes only the refusal of a subject it sends: when a Get draws no answer and the
// record holds a refusal of the bare direct get of the bucket's stream, which only a read by
// revision sends, the Get reports its own silence, and the refusal stays in the record for the
// read that earned it. The grant is shaped as the listener's bucket grant is (infra/core
// bucketPermissions), every direct get by subject, the leader get and the keys, without the bare
// direct get.
func TestARefusalOfASubjectAReadDoesNotSendAnswersItNothing(t *testing.T) {
	const bucket = "envoy_denied_cross"
	stream := "KV_" + bucket
	bare := jsAPI("DIRECT.GET", stream)
	conn, js := grantedClient(t, accountInfoSubject(), openSubject(bucket), createSubject(bucket),
		"$KV."+bucket+".>", bare+".>", jsAPI("STREAM.MSG.GET", stream))
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	owner := clientOwning(conn)
	if owner == nil {
		t.Fatal("no client owns the connection, so nothing records the server's refusals")
	}
	sentAt := time.Now()
	refusing := NewKeyValue(refusingGetLegacy{KeyValue: kv.Legacy(), conn: conn, owner: owner, subject: bare, wait: testwait.Remaining(t, refusalRecordSpare)}, kv.WatchHandle(), kv.Streams())
	_, err = refusing.Get("key")
	if errors.Is(err, ErrPublishDenied) || !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("a Get that drew no answer beside a refusal of %s, which a Get never sends = %v, want its own nats.ErrTimeout", bare, err)
	}
	if denial := owner.deniedPublish(bare, sentAt); denial == nil {
		t.Fatalf("the record holds no refusal of %s after the Get: the Get took it, or it was never recorded and this row holds nothing", bare)
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
// answer, so the lookup is sent again, and the start ends at its hang guard. refusedBy reads
// the record under the subjects the step publishes to, and that list is what keeps the two apart:
// a read of whatever refusal the connection holds would end this start at its first send with
// another subject's violation, and every other row of this package would still pass, since no
// other one records a refusal the step under it did not earn.
//
// The grant covers every subject a start of this bucket publishes to, so nothing the call sends
// is refused, and the relay drops every send of the lookup, so the call draws no answer at all.
// The refusal the row plants beside it is of the delete of the bucket's stream, which no step of
// EnsureKeyValue publishes to, so it is the only refusal the connection can record: any
// ErrPublishDenied the call ends with is that refusal, read by a step that did not earn it, and the
// row names that first. Only then does it require the record itself, because a row that never saw
// the refusal recorded would pass whatever refusedBy read, and requires it recorded before the
// call ended, since one recorded after could not have answered it. The wait for that record ends
// when the call does (awaitRefusal), so a step that took the refusal is named within seconds of
// its start, not at the test binary's deadline.
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

	// Every send of the lookup is dropped, so the step sends until its hang guard passes: each send
	// still unanswered at its read limit starts another beside it (readAgain).
	relay.arm(1 << 30)
	recorded := make(chan refusalSeen, 1)
	callEnded := make(chan struct{})
	go func() {
		denial := refusalOfAnotherSubject(t, relay, owner, conn, deleteSubject, callEnded)
		recorded <- refusalSeen{denial: denial, at: time.Now()}
	}()
	_, err := ensureKeyValue(context.Background(), slog.Default(), refusalGuard, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	ended := time.Now()
	close(callEnded)
	seen := <-recorded
	denial := seen.denial

	if errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a start whose lookup nothing answered ended after %d sends with %v, want no ErrPublishDenied: the refusal beside it, of %s, is of a subject the lookup never published to",
			relay.sent(), err, deleteSubject)
	}
	if denial == nil || !seen.at.Before(ended) {
		t.Fatalf("the connection recorded no refusal of %s while the lookup was in flight (%v), so the start met none and this row holds nothing",
			deleteSubject, denial)
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
// times, each starting the next at its read limit, and for the refusal of another subject to be
// recorded while one of them is in flight.
const refusalGuard = RequestTimeout + 5*time.Second

// refusalRecordWait is the window TestAWatchIsAnsweredByItsOwnCreateAndNotByAnotherConsumersRefusal
// gives a refusal to answer a held watch before it lets the watch's own create through, and
// refusalRecordPoll is the pace of the waits on the connection's record, that row's and
// awaitRefusal's: 20 ms costs nothing beside a wait of seconds.
const (
	refusalRecordWait = 5 * time.Second
	refusalRecordPoll = 20 * time.Millisecond
)

// refusalRecordSpare is what a wait of this file's on the relay's count or on the connection's
// record leaves of the test binary's deadline when the call under test does not end first: more
// than the margin testwait's own waits leave, so such a wait gives up, and hands the row its
// answer, before a wait of the row's own does.
const refusalRecordSpare = 45 * time.Second

// awaitRefusal polls done until it reports true, or until callEnded is closed, and reports whether
// done held. Both readings it serves are written by another goroutine: the relay counts a send on
// the connection it filters, and the record is written where nats.go hands the server's report
// over (onAsyncError). Neither can come once the call under test has returned, since the step that
// sends is over and a refusal recorded then could not have answered it, so the call's end ends the
// wait: a step that took a refusal it did not earn ends the call at once, and the row then names
// that within seconds rather than at the test binary's deadline. It runs on a goroutine beside the
// call, where t.Fatal is the test goroutine's to call, so it fails nothing itself: its bound when
// the call does not end is the test binary's deadline less refusalRecordSpare (testwait.Left), and
// a wait that begins with no more than that left says so with t.Error.
func awaitRefusal(t *testing.T, callEnded <-chan struct{}, done func() bool) bool {
	bound, err := testwait.Left(t, refusalRecordSpare)
	if err != nil {
		t.Error(err)
		return false
	}
	testwait.Holds(bound, refusalRecordPoll, func() bool {
		if done() {
			return true
		}
		select {
		case <-callEnded:
			return true
		default:
			return false
		}
	})
	return done()
}

// refusalOfAnotherSubject has the connection earn the server's refusal of subject, which its
// grant does not cover, while the step under test waits for an answer, and returns the refusal
// the client recorded of it, nil when none was recorded before the call ended (callEnded) or in
// time. Its caller reads that answer once it has judged the step's own error: a refusal that was
// never recorded is one the step could not have read either.
func refusalOfAnotherSubject(t *testing.T, relay *unansweredRelay, owner *Client, conn *nats.Conn, subject string, callEnded <-chan struct{}) error {
	t.Helper()
	// The refusal is published once the step's first send is on the wire, so it is recorded at
	// or after the moment that send began and deniedPublish's sentAt guard cannot skip it as an
	// earlier publish's.
	if !awaitRefusal(t, callEnded, func() bool { return relay.sent() >= 1 }) {
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
	// Both refusals must be in the record before one is taken: nats.go hands each -ERR to the
	// client's error handler on its own callback goroutine (nc.ach), so the flush above can return
	// with the second refusal not yet recorded. A row that judged the record then would read the
	// missing refusal as one the step took (besideRefusal's requireLeft); it did, under load, in CI.
	awaitRefusal(t, callEnded, func() bool { return owner.recordedDenials(subject, sentAt) >= 2 })
	return owner.deniedPublish(subject, sentAt)
}

// recordedDenials is how many refusals of subject the record holds that were recorded at or after
// sentAt, read without taking any (deniedPublish takes one).
func (c *Client) recordedDenials(subject string, sentAt time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	recorded := 0
	for _, denial := range c.deniedPublishes[subject] {
		if !denial.at.Before(sentAt) {
			recorded++
		}
	}
	return recorded
}
