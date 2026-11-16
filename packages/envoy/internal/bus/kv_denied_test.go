package bus

import (
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
// (recordPublishDenial), so before this change the step read that silence as a member still
// working and spent unansweredSends sends of RequestTimeout each, ending the start after 30 s
// with a bare "KV_envoy_denied_lookup STREAM.INFO: context deadline exceeded".
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
	_, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})

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
}

// A start whose lookup of a bucket draws no answer is not ended by the server's refusal of a
// subject that lookup never published to: a refusal of another subject is not this send's
// answer, so the lookup is sent again, up to unansweredSends sends, and the start ends on its
// own deadline. refusedSend reads the record under the subjects the step publishes to, and that
// list is what keeps the two apart: a read of whatever refusal the connection holds would end
// this start at its first send with another subject's violation, and every other row of this
// package would still pass, since no other one records a refusal the step under it did not earn.
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

	// Every send of the lookup is dropped, so the step spends all of its sends and the start
	// ends with the deadline of the last one.
	relay.arm(unansweredSends)
	recorded := make(chan error, 1)
	go func() { recorded <- refusalOfAnotherSubject(t, relay, owner, conn, deleteSubject) }()
	_, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	denial := <-recorded

	if denial == nil {
		t.Fatalf("the connection recorded no refusal of %s while the lookup was in flight, so the start met none and this row holds nothing",
			deleteSubject)
	}
	if errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a start whose lookup nothing answered ended after %d of its %d sends with %v, want no ErrPublishDenied: the refusal beside it, %v, is of a subject the lookup never published to",
			relay.sent(), unansweredSends, err, denial)
	}
	if err == nil || !strings.Contains(err.Error(), lookup) || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("a start whose lookup nothing answered = %v, want %q and its deadline: a send another subject's refusal does not answer ends on the wait it held",
			err, lookup)
	}
	if sends := relay.sent(); sends != unansweredSends {
		t.Fatalf("lookups sent = %d, want %d: a send a refusal of another subject does not answer is sent again until the step's sends run out",
			sends, unansweredSends)
	}
	if strings.Contains(err.Error(), deleteSubject) {
		t.Fatalf("the start's error %q carries %s, a subject no step of it publishes to", err, deleteSubject)
	}
}

// refusalRecordWait and refusalRecordPoll bound the waits the row above makes on the relay's
// count and on the connection's record, and are waitUntil's bound and poll. That helper itself
// does not serve here: these waits run on a goroutine beside the call under test, where t.Fatalf
// is the test goroutine's to call. The call lasts unansweredSends waits of RequestTimeout, half
// a minute, so a bound of five seconds ends a wait whose refusal never came while the call is
// still running, and a poll of 20 ms costs nothing beside a wait of ten seconds.
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
