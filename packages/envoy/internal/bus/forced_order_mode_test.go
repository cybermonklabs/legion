//go:build odatrace

package bus

import (
	"context"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
)

// The steps of the forced-order rows that name the consumer naming mode: everything else in the
// forced_order*_test.go files is the bus's own API, which the fixed build has too, so the rows build
// and run against the fixed build with these swapped for its own (the race regressions' run at the
// base: SessionConsumers in the fixed mode, the fixed name, Pull by session id, the bucket made raw).

// forcedSessionConsumers is SessionConsumers in the fresh mode on urls, over a selection bucket at
// replicas copies conn makes as the operator's create-bucket does, its sentinel saying the cutover
// is done. The create is bounded by forcedLed, not by a request's default 5 s: the server picks a
// new stream's preferred leader at random among the online members, and a preferred member that
// has not applied the stream's assignment never campaigns, so the stream is led, and its create
// answered, only after an election timeout (nats-server v2.15.1 jetstream_cluster.go:11096-11097,
// setPreferredMember :3945-3985); TestForcedOrderASelectionBucketIsReadOnlyOnceTheMetaLeaderHasAppliedIt
// parks the meta leader so, and failed its create with "context deadline exceeded" when that member
// was picked.
func forcedSessionConsumers(t *testing.T, urls []string, conn *natsgo.Conn, replicas int) *SessionConsumers {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), forcedLed)
	defer cancel()
	if _, err := consumernames.CreateSessionBucket(ctx, conn, replicas, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatalf("create the selection bucket: %v", err)
	}
	return forcedProcess(t, urls)
}

// forcedProcess is another process's SessionConsumers on uri, in the build's mode, over the bucket
// the rig made.
func forcedProcess(t *testing.T, urls []string) *SessionConsumers {
	t.Helper()
	consumers, err := ConnectSessionConsumers(urls, 0, nil, WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatalf("connect session consumers in the fresh mode: %v", err)
	}
	t.Cleanup(consumers.Close)
	return consumers
}

// forcedName is the name of the consumer a call answered as start, for sessionID.
func forcedName(start SessionConsumerStart, _ string) string { return start.Name }

// forcedPull pulls the consumer a call answered as start, for sessionID, once the server answers
// that the consumer has a leader (consumerLed), or answers ctx's end, or forcedLed's when ctx has
// no deadline. A registration whose create the server answered late settles it with an info,
// which finds the candidate before its servers have applied its assignment, so the call can answer
// a consumer that delivers nothing until it is led
// (TestForcedOrderARegistrationAnsweredBeforeItsConsumerIsLedIsReadOnceItIs).
func forcedPull(ctx context.Context, consumers *SessionConsumers, start SessionConsumerStart, sessionID string) (*SessionPull, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, forcedLed)
		defer cancel()
	}
	for {
		info, err := consumers.info(ctx, start.Name)
		if err == nil && consumerLed(consumers, info) {
			return consumers.Pull(ctx, sessionID, start.Name)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("consumer %s has no leader: %w (last read: %+v, %v)", start.Name, ctx.Err(), info, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// forcedLostState reports whether err refuses a call for lost selection state.
func forcedLostState(err error) bool { return consumernames.IsSelectionStateLost(err) }

// forcedCleanup deletes sessionID's retired consumers, as the listener's sweep does.
func forcedCleanup(t *testing.T, consumers *SessionConsumers, sessionID string) {
	t.Helper()
	if _, err := consumers.CleanRetired(context.Background(), sessionID); err != nil {
		t.Fatalf("delete the retired consumers of %s: %v", sessionID, err)
	}
}
