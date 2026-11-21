package writerhold

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// seedProtectedStreams makes every protected stream the preflight reads by name, three copies
// each, as the hub holds them: the operator makes them under a hold of its own (the held grant is
// the one that creates a protected stream), which it then releases with nothing changed.
func (h *heldStack) seedProtectedStreams() {
	h.t.Helper()
	ctx := context.Background()
	id := h.prepared(holdrecord.WorkflowStreamsUpdate)
	h.heldWithoutTheSteps(id)
	nc, err := h.op.Connect(ctx)
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}, Replicas: 3})
	require.NoError(h.t, err, "create %s", bus.Stream)
	buckets := append([]string{bus.SessionConsumersBucket, bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket}, bus.CopiedBuckets()...)
	for _, bucket := range buckets {
		_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, Replicas: 3})
		require.NoError(h.t, err, "create bucket %s", bucket)
	}
	nc.Close()
	code, _, _ := h.run(actionRelease, "--hold", id, "--reason", "the streams are made")
	require.Zero(h.t, code)
}

// listener connects a listener entry of machine, reconnecting as a listener does, and returns it
// with a channel that is closed once its connection has been refused at a reconnect.
func (h *heldStack) listener(machine string) (*nats.Conn, <-chan struct{}) {
	h.t.Helper()
	refused := make(chan struct{})
	var once sync.Once
	nc, err := nats.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), nats.Name("envoy-listener "+machine),
		nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrAuthorization) {
				once.Do(func() { close(refused) })
			}
		}))
	require.NoError(h.t, err, "the listener %s was refused before the hold", machine)
	h.t.Cleanup(nc.Close)
	return nc, refused
}

// During the hold no other writer can do anything at the hub: a listener connected before the hold
// is disconnected by its server's restart and refused at its reconnect, and a listener that
// connects after the report, whatever binary or image it runs, is refused at its connect, so none
// can create a consumer (a fixed name F included) or write a bucket until the release. The control
// is the same listener before the hold, which writes a bucket and is admitted; and after the
// release a listener is admitted again. It fails when the auth service admits a writer during the
// hold, or when hold set's restart leaves a writer connected.
func TestHoldRefusesEveryOtherWriter(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()

	before, refused := h.listener(holdtest.ListenerMachines[0])
	js, err := jetstream.New(before)
	require.NoError(t, err)
	kv, err := js.KeyValue(ctx, bus.SessionBucket)
	require.NoError(t, err, "the control: the listener opens its bucket before the hold")
	_, err = kv.Put(ctx, "before-the-hold", []byte("written"))
	require.NoError(t, err, "the control: the listener writes its bucket before the hold")

	code, stdout, _ := h.run(actionSet, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "the row's hold")
	require.Zero(t, code, "hold set")
	require.Contains(t, stdout, "only the operator")
	select {
	case <-refused:
	case <-time.After(30 * time.Second):
		t.Fatal("the listener connected before the hold was not refused at a reconnect during it")
	}

	for _, machine := range holdtest.ListenerMachines {
		nc, err := nats.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), nats.NoReconnect(), nats.Timeout(10*time.Second))
		if err == nil {
			nc.Close()
			t.Fatalf("listener %s was admitted during the hold", machine)
		}
		require.ErrorIs(t, err, nats.ErrAuthorization, "listener %s", machine)
	}
	_, err = kv.Put(ctx, "during-the-hold", []byte("written"))
	require.Error(t, err, "the listener connected before the hold wrote its bucket during it")
	_, err = js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{Durable: "s-fixed-name", FilterSubject: "notifications.x"})
	require.Error(t, err, "the listener connected before the hold made a consumer during it")

	row := h.row()
	require.Equal(t, natsgrant.OperatorPhaseHeld, row.Phase)
	d, _, err := h.s.Records.Journal(ctx, row.HoldID)
	require.NoError(t, err)
	require.Equal(t, "done", latestState(d, holdrecord.CommandSet), "hold set's done entry")

	code, _, _ = h.run(actionRelease, "--hold", row.HoldID, "--reason", "nothing changed")
	require.Zero(t, code)
	require.True(t, h.listenerAdmitted(), "a listener was refused after the release")
}
