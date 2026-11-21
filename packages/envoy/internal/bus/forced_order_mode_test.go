//go:build odatrace

package bus

import (
	"context"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
)

// The steps of the forced-order rows that name the consumer naming mode: everything else in the
// forced_order*_test.go files is the bus's own API, which the fixed build has too, so the rows build
// and run against the fixed build with these swapped for its own (the race regressions' run at the
// base: SessionConsumers in the fixed mode, the fixed name, Pull by session id, the bucket made raw).

// forcedSessionConsumers is SessionConsumers in the fresh mode on urls, over a selection bucket at
// replicas copies conn makes as the operator's create-bucket does, its sentinel saying the cutover
// is done.
func forcedSessionConsumers(t *testing.T, urls []string, conn *natsgo.Conn, replicas int) *SessionConsumers {
	t.Helper()
	if _, err := consumernames.CreateSessionBucket(context.Background(), conn, replicas, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
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

// forcedPull pulls the consumer a call answered as start, for sessionID.
func forcedPull(ctx context.Context, consumers *SessionConsumers, start SessionConsumerStart, sessionID string) (*SessionPull, error) {
	return consumers.Pull(ctx, sessionID, start.Name)
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
