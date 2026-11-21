package main

// Check l1 of dispatch://INFRA-3589: a message on the agent subject of a session is the session's
// own consumer's alone. The listener delivers it never, tries it again never and gives it up
// never. For a session with no record it waits once and then stores one wake report.
//
// This file uses only what the listener package had before that change, and names each new log
// line, metric label and state by its text, so it compiles against the code before the change and
// fails there on its assertions.

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// aloneWindow is the wait both listeners take (ENVOY_UNPLACEABLE_RETRY_WINDOW).
	aloneWindow = 2 * time.Second
	// aloneLatest is the latest a wake may be stored after its message: the wait, the
	// redelivery, and one publish bounded at 2 s.
	aloneLatest = 6 * time.Second
	// aloneMachineA and aloneMachineB are the two listeners' machine ids.
	aloneMachineA = "alone-a"
	aloneMachineB = "alone-b"
	// aloneNoListener is a machine id no listener of the rig runs as, for a record written
	// straight into the session bucket that no listener must act on as its own.
	aloneNoListener = "alone-absent"
)

// The log lines of the path that the change removed for an agent subject. No listener writes one
// of them for a message on an agent subject.
var aloneRemovedLines = []string{
	"listener agent delivery deferred",
	"listener agent delivery given up",
	"listener agent delivery failed",
}

// aloneWake is one agent wake report the stream stored: the session it names and when the stream
// stored it.
type aloneWake struct {
	session string
	stored  time.Time
}

// aloneRig is two listener processes, A and B, built from this tree, on a NATS server of their
// own, with a JetStream reader of every wake report the stream stores and a reader of every
// exception published on an agent subject's lane.
type aloneRig struct {
	uri       string
	a, b      *listenerProcess
	publisher *bus.Client

	mu         sync.Mutex
	wakes      []aloneWake
	exceptions []string
}

func startAloneRig(t *testing.T) *aloneRig {
	t.Helper()
	_, uri := testnats.Start(t)
	binary := cmdtest.Build(t, "envoy-listener")
	rig := &aloneRig{uri: uri}
	rig.a = startListenerProcess(t, binary, uri, aloneMachineA, "ENVOY_UNPLACEABLE_RETRY_WINDOW=2s")
	rig.b = startListenerProcess(t, binary, uri, aloneMachineB, "ENVOY_UNPLACEABLE_RETRY_WINDOW=2s")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("listener A's output:\n%s\nlistener B's output:\n%s", rig.a.Output.String(), rig.b.Output.String())
		}
	})
	rig.a.waitHealthy(t)
	rig.b.waitHealthy(t)
	rig.publisher = connectPublisher(t, uri)

	wakes, err := rig.publisher.JS().Subscribe(bus.LegionWakeTopic, func(msg *natsgo.Msg) {
		meta, err := msg.Metadata()
		if err != nil {
			return
		}
		var item contracts.Envelope
		var report wakeReport
		if json.Unmarshal(msg.Data, &item) != nil || json.Unmarshal([]byte(item.Payload), &report) != nil || report.Kind != wakeKindAgent {
			return
		}
		rig.mu.Lock()
		rig.wakes = append(rig.wakes, aloneWake{session: report.Session, stored: meta.Timestamp})
		rig.mu.Unlock()
	}, natsgo.OrderedConsumer(), natsgo.DeliverAll())
	if err != nil {
		t.Fatalf("read the wake reports the stream stores: %v", err)
	}
	t.Cleanup(func() { _ = wakes.Unsubscribe() })
	exceptions, err := rig.publisher.Conn.Subscribe(bus.ExceptionTopicPrefix+contracts.AgentTopicPrefix+">", func(msg *natsgo.Msg) {
		rig.mu.Lock()
		rig.exceptions = append(rig.exceptions, msg.Subject)
		rig.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("read the exceptions published on the agent subjects' lanes: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := rig.publisher.Conn.Flush(); err != nil {
		t.Fatalf("flush the readers: %v", err)
	}
	return rig
}

// store stores a Dispatch envelope on sessionID's agent subject, and returns it with its stream
// sequence and the time the stream stored it.
func (r *aloneRig) store(t *testing.T, sessionID string) (contracts.Envelope, uint64, time.Time) {
	t.Helper()
	item := newEnvelope("dispatch", contracts.LegacyAgentSubject(sessionID), "a comment for "+sessionID)
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode the message for %s: %v", sessionID, err)
	}
	ack, err := r.publisher.JS().Publish(item.Topic, data)
	if err != nil {
		t.Fatalf("store the message for %s: %v", sessionID, err)
	}
	stored, err := r.publisher.JS().GetMsg(bus.Stream, ack.Sequence)
	if err != nil {
		t.Fatalf("read back the stored message for %s: %v", sessionID, err)
	}
	return item, ack.Sequence, stored.Time
}

// wakesFor is every agent wake report the stream stored for sessionID so far, oldest first.
func (r *aloneRig) wakesFor(sessionID string) []aloneWake {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []aloneWake
	for _, wake := range r.wakes {
		if wake.session == sessionID {
			found = append(found, wake)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].stored.Before(found[j].stored) })
	return found
}

// awaitWakes waits until the stream holds n wake reports for sessionID, or until the message
// stored at stored is aloneLatest and 2 s more old, and then a further half second, so a report
// past the n it waited for is counted too. It returns what the stream holds then.
func (r *aloneRig) awaitWakes(sessionID string, n int, stored time.Time) []aloneWake {
	deadline := stored.Add(aloneLatest + 2*time.Second)
	for len(r.wakesFor(sessionID)) < n && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	return r.wakesFor(sessionID)
}

// requireWakes fails the test unless wakes are exactly want reports, each stored between
// aloneWindow and aloneLatest after the message the stream stored at stored.
func requireWakes(t *testing.T, wakes []aloneWake, want int, stored time.Time) {
	t.Helper()
	if len(wakes) != want {
		t.Errorf("the stream holds %d wake reports for the session, want %d", len(wakes), want)
	}
	for _, wake := range wakes {
		if after := wake.stored.Sub(stored); after < aloneWindow || after > aloneLatest {
			t.Errorf("a wake report was stored %s after the message, want between %s and %s", after, aloneWindow, aloneLatest)
		}
	}
}

// requireNoWakeUntil waits until the message stored at stored is aloneLatest and 1 s old and fails
// the test when the stream holds a wake report for sessionID by then.
func (r *aloneRig) requireNoWakeUntil(t *testing.T, sessionID string, stored time.Time) {
	t.Helper()
	time.Sleep(time.Until(stored.Add(aloneLatest + time.Second)))
	if wakes := r.wakesFor(sessionID); len(wakes) != 0 {
		t.Errorf("the stream holds %d wake reports for %s, a session with a record, want none", len(wakes), sessionID)
	}
}

// requireQuiet fails the test when either listener wrote a line of the removed path for
// sessionID, or when an exception was published on its agent subject's lane.
func (r *aloneRig) requireQuiet(t *testing.T, sessionID string) {
	t.Helper()
	for name, listener := range map[string]*listenerProcess{"A": r.a, "B": r.b} {
		for _, line := range aloneRemovedLines {
			if got := aloneLines(listener, line, sessionID, ""); got != 0 {
				t.Errorf("listener %s logged %q %d times for %s, want never", name, line, got, sessionID)
			}
		}
	}
	lane := bus.ExceptionTopicPrefix + contracts.LegacyAgentSubject(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, subject := range r.exceptions {
		if subject == lane {
			t.Errorf("an exception was published on %s, want none", lane)
		}
	}
}

// requireAcknowledged fails the test unless both listeners' durables have acknowledged every
// message through seq, with nothing waiting for an acknowledgement.
func (r *aloneRig) requireAcknowledged(t *testing.T, seq uint64) {
	t.Helper()
	awaitDurableState(t, r.publisher, bus.ListenerDurable(aloneMachineA), 0, seq)
	awaitDurableState(t, r.publisher, bus.ListenerDurable(aloneMachineB), 0, seq)
}

// aloneLines counts the JSON lines named name that listener wrote for sessionID, and for the event
// eventID unless it is empty.
func aloneLines(listener *listenerProcess, name, sessionID, eventID string) int {
	return countRecords(listener, name, func(record map[string]any) bool {
		return record["session_id"] == sessionID && (eventID == "" || record["event_id"] == eventID)
	})
}

// aloneReceived counts the deliveries of the event eventID that reached listener's delivery
// handler, which writes one "listener received" line for each.
func aloneReceived(listener *listenerProcess, eventID string) int {
	return countRecords(listener, "listener received", func(record map[string]any) bool {
		return record["event_id"] == eventID
	})
}

// requireOneWakeLine fails the test unless listener wrote exactly one INFO line that it reported
// item's wake, naming the event, the session and the topic.
func requireOneWakeLine(t *testing.T, name string, listener *listenerProcess, item contracts.Envelope, sessionID string) {
	t.Helper()
	got := countRecords(listener, "listener agent wake reported", func(record map[string]any) bool {
		return record["level"] == "INFO" && record["session_id"] == sessionID && record["event_id"] == item.EventID && record["topic"] == item.Topic
	})
	if got != 1 {
		t.Errorf("listener %s wrote %d INFO lines that it reported the wake of %s for event %s on %s, want one", name, got, sessionID, item.EventID, item.Topic)
	}
}

// stopCleanly ends sessionID as its plugin does when it stops, DELETE /v1/sessions/<id> through
// listener, and waits until neither listener lists the session.
func (r *aloneRig) stopCleanly(t *testing.T, listener *listenerProcess, sessionID string) {
	t.Helper()
	if status, answer := callListener(t, listener.port, http.MethodDelete, "/v1/sessions/"+sessionID, ""); status != http.StatusOK {
		t.Fatalf("stop %s: status %d: %s", sessionID, status, answer)
	}
	r.awaitUnlisted(t, sessionID)
}

// awaitUnlisted waits up to 10 s until neither listener's GET /v1/sessions lists sessionID.
func (r *aloneRig) awaitUnlisted(t *testing.T, sessionID string) {
	t.Helper()
	for _, listener := range []*listenerProcess{r.a, r.b} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			status, body := callListener(t, listener.port, http.MethodGet, "/v1/sessions", "")
			if status == http.StatusOK && !strings.Contains(body, `"`+sessionID+`"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the listener on port %d still lists %s after 10s: %d %s", listener.port, sessionID, status, body)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// putRecord writes entry as sessionID's record straight into the session bucket, as another
// writer's record arrives, so its registration time is the one the test chose.
func putRecord(t *testing.T, client *bus.Client, sessionID string, entry session.SessionEntry) {
	t.Helper()
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode the record of %s: %v", sessionID, err)
	}
	bucket, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := bucket.Put(sessionID, value); err != nil {
		t.Fatalf("store the record of %s: %v", sessionID, err)
	}
}

// aloneDeliveries is what a delivery rig's handler saw: the delivery count of each delivery it
// took, in order, the time the handler had settled each, and the stream sequence of the last.
type aloneDeliveries struct {
	mu    sync.Mutex
	count []uint64
	at    []time.Time
	seq   uint64
}

func (d *aloneDeliveries) add(meta *natsgo.MsgMetadata) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count = append(d.count, meta.NumDelivered)
	d.at = append(d.at, time.Now())
	d.seq = meta.Sequence.Stream
}

func (d *aloneDeliveries) seen() ([]uint64, []time.Time, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]uint64(nil), d.count...), append([]time.Time(nil), d.at...), d.seq
}

// awaitSeen waits up to within for the handler to have seen n deliveries.
func (d *aloneDeliveries) awaitSeen(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		count, _, _ := d.seen()
		if len(count) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the handler saw deliveries %v within %s, want %d of them", count, within, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startAloneDelivery runs one envelope for sessionID through the listener's own durable handler,
// built from harness's configuration with the agent wait set to aloneWindow, over a durable of its
// own. before sees each delivery first and reports whether the handler takes it: one it keeps
// back, it settles itself. after runs once the handler has settled a delivery.
func startAloneDelivery(t *testing.T, harness listenerDeliveryHarness, sessionID string, before func(*natsgo.Msg, *natsgo.MsgMetadata) bool, after func(*natsgo.MsgMetadata)) (unplaceableDeliveryRun, *aloneDeliveries) {
	t.Helper()
	cfg := harness.config
	cfg.unplaceableRetryWindow = aloneWindow
	handler := jetStreamDeliveryHandler(cfg)
	seen := &aloneDeliveries{}
	item := listenerTestEnvelope(contracts.LegacyAgentSubject(sessionID), "l1-"+sessionID)
	run := startEnvelopeDeliveryThrough(t, harness, func(msg *natsgo.Msg) {
		meta, err := msg.Metadata()
		if err != nil {
			t.Errorf("read the metadata of a delivery of %s: %v", sessionID, err)
			return
		}
		if before != nil && !before(msg, meta) {
			return
		}
		handler(msg)
		seen.add(meta)
		if after != nil {
			after(meta)
		}
	}, sessionID, item, 0)
	return run, seen
}

// storedWakeCount is how many wake reports the stream on client's server holds.
func storedWakeCount(t *testing.T, client *bus.Client) int {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: bus.LegionWakeTopic})
	if err != nil {
		t.Fatalf("read the stream's count of %s: %v", bus.LegionWakeTopic, err)
	}
	return int(info.State.Subjects[bus.LegionWakeTopic])
}

// sessionReadDenied is the listener's grant with the session bucket's reads left out: the direct
// get and the message get a KV read of one key makes.
func sessionReadDenied() string {
	bucket := "KV_" + bus.SessionBucket
	return listenerGrant("$JS.API.DIRECT.GET."+bucket, "$JS.API.DIRECT.GET."+bucket+".>", "$JS.API.STREAM.MSG.GET."+bucket)
}

// TestAnAgentMessageIsTheSessionConsumersAlone is check l1. Each subtest is one row of it.
//
// Rows a to f and i run on two listener processes A and B, built from this tree, with a wait of
// 2 s, on a NATS server of their own. Rows g and j change what the server lets the listener's user
// do between two deliveries of one message, so they run the listener's own durable handler in this
// process, where the test sees each delivery: a process rig cannot say which delivery of a message
// a grant change came between.
//
// Rows a, b and i end with a watch of 30 s or 10 s. That watch runs in parallel with the other
// rows' watches, once every row has run its first part.
func TestAnAgentMessageIsTheSessionConsumersAlone(t *testing.T) {
	rig := startAloneRig(t)

	// Rows a and b: the session registered through A, so A holds its interest row, and B's
	// cached row names A. A waits once and reports the wake; B acknowledges the message and
	// reports nothing. The session's consumer relays the message when it registers again.
	stopped := func(t *testing.T, sessionID string, stop func(t *testing.T)) {
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		rig.b.awaitSessionMachine(t, sessionID, aloneMachineA)
		stop(t)
		item, seq, stored := rig.store(t, sessionID)

		requireWakes(t, rig.awaitWakes(sessionID, 1, stored), 1, stored)
		requireOneWakeLine(t, "A", rig.a, item, sessionID)
		if got := aloneLines(rig.b, "listener agent wake reported", sessionID, ""); got != 0 {
			t.Errorf("listener B, whose row names A, reported the wake of %s %d times, want never", sessionID, got)
		}
		rig.requireQuiet(t, sessionID)
		if t.Failed() {
			return
		}
		rig.requireAcknowledged(t, seq)

		standIn := startSessionStandIn(t, rig.uri, sessionID, answerDone)
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2"))
		requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
		t.Parallel()
		standIn.none(t, 30*time.Second)
	}

	t.Run("a: a session that stopped cleanly wakes once after the wait", func(t *testing.T) {
		sessionID := "ses_l1_a_" + id.New()
		stopped(t, sessionID, func(t *testing.T) { rig.stopCleanly(t, rig.a, sessionID) })
	})

	t.Run("b: a session whose record lapsed wakes once after the wait", func(t *testing.T) {
		sessionID := "ses_l1_b_" + id.New()
		stopped(t, sessionID, func(t *testing.T) {
			bucket, err := rig.publisher.JS().KeyValue(bus.SessionBucket)
			if err != nil {
				t.Fatalf("open the session bucket: %v", err)
			}
			if err := bucket.Delete(sessionID); err != nil {
				t.Fatalf("delete the record of %s: %v", sessionID, err)
			}
			rig.awaitUnlisted(t, sessionID)
		})
	})

	// Rows c and d: no listener holds a row, so each listener waits once and reports one wake.
	unowned := func(t *testing.T, sessionID string) {
		item, seq, stored := rig.store(t, sessionID)
		requireWakes(t, rig.awaitWakes(sessionID, 2, stored), 2, stored)
		requireOneWakeLine(t, "A", rig.a, item, sessionID)
		requireOneWakeLine(t, "B", rig.b, item, sessionID)
		rig.requireQuiet(t, sessionID)
		if t.Failed() {
			return
		}
		rig.requireAcknowledged(t, seq)
	}

	t.Run("c: a session with no record and no row wakes once through each listener", func(t *testing.T) {
		unowned(t, "ses_l1_c_"+id.New())
	})

	t.Run("d: a session whose record is past the gone horizon wakes as one with no record", func(t *testing.T) {
		sessionID := "ses_l1_d_" + id.New()
		putRecord(t, rig.publisher, sessionID, session.SessionEntry{
			MachineID: aloneNoListener, SelfSubscribed: true, Delivery: session.DeliveryConsumer, Epoch: "e",
			UpdatedAt: time.Now().Add(-(session.GoneHorizon + 10*time.Minute)).UnixMilli(),
		})
		// Both listeners' caches apply the bucket's records in the bucket's order (kvwatch), so a
		// live record put after this one fences it: once both list the fence, both have applied
		// this record before the message, and the row reads the record's state, not its absence.
		fence := "ses_l1_d_fence_" + id.New()
		putRecord(t, rig.publisher, fence, session.SessionEntry{MachineID: aloneNoListener, UpdatedAt: time.Now().UnixMilli()})
		rig.a.awaitSessionMachine(t, fence, aloneNoListener)
		rig.b.awaitSessionMachine(t, fence, aloneNoListener)
		unowned(t, sessionID)
	})

	t.Run("e: a session that registers again during the wait raises no wake", func(t *testing.T) {
		sessionID := "ses_l1_e_" + id.New()
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		rig.b.awaitSessionMachine(t, sessionID, aloneMachineA)
		rig.stopCleanly(t, rig.a, sessionID)
		postListener(t, rig.a.port, "/v1/interests/unsubscribe", `{"session_id":"`+sessionID+`","topics":[]}`)
		item, seq, stored := rig.store(t, sessionID)
		// A's first delivery found no record: the session registers again only after it.
		deadline := time.Now().Add(5 * time.Second)
		for aloneReceived(rig.a, item.EventID) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("listener A's handler did not take the first delivery of %s within 5s", item.EventID)
			}
			time.Sleep(20 * time.Millisecond)
		}
		standIn := startSessionStandIn(t, rig.uri, sessionID, answerDone)
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2"))
		if late := time.Since(stored); late >= aloneWindow {
			t.Fatalf("the session registered again %s after the message, not within the %s wait: the rig is too slow for this row", late, aloneWindow)
		}

		requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
		rig.requireNoWakeUntil(t, sessionID, stored)
		rig.requireQuiet(t, sessionID)
		rig.requireAcknowledged(t, seq)
		standIn.none(t, time.Second)
	})

	t.Run("f: a live session, an unresponsive one and one whose consumer starts later raise no wake", func(t *testing.T) {
		live := "ses_l1_f_live_" + id.New()
		unresponsive := "ses_l1_f_unresponsive_" + id.New()
		later := "ses_l1_f_later_" + id.New()
		standIn := startSessionStandIn(t, rig.uri, live, answerDone)
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(live, "epoch-1"))
		rig.b.awaitSessionMachine(t, live, aloneMachineA)
		putRecord(t, rig.publisher, unresponsive, session.SessionEntry{
			MachineID: aloneNoListener, SelfSubscribed: true, Delivery: session.DeliveryConsumer, Epoch: "e",
			UpdatedAt: time.Now().Add(-(session.LiveWindow + 5*time.Minute)).UnixMilli(),
		})
		putRecord(t, rig.publisher, later, session.SessionEntry{
			MachineID: aloneNoListener, SelfSubscribed: true, Delivery: session.DeliveryConsumer, Epoch: "e",
			UpdatedAt: time.Now().UnixMilli(), ConsumerStart: math.MaxUint32,
		})
		// Both listeners' caches apply the bucket's records in the bucket's order (kvwatch), so once
		// both list later, put last, both have applied all three records before the messages.
		rig.a.awaitSessionMachine(t, later, aloneNoListener)
		rig.b.awaitSessionMachine(t, later, aloneNoListener)

		liveItem, _, stored := rig.store(t, live)
		rig.store(t, unresponsive)
		_, seq, _ := rig.store(t, later)

		requireFrames(t, standIn.collect(t, 1, 15*time.Second), liveItem)
		for _, sessionID := range []string{live, unresponsive, later} {
			rig.requireNoWakeUntil(t, sessionID, stored)
			rig.requireQuiet(t, sessionID)
		}
		rig.requireAcknowledged(t, seq)
		standIn.none(t, time.Second)
	})

	t.Run("i: a send to a stopped session wakes it from the route and once more after the wait", func(t *testing.T) {
		sessionID := "ses_l1_i_" + id.New()
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		rig.b.awaitSessionMachine(t, sessionID, aloneMachineA)
		rig.stopCleanly(t, rig.a, sessionID)

		status, body := callListener(t, rig.a.port, http.MethodPost, "/v1/messages/send",
			`{"target_session":"`+sessionID+`","message":"a message while the session is stopped"}`)
		var answer struct {
			EventID string `json:"event_id"`
			State   string `json:"state"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil || answer.State != "stopped" {
			t.Fatalf("the send to the stopped %s answered %d %s, want 200 with state stopped", sessionID, status, body)
		}
		subject := contracts.LegacyAgentSubject(sessionID)
		storedMsg, err := rig.publisher.JS().GetLastMsg(bus.Stream, subject)
		if err != nil {
			t.Fatalf("read the stored copy on %s: %v", subject, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(storedMsg.Data, &item); err != nil || item.EventID != answer.EventID {
			t.Fatalf("the stream holds %s on %s, want the sent event %s", storedMsg.Data, subject, answer.EventID)
		}
		stored := storedMsg.Time

		// The route reports the wake at once, off its answer; A reports one more after the wait.
		wakes := rig.awaitWakes(sessionID, 2, stored)
		var fromRoute, fromDelivery []aloneWake
		for _, wake := range wakes {
			if wake.stored.Sub(stored) < aloneWindow {
				fromRoute = append(fromRoute, wake)
			} else {
				fromDelivery = append(fromDelivery, wake)
			}
		}
		if len(fromRoute) != 1 {
			t.Errorf("the stream holds %d wake reports stored within %s of the send, want the route's one", len(fromRoute), aloneWindow)
		}
		requireWakes(t, fromDelivery, 1, stored)
		if got := countRecords(rig.a, "legion wake report published", func(record map[string]any) bool {
			return record["kind"] == wakeKindAgent && record["session"] == sessionID
		}); got != 1 {
			t.Errorf("the send route reported the wake of %s %d times, want once", sessionID, got)
		}
		requireOneWakeLine(t, "A", rig.a, item, sessionID)
		if got := aloneLines(rig.b, "listener agent wake reported", sessionID, ""); got != 0 {
			t.Errorf("listener B, whose row names A, reported the wake of %s %d times, want never", sessionID, got)
		}
		rig.requireQuiet(t, sessionID)
		if got := aloneReceived(rig.a, item.EventID); got != 2 {
			t.Errorf("listener A's durable delivered the message %d times, want 2: the first, which waited, and the one that reported the wake", got)
		}
		if t.Failed() {
			return
		}
		rig.requireAcknowledged(t, storedMsg.Sequence)
		counted := time.Now()
		durableA := bus.ListenerDurable(aloneMachineA)
		deliveredByA := func() uint64 {
			t.Helper()
			info, err := rig.publisher.JS().ConsumerInfo(bus.Stream, durableA)
			if err != nil {
				t.Fatalf("read the durable %s: %v", durableA, err)
			}
			return info.Delivered.Consumer
		}
		acknowledged := deliveredByA()

		standIn := startSessionStandIn(t, rig.uri, sessionID, answerDone)
		postListener(t, rig.a.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2"))
		requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
		t.Parallel()
		standIn.none(t, time.Until(counted.Add(10*time.Second)))
		if later := deliveredByA(); later != acknowledged {
			t.Errorf("listener A's durable had made %d deliveries when the message was acknowledged and %d ten seconds later, want no more", acknowledged, later)
		}
	})

	// Row g: the first report is refused by the server, so the stream does not store it. The
	// message comes back after the wait and its second report is stored.
	t.Run("g: a wake report the stream did not store is tried again after the wait", func(t *testing.T) {
		t.Parallel()
		server, uri := testnats.StartConfig(t, listenerGrant(bus.LegionWakeTopic), []string{"-js"}, false)
		uri = asListenerUser(t, uri)
		client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect the listener's user: %v", err)
		}
		t.Cleanup(client.Close)
		harness := newListenerDeliveryHarnessOn(t, client)
		sessionID := "ses_l1_g_" + id.New()
		refused := make(chan struct{})
		run, seen := startAloneDelivery(t, harness, sessionID, nil, func(meta *natsgo.MsgMetadata) {
			if meta.NumDelivered == 2 {
				close(refused)
			}
		})

		select {
		case <-refused:
		case <-time.After(15 * time.Second):
			count, _, _ := seen.seen()
			t.Fatalf("the handler saw deliveries %v within 15s, want a second one, whose report the server refuses", count)
		}
		if got := storedWakeCount(t, client); got != 0 {
			t.Fatalf("the stream holds %d wake reports while the grant leaves the wake subject out, want none", got)
		}
		if err := server.Reload(t, listenerGrant()); err != nil {
			t.Fatalf("give the listener's user the wake subject: %v", err)
		}
		seen.awaitSeen(t, 3, 15*time.Second)
		time.Sleep(3 * time.Second)

		count, at, seq := seen.seen()
		if len(count) != 3 || count[0] != 1 || count[1] != 2 || count[2] != 3 {
			t.Errorf("the handler saw deliveries %v, want 1, 2 and 3", count)
		}
		if len(at) == 3 && at[2].Sub(at[1]) < aloneWindow {
			t.Errorf("the message came back %s after the report the server refused, want at least the %s wait", at[2].Sub(at[1]), aloneWindow)
		}
		if got := storedWakeCount(t, client); got != 1 {
			t.Errorf("the stream holds %d wake reports, want the second report's one", got)
		}
		logs := harness.logs.String()
		if reports := deliveryRecords(t, logs, "listener agent wake reported"); len(reports) != 1 || reports[0].SessionID != sessionID || reports[0].EventID != run.item.EventID {
			t.Errorf("want one wake line naming %s and event %s, got %+v", sessionID, run.item.EventID, reports)
		}
		if got := deliveryRecords(t, logs, "listener agent wake not stored"); len(got) != 0 {
			t.Errorf("%d lines say a report was not stored, want none below the stream's last delivery", len(got))
		}
		if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
			t.Errorf("a delivery exception was published: %s", message.Data)
		}
		awaitDurableState(t, client, run.consumer, 0, seq)
	})

	// Row j: the read of a live session's record fails, so the listener cannot tell a live
	// session from a gone one. It sends the message back, says so once, and decides again.
	t.Run("j: an unanswered read of the record sends the message back and decides again", func(t *testing.T) {
		t.Parallel()
		// unreadRig is a delivery harness on a server of its own whose session bucket holds a live
		// record of sessionID that the harness's cache has not seen, with the bucket's reads denied.
		unreadRig := func(t *testing.T, sessionID string) (*testnats.Server, *bus.Client, listenerDeliveryHarness) {
			t.Helper()
			server, uri := testnats.StartConfig(t, listenerGrant(), []string{"-js"}, false)
			uri = asListenerUser(t, uri)
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's user: %v", err)
			}
			t.Cleanup(client.Close)
			harness := newListenerDeliveryHarnessOn(t, client)
			harness.sessions.StopWatch()
			registrar, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the registering listener's session registry: %v", err)
			}
			if err := registrar.Put(sessionID, session.SessionEntry{
				SelfSubscribed: true, MachineID: aloneNoListener, Delivery: session.DeliveryConsumer, Epoch: "e",
			}); err != nil {
				t.Fatalf("register %s: %v", sessionID, err)
			}
			if err := server.Reload(t, sessionReadDenied()); err != nil {
				t.Fatalf("leave the session bucket's reads out of the grant: %v", err)
			}
			return server, client, harness
		}
		unreadLines := func(t *testing.T, harness listenerDeliveryHarness, run unplaceableDeliveryRun, sessionID string) {
			t.Helper()
			lines := deliveryRecords(t, harness.logs.String(), "listener agent record unread")
			if len(lines) != 1 || lines[0].Level != "WARN" || lines[0].SessionID != sessionID || lines[0].EventID != run.item.EventID || lines[0].Error == "" {
				t.Errorf("want one WARN that the record of %s was not read, naming event %s and the read's error, got %+v", sessionID, run.item.EventID, lines)
			}
		}

		t.Run("the read answers with the live record", func(t *testing.T) {
			sessionID := "ses_l1_j_answers_" + id.New()
			server, client, harness := unreadRig(t, sessionID)
			third := make(chan struct{})
			run, seen := startAloneDelivery(t, harness, sessionID, nil, func(meta *natsgo.MsgMetadata) {
				if meta.NumDelivered == 3 {
					close(third)
				}
			})
			select {
			case <-third:
			case <-time.After(30 * time.Second):
				count, _, _ := seen.seen()
				t.Fatalf("the handler saw deliveries %v within 30s, want a third one while the read fails", count)
			}
			if got := storedWakeCount(t, client); got != 0 {
				t.Errorf("the stream holds %d wake reports while the read fails, want none", got)
			}
			if err := server.Reload(t, listenerGrant()); err != nil {
				t.Fatalf("give the session bucket's reads back: %v", err)
			}
			seen.awaitSeen(t, 4, 15*time.Second)
			time.Sleep(3 * time.Second)

			count, _, seq := seen.seen()
			if len(count) != 4 {
				t.Errorf("the handler saw deliveries %v, want 1 to 4: three while the read failed and the one it answered", count)
			}
			unreadLines(t, harness, run, sessionID)
			if got := storedWakeCount(t, client); got != 0 {
				t.Errorf("the stream holds %d wake reports for a live session, want none", got)
			}
			body := metricsBody(t, harness)
			if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
				t.Errorf("%s = %d, want 1: the read that answered found the live record", deliveredSeries("skipped"), got)
			}
			if message, err := run.exceptions.NextMsg(500 * time.Millisecond); err == nil {
				t.Errorf("a delivery exception was published: %s", message.Data)
			}
			awaitDurableState(t, client, run.consumer, 0, seq)
		})

		t.Run("the read still fails at the stream's last delivery", func(t *testing.T) {
			sessionID := "ses_l1_j_fails_" + id.New()
			_, client, harness := unreadRig(t, sessionID)
			// Every delivery before the last two is sent back at once, so the handler's first
			// sight of the message is the one before the stream's last.
			run, seen := startAloneDelivery(t, harness, sessionID, func(msg *natsgo.Msg, meta *natsgo.MsgMetadata) bool {
				if meta.NumDelivered < consumerMaxDeliver-1 {
					if err := msg.Nak(); err != nil {
						t.Errorf("send back an early delivery of %s: %v", sessionID, err)
					}
					return false
				}
				return true
			}, nil)
			seen.awaitSeen(t, 2, 45*time.Second)

			count, _, seq := seen.seen()
			if len(count) != 2 || count[0] != consumerMaxDeliver-1 || count[1] != consumerMaxDeliver {
				t.Errorf("the handler saw deliveries %v, want %d and %d", count, consumerMaxDeliver-1, consumerMaxDeliver)
			}
			unreadLines(t, harness, run, sessionID)
			if got := storedWakeCount(t, client); got != 1 {
				t.Errorf("the stream holds %d wake reports after the last delivery, want one", got)
			}
			if reports := deliveryRecords(t, harness.logs.String(), "listener agent wake reported"); len(reports) != 1 || reports[0].EventID != run.item.EventID {
				t.Errorf("want one wake line for event %s, got %+v", run.item.EventID, reports)
			}
			awaitDurableState(t, client, run.consumer, 0, seq)
		})

		// The handler first sees the message at the stream's last delivery: the test sends every
		// earlier delivery back itself, as happens when the earlier deliveries were sent back for
		// reports the stream did not store. So the first unanswered read is at the last delivery.
		// It still logs its one WARN with the read's error, and then the wake is reported.
		t.Run("the first unanswered read is at the stream's last delivery", func(t *testing.T) {
			sessionID := "ses_l1_j_last_" + id.New()
			_, client, harness := unreadRig(t, sessionID)
			run, seen := startAloneDelivery(t, harness, sessionID, func(msg *natsgo.Msg, meta *natsgo.MsgMetadata) bool {
				if meta.NumDelivered < consumerMaxDeliver {
					if err := msg.Nak(); err != nil {
						t.Errorf("send back an early delivery of %s: %v", sessionID, err)
					}
					return false
				}
				return true
			}, nil)
			seen.awaitSeen(t, 1, 45*time.Second)

			count, _, seq := seen.seen()
			if len(count) != 1 || count[0] != consumerMaxDeliver {
				t.Errorf("the handler saw deliveries %v, want only the stream's last, %d", count, consumerMaxDeliver)
			}
			unreadLines(t, harness, run, sessionID)
			if got := storedWakeCount(t, client); got != 1 {
				t.Errorf("the stream holds %d wake reports after the last delivery, want one", got)
			}
			if reports := deliveryRecords(t, harness.logs.String(), "listener agent wake reported"); len(reports) != 1 || reports[0].EventID != run.item.EventID {
				t.Errorf("want one wake line for event %s, got %+v", run.item.EventID, reports)
			}
			awaitDurableState(t, client, run.consumer, 0, seq)
		})
	})
}
