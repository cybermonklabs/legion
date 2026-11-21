//go:build odatrace

package consumernames

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/forcedorder"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The held-write collision through the protocol itself, on the hooked server
// (scripts/forced-order.sh), on each of the plan's topologies (forcedorder.Topologies): a family of
// this file's own (jsFamily, KVStore) whose consumer's state write is held while Names replaces it,
// or deletes it and creates it again. The package has no build before this one to run the rows
// against, so each row's control is that build's order run raw on the same servers first: a delete
// and a create of one name under the same held write, which must lose the acknowledged state's
// messages.

const (
	protocolMessages = 30
	protocolAcked    = 20
	protocolWindow   = 8 * time.Second
)

// protocolRig is hooked servers in a topology with testStream and the selection bucket.
type protocolRig struct {
	*forcedorder.Servers
	live *liveStore
}

func newProtocolRig(t *testing.T, topo forcedorder.Topology) *protocolRig {
	t.Helper()
	servers := forcedorder.Start(t, topo)
	// No boundary: a boundary would start each first create no later than it, before the row's own
	// messages.
	return &protocolRig{Servers: servers, live: newLiveStoreAt(t, servers.URL, &StoreState{Cutover: CutoverDone}, topo.Replicas)}
}

func (r *protocolRig) publish(t *testing.T, subject string) []uint64 {
	t.Helper()
	seqs := make([]uint64, protocolMessages)
	for i := range seqs {
		ack, err := r.live.js.Publish(context.Background(), subject, fmt.Appendf(nil, "m%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		seqs[i] = ack.Sequence
	}
	return seqs
}

// heldConsumer fetches every message of consumer name, acks the first protocolAcked, waits for its
// o.dat to hold that floor on every host, and holds, before its I/O, the write of the next ack.
func (r *protocolRig) heldConsumer(t *testing.T, name string, seqs []uint64) forcedorder.Held {
	t.Helper()
	ctx := context.Background()
	consumer, err := r.live.js.Consumer(ctx, testStream, name)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := consumer.Fetch(protocolMessages, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []jetstream.Msg
	for msg := range batch.Messages() {
		msgs = append(msgs, msg)
	}
	if len(msgs) != protocolMessages {
		t.Fatalf("%s delivered %d of %d", name, len(msgs), protocolMessages)
	}
	for _, msg := range msgs[:protocolAcked] {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	r.AwaitStoredFloor(t, r.live.conn, testStream, name, seqs[protocolAcked-1])
	held := r.HoldWrites(t, r.live.conn, testnats.WritePreIO, testStream, name, seqs[protocolAcked])
	if err := msgs[protocolAcked].DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	held.Reached(t)
	return held
}

// releaseAndRestart waits for the replacement's own first state write on every host, releases the
// held writes, waits for each to finish, restarts the servers, and goes on with a new store
// connection, since the restart closed the old one and it does not reconnect.
func (r *protocolRig) releaseAndRestart(t *testing.T, mark int, held forcedorder.Held) {
	t.Helper()
	r.AwaitNewerWrites(t, mark, testStream, held.Stores)
	r.ReleaseAndLand(t, held)
	r.Restart(t, testStream)
	r.live = reconnectLiveStore(t, r.URL)
}

// delivered reads consumer name for protocolWindow, acking each, until it has every one of until,
// and answers the stream sequences, sorted and once each.
func (r *protocolRig) delivered(t *testing.T, name string, until []uint64) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), protocolWindow)
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
		consumer, err := r.live.js.Consumer(ctx, testStream, name)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		batch, err := consumer.Fetch(protocolMessages, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			continue
		}
		for msg := range batch.Messages() {
			if meta, err := msg.Metadata(); err == nil {
				seqs = append(seqs, meta.Sequence.Stream)
			}
			_ = msg.Ack()
		}
	}
	slices.Sort(seqs)
	return slices.Compact(seqs)
}

// sameNameControl runs the order of the build before this package raw: a delete and a create of one
// name at the sequence after the floor, under the held write. After the restart the consumer must
// deliver none of what it was created to deliver within the window.
func (r *protocolRig) sameNameControl(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	subject := "names.control"
	seqs := r.publish(t, subject)
	name := "control"
	config := jetstream.ConsumerConfig{Durable: name, AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Minute, FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: seqs[0]}
	if _, err := r.live.js.CreateConsumer(ctx, testStream, config); err != nil {
		t.Fatal(err)
	}
	held := r.heldConsumer(t, name, seqs)
	mark := r.Hooks.Mark()
	if err := r.live.js.DeleteConsumer(ctx, testStream, name); err != nil {
		t.Fatal(err)
	}
	config.OptStartSeq = seqs[protocolAcked+1]
	if _, err := r.live.js.CreateConsumer(ctx, testStream, config); err != nil {
		t.Fatal(err)
	}
	r.releaseAndRestart(t, mark, held)
	got := r.delivered(t, name, seqs[protocolAcked+1:])
	if slices.Contains(got, seqs[protocolAcked+1]) {
		t.Fatalf("control: the same-name re-creation delivered %v after the restart: the held write did not reach it, so this topology does not reproduce the collision", got)
	}
	t.Logf("control: the same-name re-creation delivered %v after the restart, none of %d..%d", got, seqs[protocolAcked+1], seqs[protocolMessages-1])
}

// reconnectLiveStore is a liveStore on a new connection to servers that already hold the stream
// and the bucket.
func reconnectLiveStore(t *testing.T, uri string) *liveStore {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return &liveStore{conn: conn, js: js, store: NewKVStore(SessionBucket, ConnRequester{conn}), family: jsFamily{js}}
}

func requireEvery(t *testing.T, got, want []uint64, what string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected message set is empty, so the row judges nothing", what)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s delivered %v after the restart; want every message it was created to deliver, %v", what, got, want)
	}
}

// Replace through the protocol under the predecessor's held write: the replacement, a name never
// made before, delivers every message after the predecessor's floor.
func TestForcedOrderAReplacementThroughTheProtocolUnderAHeldWriteDeliversEveryMessage(t *testing.T) {
	for _, topo := range forcedorder.Topologies {
		t.Run(topo.Name, func(t *testing.T) {
			r := newProtocolRig(t, topo)
			r.sameNameControl(t)
			ctx := context.Background()
			seqs := r.publish(t, "names.replace")
			names := r.live.names("p1")
			sel, _, err := names.Ensure(ctx, "ses-replace", CreateRequest{Create: r.live.family.create("ses-replace"), First: first(seqs[0])})
			if err != nil {
				t.Fatal(err)
			}
			held := r.heldConsumer(t, sel.Record.Current, seqs)
			sel, err = names.Select(ctx, "ses-replace", nil)
			if err != nil {
				t.Fatal(err)
			}
			mark := r.Hooks.Mark()
			replaced, err := names.Replace(ctx, sel, seqs[protocolAcked+1], r.live.family.create("ses-replace"))
			if err != nil {
				t.Fatalf("Replace: %v", err)
			}
			r.releaseAndRestart(t, mark, held)
			want := seqs[protocolAcked+1:]
			requireEvery(t, r.delivered(t, replaced.Record.Current, want), want, "the replacement")
		})
	}
}

// Delete through the protocol, then Ensure, under the deleted consumer's held write: the consumer
// created again at the start the delete saved delivers every message after the floor.
func TestForcedOrderADeleteThenEnsureThroughTheProtocolUnderAHeldWriteDeliversEveryMessage(t *testing.T) {
	for _, topo := range forcedorder.Topologies {
		t.Run(topo.Name, func(t *testing.T) {
			r := newProtocolRig(t, topo)
			r.sameNameControl(t)
			ctx := context.Background()
			seqs := r.publish(t, "names.delete")
			names := r.live.names("p1")
			sel, _, err := names.Ensure(ctx, "ses-delete", CreateRequest{Create: r.live.family.create("ses-delete"), First: first(seqs[0])})
			if err != nil {
				t.Fatal(err)
			}
			held := r.heldConsumer(t, sel.Record.Current, seqs)
			floor, err := names.Delete(ctx, "ses-delete", nil)
			if err != nil || floor != seqs[protocolAcked] {
				t.Fatalf("Delete = %d, %v; want the floor %d", floor, err, seqs[protocolAcked])
			}
			mark := r.Hooks.Mark()
			again, _, err := names.Ensure(ctx, "ses-delete", CreateRequest{Create: r.live.family.create("ses-delete"), First: first(seqs[0])})
			if err != nil {
				t.Fatal(err)
			}
			r.releaseAndRestart(t, mark, held)
			want := seqs[protocolAcked+1:]
			requireEvery(t, r.delivered(t, again.Record.Current, want), want, "the consumer created again")
		})
	}
}
