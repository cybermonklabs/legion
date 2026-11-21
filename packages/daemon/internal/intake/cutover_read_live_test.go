package intake

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/natsgrant"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// cutoverRig is a server that admits an admin, a reader with the in-cluster listener's grant
// (natsgrant.ListenerHost), and a reader refused the stream's consumer list, with the notification
// stream made.
type cutoverRig struct {
	admin   jetstream.JetStream
	stream  jetstream.Stream
	reader  *CutoverReader
	refused *CutoverReader
}

func newCutoverRig(t *testing.T) *cutoverRig {
	t.Helper()
	listenerSeed, listenerPublic := testnkeys.User(t)
	refusedSeed, refusedPublic := testnkeys.User(t)
	adminSeed, adminPublic := testnkeys.User(t)
	_, uri := testnats.StartNkeyAuthorizedUsers(t, testnats.Image,
		testnats.NkeyUser{Public: listenerPublic, Permissions: grantConfig(t, natsgrant.ListenerHost("gke"))},
		testnats.NkeyUser{Public: refusedPublic, Permissions: `{ publish: { deny: ["$JS.API.CONSUMER.LIST.>"] } }`},
		testnats.NkeyUser{Public: adminPublic})
	jsOf := func(seed, who string) jetstream.JetStream {
		conn, err := natsauth.Connect([]string{uri}, seed, t.Name()+" "+who)
		if err != nil {
			t.Fatalf("connect %s: %v", who, err)
		}
		t.Cleanup(conn.Close)
		js, err := jetstream.New(conn)
		if err != nil {
			t.Fatalf("open %s's JetStream: %v", who, err)
		}
		return js
	}
	admin := jsOf(adminSeed, "admin")
	stream, err := admin.CreateStream(context.Background(), jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}})
	if err != nil {
		t.Fatalf("create the notification stream: %v", err)
	}
	return &cutoverRig{admin: admin, stream: stream,
		reader: NewCutoverReader(jsOf(listenerSeed, "the listener-grant reader")), refused: NewCutoverReader(jsOf(refusedSeed, "the refused reader"))}
}

// durable creates name on filters, as a daemon's boot left it.
func (r *cutoverRig) durable(t *testing.T, name string, filters ...string) {
	t.Helper()
	config := jetstream.ConsumerConfig{Durable: name, FilterSubjects: filters, AckPolicy: jetstream.AckExplicitPolicy}
	if len(filters) == 1 {
		config.FilterSubject, config.FilterSubjects = filters[0], nil
	}
	if _, err := r.stream.CreateOrUpdateConsumer(context.Background(), config); err != nil {
		t.Fatalf("create or update %s on %v: %v", name, filters, err)
	}
}

// acknowledge has name acknowledge n messages, as its daemon applies them.
func (r *cutoverRig) acknowledge(t *testing.T, name string, n int) {
	t.Helper()
	handle, err := r.stream.Consumer(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := handle.Fetch(n, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for message := range batch.Messages() {
		if err := message.DoubleAck(context.Background()); err != nil {
			t.Fatalf("acknowledge %s's message: %v", name, err)
		}
		got++
	}
	if got != n {
		t.Fatalf("%s acknowledged %d messages, want %d", name, got, n)
	}
}

func requireReading(t *testing.T, what string, reading CutoverReading, verdict string, lines ...string) {
	t.Helper()
	if reading.Verdict != verdict {
		t.Fatalf("%s = %s, want %s:\n%s", what, reading.Verdict, verdict, strings.Join(reading.Lines, "\n"))
	}
	for _, want := range lines {
		found := false
		for _, line := range reading.Lines {
			found = found || strings.Contains(line, want)
		}
		if !found {
			t.Fatalf("%s holds no line with %q:\n%s", what, want, strings.Join(reading.Lines, "\n"))
		}
	}
}

// Plan C's drain read (step 3 of the wake migration), under the in-cluster listener's grant: a
// two-filter legion-go-PUB-wake whose ack floor sits before an agent_message on the shared subject,
// with listener_ready messages after it, reads NOT DRAINED naming PUB, that sequence and
// agent_message, and GATE SHUT; the durable's configuration and ack floor are the same after the
// read as before it; once the durable acknowledges through it, only listener_ready lies past its
// floor and the read is GATE OPEN. A project with no wake durable, a wake durable on one filter and
// an empty --projects each read UNKNOWN.
//
// Control: a read that starts at its own creation, as a read of what a new consumer would be given
// does, passes with the agent wake unread; the test fails naming it if it does not.
func TestTheDrainReadRefusesAnUnreadSharedWake(t *testing.T) {
	ctx := context.Background()
	rig := newCutoverRig(t)
	pub := wakeConsumerName("PUB")
	own, err := wakeSubject("PUB")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, rig.admin, legionWakeSubject, wakeReport(t, "ready-0", ListenerWakeReady, ""))
	rig.durable(t, pub, legionWakeSubject, own)
	rig.acknowledge(t, pub, 1)
	agent := publish(t, rig.admin, legionWakeSubject, wakeReport(t, "agent-1", ListenerWakeAgent, "ses_pub"))
	publish(t, rig.admin, legionWakeSubject, wakeReport(t, "ready-1", ListenerWakeReady, ""))
	publish(t, rig.admin, legionWakeSubject, wakeReport(t, "ready-2", ListenerWakeReady, ""))
	before := consumerInfo(t, rig.stream, pub)

	requireReading(t, "the drain read with an agent wake past the floor", rig.reader.Drain(ctx, []string{"PUB"}), GateShut,
		fmt.Sprintf("NOT DRAINED PUB: %s holds agent_message at sequence %d", legionWakeSubject, agent.Sequence))
	control := rig.reader.drain(ctx, []string{"PUB"}, func(_, last uint64) uint64 { return last + 1 })
	if !control.Open() {
		t.Fatalf("the control, a read from its own creation, did not pass with the agent wake unread, so this test shows nothing:\n%s", strings.Join(control.Lines, "\n"))
	}
	after := consumerInfo(t, rig.stream, pub)
	if !sameSet(filtersOf(*after), filtersOf(*before)) || after.AckFloor.Stream != before.AckFloor.Stream || after.AckFloor.Consumer != before.AckFloor.Consumer ||
		after.Delivered.Stream != before.Delivered.Stream || after.Delivered.Consumer != before.Delivered.Consumer || after.NumAckPending != before.NumAckPending {
		t.Fatalf("the reads changed %s: before %+v %v, after %+v %v", pub, before.AckFloor, filtersOf(*before), after.AckFloor, filtersOf(*after))
	}

	rig.acknowledge(t, pub, 3)
	requireReading(t, "the drain read once the durable acknowledged the agent wake", rig.reader.Drain(ctx, []string{"PUB"}), GateOpen,
		fmt.Sprintf("DRAINED PUB: 0 listener_ready past %s's ack floor", pub))
	publish(t, rig.admin, legionWakeSubject, wakeReport(t, "ready-3", ListenerWakeReady, ""))
	requireReading(t, "the drain read with only listener_ready past the floor", rig.reader.Drain(ctx, []string{"PUB"}), GateOpen,
		fmt.Sprintf("DRAINED PUB: 1 listener_ready past %s's ack floor", pub))

	requireReading(t, "the drain read of a project with no wake durable", rig.reader.Drain(ctx, []string{"PUB", "NONE"}), GateUnknown,
		"UNKNOWN NONE: the stream holds no wake durable legion-go-NONE-wake", "DRAINED PUB")
	oneOwn, err := wakeSubject("ONE")
	if err != nil {
		t.Fatal(err)
	}
	rig.durable(t, wakeConsumerName("ONE"), oneOwn)
	requireReading(t, "the drain read of a wake durable on one filter", rig.reader.Drain(ctx, []string{"PUB"}), GateUnknown,
		"UNKNOWN ONE: legion-go-ONE-wake filters "+oneOwn+", not deploy 1's shape")
	requireReading(t, "the drain read of no project", rig.reader.Drain(ctx, nil), GateUnknown, "--projects is empty")
}

// Plan C's filter read, after deploy 4 is live: four durables per project in their deploy-4 shape
// read GATE OPEN; legion-go-PUB-dispatch left on notifications.dispatch.issue.> reads GATE SHUT
// naming it, and so does legion-go-PUB-wake still holding notifications.legion.wake; a project
// missing a durable and an unreadable consumer list read UNKNOWN.
//
// Control: a read that checks only that each durable exists by name, as WithinProjects holds a
// daemon to its consumers, passes the stale Dispatch durable; the test fails naming it if it does
// not.
func TestTheFilterReadRefusesAConsumerLeftOnItsOldFilter(t *testing.T) {
	ctx := context.Background()
	rig := newCutoverRig(t)
	want, err := daemonFilters("PUB")
	if err != nil {
		t.Fatal(err)
	}
	for name, filters := range want {
		rig.durable(t, name, filters...)
	}
	requireReading(t, "the filter read of the deploy-4 shape", rig.reader.Filters(ctx, []string{"PUB"}), GateOpen, "FILTERED PUB")

	dispatch := dispatchConsumerName("PUB")
	rig.durable(t, dispatch, "notifications.dispatch.issue.>")
	requireReading(t, "the filter read of a stale Dispatch durable", rig.reader.Filters(ctx, []string{"PUB"}), GateShut,
		"GATE SHUT PUB: legion-go-PUB-dispatch on notifications.dispatch.issue.>, want notifications.dispatch.issue.PUB.>")
	if control := rig.reader.filters(ctx, []string{"PUB"}, durableExists); !control.Open() {
		t.Fatalf("the control, a read of names alone, did not pass the stale Dispatch durable, so this test shows nothing:\n%s", strings.Join(control.Lines, "\n"))
	}
	rig.durable(t, dispatch, want[dispatch]...)

	wake := wakeConsumerName("PUB")
	rig.durable(t, wake, legionWakeSubject, want[wake][0])
	requireReading(t, "the filter read of a wake durable still on the shared subject", rig.reader.Filters(ctx, []string{"PUB"}), GateShut,
		"legion-go-PUB-wake on "+legionWakeSubject)
	rig.durable(t, wake, want[wake]...)

	if err := rig.stream.DeleteConsumer(ctx, readyConsumerName("PUB")); err != nil {
		t.Fatal(err)
	}
	requireReading(t, "the filter read of a project missing a durable", rig.reader.Filters(ctx, []string{"PUB"}), GateUnknown,
		"UNKNOWN PUB: the stream holds no legion-go-PUB-ready")
	requireReading(t, "the filter read on a consumer list it cannot read", rig.refused.Filters(ctx, []string{"PUB"}), GateUnknown,
		"UNKNOWN: list the consumers of ENVOY_NOTIFICATIONS")
}
