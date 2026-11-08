package bus

import (
	"errors"
	"strings"
	"testing"

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
	client, err := Connect([]string{relay.url})
	if err != nil {
		t.Fatalf("connect the client whose connection the start opens the bucket on: %v", err)
	}
	t.Cleanup(client.Close)

	relay.arm(0)
	_, err = EnsureKeyValue(client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})

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
