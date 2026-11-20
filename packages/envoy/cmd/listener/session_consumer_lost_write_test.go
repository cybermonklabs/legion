package main

// A consumer create or update whose answer did not come back can land late, and a landing late
// enough costs more than a filter (bus.LateWrite, sessionDelivery.unansweredWrite):
//
//   - a write whose answer is lost with its connection can reach the server after nats.go has
//     reconnected and after the listener has judged the consumer on the new connection, since the
//     server goes on reading the old connection until its stale limit (bus.StaleConnectionLimit);
//     the listener judges the consumer at each pause for that long, and the filter ends on the row
//     the bucket holds whenever within it the write lands;
//   - a write that lands with a filter that lacks a topic of the stored row, on a consumer a relay
//     pulls, makes the consumer skip the messages stored on that topic while it stands; the
//     listener takes the consumer back to the first of them, and none is lost.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lostWritePingInterval and lostWritePingMax are the pings of the server the lost-write cases
// start: long enough that the server keeps the held link open through the registration's budget,
// the reconnect and the listener's first judgment after it (three intervals at least from its last
// read), short enough that the watch the listener keeps ends within a minute.
const (
	lostWritePingInterval = 5 * time.Second
	lostWritePingMax      = 2
)

// A registration whose consumer update is held on its link past its budget is answered 503. The
// rows then make what a write delayed in the network can be:
//
//   - "same-connection" (control): the held bytes reach the server on the same link, the write's
//     late answer comes back, and the listener judges the consumer after it;
//   - "after-a-reconnect": the link's client side closes, nats.go reconnects on a new link, the
//     listener judges the consumer there, and only then do the held bytes reach the server on the
//     old link, whose server side the server has not closed yet: the write lands the plan the 503
//     registration never stored, after the listener's first judgment, and the listener's watch
//     of the consumer takes the filter back to the row the bucket holds;
//   - "never-lands" (control): the link's client side closes and the held bytes never reach the
//     server, which closes the old link as stale; the watch ends with the filter on the row the
//     bucket holds, and nothing is taken back.
//
// Each row requires the filter on the stored row once the listener's work has ended
// (awaitSettled), which for a lost answer is the end of its watch.
func TestALateWriteLostWithItsConnectionLeavesNoUnstoredPlan(t *testing.T) {
	for _, c := range []struct {
		name    string
		drop    bool
		release bool
	}{
		{"same-connection", false, true},
		{"after-a-reconnect", true, true},
		{"never-lands", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			uri := startPingServer(t, lostWritePingInterval, lostWritePingMax)
			audits := watchAPIAudits(t, uri)
			p := startLinkProxy(t, uri)
			clientA, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect listener A: %v", err)
			}
			t.Cleanup(clientA.Close)
			a := newSessionConsumerRigOn(t, clientA, p.url)
			a.deps.consumers.staleLimit = bus.StaleConnectionLimit(lostWritePingInterval, lostWritePingMax)
			clientB, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect listener B: %v", err)
			}
			t.Cleanup(clientB.Close)
			b := newSessionConsumerRigOn(t, clientB, uri)
			sessionID := "ses_lost_write_" + strings.ReplaceAll(c.name, "-", "_")
			const stored = "notifications.dispatch.issue.LOSTWRITESTORED.>"
			const planned = "notifications.dispatch.issue.LOSTWRITEPLANNED.>"
			b.subscribe(t, consumerRegistration(sessionID, "epoch-1", stored))

			p.arm("$JS.API.CONSUMER.CREATE.")
			recorder := httptest.NewRecorder()
			a.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
				strings.NewReader(consumerRegistration(sessionID, "epoch-1", planned))))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("the registration whose update is held answered %d, want 503: %s", recorder.Code, recorder.Body.String())
			}
			select {
			case <-p.heldCh:
			default:
				t.Fatal("no consumer write of the registration was held")
			}
			if !c.drop {
				p.release(t)
				audits.awaitApplied(t, 0, sessionID, carrying("LOSTWRITEPLANNED"), 30*time.Second)
			} else {
				// The watch's second attempt begins only once its first has judged the consumer on
				// the new link: the held bytes go then.
				judged := make(chan struct{})
				var attempts atomic.Int32
				a.deps.consumers.beforeRepairPlan = func() {
					if strings.Contains(a.harness.logs.String(), lateWatchLine) && attempts.Add(1) == 2 {
						close(judged)
					}
				}
				p.dropClient()
				select {
				case <-judged:
				case <-time.After(2 * (lateWatchMargin + a.deps.consumers.staleLimit)):
					t.Fatalf("the listener made no second attempt at the consumer after the reconnect:\n%s", a.harness.logs.String())
				}
				requireFilteredOnStoredRow(t, b, sessionID)
				if c.release {
					p.release(t)
					audits.awaitApplied(t, 0, sessionID, carrying("LOSTWRITEPLANNED"), 30*time.Second)
					t.Logf("the late write landed after the listener's first judgment on the new link: %v", b.filters(t, sessionID))
				}
			}
			awaitSettled(a)
			a.deps.consumers.beforeRepairPlan = nil
			t.Logf("at the end: %v", b.filters(t, sessionID))
			requireFilteredOnStoredRow(t, b, sessionID)
			if c.drop {
				if n := len(settleLines(t, a, sessionID, lateWatchEndLine)); n != 1 {
					t.Errorf("the listener logged %d ends of its watch of %s, want 1", n, sessionID)
				}
				if n := strings.Count(a.harness.logs.String(), rewoundLine); n != 0 {
					t.Errorf("the listener took the consumer back %d times; no message was stored, so none was skipped", n)
				}
			}
		})
	}
}

// lateNarrowHold is how long the late-narrowing case holds its registration's consumer call: past
// the call's own budget, so the registration is answered 503 and the call lands after it.
const lateNarrowHold = sessionConsumerCallTimeout + 500*time.Millisecond

// A registration of the session on listener A loses its consumer call's answer (the call is held
// at A's TCP relay past its budget) and is answered 503. Under the held call a registration on
// listener B stores a newer row with a topic A's plan lacks, and B is answered 200. Once the held
// call has landed (the consumer's filter no longer holds B's topic), and before the listener's
// judgment after it writes the filter (afterRepairPlan), a message is stored on B's topic, then a
// direct message for the session. The session's relay delivers the direct message under the late
// filter, which moves the consumer past the message on B's topic. The case requires the message on
// B's topic to reach the session once the listener's work behind the answer has ended: the
// listener takes the consumer back to it (bus.LateWrite).
//
// Rows: "widening" (the control, the same path): no newer row, and the held call keeps the stored
// topic, so the message on it must arrive at every head; "under-a-newer-row": the case above.
func TestALateWriteThatNarrowsALiveRelaysFilterLosesNoMessageOnAStoredTopic(t *testing.T) {
	for _, c := range []struct {
		name  string
		newer bool
	}{
		{"widening", false},
		{"under-a-newer-row", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := testnats.URL(t)
			tcp := startListenerRelay(t, url)
			client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect listener A: %v", err)
			}
			t.Cleanup(client.Close)
			a := newSessionConsumerRigOn(t, client, tcp.relayed)
			relays, err := startSessionRelays(a.deps.consumers, a.harness.config)
			if err != nil {
				t.Fatalf("start A's relays: %v", err)
			}
			relays.pauseFirst, relays.pauseMax, relays.receiptWait = relayPauseFirst, relayPauseMax, relayReceiptWait
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				relays.stop(ctx)
			})
			relays.setOwned(true)
			ar := &relayRig{sessionConsumerRig: a, relays: relays, url: url}
			b := newSessionConsumerRig(t)

			sessionID := "ses_late_narrow_" + strings.ReplaceAll(c.name, "-", "_")
			const stored = "notifications.dispatch.issue.LATENARROWSTORED.>"
			const planned = "notifications.dispatch.issue.LATENARROWPLANNED.>"
			const newer = "notifications.dispatch.issue.LATENARROWNEWER.>"
			target := "notifications.dispatch.issue.LATENARROWSTORED.comment"
			if c.newer {
				target = "notifications.dispatch.issue.LATENARROWNEWER.comment"
			}
			agent := contracts.LegacyAgentSubject(sessionID)
			standIn := startSessionStandIn(t, url, sessionID, answerDone)
			a.subscribe(t, consumerRegistration(sessionID, "epoch-1", stored))
			first, _ := ar.storeEvent(t, "notifications.dispatch.issue.LATENARROWSTORED.comment")
			requireFrames(t, standIn.collect(t, 1, 15*time.Second), first)

			newerForms := bus.SessionConsumerFilters([]string{newer})
			var landed atomic.Bool
			held := make(chan struct{})
			tcp.pause("$JS.API.CONSUMER.CREATE.", 1, func() {
				if c.newer {
					// Under the held call: another listener stores a newer row and is answered 200.
					b.subscribe(t, consumerRegistration(sessionID, "epoch-1", stored, newer))
				}
				time.Sleep(lateNarrowHold)
				landed.Store(true)
				close(held)
			})
			judging := make(chan struct{}, 1)
			release := make(chan struct{})
			var blocked atomic.Bool
			a.deps.consumers.afterRepairPlan = func() {
				if !landed.Load() || !blocked.CompareAndSwap(false, true) {
					return
				}
				judging <- struct{}{}
				select {
				case <-release:
				case <-time.After(60 * time.Second):
				}
			}
			t.Cleanup(func() { a.deps.consumers.afterRepairPlan = nil })

			recorder := httptest.NewRecorder()
			a.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
				strings.NewReader(consumerRegistration(sessionID, "epoch-1", stored, planned))))
			if recorder.Code != http.StatusServiceUnavailable {
				close(release)
				t.Fatalf("the registration whose call lost its answer answered %d, want 503: %s", recorder.Code, recorder.Body.String())
			}
			<-held
			if c.newer {
				// The late call has been forwarded; wait to see it land: B's topic leaves the filter.
				sawLate := false
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
					if got := b.filters(t, sessionID); !slices.Contains(got, newerForms[0]) {
						sawLate = true
						t.Logf("the late call landed: the consumer filters on %v, without B's %s", got, newer)
						break
					}
				}
				if !sawLate {
					close(release)
					t.Fatalf("the late call was not seen to land within 10 s; the consumer filters on %v", b.filters(t, sessionID))
				}
			}

			lost, lostSeq := ar.storeEvent(t, target)
			direct, directSeq := ar.storeEvent(t, agent)
			t.Logf("stored message %s at seq %d on %s, then direct message %s at seq %d", lost.EventID, lostSeq, target, direct.EventID, directSeq)
			seen := map[string]bool{}
			for deadline := time.After(15 * time.Second); !seen[direct.EventID]; {
				select {
				case frame := <-standIn.frames:
					seen[frame.item.EventID] = true
				case <-deadline:
					close(release)
					t.Fatalf("the direct message never reached the session under the late filter")
				}
			}
			close(release)
			awaitSettled(a)
			t.Logf("the consumer filters on %v once the listener's work ended", b.filters(t, sessionID))

			for deadline := time.After(15 * time.Second); !seen[lost.EventID]; {
				select {
				case frame := <-standIn.frames:
					seen[frame.item.EventID] = true
				case <-deadline:
					t.Fatalf("the message stored on %s (seq %d), a topic of the row the bucket holds, never reached the session: it was stored while the late write's filter stood, and the direct message stored after it (seq %d) was delivered under that filter:\n%s", target, lostSeq, directSeq, a.harness.logs.String())
				}
			}
			requireFilteredOnStoredRow(t, a, sessionID)
			if rewound := strings.Count(a.harness.logs.String(), rewoundLine); c.newer && rewound == 0 {
				t.Errorf("the message reached the session, but the listener logged no line taking the consumer back")
			} else if !c.newer && rewound != 0 {
				t.Errorf("the listener took the consumer back %d times for a late write that kept every topic of the row", rewound)
			}
		})
	}
}
