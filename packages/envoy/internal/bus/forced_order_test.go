//go:build odatrace

package bus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/forcedorder"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsversion"
)

// The held-write collision at every creation site of the session family (the plan's first
// forced-order family), on the hooked server (packages/envoy/scripts/forced-order.sh). nats-server
// returns from a consumer delete while the old consumer's state write can still land; a create of
// the same name writes the same file, and the late write gives the new consumer the old one's
// state, so after a restart it skips what it was created to deliver.
//
// Each row first runs its control, a raw delete and create of one name under the same held write,
// and requires the loss it shows; then it drives one site of SessionConsumers into the same order:
// the predecessor's write is held, the site replaces the consumer, the replacement's own state
// write lands, the held write is released and lands, and the servers restart. The row requires
// every message the replacement was created to deliver to arrive. Each hold is required reached.
// Run against the fixed build, whose sites create the same name again, every row fails.
//
// Every row runs on each of the plan's topologies (forcedorder.Topologies): one copy, a
// three-member cluster holding the stream at one copy, and the stream at three copies.

const (
	// forcedMessages are published on a session's agent subject before its consumer is made.
	forcedMessages = 30
	// forcedAcked is how many of them the predecessor has acked when its write is held; the held
	// write carries the ack of the one after them.
	forcedAcked = 20
	// forcedWindow is how long, at most, a row reads what a consumer delivers: well inside the
	// consumers' 60 s ack wait, so a message the late state marked pending is not delivered again
	// within it. A read that has every message it waits for ends before it (delivered).
	forcedWindow = 8 * time.Second
)

// forcedTopology is where a row's stream lives (forcedorder.Topology).
type forcedTopology = forcedorder.Topology

// forEachTopology runs row on each of forcedorder.Topologies, as a subtest named for it. On a
// nats-server the session consumers refuse (natsversion.Refusal: every 2.10, v2.10.29 among them)
// no session consumer can exist, at this build or the one before it: the row confirms that the
// connection is refused for that reason, and skips itself, never a parent passing over subtests
// that skip.
func forEachTopology(t *testing.T, row func(t *testing.T, topo forcedTopology)) {
	t.Helper()
	skipOnARefusedServer(t)
	for _, topo := range forcedorder.Topologies {
		t.Run(topo.Name, func(t *testing.T) { row(t, topo) })
	}
}

func skipOnARefusedServer(t *testing.T) {
	t.Helper()
	refusal := natsversion.Refusal(natsserver.VERSION)
	if refusal == nil {
		return
	}
	_, uri := testnats.Start(t)
	_, err := ConnectSessionConsumers([]string{uri}, 0, nil)
	if err == nil || !strings.Contains(err.Error(), refusal.Error()) {
		t.Fatalf("nats-server %s is refused by natsversion (%v), but the session consumers connected: %v", natsserver.VERSION, refusal, err)
	}
	t.Skipf("the session consumers refuse nats-server %s, so no session consumer exists to collide: %v", natsserver.VERSION, err)
}

// forcedRig is hooked servers in a topology, the notification stream on them, SessionConsumers of
// the build under test, and a plain connection of the test's own.
type forcedRig struct {
	*forcedorder.Servers
	// urls are the members' URLs, one each: the bus refuses a comma-joined list as a remote server.
	urls      []string
	owner     *Client
	consumers *SessionConsumers
	conn      *natsgo.Conn
}

func newForcedRig(t *testing.T, topo forcedTopology, ownerOptions ...ConnectOption) *forcedRig {
	t.Helper()
	r := &forcedRig{Servers: forcedorder.Start(t, topo)}
	for _, srv := range r.Servers.Servers {
		r.urls = append(r.urls, srv.URL())
	}
	owner, err := ConnectOwningStream(r.urls, append([]ConnectOption{WithReplicas(topo.Replicas)}, ownerOptions...)...)
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	r.owner = owner
	r.conn = testnats.Connect(t, r.URL)
	t.Cleanup(r.conn.Close)
	r.consumers = forcedSessionConsumers(t, r.urls, r.conn, topo.Replicas)
	return r
}

// The forcedorder steps on the notification stream, read on the rig's own connection.

func (r *forcedRig) awaitStoredFloor(t *testing.T, name string, floor uint64) {
	t.Helper()
	r.AwaitStoredFloor(t, r.conn, Stream, name, floor)
}

func (r *forcedRig) stores(t *testing.T, name string) []forcedorder.Store {
	t.Helper()
	return r.Stores(t, r.conn, Stream, name)
}

func (r *forcedRig) holdWrites(t *testing.T, name string, floor uint64) forcedorder.Held {
	t.Helper()
	return r.HoldWrites(t, r.conn, testnats.WritePreIO, Stream, name, floor)
}

func (r *forcedRig) awaitNewerWrites(t *testing.T, mark int, stores []forcedorder.Store) {
	t.Helper()
	r.AwaitNewerWrites(t, mark, Stream, stores)
}

func (r *forcedRig) restart(t *testing.T) {
	t.Helper()
	r.conn = r.Restart(t, Stream)
}

// publish stores n messages on subject and answers their stream sequences.
func (r *forcedRig) publish(t *testing.T, subject string, n int) []uint64 {
	t.Helper()
	seqs := make([]uint64, n)
	for i := range seqs {
		ack, err := r.owner.JS().Publish(subject, fmt.Appendf(nil, "m%d", i+1))
		if err != nil {
			t.Fatalf("publish message %d: %v", i+1, err)
		}
		seqs[i] = ack.Sequence
	}
	return seqs
}

// delivered reads what a pull of the consumer delivers, acking each, until it has every one of
// until or forcedWindow passes, and answers the stream sequences, sorted and once each. pull starts
// a pull; a pull that ends early (a connection the restart replaced) is started again.
func delivered(t *testing.T, until []uint64, pull func(context.Context) (*SessionPull, error)) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), forcedWindow)
	defer cancel()
	var seqs []uint64
	complete := func() bool {
		for _, seq := range until {
			if !slices.Contains(seqs, seq) {
				return false
			}
		}
		return true
	}
	for ctx.Err() == nil && !complete() {
		p, err := pull(ctx)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for !complete() {
			msg, err := p.Next(ctx)
			if err != nil {
				break
			}
			seqs = append(seqs, msg.StreamSeq)
			_ = msg.Ack()
		}
		p.Stop()
	}
	slices.Sort(seqs)
	return slices.Compact(seqs)
}

// requireDelivered fails t unless got is exactly want.
func requireDelivered(t *testing.T, got, want []uint64, what string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected message set is empty, so the row judges nothing", what)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s delivered %v; want every message it was created to deliver, %v", what, got, want)
	}
}

// sameNameControl is every family-1 row's control: a raw consumer whose write of its 21st ack is
// held on every host, a delete and a create of the same name at the same start, the held writes
// released after the new consumer's own, and a restart. The new consumer must lose the messages the
// old one had acked (the measured M4): otherwise this topology does not reproduce the collision,
// and no row's pass means anything.
func (r *forcedRig) sameNameControl(t *testing.T) {
	t.Helper()
	const session = "ses_forced_control"
	name := "s-" + session
	subject := contracts.LegacyAgentSubject(session)
	seqs := r.publish(t, subject, forcedMessages)
	js, err := r.conn.JetStream(natsgo.MaxWait(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	config := &natsgo.ConsumerConfig{Durable: name, FilterSubject: subject, AckPolicy: natsgo.AckExplicitPolicy, AckWait: time.Minute,
		DeliverPolicy: natsgo.DeliverByStartSequencePolicy, OptStartSeq: seqs[0]}
	if _, err := js.AddConsumer(Stream, config); err != nil {
		t.Fatalf("control: create %s: %v", name, err)
	}
	fetchAck := func(n int) {
		sub, err := js.PullSubscribe(subject, name, natsgo.Bind(Stream, name))
		if err != nil {
			t.Fatalf("control: bind %s: %v", name, err)
		}
		defer func() { _ = sub.Unsubscribe() }()
		msgs, err := sub.Fetch(n, natsgo.MaxWait(10*time.Second))
		if err != nil || len(msgs) != n {
			t.Fatalf("control: fetch %d of %s: %d, %v", n, name, len(msgs), err)
		}
		for _, msg := range msgs {
			if err := msg.AckSync(); err != nil {
				t.Fatalf("control: ack: %v", err)
			}
		}
	}
	fetchAck(forcedAcked)
	r.awaitStoredFloor(t, name, seqs[forcedAcked-1])
	held := r.holdWrites(t, name, seqs[forcedAcked])
	fetchAck(1)
	held.Reached(t)
	mark := r.Hooks.Mark()
	if err := js.DeleteConsumer(Stream, name); err != nil {
		t.Fatalf("control: delete %s: %v", name, err)
	}
	if _, err := js.AddConsumer(Stream, config); err != nil {
		t.Fatalf("control: create %s again: %v", name, err)
	}
	r.awaitNewerWrites(t, mark, held.Stores)
	r.ReleaseAndLand(t, held)
	r.restart(t)
	if js, err = r.conn.JetStream(natsgo.MaxWait(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sub, err := js.PullSubscribe(subject, name, natsgo.Bind(Stream, name))
	if err != nil {
		t.Fatalf("control: bind %s after the restart: %v", name, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	var got []uint64
	deadline := time.Now().Add(forcedWindow)
	for time.Now().Before(deadline) {
		msgs, _ := sub.Fetch(forcedMessages, natsgo.MaxWait(time.Second))
		for _, msg := range msgs {
			if meta, err := msg.Metadata(); err == nil {
				got = append(got, meta.Sequence.Stream)
			}
			_ = msg.Ack()
		}
	}
	slices.Sort(got)
	got = slices.Compact(got)
	if slices.Contains(got, seqs[0]) {
		t.Fatalf("control: the same-name re-creation delivered %v after the restart, its first message included: the held write did not reach it, so this topology does not reproduce the collision", got)
	}
	t.Logf("control: the same-name re-creation delivered %v after the restart, losing %d..%d as measured", got, seqs[0], seqs[forcedAcked])
}

// heldSession is a session's consumer, made by the build under test, that has delivered every one of
// its forcedMessages and acked forcedAcked, with the write of its next ack held on every host.
type heldSession struct {
	id, agent string
	seqs      []uint64
	start     SessionConsumerStart
	name      string
	held      forcedorder.Held
}

func (r *forcedRig) heldSession(t *testing.T, id string) *heldSession {
	t.Helper()
	ctx := context.Background()
	s := &heldSession{id: id, agent: contracts.LegacyAgentSubject(id)}
	s.seqs = r.publish(t, s.agent, forcedMessages)
	start, err := r.consumers.Ensure(ctx, id, []string{s.agent}, s.seqs[0])
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	s.start, s.name = start, forcedName(start, id)
	pull, err := forcedPull(ctx, r.consumers, start, id)
	if err != nil {
		t.Fatalf("pull %s: %v", s.name, err)
	}
	var msgs []SessionMessage
	for range forcedMessages {
		msgs = append(msgs, next(t, pull))
	}
	for _, msg := range msgs[:forcedAcked] {
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	r.awaitStoredFloor(t, s.name, s.seqs[forcedAcked-1])
	s.held = r.holdWrites(t, s.name, s.seqs[forcedAcked])
	if err := msgs[forcedAcked].Ack(); err != nil {
		t.Fatal(err)
	}
	s.held.Reached(t)
	pull.Stop()
	return s
}

// restartUnder runs the relay's restart of s while the replacement is held at point on every host,
// releases the predecessor's held writes first and the replacement's after them, and answers the
// restart, retried once as the relay retries a failed restart.
func (r *forcedRig) restartUnder(t *testing.T, point testnats.Point, s *heldSession) SessionConsumerStart {
	t.Helper()
	held := r.HoldNewer(point, Stream, s.held.Stores)
	type answer struct {
		start SessionConsumerStart
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		start, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
		done <- answer{start, err}
	}()
	for _, hold := range held {
		t.Logf("held the replacement at %s: %s", point, hold.Reached(t))
	}
	r.ReleaseAndLand(t, s.held)
	for _, hold := range held {
		hold.Release()
	}
	select {
	case a := <-done:
		if a.err == nil {
			return a.start
		}
		t.Logf("the restart answered %v; the relay retries it", a.err)
		restarted, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
		if err != nil {
			t.Fatalf("the retried restart: %v", err)
		}
		return restarted
	case <-time.After(30 * time.Second):
		t.Fatal("the restart did not answer within 30 s of the replacement's release")
	}
	return SessionConsumerStart{}
}

// The create-error order (10012): the predecessor's write is held before its I/O, the
// replacement's creation write is held before its rename, then the predecessor's write is released
// into the directory and the replacement's rename after it. Of the same name, the replacement's
// create fails or keeps the predecessor's state; of a fresh name, the two writes touch different
// files. Every other row holds the predecessor's write before its I/O only: held before its rename,
// it does not land in a replacement of the same name, since the delete removes the temporary file
// with the consumer's directory and the rename answers ENOENT (measured on nats-server v2.15.1), so
// the rename hold is the replacement's own write's, here. The replacement must deliver every
// message after the predecessor's floor.
func TestForcedOrderARelayRestartInTheCreateErrorOrderDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		if ok, why := testnats.WritePreRename.Exists(); !ok {
			t.Skipf("nats-server %s has no %s, so no create-error order: %s", natsserver.VERSION, testnats.WritePreRename, why)
		}
		s := r.heldSession(t, "ses_forced_create_error")
		restarted := r.restartUnder(t, testnats.WritePreRename, s)
		r.restart(t)
		want := s.seqs[forcedAcked+1:]
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
		requireDelivered(t, got, want, "the restarted consumer")
	})
}

// The relay restart of a consumer with messages awaiting an ack (RestartAtFloor) replaces it at
// the sequence after its floor. The held write of the predecessor's last ack must not reach the
// replacement, which must deliver every message after that floor again.
func TestForcedOrderARelayRestartUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		s := r.heldSession(t, "ses_forced_restart")
		mark := r.Hooks.Mark()
		restarted, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if restarted.StartSeq != s.seqs[forcedAcked+1] {
			t.Fatalf("the restart starts at %d, want %d", restarted.StartSeq, s.seqs[forcedAcked+1])
		}
		r.awaitNewerWrites(t, mark, s.held.Stores)
		r.ReleaseAndLand(t, s.held)
		r.restart(t)
		want := s.seqs[forcedAcked+1:]
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
		requireDelivered(t, got, want, "the restarted consumer")
	})
}

// The predecessor's held write released after the replacement's store is made and before it loads
// any state (store.create): the replacement must load nothing of the predecessor's.
func TestForcedOrderARelayRestartWhoseReplacementLoadsAfterTheHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		s := r.heldSession(t, "ses_forced_adopt")
		restarted := r.restartUnder(t, testnats.StoreCreate, s)
		r.restart(t)
		want := s.seqs[forcedAcked+1:]
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
		requireDelivered(t, got, want, "the restarted consumer")
	})
}

// A registration after a delete (a deregistration's, the sweep's or the reaper's, which all reach
// SessionConsumers.Delete) creates the session's consumer again at the floor the delete recorded.
// The held write of the deleted consumer's last ack must not reach it.
func TestForcedOrderARegistrationAfterADeleteUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		s := r.heldSession(t, "ses_forced_reregister")
		ctx := context.Background()
		floor, err := r.consumers.Delete(ctx, s.id, nil)
		if err != nil || floor != s.seqs[forcedAcked] {
			t.Fatalf("Delete = %d, %v; want the floor %d", floor, err, s.seqs[forcedAcked])
		}
		mark := r.Hooks.Mark()
		again, err := r.consumers.Ensure(ctx, s.id, []string{s.agent}, 0)
		if err != nil {
			t.Fatalf("register again: %v", err)
		}
		if again.StartSeq != s.seqs[forcedAcked+1] {
			t.Fatalf("the registration after the delete starts at %d, want %d", again.StartSeq, s.seqs[forcedAcked+1])
		}
		r.awaitNewerWrites(t, mark, s.held.Stores)
		r.ReleaseAndLand(t, s.held)
		r.restart(t)
		want := s.seqs[forcedAcked+1:]
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, s.id) })
		requireDelivered(t, got, want, "the consumer registered after the delete")
	})
}

// A registration after the server removed the session's consumer (an inactivity removal, played by
// a raw delete of that consumer) creates it again at the start the session's own position proves.
// The removed consumer's held write must not reach the new one.
func TestForcedOrderARegistrationAfterAnOutsideRemovalUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		s := r.heldSession(t, "ses_forced_removed")
		js, err := r.conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := js.DeleteConsumer(Stream, s.name); err != nil {
			t.Fatalf("remove %s from outside: %v", s.name, err)
		}
		mark := r.Hooks.Mark()
		again, err := r.consumers.Ensure(context.Background(), s.id, []string{s.agent}, 0)
		if err != nil {
			t.Fatalf("register again: %v", err)
		}
		r.awaitNewerWrites(t, mark, s.held.Stores)
		r.ReleaseAndLand(t, s.held)
		r.restart(t)
		var want []uint64
		for _, seq := range s.seqs {
			if seq >= again.StartSeq {
				want = append(want, seq)
			}
		}
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, s.id) })
		requireDelivered(t, got, want, "the consumer registered after the removal")
	})
}

// A→B→C: the first predecessor's write is held across two replacements, and released only once
// the third consumer stands; it must reach neither replacement.
func TestForcedOrderAReplacementOfAReplacementUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	forEachTopology(t, func(t *testing.T, topo forcedTopology) {
		r := newForcedRig(t, topo)
		r.sameNameControl(t)
		s := r.heldSession(t, "ses_forced_abc")
		ctx := context.Background()
		b, err := r.consumers.RestartAtFloor(ctx, s.id, []string{s.agent})
		if err != nil {
			t.Fatalf("the first restart: %v", err)
		}
		pull, err := forcedPull(ctx, r.consumers, b, s.id)
		if err != nil {
			t.Fatal(err)
		}
		for range forcedMessages - forcedAcked - 1 {
			next(t, pull)
		}
		pull.Stop()
		waitForAckPending(t, r.consumers, s.id, forcedMessages-forcedAcked-1)
		storesB := r.stores(t, forcedName(b, s.id))
		mark := r.Hooks.Mark()
		c, err := r.consumers.RestartAtFloor(ctx, s.id, []string{s.agent})
		if err != nil {
			t.Fatalf("the second restart: %v", err)
		}
		r.awaitNewerWrites(t, mark, storesB)
		r.ReleaseAndLand(t, s.held)
		r.restart(t)
		want := s.seqs[forcedAcked+1:]
		got := delivered(t, want, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, c, s.id) })
		requireDelivered(t, got, want, "the third consumer")
	})
}
