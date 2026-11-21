//go:build odatrace

package main

import (
	"context"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
)

// The steps of the listener's forced-order rows that name the consumer naming mode, swapped for the
// fixed build's own to run the race regressions at the build before this one.

// forcedListenerClient is the listener's client in the fresh mode on urls, the stream at replicas
// copies, over a selection bucket conn makes as the operator's create-bucket does.
func forcedListenerClient(t *testing.T, urls []string, conn *natsgo.Conn, replicas int) *bus.Client {
	t.Helper()
	if _, err := consumernames.CreateSessionBucket(context.Background(), conn, replicas, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatalf("create the selection bucket: %v", err)
	}
	client, err := bus.ConnectOwningStream(urls, bus.WithReplicas(replicas), bus.WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatalf("connect the listener's client: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// forcedDurableName is the physical name of the listener's durable named durable now: its record's
// current.
func forcedDurableName(t *testing.T, client *bus.Client, durable string) string {
	t.Helper()
	record, _, err := client.SelectionStore().Read(context.Background(), consumernames.DurableKey(durable))
	if err != nil {
		t.Fatalf("read the record of %s: %v", durable, err)
	}
	return record.Current
}
