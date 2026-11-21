//go:build odatrace

package bus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
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
// write lands, the held write is released and lands, and the server restarts. The row requires
// every message the replacement was created to deliver to arrive. Each hold is required reached.
// Run against the fixed build, whose sites create the same name again, every row fails.

const (
	// forcedMessages are published on a session's agent subject before its consumer is made.
	forcedMessages = 30
	// forcedAcked is how many of them the predecessor has acked when its write is held; the held
	// write carries the ack of the one after them.
	forcedAcked = 20
	// forcedWindow is how long a row reads what the replacement delivers after the restart: well
	// inside the consumers' 60 s ack wait, so a message the late state marked pending is not
	// delivered again within it.
	forcedWindow = 8 * time.Second
)

// forcedRig is a lone hooked server, the notification stream on it, SessionConsumers of the build
// under test, and a plain connection of the test's own.
type forcedRig struct {
	srv       *testnats.Server
	hooks     *testnats.Hooks
	owner     *Client
	consumers *SessionConsumers
	conn      *natsgo.Conn
}

// newForcedRig starts the rig. On a nats-server the session consumers refuse
// (natsversion.Refusal: every 2.10, v2.10.29 among them) no session consumer can exist, at this
// build or the one before it, so no creation site of the family can meet a held write there: the
// row confirms that the connection is refused for that reason, and skips. A skip is no pass.
func newForcedRig(t *testing.T) *forcedRig {
	t.Helper()
	srv, uri := testnats.Start(t)
	if refusal := natsversion.Refusal(natsserver.VERSION); refusal != nil {
		_, err := ConnectSessionConsumers([]string{uri}, 0, nil)
		if err == nil || !strings.Contains(err.Error(), refusal.Error()) {
			t.Fatalf("nats-server %s is refused by natsversion (%v), but the session consumers connected: %v", natsserver.VERSION, refusal, err)
		}
		t.Skipf("the session consumers refuse nats-server %s, so no session consumer exists to collide: %v", natsserver.VERSION, err)
	}
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	consumers := forcedSessionConsumers(t, uri, conn)
	return &forcedRig{srv: srv, hooks: testnats.Hook(t), owner: owner, consumers: consumers, conn: conn}
}

// storedState is consumer name's state as its o.dat holds it, or nil while there is none or it does
// not decode (nats-server v2.10.29 writes o.dat in place).
func (r *forcedRig) storedState(t *testing.T, name string) *natsserver.ConsumerState {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(r.srv.StoreDir(), "jetstream", "$G", "streams", Stream, "obs", name, "o.dat"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s's o.dat: %v", name, err)
	}
	state, err := natsserver.MeasureDecodeConsumerState(buf)
	if err != nil {
		return nil
	}
	return state
}

// awaitStoredFloor waits for consumer name's o.dat to hold ack floor floor.
func (r *forcedRig) awaitStoredFloor(t *testing.T, name string, floor uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		state := r.storedState(t, name)
		if state != nil && state.AckFloor.Stream == floor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's o.dat did not reach ack floor %d: %+v", name, floor, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gen is the generation of consumer name's newest store on the server.
func (r *forcedRig) gen(t *testing.T, name string) uint64 {
	t.Helper()
	events := r.hooks.Events(testnats.Point("store.loaded"), testnats.OnServer(r.srv), testnats.Consumer(name))
	if len(events) == 0 {
		t.Fatalf("the hooked server reported no store of %s", name)
	}
	return events[len(events)-1].Gen
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

// holdWrite arms the hold, at point, of the state write of consumer name's store gen that carries
// ack floor floor.
func (r *forcedRig) holdWrite(point testnats.Point, name string, gen, floor uint64) *testnats.Hold {
	return r.hooks.Hold(point, testnats.OnServer(r.srv), testnats.Consumer(name),
		testnats.Where(fmt.Sprintf("store %d's write of ack floor %d", gen, floor), func(e testnats.Event) bool {
			return e.Gen == gen && e.State != nil && e.State.AckFloor.Stream == floor
		}))
}

// awaitReplacementWrite waits for the first completed state write, after mark, of a consumer store
// of the stream newer than gen: the replacement's own write, made before the held one lands.
func (r *forcedRig) awaitReplacementWrite(t *testing.T, mark int, gen uint64) testnats.Event {
	t.Helper()
	return r.hooks.Await(t, testnats.Point("write.done"), testnats.OnServer(r.srv), testnats.Stream(Stream), testnats.After(mark),
		testnats.Where(fmt.Sprintf("a store newer than %d", gen), func(e testnats.Event) bool { return e.Gen > gen && e.Err == "" }))
}

// releaseAndLand releases hold and waits for store gen's held write to finish.
func (r *forcedRig) releaseAndLand(t *testing.T, hold *testnats.Hold, gen uint64) {
	t.Helper()
	mark := r.hooks.Mark()
	hold.Release()
	landed := r.hooks.Await(t, testnats.Point("write.done"), testnats.OnServer(r.srv), testnats.After(mark),
		testnats.Where(fmt.Sprintf("store %d", gen), func(e testnats.Event) bool { return e.Gen == gen }))
	t.Logf("the held write of store %d finished after its release: err %q, path %s", gen, landed.Err, landed.Path)
}

// restart stops the server and starts it again on its store, and waits for the stream to answer.
func (r *forcedRig) restart(t *testing.T) {
	t.Helper()
	r.srv.Stop(t)
	r.srv.Start(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := r.owner.JS().StreamInfo(Stream); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("the stream did not answer within 30 s of the restart: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// delivered reads what a pull of the consumer delivers for forcedWindow, acking each, and answers
// the stream sequences, sorted and once each. pull starts a pull; a pull that ends before the window
// does (a connection the restart replaced) is started again.
func delivered(t *testing.T, pull func(context.Context) (*SessionPull, error)) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), forcedWindow)
	defer cancel()
	var seqs []uint64
	for ctx.Err() == nil {
		p, err := pull(ctx)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for {
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
	if !slices.Equal(got, want) {
		t.Fatalf("%s delivered %v after the restart; want every message it was created to deliver, %v", what, got, want)
	}
	if len(want) == 0 {
		t.Fatalf("%s: the expected message set is empty, so the row judges nothing", what)
	}
}

// sameNameControl is every row's control: a raw consumer whose write of its 21st ack is held, a
// delete and a create of the same name at the same start, the held write released after the new
// consumer's own write, and a restart. The new consumer must lose the messages the old one had acked
// (the measured M4): otherwise the hooked server does not reproduce the collision, and no row's pass
// means anything.
func (r *forcedRig) sameNameControl(t *testing.T, point testnats.Point) {
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
	gen := r.gen(t, name)
	hold := r.holdWrite(point, name, gen, seqs[forcedAcked])
	fetchAck(1)
	t.Logf("control: held %s", hold.Reached(t))
	mark := r.hooks.Mark()
	if err := js.DeleteConsumer(Stream, name); err != nil {
		t.Fatalf("control: delete %s: %v", name, err)
	}
	if _, err := js.AddConsumer(Stream, config); err != nil {
		t.Fatalf("control: create %s again: %v", name, err)
	}
	r.awaitReplacementWrite(t, mark, gen)
	r.releaseAndLand(t, hold, gen)
	r.restart(t)
	// The test's own connection does not reconnect (testnats.Connect), so the read after the
	// restart, and every later call of the row on r.conn, dials a fresh one.
	after := testnats.Connect(t, r.srv.URL())
	t.Cleanup(after.Close)
	r.conn = after
	if js, err = after.JetStream(natsgo.MaxWait(30 * time.Second)); err != nil {
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
			meta, err := msg.Metadata()
			if err == nil {
				got = append(got, meta.Sequence.Stream)
			}
			_ = msg.Ack()
		}
	}
	slices.Sort(got)
	got = slices.Compact(got)
	if slices.Contains(got, seqs[0]) {
		t.Fatalf("control: the same-name re-creation delivered %v after the restart, its first message included: the held write did not reach it, so this server does not reproduce the collision", got)
	}
	t.Logf("control: the same-name re-creation delivered %v after the restart, losing %d..%d as measured", got, seqs[0], seqs[forcedAcked])
}

// heldSession is a session's consumer, made by the build under test, that has delivered every one of
// its forcedMessages and acked forcedAcked, with the write of its next ack held at point.
type heldSession struct {
	id, agent string
	seqs      []uint64
	start     SessionConsumerStart
	name      string
	gen       uint64
	hold      *testnats.Hold
}

func (r *forcedRig) heldSession(t *testing.T, id string, point testnats.Point) *heldSession {
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
	s.gen = r.gen(t, s.name)
	s.hold = r.holdWrite(point, s.name, s.gen, s.seqs[forcedAcked])
	if err := msgs[forcedAcked].Ack(); err != nil {
		t.Fatal(err)
	}
	t.Logf("held %s", s.hold.Reached(t))
	pull.Stop()
	return s
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
	if ok, why := testnats.WritePreRename.Exists(); !ok {
		t.Logf("nats-server %s has no %s, so no create-error order: %s", natsserver.VERSION, testnats.WritePreRename, why)
		r := newForcedRig(t)
		r.sameNameControl(t, testnats.WritePreIO)
		return
	}
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_create_error", testnats.WritePreIO)
	renaming := r.hooks.Hold(testnats.WritePreRename, testnats.OnServer(r.srv), testnats.Stream(Stream),
		testnats.Where(fmt.Sprintf("a store newer than %d", s.gen), func(e testnats.Event) bool { return e.Gen > s.gen }))
	type answer struct {
		start SessionConsumerStart
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		start, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
		done <- answer{start, err}
	}()
	t.Logf("held the replacement's creation write before its rename: %s", renaming.Reached(t))
	r.releaseAndLand(t, s.hold, s.gen)
	renaming.Release()
	var restarted SessionConsumerStart
	select {
	case a := <-done:
		if a.err != nil {
			// A create the collision failed is retried, as the relay retries a failed restart.
			t.Logf("the restart answered %v; the relay retries it", a.err)
			if restarted, a.err = r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent}); a.err != nil {
				t.Fatalf("the retried restart: %v", a.err)
			}
		} else {
			restarted = a.start
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the restart did not answer within 30 s of its write's release")
	}
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked+1:], "the restarted consumer")
}

// The relay restart of a consumer with messages awaiting an ack (RestartAtFloor) replaces it at
// the sequence after its floor. The held write of the predecessor's last ack must not reach the
// replacement, which must deliver every message after that floor again.
func TestForcedOrderARelayRestartUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_restart", testnats.WritePreIO)
	mark := r.hooks.Mark()
	restarted, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.StartSeq != s.seqs[forcedAcked+1] {
		t.Fatalf("the restart starts at %d, want %d", restarted.StartSeq, s.seqs[forcedAcked+1])
	}
	r.awaitReplacementWrite(t, mark, s.gen)
	r.releaseAndLand(t, s.hold, s.gen)
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked+1:], "the restarted consumer")
}

// The predecessor's held write released after the replacement's store is made and before it loads
// any state (store.create): the replacement must load nothing of the predecessor's.
func TestForcedOrderARelayRestartWhoseReplacementLoadsAfterTheHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_adopt", testnats.WritePreIO)
	loading := r.hooks.Hold(testnats.StoreCreate, testnats.OnServer(r.srv), testnats.Stream(Stream),
		testnats.Where(fmt.Sprintf("a store newer than %d", s.gen), func(e testnats.Event) bool { return e.Gen > s.gen }))
	type answer struct {
		start SessionConsumerStart
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		start, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
		done <- answer{start, err}
	}()
	t.Logf("held the replacement's store before it loads: %s", loading.Reached(t))
	r.releaseAndLand(t, s.hold, s.gen)
	loading.Release()
	var restarted SessionConsumerStart
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("restart: %v", a.err)
		}
		restarted = a.start
	case <-time.After(30 * time.Second):
		t.Fatal("the restart did not answer within 30 s of its store's release")
	}
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked+1:], "the restarted consumer")
}

// A registration after a delete (a deregistration's, the sweep's or the reaper's, which all reach
// SessionConsumers.Delete) creates the session's consumer again at the floor the delete recorded.
// The held write of the deleted consumer's last ack must not reach it.
func TestForcedOrderARegistrationAfterADeleteUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_reregister", testnats.WritePreIO)
	ctx := context.Background()
	floor, err := r.consumers.Delete(ctx, s.id, nil)
	if err != nil || floor != s.seqs[forcedAcked] {
		t.Fatalf("Delete = %d, %v; want the floor %d", floor, err, s.seqs[forcedAcked])
	}
	mark := r.hooks.Mark()
	again, err := r.consumers.Ensure(ctx, s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("register again: %v", err)
	}
	if again.StartSeq != s.seqs[forcedAcked+1] {
		t.Fatalf("the registration after the delete starts at %d, want %d", again.StartSeq, s.seqs[forcedAcked+1])
	}
	r.awaitReplacementWrite(t, mark, s.gen)
	r.releaseAndLand(t, s.hold, s.gen)
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked+1:], "the consumer registered after the delete")
}

// A registration after the server removed the session's consumer (an inactivity removal, played by
// a raw delete of that consumer) creates it again at the start the session's own position proves.
// The removed consumer's held write must not reach the new one.
func TestForcedOrderARegistrationAfterAnOutsideRemovalUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_removed", testnats.WritePreIO)
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteConsumer(Stream, s.name); err != nil {
		t.Fatalf("remove %s from outside: %v", s.name, err)
	}
	mark := r.hooks.Mark()
	again, err := r.consumers.Ensure(context.Background(), s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("register again: %v", err)
	}
	r.awaitReplacementWrite(t, mark, s.gen)
	r.releaseAndLand(t, s.hold, s.gen)
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, s.id) })
	var want []uint64
	for _, seq := range s.seqs {
		if seq >= again.StartSeq {
			want = append(want, seq)
		}
	}
	requireDelivered(t, got, want, "the consumer registered after the removal")
}

// A→B→C: the first predecessor's write is held across two replacements, and released only once
// the third consumer stands; it must reach neither replacement.
func TestForcedOrderAReplacementOfAReplacementUnderAHeldWriteDeliversEveryRequestedMessage(t *testing.T) {
	r := newForcedRig(t)
	r.sameNameControl(t, testnats.WritePreIO)
	s := r.heldSession(t, "ses_forced_abc", testnats.WritePreIO)
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
	genB := r.gen(t, forcedName(b, s.id))
	mark := r.hooks.Mark()
	c, err := r.consumers.RestartAtFloor(ctx, s.id, []string{s.agent})
	if err != nil {
		t.Fatalf("the second restart: %v", err)
	}
	r.awaitReplacementWrite(t, mark, genB)
	r.releaseAndLand(t, s.hold, s.gen)
	r.restart(t)
	got := delivered(t, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, c, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked+1:], "the third consumer")
}
