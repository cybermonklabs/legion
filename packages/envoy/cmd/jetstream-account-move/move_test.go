package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/cmd/jetstream-account-move/internal/accounthub"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// testdata is where packages/envoy/infra/core keeps the goldens of its render, which the rehearsal's
// hubs run (accounthub).
const testdata = "../../infra/core/testdata"

// The rehearsal's stream identity, session and durables.
const (
	streamID      = "rehearsal-stream-id"
	session       = "s1"
	daemonDurable = "legion-go-PUB-dispatch"
	eventSubject  = "notifications.dispatch.issue.PUB."
	ackWait       = time.Second
)

// rehearsalGrants are the clients' grants at the stand-in auth service, each natsgrant's own, and
// the operator's during the move, everything.
func rehearsalGrants() map[string]natsgrant.Permissions {
	return map[string]natsgrant.Permissions{
		"operator": accounthub.Everything(),
		"dispatch": natsgrant.Dispatch("m1"),
		"listener": natsgrant.Listener("m1"),
		"daemon":   natsgrant.Daemon(),
	}
}

// rehearsal is one move on a test hub: its keys and store, the hub of the layout it is at, the
// files its steps read, and Dispatch's outbox.
type rehearsal struct {
	t         *testing.T
	keys      accounthub.Keys
	store     string
	hub       *accounthub.Hub
	files     map[string]string
	record    string
	snapshots string
	outbox    *outbox
}

func newRehearsal(t *testing.T) *rehearsal {
	t.Helper()
	keys, err := accounthub.NewKeys()
	require.NoError(t, err)
	dir := t.TempDir()
	r := &rehearsal{t: t, keys: keys, store: filepath.Join(dir, "jetstream"), files: map[string]string{},
		record: filepath.Join(dir, "fence-record.json"), snapshots: filepath.Join(dir, "snapshots"), outbox: &outbox{}}
	for name, content := range map[string]string{"operator-token": "operator", "listener-token": "listener", "jetstream-operator-seed": keys.OperatorSeed} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		r.files[name] = path
	}
	t.Cleanup(func() {
		if r.hub != nil {
			r.hub.Stop()
		}
	})
	return r
}

// render stops the hub at the layout before, if any, and starts it on layout, its stand-in auth
// service placing each client it admits in place.
func (r *rehearsal) render(layout accounthub.Layout, place string) {
	r.t.Helper()
	if r.hub != nil {
		r.hub.Stop()
	}
	hub, err := accounthub.Start(testdata, layout, r.keys, r.store, place, rehearsalGrants())
	require.NoError(r.t, err)
	r.hub = hub
}

// connect admits a client under token.
func (r *rehearsal) connect(token string) *nats.Conn {
	r.t.Helper()
	nc, err := accounthub.ConnectToken(r.hub.URL, token)
	require.NoError(r.t, err, "the stand-in auth service did not admit %s", token)
	r.t.Cleanup(nc.Close)
	return nc
}

// js is token's JetStream context.
func (r *rehearsal) js(token string) nats.JetStreamContext {
	r.t.Helper()
	js, err := r.connect(token).JetStream()
	require.NoError(r.t, err)
	return js
}

// jetstreamJS is the JETSTREAM operator's JetStream context, by its seed: the account's own reads.
func (r *rehearsal) jetstreamJS() nats.JetStreamContext {
	r.t.Helper()
	nc, err := accounthub.ConnectSeed(r.hub.URL, r.keys.OperatorSeed)
	require.NoError(r.t, err)
	r.t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	require.NoError(r.t, err)
	return js
}

// step runs one step of the command against the hub, as identity: the operator through the auth
// service (its token), the JETSTREAM operator by its seed, or a listener. It returns what the step
// printed and its exit code.
func (r *rehearsal) step(name, identity string, extra ...string) (string, int) {
	r.t.Helper()
	args := []string{name, "--servers", r.hub.URL, "--record", r.record}
	switch identity {
	case "operator":
		args = append(args, "--token-file", r.files["operator-token"])
	case "listener":
		args = append(args, "--token-file", r.files["listener-token"])
	case "jetstream":
		args = append(args, "--nkey-seed-file", r.files["jetstream-operator-seed"])
	}
	if name == "snapshot" || name == "restore" || name == "rollback" {
		args = append(args, "--dir", r.snapshots)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append(args, extra...), &stdout, &stderr)
	r.t.Logf("jetstream-account-move %s (exit %d):\n%s%s", name, code, stdout.String(), stderr.String())
	return stdout.String(), code
}

// requireStep runs a step and requires its verdict word as its last line, and exit 0.
func (r *rehearsal) requireStep(name, identity, word string) string {
	r.t.Helper()
	printed, code := r.step(name, identity)
	require.Equal(r.t, exitOK, code, "%s: %s", name, printed)
	require.Equal(r.t, word, lastLine(printed))
	return printed
}

func lastLine(printed string) string {
	lines := strings.Split(strings.TrimRight(printed, "\n"), "\n")
	return lines[len(lines)-1]
}

// event is one of Dispatch's events: its id, the dedupe key every reader holds, and its subject.
type event struct{ id, subject string }

func eventN(n int) event {
	return event{id: fmt.Sprintf("E%d", n), subject: fmt.Sprintf("%s%d.comment.created", eventSubject, n)}
}

// outbox stands in for Dispatch's outbox (internal/dispatch/outbox), which needs a database the
// check's run does not give: it publishes each event to the stream under its id as Nats-Msg-Id,
// keeps an event whose publish was refused or not acknowledged, and sends every kept event again,
// in order, until the stream acknowledges it. It never drops one, as the outbox dead-letters no
// publish failure (internal/dispatch/retry's note); the pacing between attempts is the outbox's
// (retry.Delay) and is not what the rehearsal holds.
type outbox struct {
	pending []event
}

// send publishes e once, keeping it when the stream refuses it, and returns the refusal.
func (o *outbox) send(js nats.JetStreamContext, e event) error {
	if _, err := js.Publish(e.subject, []byte(`{"event_id":"`+e.id+`"}`), nats.MsgId(e.id)); err != nil {
		o.pending = append(o.pending, e)
		return err
	}
	return nil
}

// sendAckLost publishes e with its acknowledgement aimed where no one listens: the stream stores
// it, and the outbox, which saw no acknowledgement, keeps it to send again.
func (o *outbox) sendAckLost(t *testing.T, nc *nats.Conn, e event) {
	t.Helper()
	msg := &nats.Msg{Subject: e.subject, Reply: "_INBOX.ack-lost." + e.id, Data: []byte(`{"event_id":"` + e.id + `"}`), Header: nats.Header{nats.MsgIdHdr: []string{e.id}}}
	require.NoError(t, nc.PublishMsg(msg))
	require.NoError(t, nc.Flush())
	o.pending = append(o.pending, e)
}

// drain sends every kept event again, in order, each until the stream acknowledges it.
func (o *outbox) drain(t *testing.T, js nats.JetStreamContext) {
	t.Helper()
	kept := o.pending
	o.pending = nil
	for _, e := range kept {
		testwait.Eventually(t, "the stream acknowledging "+e.id, func() bool {
			_, err := js.Publish(e.subject, []byte(`{"event_id":"`+e.id+`"}`), nats.MsgId(e.id))
			return err == nil
		})
	}
}

// reader reads a pull consumer as one of the rehearsal's readers does, acknowledging each message:
// redelived counts each delivery the server counts as a repeat, and applied holds each event once,
// a copy of an event already applied being acknowledged and dropped, as the daemon's
// processed_events and the listener's dedupe cache (cmd/listener/delivery.go) drop one.
type reader struct {
	applied   map[string]int
	redelived map[string]int
}

func newReader() *reader {
	return &reader{applied: map[string]int{}, redelived: map[string]int{}}
}

// read fetches from the consumer until every event of want has been applied, acknowledging each
// message but those named in hold, which it leaves unacknowledged; then it requires the consumer to
// hold nothing more, pending or unacknowledged.
func (rd *reader) read(t *testing.T, js nats.JetStreamContext, fetch fetchFunc, stream, name string, want []string, hold ...string) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("%s applying %v", name, want), func() bool {
		for _, d := range fetch() {
			if d.repeat {
				rd.redelived[d.id]++
			}
			if slices.Contains(hold, d.id) {
				continue
			}
			rd.applied[d.id] = 1
			require.NoError(t, d.ack())
		}
		for _, id := range want {
			if rd.applied[id] == 0 && !slices.Contains(hold, id) {
				return false
			}
		}
		return true
	})
	if len(hold) > 0 {
		return
	}
	testwait.Eventually(t, name+" holding nothing more", func() bool {
		info, err := js.ConsumerInfo(stream, name)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
}

// once requires every event of want applied exactly once by rd, naming each that was not.
func (rd *reader) once(t *testing.T, who string, want []string) {
	t.Helper()
	var wrong []string
	for _, id := range want {
		if rd.applied[id] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s applied %d times", id, rd.applied[id]))
		}
	}
	require.Empty(t, wrong, "%s", who)
}

// delivery is one message a consumer handed a reader: its event, whether the server counts it a
// repeat, and its acknowledgement, which waits for the server's confirmation.
type delivery struct {
	id     string
	repeat bool
	ack    func() error
}

type fetchFunc func() []delivery

// pull reads the durable name as token, size messages a fetch, through nats.go's jetstream API,
// which binds a consumer of several filters (a session consumer's) by its name alone.
func (r *rehearsal) pull(token, name string, size ...int) fetchFunc {
	r.t.Helper()
	js, err := jetstream.New(r.connect(token))
	require.NoError(r.t, err)
	consumer, err := js.Consumer(context.Background(), bus.Stream, name)
	require.NoError(r.t, err)
	batchSize := 10
	if len(size) > 0 {
		batchSize = size[0]
	}
	return func() []delivery {
		batch, err := consumer.Fetch(batchSize, jetstream.FetchMaxWait(200*time.Millisecond))
		if err != nil {
			return nil
		}
		var out []delivery
		for m := range batch.Messages() {
			var e struct {
				ID string `json:"event_id"`
			}
			_ = json.Unmarshal(m.Data(), &e)
			meta, err := m.Metadata()
			out = append(out, delivery{id: e.ID, repeat: err == nil && meta.NumDelivered > 1,
				ack: func() error { return m.DoubleAck(context.Background()) }})
		}
		return out
	}
}

// storedEvents is every event id the stream holds on the dispatch subjects, read message by
// message, with how many copies of each.
func storedEvents(t *testing.T, js nats.JetStreamContext) map[string]int {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	require.NoError(t, err)
	found := map[string]int{}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		m, err := js.GetMsg(bus.Stream, seq)
		if err != nil {
			continue
		}
		if strings.HasPrefix(m.Subject, eventSubject) {
			var e struct {
				ID string `json:"event_id"`
			}
			_ = json.Unmarshal(m.Data, &e)
			found[e.ID]++
		}
	}
	return found
}

// missing names each event of want the stream does not hold.
func missing(t *testing.T, js nats.JetStreamContext, want []string) []string {
	t.Helper()
	held := storedEvents(t, js)
	var gone []string
	for _, id := range want {
		if held[id] == 0 {
			gone = append(gone, id)
		}
	}
	return gone
}

// setUp makes, as the operator in $G on the merge base's render, the notification stream with its
// identity, the interest and session buckets with a key each, the listener's and the daemon's
// durables, and the session's consumer through bus.SessionConsumers, the listener's own call.
func (r *rehearsal) setUp() *bus.SessionConsumers {
	r.t.Helper()
	t := r.t
	js := r.js("operator")
	_, err := js.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects(), Duplicates: 2 * time.Minute,
		Metadata: map[string]string{bus.StreamIDMetadataKey: streamID}})
	require.NoError(t, err)
	for bucket, value := range map[string]string{bus.InterestBucket: `{"topics":["notifications.dispatch.issue.PUB.>"]}`, bus.SessionBucket: `{"session":"s1"}`} {
		kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
		require.NoError(t, err)
		_, err = kv.Put(session, []byte(value))
		require.NoError(t, err)
	}
	listener := r.js("listener")
	_, err = listener.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: bus.ListenerDurable("m1"), FilterSubject: "notifications.dispatch.>", AckPolicy: nats.AckExplicitPolicy, AckWait: ackWait})
	require.NoError(t, err)
	_, err = r.js("daemon").AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: daemonDurable, FilterSubject: eventSubject + ">", AckPolicy: nats.AckExplicitPolicy, AckWait: ackWait})
	require.NoError(t, err)
	consumers := r.sessionConsumers()
	_, err = consumers.Ensure(context.Background(), session, []string{eventSubject + ">"}, 0)
	require.NoError(t, err)
	return consumers
}

// sessionConsumers connects bus.SessionConsumers as the listener, through the auth service with the
// listener's token (NATS_SERVICE_TOKEN_FILE).
func (r *rehearsal) sessionConsumers() *bus.SessionConsumers {
	r.t.Helper()
	env := map[string]string{bus.ServiceTokenFileEnvVar: r.files["listener-token"]}
	consumers, err := bus.ConnectSessionConsumers([]string{r.hub.URL}, time.Hour, nil, bus.WithEnvironment(func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}))
	require.NoError(r.t, err)
	r.t.Cleanup(consumers.Close)
	return consumers
}

// sessionName is the session's consumer.
func sessionName(t *testing.T) string {
	t.Helper()
	name, ok := bus.SessionConsumerName(session)
	require.True(t, ok)
	return name
}

// before runs the rehearsal up to the fence on the merge base's render: E1 to E3 published and
// read, the listener holding E2 delivered and unacknowledged, the session offline after E1, and E4
// stored with its acknowledgement lost, so the outbox keeps it.
func (r *rehearsal) before() (listener, daemon, sess *reader, sessionFloor uint64) {
	r.t.Helper()
	t := r.t
	r.render(accounthub.MergeBase, accounthub.GlobalAccount)
	r.setUp()
	dispatch := r.js("dispatch")
	for n := 1; n <= 3; n++ {
		require.NoError(t, r.outbox.send(dispatch, eventN(n)))
	}
	listener, daemon, sess = newReader(), newReader(), newReader()
	ljs := r.js("listener")
	listener.read(t, ljs, r.pull("listener", bus.ListenerDurable("m1")), bus.Stream, bus.ListenerDurable("m1"), []string{"E1", "E3"}, "E2")
	djs := r.js("daemon")
	daemon.read(t, djs, r.pull("daemon", daemonDurable), bus.Stream, daemonDurable, []string{"E1", "E2", "E3"})
	sjs := r.js("listener")
	var first []delivery
	sessionRead := r.pull("listener", sessionName(t), 1)
	testwait.Eventually(t, "the session's first delivery", func() bool {
		first = sessionRead()
		return len(first) > 0
	})
	require.Len(t, first, 1)
	require.Equal(t, "E1", first[0].id)
	require.NoError(t, first[0].ack())
	sess.applied["E1"] = 1
	info, err := sjs.ConsumerInfo(bus.Stream, sessionName(t))
	require.NoError(t, err)
	sessionFloor = info.AckFloor.Stream

	ops := r.js("operator")
	last, err := ops.StreamInfo(bus.Stream)
	require.NoError(t, err)
	r.outbox.sendAckLost(t, r.connect("dispatch"), eventN(4))
	testwait.Eventually(t, "the stream storing E4, whose acknowledgement was lost", func() bool {
		info, err := ops.StreamInfo(bus.Stream)
		return err == nil && info.State.LastSeq == last.State.LastSeq+1
	})
	return listener, daemon, sess, sessionFloor
}

// moveUpToRestore fences, refuses E5 at the seal, snapshots, renders the accounts layout with the
// broker still placing clients in $G, refuses E6 there, reads the layout, and restores: the move's
// steps (1) to (4).
func (r *rehearsal) moveUpToRestore() {
	r.t.Helper()
	t := r.t
	r.requireStep("fence", "operator", "FENCED")
	err := r.outbox.send(r.js("dispatch"), eventN(5))
	require.ErrorContains(t, err, "sealed", "a publish after the fence was not refused by the seal")
	r.requireStep("snapshot", "operator", "SNAPSHOT TAKEN")
	r.render(accounthub.Accounts, accounthub.GlobalAccount)
	err = r.outbox.send(r.js("dispatch"), eventN(6))
	require.Error(t, err, "a publish between the layout and the switch, in $G where JetStream is not, was taken")
	r.requireStep("layout", "jetstream", "LAYOUT APPLIED")
	r.requireStep("restore", "jetstream", "MOVED")
}

var allEvents = []string{"E1", "E2", "E3", "E4", "E5", "E6"}

// after holds every reader to every event once, after the switch or the rollback: the outbox's
// kept events (E4, E5, E6) stored, the daemon applying each event once though E4 is stored twice,
// the listener getting E2 again once, the offline session reading from its ack floor with no gap
// and its stream identity unchanged, every KV key its value, and a new publish and KV put taken.
func (r *rehearsal) after(listener, daemon, sess *reader, sessionFloor uint64) {
	r.t.Helper()
	t := r.t
	r.outbox.drain(t, r.js("dispatch"))
	require.Empty(t, r.outbox.pending)

	djs := r.js("daemon")
	daemon.read(t, djs, r.pull("daemon", daemonDurable), bus.Stream, daemonDurable, allEvents)
	daemon.once(t, "the daemon", allEvents)

	ljs := r.js("listener")
	listener.read(t, ljs, r.pull("listener", bus.ListenerDurable("m1")), bus.Stream, bus.ListenerDurable("m1"), allEvents)
	listener.once(t, "the listener", allEvents)
	require.Equal(t, 1, listener.redelived["E2"], "the listener's delivered, unacknowledged E2 was delivered again %d times", listener.redelived["E2"])

	consumers := r.sessionConsumers()
	id, err := consumers.StreamID(context.Background())
	require.NoError(t, err)
	require.Equal(t, streamID, id, "the stream's identity changed, so every session's cursor reads as another stream's")
	start, err := consumers.Ensure(context.Background(), session, []string{eventSubject + ">"}, sessionFloor+1)
	require.NoError(t, err)
	require.False(t, start.Gap, "the session reads a gap")
	require.Equal(t, sessionFloor, start.AckFloor, "the session's consumer lost its ack floor")
	sjs := r.js("listener")
	sess.read(t, sjs, r.pull("listener", sessionName(t)), bus.Stream, sessionName(t), allEvents)
	sess.once(t, "the session", allEvents)

	ops := r.js("listener")
	for bucket, value := range map[string]string{bus.InterestBucket: `{"topics":["notifications.dispatch.issue.PUB.>"]}`, bus.SessionBucket: `{"session":"s1"}`} {
		kv, err := ops.KeyValue(bucket)
		require.NoError(t, err)
		entry, err := kv.Get(session)
		require.NoError(t, err, "%s lost %s", bucket, session)
		require.Equal(t, value, string(entry.Value()))
		_, err = kv.Put("after", []byte("new"))
		require.NoError(t, err, "a new KV put to %s", bucket)
	}
	ack, err := r.js("dispatch").Publish(eventSubject+"7.comment.created", []byte(`{"event_id":"E7"}`))
	require.NoError(t, err, "a new publish")
	info, err := ops.StreamInfo(bus.Stream)
	require.NoError(t, err)
	require.Equal(t, info.State.LastSeq, ack.Sequence)
	require.Equal(t, uint64(1), info.State.FirstSeq, "the stream's sequences moved")
}

// TestTheMoveLosesNothing (t-c-29's rehearsal): on a test hub running core's render, the move's
// fence, snapshot, layout, restore and switch, with Dispatch's outbox, a listener, a daemon and a
// session reading through it, lose nothing and deliver nothing twice:
//   - a Dispatch event refused at the seal, during the snapshot (E5), and one refused between the
//     layout and the switch (E6) each reach the daemon and the session exactly once after it;
//   - an event stored at the seal whose acknowledgement was lost (E4) is sent again after it, is
//     stored a second time, and is applied once by the daemon and delivered once to the session;
//   - the offline session's durable reads every event after its ack floor, with no gap and the
//     stream's identity unchanged, and the listener's delivered, unacknowledged message is
//     delivered again once;
//   - every KV key holds its value, and a new publish and KV put are taken.
//
// Control, in the same test: a restore given a snapshot that omits one message (E3), as a write
// that lands after an unfenced snapshot leaves it; the restore's verify prints GATE SHUT naming
// ENVOY_NOTIFICATIONS, and the event check names E3 as missing.
func TestTheMoveLosesNothing(t *testing.T) {
	t.Run("the move", func(t *testing.T) {
		r := newRehearsal(t)
		listener, daemon, sess, floor := r.before()
		r.moveUpToRestore()
		r.hub.Place(accounthub.ClientAccount)
		r.requireStep("switch", "listener", "SWITCHED")
		require.Empty(t, missing(t, r.js("listener"), []string{"E1", "E2", "E3", "E4"}), "the move lost an event stored before the fence")
		r.after(listener, daemon, sess, floor)
	})

	t.Run("control: a snapshot that omits one message", func(t *testing.T) {
		r := newRehearsal(t)
		r.render(accounthub.MergeBase, accounthub.GlobalAccount)
		r.setUp()
		dispatch := r.js("dispatch")
		for n := 1; n <= 2; n++ {
			require.NoError(t, r.outbox.send(dispatch, eventN(n)))
		}
		early := filepath.Join(t.TempDir(), "early")
		ops := r.connect("operator")
		_, _, err := takeSnapshot(context.Background(), ops, bus.Stream, early)
		require.NoError(t, err)
		require.NoError(t, r.outbox.send(dispatch, eventN(3)))
		r.requireStep("fence", "operator", "FENCED")
		r.requireStep("snapshot", "operator", "SNAPSHOT TAKEN")
		require.NoError(t, os.RemoveAll(filepath.Join(r.snapshots, bus.Stream)))
		require.NoError(t, os.Rename(filepath.Join(early, bus.Stream), filepath.Join(r.snapshots, bus.Stream)))
		r.render(accounthub.Accounts, accounthub.GlobalAccount)
		r.requireStep("layout", "jetstream", "LAYOUT APPLIED")
		printed, code := r.step("restore", "jetstream")
		require.Equal(t, exitShut, code)
		require.Equal(t, "GATE SHUT", lastLine(printed))
		require.Contains(t, printed, "GATE SHUT "+bus.Stream+": ")
		require.Equal(t, []string{"E3"}, missing(t, r.jetstreamJS(), []string{"E1", "E2", "E3"}),
			"the event check names the event the snapshot omitted")
	})
}

// TestTheMoveRollsBack: after the move's steps (1) to (4), the rollback renders the merge base
// again, finds every $G stream and bucket still sealed (nats-server refuses to lift a seal),
// deletes each and restores it from the move's own snapshot with the seal lifted, and verifies it
// against the fence record; then every event and KV key is there once, the session reads no gap,
// a new publish and a new KV put each succeed, and Dispatch's events refused during the move (E5,
// E6) each reach the daemon and the session once.
//
// Control: a rollback from a snapshot that omits one message (E3); its verify prints GATE SHUT
// naming ENVOY_NOTIFICATIONS, and the event check names E3.
func TestTheMoveRollsBack(t *testing.T) {
	t.Run("the rollback", func(t *testing.T) {
		r := newRehearsal(t)
		listener, daemon, sess, floor := r.before()
		r.moveUpToRestore()
		r.render(accounthub.MergeBase, accounthub.GlobalAccount)
		printed := r.requireStep("rollback", "operator", "ROLLED BACK")
		require.Contains(t, printed, "DELETED "+bus.Stream+", sealed as the fence left it")
		require.Empty(t, missing(t, r.js("operator"), []string{"E1", "E2", "E3", "E4"}), "the rollback lost an event stored before the fence")
		r.after(listener, daemon, sess, floor)
	})

	t.Run("control: a rollback from a snapshot that omits one message", func(t *testing.T) {
		r := newRehearsal(t)
		r.render(accounthub.MergeBase, accounthub.GlobalAccount)
		r.setUp()
		dispatch := r.js("dispatch")
		for n := 1; n <= 2; n++ {
			require.NoError(t, r.outbox.send(dispatch, eventN(n)))
		}
		early := filepath.Join(t.TempDir(), "early")
		_, _, err := takeSnapshot(context.Background(), r.connect("operator"), bus.Stream, early)
		require.NoError(t, err)
		require.NoError(t, r.outbox.send(dispatch, eventN(3)))
		r.requireStep("fence", "operator", "FENCED")
		r.requireStep("snapshot", "operator", "SNAPSHOT TAKEN")
		require.NoError(t, os.RemoveAll(filepath.Join(r.snapshots, bus.Stream)))
		require.NoError(t, os.Rename(filepath.Join(early, bus.Stream), filepath.Join(r.snapshots, bus.Stream)))
		r.render(accounthub.Accounts, accounthub.GlobalAccount)
		r.render(accounthub.MergeBase, accounthub.GlobalAccount)
		printed, code := r.step("rollback", "operator")
		require.Equal(t, exitShut, code)
		require.Equal(t, "GATE SHUT", lastLine(printed))
		require.Contains(t, printed, "GATE SHUT "+bus.Stream+": ")
		require.Equal(t, []string{"E3"}, missing(t, r.js("operator"), []string{"E1", "E2", "E3"}),
			"the event check names the event the snapshot omitted")
	})
}
