package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// wakeMarks is a handler that writes each listener wake it applies as wake-<kind>-<role or
// session>, and any other fact by its type.
func wakeMarks() Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		mark := fmt.Sprintf("%T", fact)
		if wake, ok := fact.(ListenerWake); ok {
			mark = "wake-" + wake.Kind + "-" + wake.Role + wake.Session
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, mark)
		return Result{}, err
	})
}

// wakeReport is a listener wake report's envelope: kind, and the role or the session it names.
func wakeReport(t *testing.T, eventID, kind, name string) []byte {
	t.Helper()
	field := ""
	switch kind {
	case ListenerWakeRole:
		field = fmt.Sprintf(`,"role":%q`, name)
	case ListenerWakeAgent:
		field = fmt.Sprintf(`,"session":%q`, name)
	}
	return envelopeJSON(t, eventID, "envoy", fmt.Sprintf(`{"kind":%q%s,"at":%d}`, kind, field, time.Now().UnixMilli()))
}

// The ready consumer, on the one wake subject every daemon shares, starts at its own creation: with
// a role wake and an agent wake stored on that subject, a ready consumer created then delivers
// neither, and delivers a listener_ready published after it. It starts there even where
// consumer_positions holds a row for it, as it does once it has applied a report and is then found
// missing: the reports it carries say nothing about the past.
//
// Control: a consumer on the same subject started at the stream's first sequence delivers the
// stored role wake, so the stream holds what the ready consumer must not be given.
func TestTheReadyConsumerStartsAtItsCreation(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	ready := readyConsumerName(spec.Project)
	if _, err := pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 1, $2)`, ready, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("store a position for the ready consumer: %v", err)
	}
	const role, session = "legion-priv-priv-7-architect", "ses_priv"
	publish(t, js, legionWakeSubject, wakeReport(t, "stored-role", ListenerWakeRole, role))
	publish(t, js, legionWakeSubject, wakeReport(t, "stored-agent", ListenerWakeAgent, session))

	control, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{FilterSubject: legionWakeSubject, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatalf("create the control consumer: %v", err)
	}
	batch, err := control.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch from the control consumer: %v", err)
	}
	controlSaw := false
	for message := range batch.Messages() {
		controlSaw = controlSaw || strings.Contains(string(message.Data()), role)
	}
	if !controlSaw {
		t.Fatal("a consumer started at the stream's first sequence did not deliver the stored role wake, so this test shows nothing")
	}

	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	if info := consumerInfo(t, stream, ready); info.Config.DeliverPolicy != jetstream.DeliverNewPolicy || info.Config.FilterSubject != legionWakeSubject || info.NumPending != 0 {
		t.Fatalf("the ready consumer starts %v on %q with %d pending, want at its creation on %s with nothing pending",
			info.Config.DeliverPolicy, info.Config.FilterSubject, info.NumPending, legionWakeSubject)
	}
	stop := runConsumers(t, consumers, wakeMarks())
	defer stop()
	publish(t, js, legionWakeSubject, wakeReport(t, "ready-after", ListenerWakeReady, ""))
	testwait.Eventually(t, "the listener_ready published after the ready consumer's creation applied", func() bool {
		return slices.Contains(writeHandlers(t, pool), "wake-listener_ready-")
	})
	waitDelivered(t, stream, ready)
	if delivered := consumerInfo(t, stream, ready).Delivered.Consumer; delivered != 1 {
		t.Fatalf("the ready consumer delivered %d messages, want only the listener_ready published after its creation", delivered)
	}
	if got := writeHandlers(t, pool); slices.ContainsFunc(got, func(mark string) bool { return strings.Contains(mark, role) || strings.Contains(mark, session) }) {
		t.Fatalf("applied %q: a wake stored before the boot reached this daemon", got)
	}
}

// The wake consumer migrates in place, keeping its name, its creation and its ack floor at each
// step. Widen is deploy 1's step, held at its own head (cybermonklabs/legion#4743): the consumer of
// today's shape, on the shared wake subject alone, is updated to both wake subjects. Narrow is this
// build's step: the consumer deploy 1 left, on both wake subjects, is updated to the project's
// subject alone, and a report the project's subject gains after the boot is delivered and applied,
// while a role report a listener still puts on the shared subject reaches neither consumer as a
// fact.
//
// Control: a consumer created at the next message in place of the update has another ack floor,
// so the comparison would name the recreate.
func TestTheWakeConsumerWidensAndNarrowsInPlace(t *testing.T) {
	t.Run("Narrow", func(t *testing.T) {
		ctx := context.Background()
		pool := migratedPool(t)
		createWrites(t, pool)
		js, stream := testJetStream(t)
		spec := consumerSpec(&lockedBuffer{})
		wake := wakeConsumerName(spec.Project)
		projectWake, err := wakeSubject(spec.Project)
		if err != nil {
			t.Fatal(err)
		}
		before := wakeConsumerInShape(t, js, stream, wake, []string{legionWakeSubject, projectWake})
		requireARecreateIsSeen(t, js, stream, before, []string{projectWake})

		consumers, err := OpenConsumers(ctx, js, pool, spec)
		if err != nil {
			t.Fatalf("OpenConsumers: %v", err)
		}
		requireUpdatedInPlace(t, stream, before, []string{projectWake})

		stop := runConsumers(t, consumers, wakeMarks())
		defer stop()
		publish(t, js, legionWakeSubject, wakeReport(t, "role-on-shared", ListenerWakeRole, "legion-capture-capture-8-architect"))
		publish(t, js, projectWake, wakeReport(t, "role-on-project", ListenerWakeRole, "legion-capture-capture-7-architect"))
		testwait.Eventually(t, "the report on the project's wake subject applied", func() bool {
			return slices.Contains(writeHandlers(t, pool), "wake-role_message-legion-capture-capture-7-architect")
		})
		waitDelivered(t, stream, readyConsumerName(spec.Project))
		if got := writeHandlers(t, pool); slices.Contains(got, "wake-role_message-legion-capture-capture-8-architect") {
			t.Fatalf("applied %q: a role report on the shared subject reached the narrowed daemon", got)
		}
	})
}

// wakeConsumerInShape creates the wake consumer on filters, as the release that left it did, and
// has it apply one report, so it stands with an ack floor; it answers the consumer's info then.
func wakeConsumerInShape(t *testing.T, js jetstream.JetStream, stream jetstream.Stream, wake string, filters []string) *jetstream.ConsumerInfo {
	t.Helper()
	ctx := context.Background()
	publish(t, js, filters[0], wakeReport(t, "ready-before-"+fmt.Sprint(len(filters)), ListenerWakeReady, ""))
	config := jetstream.ConsumerConfig{Durable: wake, FilterSubjects: filters, AckPolicy: jetstream.AckExplicitPolicy}
	if len(filters) == 1 {
		config.FilterSubject, config.FilterSubjects = filters[0], nil
	}
	if _, err := stream.CreateConsumer(ctx, config); err != nil {
		t.Fatalf("create the wake consumer on %q: %v", filters, err)
	}
	handle, err := stream.Consumer(ctx, wake)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := handle.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for message := range batch.Messages() {
		if err := message.DoubleAck(ctx); err != nil {
			t.Fatalf("acknowledge the report: %v", err)
		}
	}
	info := consumerInfo(t, stream, wake)
	if info.AckFloor.Stream == 0 {
		t.Fatal("the wake consumer has no ack floor, so this test shows nothing")
	}
	return info
}

// requireARecreateIsSeen is the control: once the stream has moved past before's ack floor, as it
// does while a daemon is down, a consumer on filters created at the next message stands at the
// stream's end, so requireUpdatedInPlace would name a recreate.
func requireARecreateIsSeen(t *testing.T, js jetstream.JetStream, stream jetstream.Stream, before *jetstream.ConsumerInfo, filters []string) {
	t.Helper()
	publish(t, js, "notifications.team.wake-migration-control", []byte(`{}`))
	control, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{FilterSubjects: filters, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatalf("create the control consumer: %v", err)
	}
	if floor := control.CachedInfo().AckFloor.Stream; floor == before.AckFloor.Stream {
		t.Fatalf("a consumer created at the next message has the ack floor %d, so the comparison cannot see a recreate", floor)
	}
}

// requireUpdatedInPlace holds the wake consumer to filters exactly, with before's name, creation,
// ack floor and deliveries.
func requireUpdatedInPlace(t *testing.T, stream jetstream.Stream, before *jetstream.ConsumerInfo, filters []string) {
	t.Helper()
	after := consumerInfo(t, stream, before.Name)
	got := after.Config.FilterSubjects
	if after.Config.FilterSubject != "" {
		got = append([]string{after.Config.FilterSubject}, got...)
	}
	if !slices.Equal(got, filters) {
		t.Fatalf("the wake consumer filters %q after the boot, want exactly %q", got, filters)
	}
	if !after.Created.Equal(before.Created) || after.AckFloor.Stream != before.AckFloor.Stream || after.Delivered.Consumer != before.Delivered.Consumer {
		t.Fatalf("the wake consumer after the boot was created %s with ack floor %d and %d delivered; want %s, %d and %d: updated in place",
			after.Created, after.AckFloor.Stream, after.Delivered.Consumer, before.Created, before.AckFloor.Stream, before.Delivered.Consumer)
	}
}
