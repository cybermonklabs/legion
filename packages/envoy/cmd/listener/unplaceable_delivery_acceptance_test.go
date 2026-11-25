package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The delivery harness names its counters test_messages_*; they are the listener's
// envoy_messages_delivered_total and envoy_messages_naked_total (main.go) under a test name, so the
// series below are those two counters' series.
const (
	deliveredCounter = "test_messages_delivered"
	nakedCounter     = "test_messages_naked"
)

// deliveryRecord is one line of the listener's delivery log, which logging.DeliveryLog writes as
// JSON: its level and message, the session, topic and event every delivery line carries, and the
// error a line of a failure adds. A field the line does not carry decodes as its zero value.
type deliveryRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	SessionID string `json:"session_id"`
	Topic     string `json:"topic"`
	EventID   string `json:"event_id"`
	Error     string `json:"error"`
}

// deliveryRecords decodes every line the harness logged whose msg is message. Counting them is how
// the tests below assert "exactly one": a message settled once, however many times it was
// delivered.
func deliveryRecords(t *testing.T, logs, message string) []deliveryRecord {
	t.Helper()
	var found []deliveryRecord
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record deliveryRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record.Msg == message {
			found = append(found, record)
		}
	}
	return found
}

// metricsBody is the listener's /metrics as an operator reads it, from the harness's own registry.
func metricsBody(t *testing.T, harness listenerDeliveryHarness) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	harness.metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/metrics: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// counterValue reads one counter series out of a metrics body: series is the metric's name with its
// label set, as /metrics renders it (`test_messages_delivered{delivery_status="delivered"}`), or a
// bare name for an unlabelled counter. It reports -1 when the body carries no such series, since a
// label set is written only once it has been incremented - so "never incremented" is the absence of
// the line, not a zero on it, and the HELP and TYPE lines, which begin with "# ", are never it.
func counterValue(t *testing.T, body, series string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		var count int64
		if _, err := fmt.Sscanf(value, "%d", &count); err != nil {
			t.Fatalf("metrics line %q does not carry a count: %v", line, err)
		}
		return count
	}
	return -1
}

// deliveredSeries is the delivered counter's series for one delivery_status label.
func deliveredSeries(status string) string {
	return fmt.Sprintf(`%s{delivery_status=%q}`, deliveredCounter, status)
}

// agentWakeProbe subscribes, on the harness's own connection, to the subject the listener's wake
// reports are published on, so a test reads each report the moment the stream stores it
// (awaitWake).
func agentWakeProbe(t *testing.T, harness listenerDeliveryHarness) *natsgo.Subscription {
	t.Helper()
	wakes, err := harness.client.Conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", bus.LegionWakeTopic, err)
	}
	t.Cleanup(func() { _ = wakes.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the wake subscription: %v", err)
	}
	return wakes
}

// A delivery to an agent subject reads its target from the session bucket itself when this
// listener's cache has not seen it, exactly as the send route does
// (TestSendReadsTheSessionStoreWhileTheCacheIsCold, send_route_acceptance_test.go): the cache
// follows the bucket through a watcher and so trails it, and a delivery that takes the cache's miss
// for an answer reports the wake of a session that is registered, whose own consumer holds the
// message all along. This drives the listener's own delivery handler with a session another
// listener registered a moment ago, through a listener whose cache was warm before that write and
// has followed nothing since, which is the same miss a cache still refilling after a reconnect has.
// The harness sets no agent wait, so a delivery that took the miss for an answer would report the
// wake at once.
//
// The negative control is the Get below: if the cache held the target the delivery would prove
// nothing about reading through to the bucket, since every warm-cache delivery acknowledges it.
func TestAgentDeliveryReadsTheSessionStoreWhileTheCacheIsCold(t *testing.T) {
	t.Parallel()
	const target = "ses_cold_cache_delivery_target"
	harness := newListenerDeliveryHarness(t)

	// trailingListeners' arrangement (api_test.go), applied to the delivery harness: the
	// delivering listener's cache is warmed and its watcher stopped, and the target is registered
	// through a second registry on the same account, so the record reaches the bucket and never
	// reaches the cache.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := harness.sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the delivering listener's session cache: %v", err)
	}
	harness.sessions.StopWatch()
	registrar, err := session.OpenSessionRegistry(t.Context(), harness.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the registering listener's session registry: %v", err)
	}
	if err := registrar.Put(target, session.SessionEntry{
		SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/" + target, Title: "cold target",
		Delivery: session.DeliveryConsumer, Epoch: "e",
	}); err != nil {
		t.Fatalf("register %s through the other listener: %v", target, err)
	}

	if entry, err := harness.sessions.Get(target); err == nil {
		t.Fatalf("the delivering listener's cache already holds %s (%+v): the delivery below would prove nothing about reading through to the bucket", target, entry)
	} else if !strings.Contains(err.Error(), natsgo.ErrKeyNotFound.Error()) {
		t.Fatalf("the delivering listener's cache answers %v for %s, want %v", err, target, natsgo.ErrKeyNotFound)
	}

	wakes := agentWakeProbe(t, harness)
	item := listenerTestEnvelope(contracts.LegacyAgentSubject(target), "cold-cache-agent-delivery")
	harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	// The record only the bucket holds is the session's, so the message is its own consumer's
	// to relay: an outcome counted skipped, with no wake. A delivery that took the cache's miss for
	// an answer would have found no record and reported the session's wake.
	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - a delivery that read through to the bucket finds the session's record:\n%s", deliveredSeries("skipped"), got, body)
	}
	if got := counterValue(t, body, deliveredSeries("wake_reported")); got != -1 {
		t.Fatalf("%s = %d, want no series at all - the bucket holds the target's record:\n%s", deliveredSeries("wake_reported"), got, body)
	}
	if reports := deliveryRecords(t, harness.logs.String(), "listener agent wake reported"); len(reports) != 0 {
		t.Fatalf("%d wake lines, want none: the bucket holds the target's record:\n%s", len(reports), harness.logs.String())
	}
	// A report is stored before the handler returns, so one would be on the probe by now; the
	// probe is read for a second more to be sure.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		msg, err := wakes.NextMsg(time.Until(deadline))
		if err != nil {
			break
		}
		if wakeNamed(wakeReportIn(t, msg), wakeKindAgent, target) {
			t.Fatalf("the delivery to %s, which the bucket holds a record of, reported its wake", target)
		}
	}
}

// unplaceableDeliveryRun is one envelope driven through the listener's own
// jetStreamDeliveryHandler over a real durable consumer. The package's other delivery tests hand
// the handler a synthetic &natsgo.Msg{Data: data}, which carries no JetStream metadata: the
// handler can bound its wait by nothing, so it reports at once, and nothing about the wait can be
// learned from it. Here the server holds the message and performs the redelivery the handler's
// NAK asks for, as it does in production.
type unplaceableDeliveryRun struct {
	harness listenerDeliveryHarness
	item    contracts.Envelope
	// exceptions reads the delivery exceptions published for the envelope's own topic.
	exceptions *natsgo.Subscription
	// consumer names the run's durable, for a test that reads back what the stream still holds.
	consumer string
}

// unplaceableHarness is the listener the runs below drive.
func unplaceableHarness(t *testing.T) listenerDeliveryHarness {
	t.Helper()
	return newListenerDeliveryHarness(t)
}

// startUnplaceableDelivery publishes one envelope for target, lets it sit in the stream for soak,
// and then runs a handler built from cfg over target's own durable consumer. It returns once the
// handler is bound; the deliveries happen on the subscription's goroutine. Several runs may share
// one harness: each has its own subject, consumer and exception probe, and they report into the
// one registry and the one log an operator would read.
//
// The publish comes first because a run that ages its message needs the message in the stream
// before anything can deliver it, and the subscription is made before the consumer is created,
// never after: the server pushes a pending message the instant the consumer exists, and a push to
// an inbox nobody is listening on is lost until AckWait, which would make the first delivery the
// handler sees its second.
func startUnplaceableDelivery(t *testing.T, harness listenerDeliveryHarness, cfg listenerDeliveryHandlerConfig, target string, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	return startDeliveryThrough(t, harness, jetStreamDeliveryHandler(cfg), target, soak)
}

// startDeliveryThrough is startUnplaceableDelivery with the subscription's handler given whole,
// for a run that must put something in front of the listener's own handler.
func startDeliveryThrough(t *testing.T, harness listenerDeliveryHarness, handler natsgo.MsgHandler, target string, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	return startEnvelopeDeliveryThrough(t, harness, handler, target, listenerTestEnvelope(contracts.LegacyAgentSubject(target), "unplaceable-"+target), soak)
}

// startEnvelopeDeliveryThrough is startDeliveryThrough with the envelope given whole, for a run
// whose envelope must carry something the default one does not; item.Topic is target's subject.
// Its exception probe's control: an exception published on the envelope's own lane before the
// run reaches the probe, so a run that reads none there reads the listener's silence.
func startEnvelopeDeliveryThrough(t *testing.T, harness listenerDeliveryHarness, handler natsgo.MsgHandler, target string, item contracts.Envelope, soak time.Duration) unplaceableDeliveryRun {
	t.Helper()
	subject := item.Topic
	exceptions, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + subject)
	if err != nil {
		t.Fatalf("subscribe the exception probe for %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}
	if err := publishDeliveryException(harness.client, item, "delivery_failed", ""); err != nil {
		t.Fatalf("control: publish an exception on %s's lane: %v", subject, err)
	}
	if _, err := exceptions.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("control: the exception probe for %s did not read the exception published on its lane: %v", subject, err)
	}
	if err := harness.client.Publish(item); err != nil {
		t.Fatalf("publish %s to %s: %v", item.EventID, subject, err)
	}
	time.Sleep(soak)

	inbox := natsgo.NewInbox()
	subscription, err := harness.client.Conn.Subscribe(inbox, handler)
	if err != nil {
		t.Fatalf("subscribe the delivery handler on %s: %v", inbox, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the delivery subscription: %v", err)
	}

	consumer := "listener-" + strings.ReplaceAll(target, "_", "-")
	// The listener's own consumer policy - explicit ack, consumerMaxDeliver - over this one
	// subject, so the run bounds nothing the production consumer does not. AckWait is the one
	// departure: the redeliveries here are the ones the handler asks for by NAK, and a 60 s
	// consumerAckWait would hold a handler that asked for neither an ack nor a retry past any
	// deadline this test could wait out.
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: inbox}
	applyListenerConsumerPolicy(&config, []string{subject})
	config.AckWait = 10 * time.Second
	if _, err := harness.client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the durable %s over %s: %v", consumer, subject, err)
	}
	t.Cleanup(func() { _ = harness.client.JS().DeleteConsumer(bus.Stream, consumer) })
	return unplaceableDeliveryRun{harness: harness, item: item, exceptions: exceptions, consumer: consumer}
}

// agentWakesFor counts the agent wake reports for sessionID that reach subscription, a core
// subscription to bus.LegionWakeTopic: it waits up to first for a report, and after each one for
// quiet more, so a second report of one message is counted too.
func agentWakesFor(t *testing.T, subscription *natsgo.Subscription, sessionID string, first, quiet time.Duration) int {
	t.Helper()
	count := 0
	wait := first
	for {
		message, err := subscription.NextMsg(wait)
		if errors.Is(err, natsgo.ErrTimeout) {
			return count
		}
		if err != nil {
			t.Fatalf("read %s: %v", bus.LegionWakeTopic, err)
		}
		if wakeNamed(wakeReportIn(t, message), wakeKindAgent, sessionID) {
			count++
			wait = quiet
		}
	}
}

// A released Legion role's session loses its record when it stops, while its interest row, which
// names the listener it registered through, stays until the reaper deletes it 10 to 20 minutes
// later. A copy on its agent subject - a person's answer Dispatch routes to it, say - then takes
// the path of a session with no record: it waits once for the agent wait, and then this listener
// reports one wake, which brings the pod back. It is never acknowledged in silence, as a copy
// whose row names another listener is, and it is never NAKed as a failed delivery. An interest
// path that failed such a copy as delivery_failed on every redelivery would report no wake, so the
// pod would stay released and the message would never reach it (#4088).
//
// The negative control is the second subtest: the same interest row over a record, one that names
// no way of receiving, is acknowledged as a skip, with no wait and no wake. So the rig takes the
// row that names this listener, and the first subtest's wake is the missing record's doing.
func TestACopyForASessionWithNoRecordWaitsOnceAndWakesEvenWhenItsInterestNamesThisListener(t *testing.T) {
	t.Parallel()
	const window = 2 * time.Second
	// start registers target's interest row naming the harness's own listener and, when entry is
	// not nil, its record, then drives one envelope on target's agent subject through the
	// listener's durable handler at an agent wait of window. It returns the run and a
	// subscription to the wake reports, made before anything is delivered.
	start := func(t *testing.T, target string, entry *session.SessionEntry) (unplaceableDeliveryRun, *natsgo.Subscription) {
		t.Helper()
		harness := newListenerDeliveryHarness(t)
		interest := store.Interest{SessionID: target, MachineID: harness.config.machineID}
		if _, err := harness.registry.Upsert(interest, []string{contracts.LegacyAgentSubject(target)}); err != nil {
			t.Fatalf("register %s's interest naming this listener: %v", target, err)
		}
		if entry != nil {
			if err := harness.sessions.Put(target, *entry); err != nil {
				t.Fatalf("register %s's record: %v", target, err)
			}
		}
		wakes := agentWakeProbe(t, harness)
		cfg := harness.config
		cfg.unplaceableRetryWindow = window
		cfg.deferredUnplaceable = newUnplaceableDeferrals(window)
		return startUnplaceableDelivery(t, harness, cfg, target, 0), wakes
	}
	// settled fails the test unless the run's message was acknowledged with no line about it but
	// the ones a message on an agent subject takes (aloneEventLines), no delivery exception and no
	// failed delivery.
	settled := func(t *testing.T, run unplaceableDeliveryRun) {
		t.Helper()
		logs := run.harness.logs.String()
		requireEventLines(t, "the listener", logs, run.item.EventID, aloneEventLines...)
		if got := counterValue(t, metricsBody(t, run.harness), deliveredSeries("failed")); got != -1 {
			t.Errorf("%s = %d, want no series at all - the message was never failed", deliveredSeries("failed"), got)
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Errorf("a message on an agent subject published a delivery exception: %s", string(message.Data))
		}
		stored, err := run.harness.client.JS().GetLastMsg(bus.Stream, run.item.Topic)
		if err != nil {
			t.Fatalf("read the stored message on %s: %v", run.item.Topic, err)
		}
		awaitDurableState(t, run.harness.client, run.consumer, 0, stored.Sequence)
	}

	t.Run("a session with no record waits once, then reports one wake", func(t *testing.T) {
		const target = "ses_interest_no_record"
		run, wakes := start(t, target, nil)
		harness := run.harness

		// The first delivery waits: it is sent back for the agent wait, with no line logged.
		waitFor(t, 15*time.Second, "the first delivery to wait", func() bool {
			return counterValue(t, metricsBody(t, harness), nakedCounter) == 1
		})
		if got := len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")); got != 0 {
			t.Fatalf("%d wake lines at the first delivery, want none - the message waits first, whatever its interest row names:\n%s", got, harness.logs.String())
		}
		// The redelivery finds no record still, and reports the wake.
		waitFor(t, 15*time.Second, "the redelivered message to report the wake", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")) > 0
		})
		logs := harness.logs.String()
		reports := deliveryRecords(t, logs, "listener agent wake reported")
		if len(reports) != 1 || reports[0].Level != "INFO" || reports[0].SessionID != target || reports[0].EventID != run.item.EventID || reports[0].Topic != run.item.Topic {
			t.Fatalf("want one INFO wake line naming %s, event %s and topic %s, got %+v:\n%s", target, run.item.EventID, run.item.Topic, reports, logs)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, deliveredSeries("wake_reported")); got != 1 {
			t.Fatalf("%s = %d, want 1:\n%s", deliveredSeries("wake_reported"), got, body)
		}
		if got := counterValue(t, body, nakedCounter); got != 1 {
			t.Fatalf("%s = %d, want 1 - the one wait's NAK and no other:\n%s", nakedCounter, got, body)
		}
		if got := agentWakesFor(t, wakes, target, 5*time.Second, time.Second); got != 1 {
			t.Fatalf("%d agent wake reports for %s, want exactly one:\n%s", got, target, logs)
		}
		settled(t, run)
	})

	t.Run("a record that names no way of receiving is acknowledged, with no wait and no wake", func(t *testing.T) {
		const target = "ses_interest_no_way_to_receive"
		run, wakes := start(t, target, &session.SessionEntry{MachineID: "test-machine"})
		harness := run.harness

		waitFor(t, 15*time.Second, "the delivery to be acknowledged as a skip", func() bool {
			return counterValue(t, metricsBody(t, harness), deliveredSeries("skipped")) == 1
		})
		// A wake would come no sooner than the agent wait, so the probe is read past it.
		if got := agentWakesFor(t, wakes, target, window+2*time.Second, time.Second); got != 0 {
			t.Fatalf("%d agent wake reports for %s, want none - a session with a record is its consumer's:\n%s", got, target, harness.logs.String())
		}
		if got := len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")); got != 0 {
			t.Fatalf("%d wake lines, want none:\n%s", got, harness.logs.String())
		}
		if got := counterValue(t, metricsBody(t, harness), nakedCounter); got != -1 {
			t.Fatalf("%s = %d, want no series at all - a session with a record is not waited for", nakedCounter, got)
		}
		settled(t, run)
	})

	// The failed series' control: a followed topic of a session with no record fails its one
	// delivery, so the series settled requires none of is the one a failed delivery writes.
	t.Run("control: a followed topic of a session with no record counts one failed delivery", func(t *testing.T) {
		harness := newListenerDeliveryHarness(t)
		const target = "ses_interest_no_record_followed"
		item := listenerTestEnvelope("notifications.github.owner.repo.issue.1", "no-record-followed-topic")
		if _, err := harness.registry.Upsert(store.Interest{SessionID: target, MachineID: harness.config.machineID}, []string{item.Topic}); err != nil {
			t.Fatalf("register %s's interest in %s: %v", target, item.Topic, err)
		}
		harness.handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
		if body := metricsBody(t, harness); counterValue(t, body, deliveredSeries("failed")) != 1 {
			t.Fatalf("%s is not 1 for a followed topic of a session with no record:\n%s", deliveredSeries("failed"), body)
		}
	})
}

// preloadDeferrals takes n slots of set for messages no test stream will ever number: sequences
// counted down from the top of the range, far above anything a stream these tests publish to
// assigns, so a release keyed on a real message's sequence can never give one of them back - only
// a release that is not keyed on the message can.
func preloadDeferrals(t *testing.T, set *unplaceableDeferrals, n int) {
	t.Helper()
	for i := range n {
		if !set.take(math.MaxUint64 - uint64(i)) {
			t.Fatalf("preloading slot %d of %d was refused: the set's cap is %d", i+1, n, unplaceableDeferralCap)
		}
	}
}

// capWait is an agent wait no test below waits out: a message that waits it is plainly told from
// one that reports its wake at once.
const capWait = time.Minute

// The cap is what keeps this listener's waiting messages from taking the durable's whole pending
// budget: a message that waits is NAKed rather than ACKed, so it holds one of
// consumerMaxAckPending slots for the whole wait, and enough of them at once would stall this
// machine's durable for every topic. At the cap, a message every other bound would hold back
// reports its wake at once instead. The negative control is the sibling subtest: one slot short of
// the cap, the same message on the same rig waits, so the report above is the cap's doing and not
// the rig's.
func TestUnplaceableDeferralsStopAtTheirCap(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T, target string, preload int) (unplaceableDeliveryRun, *unplaceableDeferrals, *natsgo.Subscription) {
		t.Helper()
		harness := unplaceableHarness(t)
		cfg := harness.config
		cfg.unplaceableRetryWindow = capWait
		held := newUnplaceableDeferrals(capWait)
		preloadDeferrals(t, held, preload)
		cfg.deferredUnplaceable = held
		wakes := agentWakeProbe(t, harness)
		return startUnplaceableDelivery(t, harness, cfg, target, 0), held, wakes
	}

	t.Run("a message that finds the cap reached reports its wake at once", func(t *testing.T) {
		const target = "ses_cap_reached"
		run, held, wakes := start(t, target, unplaceableDeferralCap)
		harness := run.harness

		waitFor(t, 15*time.Second, "the delivery that found the cap reached to report its wake", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")) > 0
		})
		awaitWake(t, wakes, wakeKindAgent, target)
		logs := harness.logs.String()
		reports := deliveryRecords(t, logs, "listener agent wake reported")
		if len(reports) != 1 || reports[0].Level != "INFO" || reports[0].SessionID != target || reports[0].EventID != run.item.EventID || reports[0].Topic != run.item.Topic {
			t.Fatalf("want one INFO wake line naming %s, event %s and topic %s, got %+v:\n%s", target, run.item.EventID, run.item.Topic, reports, logs)
		}
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the report, want the %d it started with - a message that did not wait takes no slot", got, unplaceableDeferralCap)
		}
		body := metricsBody(t, harness)
		if got := counterValue(t, body, nakedCounter); got != -1 {
			t.Fatalf("%s = %d, want no series at all - a message that reports on its first delivery is never NAKed:\n%s", nakedCounter, got, body)
		}
		if got := counterValue(t, body, deliveredSeries("wake_reported")); got != 1 {
			t.Fatalf("%s = %d, want 1:\n%s", deliveredSeries("wake_reported"), got, body)
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Fatalf("a message on an agent subject published a delivery exception: %s", string(message.Data))
		}
	})

	t.Run("the last slot under the cap still waits", func(t *testing.T) {
		run, held, _ := start(t, "ses_cap_last_slot", unplaceableDeferralCap-1)
		harness := run.harness

		waitFor(t, 15*time.Second, "the delivery in the last slot to wait", func() bool {
			return counterValue(t, metricsBody(t, harness), nakedCounter) == 1
		})
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots after the wait began, want the cap of %d - the wait takes the last slot", got, unplaceableDeferralCap)
		}
		logs := harness.logs.String()
		if got := len(deliveryRecords(t, logs, "listener agent wake reported")); got != 0 {
			t.Fatalf("%d wake lines one slot under the cap, want none - the message waits:\n%s", got, logs)
		}
		if got := counterValue(t, metricsBody(t, harness), deliveredSeries("wake_reported")); got != -1 {
			t.Fatalf("%s = %d while the message waits, want no series at all", deliveredSeries("wake_reported"), got)
		}
	})
}

// A redelivery gives its slot back. The set (unplaceableDeferrals) holds the messages this
// listener holds back for the agent wait right now, so a slot that is taken and never released is
// a slot gone until its expiry (unplaceableDeferralExpiry): after unplaceableDeferralCap waits
// inside that time the listener would hold back nothing more until the oldest expired, and every
// message to a session with no record would report its wake at once meanwhile, with nothing
// saying why. Nothing else pins it: the cap's own test preloads the set and reads it back after
// one decision, and every other test runs with the set nearly empty, far from the cap.
//
// Each run starts one slot under the cap, so the message holds the last slot while it waits, and
// the set is read at both ends: at the cap while the message waits, and back at the slot below it
// once the redelivery has settled the message. Without the release the message's slot outlives
// it. The redelivery is settled two ways: it reports the wake, or the durable acknowledges it
// before it decodes it, because the session registered during the wait.
func TestARedeliveryGivesItsDeferralSlotBack(t *testing.T) {
	t.Parallel()
	const window = 2 * time.Second
	start := func(t *testing.T, target string) (listenerDeliveryHarness, *unplaceableDeferrals) {
		t.Helper()
		harness := unplaceableHarness(t)
		cfg := harness.config
		cfg.unplaceableRetryWindow = window
		held := newUnplaceableDeferrals(window)
		preloadDeferrals(t, held, unplaceableDeferralCap-1)
		cfg.deferredUnplaceable = held
		startUnplaceableDelivery(t, harness, cfg, target, 0)
		waitFor(t, 15*time.Second, "the first delivery to wait", func() bool {
			return counterValue(t, metricsBody(t, harness), nakedCounter) == 1
		})
		if got := held.size(); got != unplaceableDeferralCap {
			t.Fatalf("the set holds %d slots while the message waits, want the cap of %d - the message holds exactly one slot", got, unplaceableDeferralCap)
		}
		return harness, held
	}

	t.Run("a redelivery that reports the wake", func(t *testing.T) {
		harness, held := start(t, "ses_slot_released_by_report")
		waitFor(t, 15*time.Second, "the redelivery to report the wake", func() bool {
			return len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")) > 0
		})
		if got := held.size(); got != unplaceableDeferralCap-1 {
			t.Fatalf("the set holds %d slots after the report, want the %d the run started with - a message no longer held must leave no slot behind",
				got, unplaceableDeferralCap-1)
		}
	})

	t.Run("a redelivery the durable acknowledges before it decodes it", func(t *testing.T) {
		const target = "ses_slot_released_by_registration"
		harness, held := start(t, target)
		// The session registers during the wait, so the redelivery finds its record in this
		// listener's cache and is acknowledged unread (agentSubjectNeedsDelivery).
		if err := harness.sessions.Put(target, session.SessionEntry{
			SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer, Epoch: "e",
		}); err != nil {
			t.Fatalf("register %s during the wait: %v", target, err)
		}
		waitFor(t, 15*time.Second, "the redelivery to be acknowledged as a skip", func() bool {
			return counterValue(t, metricsBody(t, harness), deliveredSeries("skipped")) == 1
		})
		if got := held.size(); got != unplaceableDeferralCap-1 {
			t.Fatalf("the set holds %d slots after the durable acknowledged the redelivery, want the %d the run started with - an early acknowledgement must give the slot back too",
				got, unplaceableDeferralCap-1)
		}
		if got := len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")); got != 0 {
			t.Fatalf("%d wake lines for a session that registered during the wait, want none:\n%s", got, harness.logs.String())
		}
	})
}

// A redelivery this listener never held back gives no slot back. Only a message this listener
// held back holds a slot, so only that message's next delivery may return one: a redelivery an
// expired AckWait produced, or one another reader's NAK asked for, is no sign a slot was ever
// taken for it. A release on every redelivery gives back slots it never took, until the budget
// reads empty while the listener is holding its whole cap, and the cap bounds nothing.
//
// The NAK here is the run's own, put in front of the listener's handler: it takes the message's
// first delivery and asks for it again at once, so the handler's first sight of the message is
// delivery 2, a redelivery it never held back. A later delivery has spent its one wait, so it
// reports the wake at once and takes no slot, whether or not one is free. The set starts with
// other messages' slots: a release keyed on the message returns none of them, and the set reads
// what it started with after the report.
func TestARedeliveryThisListenerNeverDeferredGivesNoSlotBack(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T, target string, preload int) (listenerDeliveryHarness, *unplaceableDeferrals) {
		t.Helper()
		harness := unplaceableHarness(t)
		cfg := harness.config
		// A wait no subtest waits out, so a redelivery that waited would never report.
		cfg.unplaceableRetryWindow = capWait
		held := newUnplaceableDeferrals(capWait)
		preloadDeferrals(t, held, preload)
		cfg.deferredUnplaceable = held
		handler := jetStreamDeliveryHandler(cfg)
		var firstTaken atomic.Bool
		startDeliveryThrough(t, harness, func(msg *natsgo.Msg) {
			if firstTaken.CompareAndSwap(false, true) {
				if err := msg.Nak(); err != nil {
					t.Errorf("NAK the first delivery of %s: %v", target, err)
				}
				return
			}
			handler(msg)
		}, target, 0)
		return harness, held
	}

	for _, tc := range []struct {
		name    string
		target  string
		preload int
	}{
		{"at the cap, the redelivery reports its wake and every slot stays held", "ses_untaken_release_at_cap", unplaceableDeferralCap},
		{"one slot under the cap, the redelivery reports its wake and takes no slot", "ses_untaken_release_last_slot", unplaceableDeferralCap - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness, held := start(t, tc.target, tc.preload)
			waitFor(t, 15*time.Second, "the redelivery to report its wake", func() bool {
				return len(deliveryRecords(t, harness.logs.String(), "listener agent wake reported")) > 0
			})
			if got := held.size(); got != tc.preload {
				t.Fatalf("the set holds %d slots after the redelivery, want the %d it started with - the redelivery gave back a slot it never took, or took one it does not wait for", got, tc.preload)
			}
			if got := counterValue(t, metricsBody(t, harness), nakedCounter); got != -1 {
				t.Fatalf("%s = %d, want no series at all - the handler NAKed a redelivery whose wait is spent", nakedCounter, got)
			}
		})
	}
}
