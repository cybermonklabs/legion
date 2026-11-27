package bus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// streamReadBehindALostOne bounds a create that joins the stream's state behind a read the server
// never answers: the overlap bound, a create's few round trips and room for a loaded test
// machine, against the 5 s sessionStreamReadWait it used to wait out, and inside the 3.5 s store
// budget of the registration that makes it.
const streamReadBehindALostOne = time.Second

// lostStreamRead connects SessionConsumers through a relay that drops the first STREAM.INFO of the
// notification stream it is armed for, starts session A's read of the stream's identity on it,
// and returns once that read's request is out and dropped: a read whose answer will never come,
// on a connection that stays up.
func lostStreamRead(t *testing.T) (*SessionConsumers, *testnats.UnansweredRelay) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	relay := testnats.StartUnansweredRelayOn(t, uri, streamInfoSubject)
	consumers, err := ConnectSessionConsumers([]string{relay.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers through the relay: %v", err)
	}
	t.Cleanup(consumers.Close)

	relay.Arm(1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _, _ = consumers.StreamID(ctx) }()
	awaitRelaySent(t, relay, 1)
	return consumers, relay
}

// ensureFresh creates sessionID's consumer, which the stream does not hold, and reports how long
// that took: a create reads the stream's state (streamState) for its start.
func ensureFresh(consumers *SessionConsumers, sessionID string) (time.Duration, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*sessionStreamReadWait)
	defer cancel()
	start, err := consumers.Ensure(ctx, sessionID, []string{contracts.LegacyAgentSubject(sessionID)}, 0)
	if err == nil && !start.Created {
		err = fmt.Errorf("Ensure of a session with no consumer = %+v, want it created", start)
	}
	return time.Since(started), err
}

// One session's stream read that the server never answers holds no other session's consumer
// create for more than the overlap bound. Session A's read is lost on a connection that stays
// up, so nothing ends it before its 5 s wait; session B's registration creates its consumer,
// which joins the next read of the stream's state. That read starts once A's has been out
// sessionStreamReadOverlap rather than once A's times out, so B's create answers inside
// streamReadBehindALostOne, as a registration's store budget needs: behind A's whole wait it ran
// out of that budget and answered 503.
func TestALostStreamReadHoldsAnotherSessionsCreateOnlyForTheOverlap(t *testing.T) {
	consumers, _ := lostStreamRead(t)

	took, err := ensureFresh(consumers, "ses_create_behind_a_lost_read")
	if err != nil {
		t.Fatalf("a create behind a lost stream read: %v", err)
	}
	if took > streamReadBehindALostOne {
		t.Fatalf("a create behind a lost stream read took %s, want within %s: the next read must not wait out the lost one",
			took.Round(time.Millisecond), streamReadBehindALostOne)
	}
}

// The overlap bound costs no read storm: a burst of creates for different sessions that all join
// while a read is out shares the one read that starts after it. Ten creates join behind a lost
// read within the bound, and the relay counts exactly one stream read beside the lost one; a
// stream state read per create would send ten.
func TestABurstOfCreatesBehindALostStreamReadSharesOneRead(t *testing.T) {
	const burst = 10
	consumers, relay := lostStreamRead(t)

	var wg sync.WaitGroup
	errs := make([]error, burst)
	took := make([]time.Duration, burst)
	for i := range burst {
		wg.Go(func() { took[i], errs[i] = ensureFresh(consumers, fmt.Sprintf("ses_burst_behind_a_lost_read_%d", i)) })
	}
	wg.Wait()

	for i := range burst {
		if errs[i] != nil {
			t.Fatalf("create %d of the burst: %v", i, errs[i])
		}
		if took[i] > streamReadBehindALostOne {
			t.Fatalf("create %d of the burst took %s, want within %s", i, took[i].Round(time.Millisecond), streamReadBehindALostOne)
		}
	}
	if reads := relay.Sent() - 1; reads != 1 {
		t.Fatalf("a burst of %d creates behind a lost stream read sent %d stream reads, want 1 shared by all of them", burst, reads)
	}
}

// Client.StreamID answers the stream's identity (StreamIDMetadataKey), and ErrNoStreamID once the
// stream carries none, as SessionConsumers.StreamID answers it: a caller that names a cursor by
// the identity never takes "" for one. The same client answers the identity before the key is
// removed, which is the control that the read sees a stream's metadata at all. With "" answered for
// a stream with no identity, as the identity, the second read fails this test.
func TestClientStreamIDAnswersErrNoStreamIDForAStreamWithNoIdentity(t *testing.T) {
	// The keeper puts the stream's identity back every defaultKeepInterval, which would undo the
	// removal below between the update and the second read, so it runs once an hour here.
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithKeepInterval(time.Hour))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	ctx := testwait.Context(t)
	js := client.JS()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	want := info.Config.Metadata[StreamIDMetadataKey]
	if id, err := client.StreamID(ctx); err != nil || id == "" || id != want {
		t.Fatalf("StreamID = %q, %v; want the stream's identity %q", id, err, want)
	}

	unidentified := info.Config
	unidentified.Metadata = maps.Clone(info.Config.Metadata)
	delete(unidentified.Metadata, StreamIDMetadataKey)
	if _, err := js.UpdateStream(&unidentified); err != nil {
		t.Fatalf("remove the stream's identity: %v", err)
	}
	if id, err := client.StreamID(ctx); !errors.Is(err, ErrNoStreamID) || id != "" {
		t.Fatalf("StreamID of a stream with no identity = %q, %v; want \"\" and ErrNoStreamID", id, err)
	}
}
