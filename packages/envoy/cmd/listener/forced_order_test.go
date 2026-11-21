//go:build odatrace

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/forcedorder"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsversion"
)

// The held-write collision at each creation site of the listener's own durable (the plan's first
// forced-order family), on the hooked server (scripts/forced-order.sh), on each of the plan's
// topologies (forcedorder.Topologies): the start's bind and the monitor's rebuild, each after the
// server removed the durable while its state write was held. In the fixed mode the durable is
// created again under its one name, at deliver policy all, and the late write gives it the removed
// one's state: every message it was created to deliver marked delivered and pending, none of which
// it delivers within the ack wait. In the fresh mode it gets a name of its own. Each row runs the
// raw same-name control first.

const (
	durableMessages = 30
	durableAcked    = 20
	// durableWindow is how long, at most, a row reads what the durable delivers: inside the
	// listener's 60 s ack wait (consumerAckWait), so a message the late state marked pending is not
	// delivered again within it.
	durableWindow = 8 * time.Second
	// durableSubject is a subject of the stream every listener durable filters on.
	durableSubject = "notifications.team.forced-durable"
)

// durableForcedRig is hooked servers in a topology, the listener's client in the build's naming
// mode, and a plain connection of the test's own.
type durableForcedRig struct {
	*forcedorder.Servers
	client  *bus.Client
	conn    *natsgo.Conn
	durable string
}

// forEachDurableTopology runs row on each topology. On a nats-server the listener refuses at its
// start (natsversion.Refusal), no durable of it exists to collide: the row confirms the refusal and
// skips itself.
func forEachDurableTopology(t *testing.T, row func(t *testing.T, r *durableForcedRig)) {
	t.Helper()
	if refusal := natsversion.Refusal(natsserver.VERSION); refusal != nil {
		_, uri := testnats.Start(t)
		_, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil)
		if err == nil || !strings.Contains(err.Error(), refusal.Error()) {
			t.Fatalf("nats-server %s is refused by natsversion (%v), but the session consumers connected: %v", natsserver.VERSION, refusal, err)
		}
		t.Skipf("the listener refuses nats-server %s at its start, so no durable of it exists to collide: %v", natsserver.VERSION, err)
	}
	for _, topo := range forcedorder.Topologies {
		t.Run(topo.Name, func(t *testing.T) {
			servers := forcedorder.Start(t, topo)
			var urls []string
			for _, srv := range servers.Servers {
				urls = append(urls, srv.URL())
			}
			conn := testnats.Connect(t, servers.URL)
			t.Cleanup(conn.Close)
			client := forcedListenerClient(t, urls, conn, topo.Replicas)
			row(t, &durableForcedRig{Servers: servers, client: client, conn: conn, durable: bus.ListenerDurable("forced-" + id.New())})
		})
	}
}

func (r *durableForcedRig) js(t *testing.T) natsgo.JetStreamContext {
	t.Helper()
	js, err := r.conn.JetStream(natsgo.MaxWait(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// publish stores n messages on durableSubject and answers their stream sequences.
func (r *durableForcedRig) publish(t *testing.T, n int) []uint64 {
	t.Helper()
	seqs := make([]uint64, n)
	for i := range seqs {
		ack, err := r.client.JS().Publish(durableSubject, fmt.Appendf(nil, "m%d", i+1))
		if err != nil {
			t.Fatalf("publish message %d: %v", i+1, err)
		}
		seqs[i] = ack.Sequence
	}
	return seqs
}

// ackHeld waits for durableMessages of msgs, acks the first durableAcked, waits for consumer
// name's o.dat to hold that floor on every host, and holds, before its I/O, the write of the next
// ack, which it then sends.
func (r *durableForcedRig) ackHeld(t *testing.T, name string, msgs chan *natsgo.Msg, seqs []uint64) forcedorder.Held {
	t.Helper()
	var got []*natsgo.Msg
	for len(got) < durableMessages {
		select {
		case msg := <-msgs:
			if meta, err := msg.Metadata(); err == nil && meta.Sequence.Stream >= seqs[0] {
				got = append(got, msg)
			} else {
				_ = msg.Ack()
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s delivered %d of %d", name, len(got), durableMessages)
		}
	}
	for _, msg := range got[:durableAcked] {
		if err := msg.AckSync(); err != nil {
			t.Fatal(err)
		}
	}
	r.AwaitStoredFloor(t, r.conn, bus.Stream, name, seqs[durableAcked-1])
	held := r.HoldWrites(t, r.conn, testnats.WritePreIO, bus.Stream, name, seqs[durableAcked])
	if err := got[durableAcked].AckSync(); err != nil {
		t.Fatal(err)
	}
	held.Reached(t)
	return held
}

// sameNameControl is every row's control: a raw push durable whose write of its 21st ack is held is
// deleted and created again under the same name, and after a restart it must deliver none of what
// it was created to deliver within the window (the measured M4): otherwise this topology does not
// reproduce the collision, and no row's pass means anything.
func (r *durableForcedRig) sameNameControl(t *testing.T) {
	t.Helper()
	js := r.js(t)
	name := "listener-forced-control-" + id.New()
	seqs := r.publish(t, durableMessages)
	config := &natsgo.ConsumerConfig{Durable: name, DeliverSubject: natsgo.NewInbox(), FilterSubject: durableSubject,
		AckPolicy: natsgo.AckExplicitPolicy, AckWait: consumerAckWait, DeliverPolicy: natsgo.DeliverByStartSequencePolicy, OptStartSeq: seqs[0]}
	if _, err := js.AddConsumer(bus.Stream, config); err != nil {
		t.Fatalf("control: create %s: %v", name, err)
	}
	msgs := make(chan *natsgo.Msg, 2*durableMessages)
	sub, err := js.ChanSubscribe(durableSubject, msgs, natsgo.Bind(bus.Stream, name), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("control: bind %s: %v", name, err)
	}
	held := r.ackHeld(t, name, msgs, seqs)
	_ = sub.Unsubscribe()
	mark := r.Hooks.Mark()
	if err := js.DeleteConsumer(bus.Stream, name); err != nil {
		t.Fatalf("control: delete %s: %v", name, err)
	}
	config.DeliverSubject = natsgo.NewInbox()
	if _, err := js.AddConsumer(bus.Stream, config); err != nil {
		t.Fatalf("control: create %s again: %v", name, err)
	}
	r.AwaitNewerWrites(t, mark, bus.Stream, held.Stores)
	r.ReleaseAndLand(t, held)
	r.conn = r.Restart(t, bus.Stream)
	after := make(chan *natsgo.Msg, 4*durableMessages)
	readSub, err := r.conn.ChanSubscribe(config.DeliverSubject, after)
	if err != nil {
		t.Fatalf("control: read %s: %v", name, err)
	}
	defer func() { _ = readSub.Unsubscribe() }()
	got := collect(t, after, seqs[durableAcked+1:])
	if slices.Contains(got, seqs[durableAcked+1]) {
		t.Fatalf("control: the same-name re-creation delivered %v after the restart: the held write did not reach it, so this topology does not reproduce the collision", got)
	}
	t.Logf("control: the same-name re-creation delivered %v after the restart, none of %d..%d", got, seqs[durableAcked+1], seqs[durableMessages-1])
}

// heldDurable binds the rig's durable as the listener does, delivers durableMessages of the
// subject to it, acks the first durableAcked, and holds the write of the next ack on every host. It
// answers the physical name, the holds, and the messages' sequences.
func (r *durableForcedRig) heldDurable(t *testing.T) (string, forcedorder.Held, []uint64) {
	t.Helper()
	msgs := make(chan *natsgo.Msg, 4*durableMessages)
	sub, err := startListenerSubscription(r.client, r.durable, func(msg *natsgo.Msg) { msgs <- msg })
	if err != nil {
		t.Fatalf("bind %s: %v", r.durable, err)
	}
	physical := forcedDurableName(t, r.client, r.durable)
	seqs := r.publish(t, durableMessages)
	held := r.ackHeld(t, physical, msgs, seqs)
	_ = sub.Unsubscribe()
	return physical, held, seqs
}

// A listener's durable is a push consumer: it delivers the moment it is created, from the state it
// loaded then, so a row that let it run before a restart would read deliveries the late write never
// touched. Each row therefore holds the replacement's store before it loads any state
// (testnats.StoreCreate) on every host, lets the removed durable's held writes land first, and only
// then lets the replacement load: of the same name, it loads the removed one's state, every message
// it was created to deliver marked delivered and pending, and delivers none of them within the ack
// wait; of a fresh name, it loads nothing.
func (r *durableForcedRig) createAfterTheHeldWrite(t *testing.T, held forcedorder.Held, create func() error) {
	t.Helper()
	loading := r.HoldNewer(testnats.StoreCreate, bus.Stream, held.Stores)
	done := make(chan error, 1)
	go func() { done <- create() }()
	for _, hold := range loading {
		t.Logf("held the replacement's store before it loads: %s", hold.Reached(t))
	}
	r.ReleaseAndLand(t, held)
	for _, hold := range loading {
		hold.Release()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the create of the replacement: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the create of the replacement did not answer within 30 s of its store's release")
	}
}

// requireListenerDelivered fails t unless got holds every message after the acked ones: a durable
// created again at deliver policy all, or at its record's start, delivers earlier ones again, which
// the listener's delivery handles, and missing one is what the row refuses.
func requireListenerDelivered(t *testing.T, got, want []uint64, what string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected message set is empty, so the row judges nothing", what)
	}
	for _, seq := range want {
		if !slices.Contains(got, seq) {
			t.Fatalf("%s delivered %v within the window; want every one of %v", what, got, want)
		}
	}
}

// The monitor's rebuild of a durable the server removed (rebuildListenerDependencies): its probe
// finds the durable gone, the bind creates it again, and the bound wait sees it bound. The removed
// durable's held write must not reach what the rebuild created.
func TestForcedOrderARebuildAfterTheDurablesRemovalUnderAHeldWriteDeliversEveryMessage(t *testing.T) {
	forEachDurableTopology(t, func(t *testing.T, r *durableForcedRig) {
		r.sameNameControl(t)
		physical, held, seqs := r.heldDurable(t)
		if err := r.js(t).DeleteConsumer(bus.Stream, physical); err != nil {
			t.Fatalf("remove %s: %v", physical, err)
		}
		received := make(chan *natsgo.Msg, 4*durableMessages)
		probe := func() error { return durableProbe(context.Background(), r.client, r.durable) }
		r.createAfterTheHeldWrite(t, held, func() error {
			return rebuildListenerDependencies(context.Background(), r.client, nil, probe, r.durable, func(msg *natsgo.Msg) { received <- msg }, nil)
		})
		want := seqs[durableAcked+1:]
		requireListenerDelivered(t, collect(t, received, want), want, "the durable the rebuild created")
	})
}

// The start's bind (startListenerSubscription, through bindListenerDurable) of a durable the server
// removed while the listener was away: the removed durable's held write must not reach the one the
// start creates.
func TestForcedOrderAStartAfterTheDurablesRemovalUnderAHeldWriteDeliversEveryMessage(t *testing.T) {
	forEachDurableTopology(t, func(t *testing.T, r *durableForcedRig) {
		r.sameNameControl(t)
		physical, held, seqs := r.heldDurable(t)
		if err := r.js(t).DeleteConsumer(bus.Stream, physical); err != nil {
			t.Fatalf("remove %s: %v", physical, err)
		}
		received := make(chan *natsgo.Msg, 4*durableMessages)
		r.createAfterTheHeldWrite(t, held, func() error {
			_, err := startListenerSubscription(r.client, r.durable, func(msg *natsgo.Msg) { received <- msg })
			return err
		})
		want := seqs[durableAcked+1:]
		requireListenerDelivered(t, collect(t, received, want), want, "the durable the start created")
	})
}

// collect reads what a subscription delivers, acking each, until it has every one of until or
// durableWindow passes, and answers the stream sequences, sorted and once each.
func collect(t *testing.T, received chan *natsgo.Msg, until []uint64) []uint64 {
	t.Helper()
	var seqs []uint64
	complete := func() bool {
		for _, seq := range until {
			if !slices.Contains(seqs, seq) {
				return false
			}
		}
		return true
	}
	deadline := time.After(durableWindow)
	for !complete() {
		select {
		case msg := <-received:
			if meta, err := msg.Metadata(); err == nil {
				seqs = append(seqs, meta.Sequence.Stream)
			}
			_ = msg.Ack()
		case <-deadline:
			slices.Sort(seqs)
			return slices.Compact(seqs)
		}
	}
	slices.Sort(seqs)
	return slices.Compact(seqs)
}
