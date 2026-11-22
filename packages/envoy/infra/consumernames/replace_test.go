package consumernames

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// replaceNotificationStream makes ENVOY_NOTIFICATIONS as `envoy-infra streams update` does
// (bus.EnsureNotificationStream, under a fresh stream identity), as the operator under a
// streams-update hold of its own, which it then releases (the hold's record holds hold set's
// entries alone, so its release is admitted): no other client's grant holds the stream's create.
func (h *stack) replaceNotificationStream() {
	h.t.Helper()
	id := h.heldHold(holdrecord.WorkflowStreamsUpdate)
	nc, _ := h.operator()
	js, err := nc.JetStream()
	require.NoError(h.t, err)
	_, err = bus.EnsureNotificationStream(context.Background(), nc, js, bus.NotificationStreamConfig(1, 0))
	require.NoError(h.t, err)
	require.NoError(h.t, h.release(id))
}

// replaceSubject is a subject of the notification stream the replace rows publish on.
const replaceSubject = "notifications.github.acme.widgets.push"

// replaceWriter connects as listener hub-a of the stack, decided under the row's phase now: the
// rows' writer of messages and acknowledgements while the row is released, which the operator's
// grant holds neither of (natsgrant.Operator). It is a writer, so h.run closes it, as every
// connection the rows open, before a held command, whose held re-read admits no other client.
func (h *stack) replaceWriter() (*nats.Conn, jetstream.JetStream) {
	h.t.Helper()
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.ListenerCredential("hub-a"))
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	h.track(nc)
	return nc, js
}

// replaceOp is a new operator connection and the selection bucket's store on it: each held
// command closes the connections before it (h.run), so a row reads again on a new one.
func (h *stack) replaceOp() (*nats.Conn, selection.Store) {
	h.t.Helper()
	nc, _ := h.operator()
	return nc, selection.NewKVStore(selection.ConnRequester{Conn: nc})
}

// replacePublish publishes n messages on replaceSubject and answers the last one's sequence.
func replacePublish(t *testing.T, js jetstream.JetStream, n int) uint64 {
	t.Helper()
	var last uint64
	for i := range n {
		ack, err := js.Publish(context.Background(), replaceSubject, []byte{byte('a' + i)})
		require.NoError(t, err)
		last = ack.Sequence
	}
	return last
}

// replaceBadConfig is a listener durable as the README's recreation found one: the listener's
// filters and window, but an idle heartbeat, which the listener refuses to bind and NATS cannot
// take away in place (cmd/listener/durable.go, listenerDurableRefusal), named name, delivering to
// deliver, carrying durable's logical name, at start by start sequence.
func replaceBadConfig(name, durable, deliver string, start uint64) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:           name,
		DeliverSubject:    deliver,
		DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:       start,
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           60 * time.Second,
		MaxAckPending:     256,
		MaxDeliver:        20,
		InactiveThreshold: 7 * 24 * time.Hour,
		FilterSubjects:    bus.StreamSubjects(),
		IdleHeartbeat:     30 * time.Second,
		Metadata:          map[string]string{selection.LogicalMetadataKey: durable},
	}
}

// replaceInfo reads consumer name of the notification stream, nil when the server does not hold it.
func replaceInfo(t *testing.T, nc *nats.Conn, name string) *jetstream.ConsumerInfo {
	t.Helper()
	info, err := consumerInfo(context.Background(), nc, notifications, name)
	require.NoError(t, err)
	return info
}

// replaceRecord reads the record of durable in the selection bucket.
func replaceRecord(t *testing.T, store selection.Store, durable string) (selection.Record, uint64) {
	t.Helper()
	record, revision, err := store.Read(context.Background(), selection.DurableKey(durable))
	require.NoError(t, err, "read %s's record", durable)
	return record, revision
}

// replaceRequireReplaced asserts the state one replace of durable leaves, whatever runs crashed
// before it, read on a new operator connection: the record selects a fresh name of durable at
// start and holds no open operation; the server holds none of the names in gone; that fresh name
// is the one consumer of durable on the stream, by start sequence at start, with no idle
// heartbeat, ack explicit, durable's logical name and filters, delivering to a new reply inbox;
// and the name it answers. Whether a gone name is still listed as retired is the selection
// library's: a predecessor whose delete the server confirmed is dropped from the retired names,
// and one whose delete failed stays for the cleanup (consumernames.Names.publish), which the
// main row asserts of its own predecessor.
func (h *stack) replaceRequireReplaced(durable string, start uint64, gone ...string) string {
	t := h.t
	t.Helper()
	nc, store := h.replaceOp()
	record, _ := replaceRecord(t, store, durable)
	require.Nil(t, record.Op, "the replace leaves no operation open on %s's record", durable)
	require.True(t, strings.HasPrefix(record.Current, durable+"~"), "%s's current %q is a fresh name of it", durable, record.Current)
	require.Equal(t, start, record.Start, "the record keeps the start %s's current was created at", durable)
	for _, name := range gone {
		require.NotEqual(t, name, record.Current)
		require.Nil(t, replaceInfo(t, nc, name), "gone %s is deleted", name)
	}
	require.Equal(t, []string{record.Current}, consumersOf(t, nc, durable), "the stream holds %s's current alone", durable)
	info := replaceInfo(t, nc, record.Current)
	require.NotNil(t, info)
	require.Equal(t, jetstream.DeliverByStartSequencePolicy, info.Config.DeliverPolicy)
	require.Equal(t, start, info.Config.OptStartSeq, "%s is created at the floor's next sequence", record.Current)
	require.Zero(t, info.Config.IdleHeartbeat, "the replace takes the idle heartbeat away")
	require.False(t, info.Config.FlowControl)
	require.Equal(t, jetstream.AckExplicitPolicy, info.Config.AckPolicy)
	require.Equal(t, durable, info.Config.Metadata[selection.LogicalMetadataKey])
	require.ElementsMatch(t, bus.StreamSubjects(), info.Config.FilterSubjects, "the replace keeps the durable's filters")
	require.True(t, strings.HasPrefix(info.Config.DeliverSubject, nats.InboxPrefix), "%s delivers to a reply inbox", record.Current)
	return record.Current
}

// replaceFirstDelivered subscribes deliver on nc and answers the stream sequence of the first
// message it receives, skipping the server's status messages (an idle heartbeat).
func replaceFirstDelivered(t *testing.T, nc *nats.Conn, deliver string) uint64 {
	t.Helper()
	sub, err := nc.SubscribeSync(deliver)
	require.NoError(t, err)
	defer sub.Unsubscribe()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		msg, err := sub.NextMsg(5 * time.Second)
		require.NoError(t, err, "a message on %s", deliver)
		if len(msg.Data) == 0 && msg.Header.Get("Status") != "" {
			continue
		}
		meta, err := msg.Metadata()
		require.NoError(t, err)
		return meta.Sequence.Stream
	}
	t.Fatalf("no message reached %s", deliver)
	return 0
}

// TestReplaceAListenerDurableAtItsFloor: `replace --listener M --hold ID`, in a consumer-cleanup
// hold, replaces the durable a listener's record selects, one whose idle heartbeat the listener
// refuses, by a fresh name at the sequence after its ack floor, retires the old name and deletes
// it; after the release the fresh name delivers from that sequence, and so replays none of the
// messages the old name's listener acknowledged. Each run writes its intent and its done entry,
// which names the name it replaced and the new current; a refused run writes its stop, recording
// that it wrote and made nothing but this hold's barrier key
// (TestAReplaceStopNamesTheBarrierKeyItPut). A rerun after a crash at each step of the replace
// (after its reservation, after its create, after its publish) ends in the same state. A floor
// past the stream's end starts at the stream's first sequence. The last rerun, with --final,
// completes the hold's record, so hold release follows. Control: a consumer created as the
// README's delete-and-let-the-listener-recreate-it does (deliver policy all) hands over the
// stream's first message, which the listener had acknowledged.
func TestReplaceAListenerDurableAtItsFloor(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	const acked = 4
	durable := bus.ListenerDurable

	// While the row is released, a listener's connection publishes ten messages and acknowledges
	// the first four of them on the main durable's current, so its ack floor is 4.
	h.replaceNotificationStream()
	wc, wjs := h.replaceWriter()
	last := replacePublish(t, wjs, 10)
	main := selection.FreshName(durable("replace-main"))
	mainDeliver := nats.NewInbox()
	sub, err := wc.SubscribeSync(mainDeliver)
	require.NoError(t, err)
	makeConsumer(t, wc, replaceBadConfig(main, durable("replace-main"), mainDeliver, 1))
	for range acked {
		msg, err := sub.NextMsg(10 * time.Second)
		require.NoError(t, err)
		require.NoError(t, msg.AckSync())
	}
	require.NoError(t, sub.Unsubscribe())
	require.Equal(t, uint64(acked), replaceInfo(t, wc, main).AckFloor.Stream, "the main durable's ack floor")

	// Control: a consumer created at deliver policy all, as the listener creates its durable after
	// the README's delete, hands over the first message again.
	control := nats.NewInbox()
	makeConsumer(t, wc, jetstream.ConsumerConfig{Durable: "replace-control", DeliverSubject: control, AckPolicy: jetstream.AckExplicitPolicy, FilterSubjects: bus.StreamSubjects()})
	require.Equal(t, uint64(1), replaceFirstDelivered(t, wc, control), "control: deliver policy all hands over the acknowledged first message")
	require.NoError(t, deleteConsumer(ctx, wc, notifications, "replace-control"))

	// The crash rows' consumers: each current at 5, so its floor is 4, and where the crash came
	// after the candidate's create, the candidate at 5 too.
	reserved := selection.FreshName(durable("replace-crash-reserve"))
	createdOld, createdNew := selection.FreshName(durable("replace-crash-create")), selection.FreshName(durable("replace-crash-create"))
	publishedOld, publishedNew := selection.FreshName(durable("replace-crash-publish")), selection.FreshName(durable("replace-crash-publish"))
	for name, logical := range map[string]string{reserved: "replace-crash-reserve", createdOld: "replace-crash-create", createdNew: "replace-crash-create",
		publishedOld: "replace-crash-publish", publishedNew: "replace-crash-publish"} {
		makeConsumer(t, wc, replaceBadConfig(name, durable(logical), nats.NewInbox(), acked+1))
	}
	// A durable an earlier build left past the stream's end: created at 1000, so its floor is 999.
	pastEnd := selection.FreshName(durable("replace-past-end"))
	makeConsumer(t, wc, replaceBadConfig(pastEnd, durable("replace-past-end"), nats.NewInbox(), 1000))
	require.Equal(t, uint64(999), replaceInfo(t, wc, pastEnd).AckFloor.Stream)
	wc.Close()

	// Refused outside a hold, naming the phase.
	code, _, stderr := h.run(actionReplace, "--hold", "no-hold", "--listener", "replace-main")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "while the hold row is held, and it is released")

	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, _ := h.operator()
	created, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	stale := time.Now().Add(-time.Minute)
	for logical, record := range map[string]selection.Record{
		"replace-main":          {Current: main, Start: 1},
		"replace-crash-reserve": {Current: reserved, Start: acked + 1, Op: &selection.Op{Kind: selection.OpReplace, Old: reserved, Candidate: selection.FreshName(durable("replace-crash-reserve")), ReservedAt: stale}},
		"replace-crash-create":  {Current: createdOld, Start: acked + 1, Op: &selection.Op{Kind: selection.OpReplace, Old: createdOld, Candidate: createdNew, ReservedAt: stale}},
		"replace-crash-publish": {Current: publishedNew, Start: acked + 1, Retired: []string{publishedOld}},
		"replace-past-end":      {Current: pastEnd, Start: 1000},
	} {
		_, err := created.Write(ctx, selection.DurableKey(durable(logical)), record, 0)
		require.NoError(t, err)
	}

	// A machine whose durable has no record is refused, naming it, and nothing is created.
	code, _, stderr = h.run(actionReplace, "--hold", id, "--listener", "replace-none")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "selects no current")
	nc, _ = h.replaceOp()
	require.Empty(t, consumersOf(t, nc, durable("replace-none")))
	stop := h.lastEntry(id, "")
	require.Equal(t, "consumer-names replace stop", stop.Command+" "+stop.State, "the refused replace ends in its stop entry")
	require.Equal(t, "this run wrote no record and made or deleted no consumer; it put "+barrierLeft, stop.Outputs["left"],
		"the refused replace's stop records that it changed nothing but the barrier key")

	code, stdout, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-main")
	require.Equal(t, 0, code)
	replaced := h.replaceRequireReplaced(durable("replace-main"), acked+1, main)
	nc, mainStore := h.replaceOp()
	mainRecord, _ := replaceRecord(t, mainStore, durable("replace-main"))
	require.NotContains(t, mainRecord.Retired, main, "a predecessor whose delete the server confirmed is dropped from the retired names")
	require.Contains(t, stdout, main+" replaced by "+replaced+" at stream sequence 5")
	require.NotEqual(t, mainDeliver, replaceInfo(t, nc, replaced).Config.DeliverSubject, "the fresh name delivers to a new inbox")
	done := h.lastEntry(id, "")
	require.Equal(t, "consumer-names replace done", done.Command+" "+done.State)
	require.Equal(t, map[string]string{"durable": durable("replace-main"), "replaced": main, "current": replaced, "start": "5"},
		map[string]string{"durable": done.Outputs["durable"], "replaced": done.Outputs["replaced"], "current": done.Outputs["current"], "start": done.Outputs["start"]},
		"the done entry names the name replaced and the new current")
	require.Equal(t, map[string]string{"listener": "replace-main"}, cleanupIntentOf(h.journal(id), "consumer-names replace").Inputs, "the intent names the listener")
	require.ErrorContains(t, h.release(id), "the record is not complete", "hold release before a run with --final")

	t.Run("a rerun after a crash after the reservation", func(t *testing.T) {
		code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-crash-reserve")
		require.Equal(t, 0, code)
		_, store := h.replaceOp()
		record, _ := replaceRecord(t, store, durable("replace-crash-reserve"))
		require.Len(t, record.Retired, 1, "the crashed run's candidate, never created, stays retired")
		require.NotContains(t, record.Retired, reserved, "the old name, whose delete the server confirmed, is dropped from the retired names")
		h.replaceRequireReplaced(durable("replace-crash-reserve"), acked+1, append([]string{reserved}, record.Retired...)...)
	})
	t.Run("a rerun after a crash after the create", func(t *testing.T) {
		code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-crash-create")
		require.Equal(t, 0, code)
		h.replaceRequireReplaced(durable("replace-crash-create"), acked+1, createdOld, createdNew)
	})
	t.Run("a rerun after a crash after the publish", func(t *testing.T) {
		nc, _ := h.replaceOp()
		require.NotNil(t, replaceInfo(t, nc, publishedOld), "the crash left the retired name on the server")
		code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-crash-publish")
		require.Equal(t, 0, code)
		h.replaceRequireReplaced(durable("replace-crash-publish"), acked+1, publishedOld, publishedNew)
	})
	t.Run("a floor past the stream's end starts at the stream's first sequence", func(t *testing.T) {
		code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-past-end")
		require.Equal(t, 0, code)
		h.replaceRequireReplaced(durable("replace-past-end"), 1, pastEnd)
	})
	t.Run("a rerun after a whole replace replaces again at the same start, and with --final completes the hold's record", func(t *testing.T) {
		code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-main", "--final")
		require.Equal(t, 0, code)
		h.replaceRequireReplaced(durable("replace-main"), acked+1, main, replaced)
		last := h.lastEntry(id, "")
		require.Equal(t, "consumer-names replace --final done", last.Command+" "+last.State)
		require.True(t, h.journal(id).Complete, "the run with --final completes the hold's record")
	})

	// After the release, the main durable's current hands over the first message its listener had
	// not acknowledged, and every message after it.
	require.NoError(t, h.release(id))
	nc, store := h.replaceOp()
	record, _ := replaceRecord(t, store, durable("replace-main"))
	deliver := replaceInfo(t, nc, record.Current).Config.DeliverSubject
	wc, _ = h.replaceWriter()
	sub, err = wc.SubscribeSync(deliver)
	require.NoError(t, err)
	for want := uint64(acked + 1); want <= last; want++ {
		msg, err := sub.NextMsg(10 * time.Second)
		require.NoError(t, err, "message %d from %s", want, record.Current)
		meta, err := msg.Metadata()
		require.NoError(t, err)
		require.Equal(t, want, meta.Sequence.Stream, "%s hands over every message from its start, in order", record.Current)
		require.NoError(t, msg.AckSync())
	}
}

// TestReplaceAListenerDurableAtAFullCap: at the stream's max_consumers the replace deletes the
// retired names the server holds and creates the fresh name in their slot; with none, it deletes
// the old name first, since its start is saved, and then creates the fresh name. The cap is never
// changed. Control: before each replace, a create of one more consumer is refused at the cap.
func TestReplaceAListenerDurableAtAFullCap(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, js := h.operator()
	config := notificationStreamConfig(t, 1)
	config.MaxConsumers = 2
	_, err := js.CreateStream(ctx, config)
	require.NoError(t, err)
	_, err = selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	requireFull := func(nc *nats.Conn) {
		t.Helper()
		_, err := writeConsumer(ctx, nc, notifications, jetstream.ConsumerConfig{Durable: "replace-one-more", AckPolicy: jetstream.AckExplicitPolicy}, actionCreateConsumer)
		var api *jetstream.APIError
		require.True(t, errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeMaximumConsumersLimit, "control: the cap refuses one more consumer, got %v", err)
	}

	// A durable whose record retires a name the server still holds: the two fill the cap.
	retiredDurable := bus.ListenerDurable("replace-cap-retired")
	old, retired := selection.FreshName(retiredDurable), selection.FreshName(retiredDurable)
	nc, store := h.replaceOp()
	makeConsumer(t, nc, replaceBadConfig(old, retiredDurable, nats.NewInbox(), 1))
	makeConsumer(t, nc, replaceBadConfig(retired, retiredDurable, nats.NewInbox(), 1))
	_, err = store.Write(ctx, selection.DurableKey(retiredDurable), selection.Record{Current: old, Start: 1, Retired: []string{retired}}, 0)
	require.NoError(t, err)
	requireFull(nc)
	code, _, _ := h.run(actionReplace, "--hold", id, "--listener", "replace-cap-retired")
	require.Equal(t, 0, code)
	kept := h.replaceRequireReplaced(retiredDurable, 1, old, retired)

	// A durable with no retired name: it and the first durable's current fill the cap.
	plainDurable := bus.ListenerDurable("replace-cap-plain")
	plain := selection.FreshName(plainDurable)
	nc, store = h.replaceOp()
	makeConsumer(t, nc, replaceBadConfig(plain, plainDurable, nats.NewInbox(), 1))
	_, err = store.Write(ctx, selection.DurableKey(plainDurable), selection.Record{Current: plain, Start: 1}, 0)
	require.NoError(t, err)
	requireFull(nc)
	code, _, _ = h.run(actionReplace, "--hold", id, "--listener", "replace-cap-plain")
	require.Equal(t, 0, code)
	h.replaceRequireReplaced(plainDurable, 1, plain)
	nc, _ = h.replaceOp()
	require.NotNil(t, replaceInfo(t, nc, kept), "the other durable's current is untouched")
	info, err := streamInfo(ctx, nc, notifications)
	require.NoError(t, err)
	require.Equal(t, 2, info.Config.MaxConsumers, "the cap is never changed")
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
	require.True(t, slices.Contains(consumersOf(t, nc, retiredDurable), kept))
}

// A replace stop counts this hold's barrier key, which the run put before it read the durable's
// record: a stop names every change its command's runs made and did not undo. A listener whose
// durable has no record makes replace refuse after its barrier ("selects no current"), creating and
// writing nothing; its stop names the key, and hold release is refused over it. Control: under the
// earlier rule that the key is no change (leftRuleSet.barrierNoChange), the same refusal records
// unchanged, and hold release is admitted over it.
func TestAReplaceStopNamesTheBarrierKeyItPut(t *testing.T) {
	h := newStack(t)
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(t.Context(), nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	release := holdrecord.Request{Command: holdrecord.CommandRelease}
	refused := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := h.run(actionReplace, "--hold", id, "--listener", "replace-none")
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "selects no current", "the hold point: the refusal follows the read behind the barrier")
		stop := h.lastEntry(id, replaceCommand)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{barrierNoChange: true})
	require.Equal(t, holdrecord.LeftUnchanged, refused().Outputs["left"], "control: with the key no change the stop records unchanged")
	require.NoError(t, holdrecord.Decide(h.journal(id), release), "control: hold release is admitted over the unchanged stop")

	leftWith(t, leftRuleSet{})
	require.Contains(t, refused().Outputs["left"], "this run wrote no record and made or deleted no consumer; it put "+barrierLeft)
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over a stop that left the key")
}

// A replace whose done entry is lost, after it replaced the durable, writes its stop, recording
// what it left as unknown, and hold abort is then the way out (requireALostDoneStops, with its
// control: the intent left pending under the earlier rule). Each run replaces the durable again
// from its record's current.
func TestAReplaceWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	h.replaceNotificationStream()
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, store := h.replaceOp()
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	durable := bus.ListenerDurable("replace-lost")
	current := selection.FreshName(durable)
	makeConsumer(t, nc, replaceBadConfig(current, durable, nats.NewInbox(), 1))
	_, err = store.Write(ctx, selection.DurableKey(durable), selection.Record{Current: current, Start: 1}, 0)
	require.NoError(t, err)
	h.requireALostDoneStops(id, replaceCommand, func() (int, string, string) {
		return h.run(actionReplace, "--hold", id, "--listener", "replace-lost")
	})
}
