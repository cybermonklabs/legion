package bus

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
//
// Each send's wait is RequestTimeout armed on the clock of the client that owns the connection
// (withinRequestTimeout), and the row hands that client a fake: once a send is on the wire, as
// the relay counts it, and its bound is armed, the row moves the fake past the bound, so the start
// spends its sends as the row moves the clock rather than in thirty seconds of the wall clock.
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
	fake := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	conn, js := relayedJetStream(t, relay, WithClock(fake))
	owner := clientOwning(conn)
	if owner == nil {
		t.Fatal("no client owns the connection the start looks the bucket up on, so nothing records the server's refusals")
	}

	// Every send of the lookup is dropped, so the step spends all of its sends and the start
	// ends with the deadline of the last one. The start runs beside the test, which earns the
	// refusal of another subject while the lookup waits.
	relay.arm(unansweredSends)
	ensured := make(chan error, 1)
	go func() {
		_, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
		ensured <- err
	}()
	denial := refusalOfAnotherSubject(t, relay, owner, conn, deleteSubject)
	if len(ensured) != 0 {
		t.Fatalf("the start ended (%v) before the connection recorded the refusal of %s, so it met none and this row holds nothing",
			<-ensured, deleteSubject)
	}
	// Each send ends on its own bound, which moves only when the row moves the fake, one
	// RequestTimeout past the moment the send armed it. A start that ended before its sends ran
	// out, on an answer, stops the moving, and the checks below say what it ended on.
	for sends := 1; sends <= unansweredSends; sends++ {
		armed := testwait.Until(t, fmt.Sprintf("lookup %d of the start on the wire with its bound armed, or the start's end", sends), func() ([]time.Time, bool) {
			if len(ensured) != 0 {
				return nil, true
			}
			armed := fake.Armed()
			return armed, relay.sent() >= sends && len(armed) > 0
		})
		if armed == nil {
			break
		}
		if want := []time.Time{fake.Now().Add(RequestTimeout)}; !slices.Equal(armed, want) {
			t.Fatalf("lookup %d of the start left %v armed on the client's clock, want its bound alone at %v, RequestTimeout after the send", sends, armed, want[0])
		}
		fake.Advance(RequestTimeout)
	}
	err := testwait.Receive(t, "the start whose lookup nothing answers to spend its sends", ensured)
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

// refusalOfAnotherSubject has the connection earn the server's refusal of subject, which its
// grant does not cover, while the step under test waits for an answer, and returns the refusal
// the client recorded of it. Its caller reads that answer before it judges the step, and checks
// the step was still waiting when it came: a refusal recorded after the step ended is one the
// step could not have read.
func refusalOfAnotherSubject(t *testing.T, relay *unansweredRelay, owner *Client, conn *nats.Conn, subject string) error {
	t.Helper()
	// The refusal is published once the step's first send is on the wire, so it is recorded at
	// or after the moment that send began and deniedPublish's sentAt guard cannot skip it as an
	// earlier publish's.
	testwait.Eventually(t, "the start's first lookup to reach the relay", func() bool { return relay.sent() >= 1 })
	sentAt := time.Now()
	// Two publishes, because the read below takes the violation it reads out of the record
	// (deniedPublish): one violation is this row's proof that the refusal was recorded, and the
	// other is left in the record, where a send reading a refusal of any subject rather than of
	// its own finds it. With one, the row would prove the record and leave nothing to catch that
	// reading. The row moves the clock that ends the step's send only once that second one is
	// recorded too, since the server reports each publish's refusal on its own.
	for range 2 {
		if err := conn.Publish(subject, nil); err != nil {
			t.Fatalf("publish to %s, which the connection's grant does not cover: %v", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the publishes the server refuses: %v", err)
	}
	denial := testwait.Until(t, "the connection's record of the refusal of "+subject, func() (error, bool) {
		denial := owner.deniedPublish(subject, sentAt)
		return denial, denial != nil
	})
	testwait.Eventually(t, "the second refusal of "+subject+" to stand in the connection's record", func() bool {
		return heldRefusals(owner, subject) > 0
	})
	return denial
}

// heldRefusals is how many refusals of subject the connection's record holds, read without taking
// any, as deniedPublish takes the one it returns.
func heldRefusals(owner *Client, subject string) int {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return len(owner.deniedPublishes[subject])
}
