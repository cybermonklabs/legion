package main

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/store"
)

// A message the durable path reads that matches an earlier build's interest row only through a
// topic the session's consumer filter leaves out is skipped for that session, and the skip is not
// silent: the listener logs filterLeftOutLine from fanout, naming the session, the row's topic, its
// kind and the subject published, once per session and topic within fanoutLeftOutLogWindow
// however many such messages arrive. A publish on it counts the session as no recipient.
//
// The skip happens in one of two places, and each case drives one. When every interest the
// subject matches is a session whose own consumer holds the message, the durable path skips the
// message before it decodes it (durableNeedsDelivery) and the fan-out never sees it. When another
// interest needs this listener, a session with no entry such as a gone session's row, the fan-out
// judges every interest, the one with the left-out topic among them (fanoutDelivery).
//
// The window keys on both: a second topic of the same session and a second session on the same
// topic each get a line of their own inside the window, and the first pair gets one again once the
// window has passed, read on the limiter's clock moved past it. The test isolates the limiter
// (leftOutLogLimiter.isolate), so no entry an earlier run stored, at a time that clock read ahead
// of the wall clock, suppresses this run's first lines.
//
// Before, both places skipped the session as one whose consumer relays the message, with no word,
// while its consumer's filter did not carry the topic.
func TestAFanOutThatMatchesAnEarlierBuildsRowThroughALeftOutTopicLogsIt(t *testing.T) {
	const leftLine = "listener left a topic out of the session's consumer filter"
	// The limiter's clock runs offset ahead of the wall clock; it is set before the durable starts
	// and put back after it stops, so no handler reads it in between.
	var offset atomic.Int64
	t.Cleanup(fanoutLeftOutLogs.isolate(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }))
	rig := newRecipientRig(t)
	registry := rig.harness.registry
	consumer := "listener-left-out-" + id.New()
	subscription, err := startListenerSubscription(rig.harness.client, consumer, rig.harness.handler)
	if err != nil {
		t.Fatalf("start the durable: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })

	// readAll waits until the durable has acknowledged every message the stream holds.
	readAll := func(t *testing.T) {
		t.Helper()
		stream, err := rig.harness.client.JS().StreamInfo(bus.Stream)
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			info, err := rig.harness.client.JS().ConsumerInfo(bus.Stream, consumer)
			if err == nil && info.AckFloor.Stream >= stream.State.LastSeq {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the durable acknowledged up to %v of %d within 15s (%v)", info, stream.State.LastSeq, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// oldRow registers sessionID through the route, then adds leftOut to its row as an earlier
	// build stored it: a merging write, which no plan of this build's registration shapes.
	oldRow := func(t *testing.T, sessionID, kept, leftOut string) {
		t.Helper()
		if kept != "" {
			rig.register(t, sessionID, []string{kept})
		}
		if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{leftOut}); err != nil {
			t.Fatalf("store %s on %s's row as an earlier build did: %v", leftOut, sessionID, err)
		}
		if row, _ := registry.CachedInterest(sessionID); !slices.Contains(row.Topics, leftOut) {
			t.Fatalf("the row of %s holds %v, without %s: the earlier build's window is not there to test", sessionID, row.Topics, leftOut)
		}
	}
	// fanoutLines publishes once on subject, counting no recipient, waits until the durable has
	// read it, and answers the fan-out lines logged meanwhile for sessionID and its topic leftOut.
	fanoutLines := func(t *testing.T, subject, key, sessionID, leftOut string) int {
		t.Helper()
		before := rig.harness.logs.Len()
		assertCounts(t, rig.publish(t, subject, key), 0, 0)
		readAll(t)
		return len(linesIn(rig.harness.logs.String()[before:], leftLine, map[string]any{"session_id": sessionID, "topic": leftOut, "from": "fanout"}))
	}

	for _, tc := range []struct {
		name, sessionID, kept, leftOut, subject string
		// gone is a session with a row on subject and no entry, which the durable path must
		// hand to the fan-out; "" for none.
		gone           string
		wantRecipients int
	}{
		{
			name:      "a session whose own consumer the durable path trusts to hold the message",
			sessionID: "ses_fanout_trusted", kept: "notifications.team.fanout-kept-a",
			leftOut: "*.team.fanout-a", subject: "notifications.team.fanout-a",
		},
		{
			name:      "a session the fan-out judges beside a gone session it must deliver to",
			sessionID: "ses_fanout_judged", kept: "notifications.team.fanout-kept-b",
			leftOut: "*.team.fanout-b", subject: "notifications.team.fanout-b",
			gone: "ses_fanout_gone", wantRecipients: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bus.SubjectContains(tc.leftOut, tc.subject) || !bus.StreamCaptures(tc.subject) {
				t.Fatalf("%s must match %s, which the stream must store", tc.leftOut, tc.subject)
			}
			oldRow(t, tc.sessionID, tc.kept, tc.leftOut)
			if tc.gone != "" {
				if _, err := registry.Upsert(store.Interest{SessionID: tc.gone, MachineID: "test-machine"}, []string{tc.subject}); err != nil {
					t.Fatalf("store %s's row: %v", tc.gone, err)
				}
			}
			before := rig.harness.logs.Len()
			for index := range 3 {
				assertCounts(t, rig.publish(t, tc.subject, tc.name+"-"+string(rune('a'+index))), tc.wantRecipients, 0)
			}
			readAll(t)
			logs := rig.harness.logs.String()[before:]
			lines := linesIn(logs, leftLine, map[string]any{"session_id": tc.sessionID, "topic": tc.leftOut, "from": "fanout"})
			if len(lines) != 1 {
				t.Fatalf("the log holds %d lines %q from fanout for %s and %s after three publishes, want one:\n%s", len(lines), leftLine, tc.sessionID, tc.leftOut, logs)
			}
			line := lines[0]
			if line["level"] != "WARN" || line["kind"] != "no_notifications_prefix" || line["subject"] != tc.subject || line["merged"] != nil {
				t.Errorf("the line is %v, want a WARN of kind no_notifications_prefix, subject %s and no merged filter", line, tc.subject)
			}
		})
	}

	t.Run("the window keys on session and topic, and opens again once it has passed", func(t *testing.T) {
		const (
			first, firstTopic, firstSubject = "ses_fanout_trusted", "*.team.fanout-a", "notifications.team.fanout-a"
			secondTopic, secondSubject      = "*.team.fanout-a2", "notifications.team.fanout-a2"
			second                          = "ses_fanout_second"
		)
		// The first case logged first's firstTopic moments ago, inside the window.
		oldRow(t, first, "", secondTopic)
		if got := fanoutLines(t, secondSubject, "window-second-topic", first, secondTopic); got != 1 {
			t.Errorf("a second topic of %s inside the window logged %d lines, want its own one", first, got)
		}
		oldRow(t, second, "notifications.team.fanout-kept-second", firstTopic)
		before := rig.harness.logs.Len()
		if got := fanoutLines(t, firstSubject, "window-second-session", second, firstTopic); got != 1 {
			t.Errorf("a second session on %s inside the window logged %d lines, want its own one", firstTopic, got)
		}
		if got := len(linesIn(rig.harness.logs.String()[before:], leftLine, map[string]any{"session_id": first, "topic": firstTopic, "from": "fanout"})); got != 0 {
			t.Errorf("%s's %s, logged inside the window already, logged %d lines more, want none", first, firstTopic, got)
		}
		offset.Store(int64(fanoutLeftOutLogWindow + time.Second))
		before = rig.harness.logs.Len()
		fanoutLines(t, firstSubject, "window-passed", first, firstTopic)
		logs := rig.harness.logs.String()[before:]
		for _, sessionID := range []string{first, second} {
			if got := len(linesIn(logs, leftLine, map[string]any{"session_id": sessionID, "topic": firstTopic, "from": "fanout"})); got != 1 {
				t.Errorf("past the window, %s's %s logged %d lines, want one again:\n%s", sessionID, firstTopic, got, logs)
			}
		}
	})
}
