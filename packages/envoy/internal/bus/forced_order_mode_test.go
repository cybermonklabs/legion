//go:build odatrace

package bus

import (
	"context"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
)

// The three steps of the forced-order rows that name the consumer naming mode: everything else in
// forced_order_test.go is the bus's own API, which the fixed build has too, so the rows build and
// run against the fixed build with these three swapped for its own (the race regressions' run at
// the base: SessionConsumers in the fixed mode, the fixed name, Pull by session id).

// forcedSessionConsumers is SessionConsumers in the fresh mode on uri, over a selection bucket
// conn makes as the operator's create-bucket does, its sentinel saying the cutover is done.
func forcedSessionConsumers(t *testing.T, uri string, conn *natsgo.Conn) *SessionConsumers {
	t.Helper()
	if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatalf("create the selection bucket: %v", err)
	}
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil, WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatalf("connect session consumers in the fresh mode: %v", err)
	}
	t.Cleanup(consumers.Close)
	return consumers
}

// forcedName is the name of the consumer a call answered as start, for sessionID.
func forcedName(start SessionConsumerStart, _ string) string { return start.Name }

// forcedPull pulls the consumer a call answered as start, for sessionID.
func forcedPull(ctx context.Context, consumers *SessionConsumers, start SessionConsumerStart, _ string) (*SessionPull, error) {
	return consumers.Pull(ctx, start.Name)
}
