package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The wake consumer is narrowed in place across the upgrade: the earlier release's
// legion-go-<P>-wake, on the shared notifications.legion.wake, keeps its name and its ack floor and
// takes the project's own wake subject as its one filter. A role wake and an agent wake an upgraded
// listener publishes on the project's subject while the daemon is down across the upgrade each
// reach the upgraded daemon once, and once only across a restart after it.
//
// Control: a role wake a listener of the earlier release left on the shared subject, unread when
// the daemon narrows, is lost: the narrowed wake consumer does not hear the shared subject, and the
// ready consumer starts at its own creation. That is why the upgrade waits until every listener
// publishes role and agent wakes on project subjects and the stored-wake read shows none left
// unread on the shared subject.
func TestAWakePublishedWhileTheDaemonIsDownAcrossTheUpgradeReachesItOnce(t *testing.T) {
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

	// The earlier release's wake consumer, on the shared subject, which has applied a liveness report.
	publish(t, js, legionWakeSubject, envelopeJSON(t, "ready-before", "envoy", fmt.Sprintf(`{"kind":"listener_ready","at":%d}`, time.Now().UnixMilli())))
	if _, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: wake, FilterSubject: legionWakeSubject, AckPolicy: jetstream.AckExplicitPolicy}); err != nil {
		t.Fatalf("create the earlier release's wake consumer: %v", err)
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
			t.Fatalf("acknowledge the earlier release's liveness report: %v", err)
		}
	}

	// While the daemon is down: a listener of the earlier release leaves a role wake on the shared
	// subject (the control), and an upgraded listener publishes a role wake and an agent wake on
	// the project's own subject.
	publish(t, js, legionWakeSubject, envelopeJSON(t, "role-left-on-shared", "envoy",
		fmt.Sprintf(`{"kind":"role_message","role":"legion-capture-capture-8-architect","at":%d}`, time.Now().UnixMilli())))
	publish(t, js, projectWake, envelopeJSON(t, "role-on-project", "envoy",
		fmt.Sprintf(`{"kind":"role_message","role":"legion-capture-capture-7-architect","at":%d}`, time.Now().UnixMilli())))
	publish(t, js, projectWake, envelopeJSON(t, "agent-on-project", "envoy",
		fmt.Sprintf(`{"kind":"agent_message","session":"ses_released","at":%d}`, time.Now().UnixMilli())))

	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers across the upgrade: %v", err)
	}
	if info := consumerInfo(t, stream, wake); info.Config.FilterSubject != projectWake || len(info.Config.FilterSubjects) != 0 || info.Delivered.Consumer != 1 {
		t.Fatalf("the wake consumer after the upgrade filters %q %v having delivered %d, want exactly %s, updated in place with its one earlier delivery kept",
			info.Config.FilterSubject, info.Config.FilterSubjects, info.Delivered.Consumer, projectWake)
	}
	want := []string{"wake-agent_message-ses_released", "wake-role_message-legion-capture-capture-7-architect"}
	applied := func() []string {
		var wakes []string
		for _, mark := range writeHandlers(t, pool) {
			if strings.HasPrefix(mark, "wake-agent_message-") || strings.HasPrefix(mark, "wake-role_message-") {
				wakes = append(wakes, mark)
			}
		}
		slices.Sort(wakes)
		return wakes
	}
	stop := runConsumers(t, consumers, marks())
	testwait.Eventually(t, "the two wakes published on the project's subject applied", func() bool { return len(applied()) >= len(want) })
	waitDelivered(t, stream, wake)
	waitDelivered(t, stream, readyConsumerName(spec.Project))
	stop()
	if got := applied(); !slices.Equal(got, want) {
		t.Fatalf("applied wakes %q, want %q, each once, and not the role wake left unread on the shared subject", got, want)
	}

	// A restart after the upgrade applies neither again.
	again, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers after a restart: %v", err)
	}
	stop = runConsumers(t, again, marks())
	waitDelivered(t, stream, wake)
	time.Sleep(time.Second)
	stop()
	if got := applied(); !slices.Equal(got, want) {
		t.Fatalf("applied wakes after a restart %q, want %q still, each once", got, want)
	}
	// The control: the role wake left on the shared subject reached neither consumer.
	if slices.ContainsFunc(writeHandlers(t, pool), func(mark string) bool { return strings.Contains(mark, "legion-capture-capture-8-architect") }) {
		t.Fatal("the role wake left on the shared subject was applied, so this shows nothing about narrowing before the stored-wake read")
	}
	if delivered := consumerInfo(t, stream, readyConsumerName(spec.Project)).Delivered.Consumer; delivered != 0 {
		t.Fatalf("the ready consumer delivered %d messages, want none: it starts at its own creation, after the role wake left on the shared subject", delivered)
	}
}
