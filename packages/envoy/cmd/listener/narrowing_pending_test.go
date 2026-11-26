package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A change of a session consumer's filter that drops a subject the consumer holds undelivered
// messages on passes those messages over: nats-server goes on from the consumer's position with
// the new filters, and a restart at the ack floor creates the consumer again without the subject.
// Those messages were counted for the session at their publish and never reach it. The listener
// names the loss in one WARN line, filterLeftOutLine from narrowing, naming the session and the
// filter subjects dropped, with the messages the stream holds on each dropped subject from the
// consumer's position on (pending_by_topic), counted on the subject itself before the change, and
// their sum (pending_passed_over). The count is the dropped subject's own, whatever the change adds.
//
// The topic dropped is notifications.envoy.exceptions.foo.*, which a newer build's deployed
// subject, notifications.envoy.exceptions.*.*, captures, so a registration plans it and the
// consumer filters on it. Two messages are stored on it. A later change of the row adds
// notifications.envoy.exceptions.*.bar, which merges with it into notifications.envoy.exceptions.>,
// so the filter leaves both out and drops the first. A widening, which drops a subject for one
// that contains it (notifications.team.widened-foo for notifications.team.>), passes nothing over
// and logs no line.
//
// A restart writes its line once its delete lands, before its create, so a create that fails
// still leaves the loss named; its retry, which creates the consumer at the pending start and
// reads no deleted consumer, writes none. A delete whose answer is lost after the server applied
// it is read back: a consumer gone gives the line as a landed delete does, and a read back that
// fails too gives it with delete_confirmed false. An update counts from the consumer's next
// sequence, past the messages it delivered before the update and awaits acks for.
func TestANarrowingFilterNamesTheMessagesItPassesOver(t *testing.T) {
	const (
		newer    = "notifications.envoy.exceptions.*.*"
		pairFoo  = "notifications.envoy.exceptions.foo.*"
		pairBar  = "notifications.envoy.exceptions.*.bar"
		stored   = "notifications.envoy.exceptions.foo.waiting"
		leftLine = "listener left a topic out of the session's consumer filter"
	)
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url, newer)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	for _, topic := range []string{pairFoo, pairBar, stored} {
		bustest.AwaitDeployedCapture(t, client, topic, true, 15*time.Second)
	}
	rig := newSessionConsumerRigOn(t, client, url)
	raw := testnats.Connect(t, url)
	t.Cleanup(raw.Close)
	js, err := raw.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	store := func(t *testing.T, subject string, keys ...string) {
		t.Helper()
		for _, key := range keys {
			if _, err := js.Publish(subject, marshalListenerEnvelope(t, listenerTestEnvelope(subject, key))); err != nil {
				t.Fatalf("store a message on %s: %v", subject, err)
			}
		}
	}
	// filteringOn registers sessionID on topics and fails unless its consumer filters on pairFoo.
	filteringOn := func(t *testing.T, sessionID string, topics ...string) {
		t.Helper()
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(sessionID, "e", topics...))
		if filters := rig.filters(t, sessionID); !slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s filters on %v, without %s: there is nothing to narrow", sessionID, filters, pairFoo)
		}
	}
	// narrowed checks that logs holds one narrowing line for sessionID naming pairFoo among the
	// dropped subjects with want messages on it, and that sum in pending_passed_over.
	narrowed := func(t *testing.T, logs, sessionID string, want float64) {
		t.Helper()
		lines := linesIn(logs, leftLine, map[string]any{"session_id": sessionID, "from": "narrowing"})
		if len(lines) != 1 {
			t.Fatalf("the log holds %d narrowing lines for %s, want one:\n%s", len(lines), sessionID, logs)
		}
		line := lines[0]
		dropped, _ := line["topics"].([]any)
		byTopic, _ := line["pending_by_topic"].(map[string]any)
		if line["level"] != "WARN" || !slices.Contains(dropped, any(pairFoo)) || byTopic[pairFoo] != want || line["pending_passed_over"] != want {
			t.Errorf("the narrowing line is %v, want a WARN naming %s among the dropped subjects with %v messages on it, and %v in all", line, pairFoo, want, want)
		}
	}
	// deliverUnacked has sessionID's consumer deliver n messages, with one pull request of exactly
	// that batch, and acks none of them, so the consumer's delivered sequence and its ack floor are
	// apart by those n. The request waits for its batch rather than asking no_wait: nats-server
	// answers a no_wait request from the consumer's count of pending messages, and that count
	// rises only once the stream's own goroutine has signalled the consumer of each message, after
	// the publish's acknowledgement went out (2.15.1: processJetStreamMsgWithBatch acknowledges,
	// then queues the signal that signalConsumersLoop hands to processStreamSignal). A no_wait
	// request sent as soon as store returned could arrive before that, and be answered "404 No
	// Messages" with the messages stored. A request that waits is served from the stream's store,
	// which holds them once their publishes were acknowledged.
	deliverUnacked := func(t *testing.T, sessionID string, n int) {
		t.Helper()
		name, ok := bus.SessionConsumerName(sessionID)
		if !ok {
			t.Fatalf("no consumer name for %s", sessionID)
		}
		inbox := raw.NewInbox()
		sub, err := raw.SubscribeSync(inbox)
		if err != nil {
			t.Fatalf("subscribe the pull's inbox: %v", err)
		}
		defer func() { _ = sub.Unsubscribe() }()
		request := fmt.Sprintf(`{"batch":%d}`, n)
		if err := raw.PublishRequest("$JS.API.CONSUMER.MSG.NEXT."+bus.Stream+"."+name, inbox, []byte(request)); err != nil {
			t.Fatalf("send the pull request of %s: %v", sessionID, err)
		}
		ctx := testwait.Context(t)
		for range n {
			msg, err := sub.NextMsgWithContext(ctx)
			if err != nil || msg.Header.Get("Status") != "" {
				t.Fatalf("the pull of %s answered %v (%v), want a message", sessionID, msg, err)
			}
		}
	}

	t.Run("a registration that drops a topic", func(t *testing.T) {
		const (
			session = "ses_narrowing"
			kept    = "notifications.team.narrowing-kept"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-1", "narrowing-2")
		before := rig.harness.logs.Len()
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(session, "e", pairFoo, pairBar, kept))
		if filters := rig.filters(t, session); slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s still filters on %v: the registration narrowed nothing", session, filters)
		}
		narrowed(t, rig.harness.logs.String()[before:], session, 2)
	})

	t.Run("a registration that drops a topic and adds one with as many messages waiting", func(t *testing.T) {
		const (
			session = "ses_narrowing_added"
			kept    = "notifications.team.narrowing-kept-added"
			added   = "notifications.team.narrowing-added"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-added-1", "narrowing-added-2")
		store(t, added, "narrowing-added-3", "narrowing-added-4")
		before := rig.harness.logs.Len()
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(session, "e", pairFoo, pairBar, kept, added))
		if filters := rig.filters(t, session); slices.Contains(filters, pairFoo) || !slices.Contains(filters, added) {
			t.Fatalf("the consumer of %s filters on %v, want %s and not %s", session, filters, added, pairFoo)
		}
		// The two messages on pairFoo stored since the consumer's start, which the two the update
		// adds on another subject leave the consumer's pending count unchanged by: the count is the
		// dropped subject's own, so the loss is named all the same.
		narrowed(t, rig.harness.logs.String()[before:], session, 2)
	})

	t.Run("a restart at the ack floor of a consumer whose messages await an ack", func(t *testing.T) {
		const (
			session = "ses_narrowing_restart"
			kept    = "notifications.team.narrowing-kept-restart"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-restart-1", "narrowing-restart-2")
		// One message is delivered and left unacked, so the restart deletes the consumer and
		// creates it again rather than keeping it (RestartAtFloor).
		deliverUnacked(t, session, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if info, err := rig.consumers.Info(ctx, session); err != nil || info.NumAckPending != 1 {
			t.Fatalf("the consumer of %s awaits %v acks (%v), want one: the restart would keep it", session, info, err)
		}
		before := rig.harness.logs.Len()
		if _, err := rig.deps.consumers.consumers.RestartAtFloor(ctx, session, sessionConsumerFilter(session, []string{pairFoo, pairBar, kept})); err != nil {
			t.Fatalf("restart the consumer of %s: %v", session, err)
		}
		if filters := rig.filters(t, session); slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s still filters on %v: the restart narrowed nothing", session, filters)
		}
		// Both messages on pairFoo above the ack floor: the one held unacked and the one after it.
		narrowed(t, rig.harness.logs.String()[before:], session, 2)
	})

	t.Run("a restart whose delete lands and whose create fails", func(t *testing.T) {
		const (
			session = "ses_narrowing_restart_faulted"
			kept    = "notifications.team.narrowing-kept-faulted"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-faulted-1", "narrowing-faulted-2")
		deliverUnacked(t, session, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if info, err := rig.consumers.Info(ctx, session); err != nil || info.NumAckPending != 1 {
			t.Fatalf("the consumer of %s awaits %v acks (%v), want one: the restart would keep it", session, info, err)
		}
		name, _ := bus.SessionConsumerName(session)
		consumers := rig.deps.consumers.consumers
		consumers.SetCreateFault(func(consumer string) error {
			if consumer == name {
				return errors.New("the create faulted")
			}
			return nil
		})
		t.Cleanup(func() { consumers.SetCreateFault(nil) })
		filter := sessionConsumerFilter(session, []string{pairFoo, pairBar, kept})
		before := rig.harness.logs.Len()
		if _, err := consumers.RestartAtFloor(ctx, session, filter); err == nil {
			t.Fatalf("the restart of %s whose create faulted reported no error", session)
		}
		if _, err := rig.consumers.Info(ctx, session); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
			t.Fatalf("the consumer of %s reads %v after the faulted create, want it deleted", session, err)
		}
		// The delete landed, so both messages on pairFoo above the ack floor are passed over, and
		// the line says so though no consumer was created.
		narrowed(t, rig.harness.logs.String()[before:], session, 2)

		consumers.SetCreateFault(nil)
		before = rig.harness.logs.Len()
		if _, err := consumers.RestartAtFloor(ctx, session, filter); err != nil {
			t.Fatalf("the retry of the restart of %s: %v", session, err)
		}
		if filters := rig.filters(t, session); slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s filters on %v after the retry, %s among them", session, filters, pairFoo)
		}
		if lines := linesIn(rig.harness.logs.String()[before:], leftLine, map[string]any{"session_id": session, "from": "narrowing"}); len(lines) != 0 {
			t.Errorf("the retry logged %v, want no second narrowing line: the restart's own named the loss", lines)
		}
	})

	// A restart whose delete answer is lost after the server applied it: the delete brings no
	// answer, so the restart reads the consumer back, and names the loss as for a landed delete
	// when the consumer is gone, or with delete_confirmed false when that read fails too.
	for _, tc := range []struct {
		name, session string
		// readBackFails fails every info read once the delete's answer was lost.
		readBackFails bool
	}{
		{name: "a restart whose delete answer is lost after the server applied it", session: "ses_narrowing_lost_answer"},
		{name: "a restart whose delete answer is lost and whose read back fails", session: "ses_narrowing_lost_unread", readBackFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := "notifications.team.narrowing-kept-" + tc.session
			filteringOn(t, tc.session, pairFoo, kept)
			store(t, stored, tc.session+"-1", tc.session+"-2")
			deliverUnacked(t, tc.session, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if info, err := rig.consumers.Info(ctx, tc.session); err != nil || info.NumAckPending != 1 {
				t.Fatalf("the consumer of %s awaits %v acks (%v), want one: the restart would keep it", tc.session, info, err)
			}
			name, _ := bus.SessionConsumerName(tc.session)
			consumers := rig.deps.consumers.consumers
			var lost atomic.Bool
			consumers.SetDeleteAnswerFault(func(consumer string) error {
				if consumer == name {
					lost.Store(true)
					return context.DeadlineExceeded
				}
				return nil
			})
			consumers.SetInfoFault(func(consumer string) error {
				if tc.readBackFails && consumer == name && lost.Load() {
					return errors.New("the read back faulted")
				}
				return nil
			})
			t.Cleanup(func() { consumers.SetDeleteAnswerFault(nil); consumers.SetInfoFault(nil) })
			before := rig.harness.logs.Len()
			if _, err := consumers.RestartAtFloor(ctx, tc.session, sessionConsumerFilter(tc.session, []string{pairFoo, pairBar, kept})); err == nil {
				t.Fatalf("the restart of %s whose delete answer was lost reported no error", tc.session)
			}
			consumers.SetDeleteAnswerFault(nil)
			consumers.SetInfoFault(nil)
			if _, err := rig.consumers.Info(ctx, tc.session); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
				t.Fatalf("the consumer of %s reads %v after the delete whose answer was lost, want it deleted", tc.session, err)
			}
			logs := rig.harness.logs.String()[before:]
			// The server applied the delete, so both messages on pairFoo above the ack floor are
			// passed over.
			narrowed(t, logs, tc.session, 2)
			line := linesIn(logs, leftLine, map[string]any{"session_id": tc.session, "from": "narrowing"})[0]
			want := any(nil)
			if tc.readBackFails {
				want = false
			}
			if line["delete_confirmed"] != want {
				t.Errorf("the narrowing line has delete_confirmed %v, want %v", line["delete_confirmed"], want)
			}
		})
	}

	t.Run("a restart whose delete answer is lost and the server never applied it", func(t *testing.T) {
		const (
			session = "ses_narrowing_lost_unapplied"
			kept    = "notifications.team.narrowing-kept-unapplied"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-unapplied-1", "narrowing-unapplied-2")
		deliverUnacked(t, session, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if info, err := rig.consumers.Info(ctx, session); err != nil || info.NumAckPending != 1 {
			t.Fatalf("the consumer of %s awaits %v acks (%v), want one: the restart would keep it", session, info, err)
		}
		name, _ := bus.SessionConsumerName(session)
		consumers := rig.deps.consumers.consumers
		// The delete never reaches the server, and brings no answer: the read back finds the
		// consumer standing, so nothing was passed over.
		consumers.SetDeleteSendFault(func(consumer string) error {
			if consumer == name {
				return context.DeadlineExceeded
			}
			return nil
		})
		t.Cleanup(func() { consumers.SetDeleteSendFault(nil) })
		filter := sessionConsumerFilter(session, []string{pairFoo, pairBar, kept})
		before := rig.harness.logs.Len()
		if _, err := consumers.RestartAtFloor(ctx, session, filter); err == nil {
			t.Fatalf("the restart of %s whose delete brought no answer reported no error", session)
		}
		consumers.SetDeleteSendFault(nil)
		if filters := rig.filters(t, session); !slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s filters on %v, without %s: the delete landed, so the case shows nothing", session, filters, pairFoo)
		}
		if lines := linesIn(rig.harness.logs.String()[before:], leftLine, map[string]any{"session_id": session, "from": "narrowing"}); len(lines) != 0 {
			t.Errorf("a delete the server never applied logged %v, want no narrowing line: the consumer still carries %s", lines, pairFoo)
		}
		// The next restart finds the consumer standing, counts it from its own floor and names
		// the loss once its delete lands.
		before = rig.harness.logs.Len()
		if _, err := consumers.RestartAtFloor(ctx, session, filter); err != nil {
			t.Fatalf("the next restart of %s: %v", session, err)
		}
		narrowed(t, rig.harness.logs.String()[before:], session, 2)
	})

	t.Run("an update of a consumer that awaits acks for messages it delivered", func(t *testing.T) {
		const (
			session = "ses_narrowing_update_unacked"
			kept    = "notifications.team.narrowing-kept-unacked"
		)
		filteringOn(t, session, pairFoo, kept)
		store(t, stored, "narrowing-unacked-1", "narrowing-unacked-2", "narrowing-unacked-3")
		deliverUnacked(t, session, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if info, err := rig.consumers.Info(ctx, session); err != nil || info.NumAckPending != 2 {
			t.Fatalf("the consumer of %s awaits %v acks (%v), want two", session, info, err)
		}
		before := rig.harness.logs.Len()
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(session, "e", pairFoo, pairBar, kept))
		if filters := rig.filters(t, session); slices.Contains(filters, pairFoo) {
			t.Fatalf("the consumer of %s still filters on %v: the registration narrowed nothing", session, filters)
		}
		// The update goes on from the consumer's next sequence, past the two it delivered and
		// awaits acks for: only the third message on pairFoo is passed over.
		narrowed(t, rig.harness.logs.String()[before:], session, 1)
	})

	t.Run("a widening that replaces a subject with one that contains it", func(t *testing.T) {
		const (
			session = "ses_narrowing_widened"
			narrow  = "notifications.team.widened-foo"
			wide    = "notifications.team.>"
		)
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(session, "e", narrow))
		if filters := rig.filters(t, session); !slices.Contains(filters, narrow) {
			t.Fatalf("the consumer of %s filters on %v, without %s", session, filters, narrow)
		}
		store(t, narrow, "widened-1", "widened-2")
		before := rig.harness.logs.Len()
		rig.post(t, "/v1/interests/subscribe", consumerRegistration(session, "e", narrow, wide))
		if filters := rig.filters(t, session); slices.Contains(filters, narrow) || !slices.Contains(filters, wide) {
			t.Fatalf("the consumer of %s filters on %v, want %s in place of %s", session, filters, wide, narrow)
		}
		// The two messages on narrow are still the consumer's to deliver, through wide.
		logs := rig.harness.logs.String()[before:]
		if lines := linesIn(logs, leftLine, map[string]any{"session_id": session, "from": "narrowing"}); len(lines) != 0 {
			t.Errorf("a widening logged %v, want no narrowing line: it passes nothing over", lines)
		}
	})
}
