package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// errDelivererRefused is what a row has a seam of the session relay answer in place of its claim
// read or its copy's publish: the read or the publish did not answer.
var errDelivererRefused = errors.New("the row refused this call")

// delivererRig is a relayRig - the listener's /v1 routes, its session consumers relayed to the
// stand-ins of the sessions it serves - whose session registry judges every record at a clock the
// test moves (stateClock) and keeps a record past the gone horizon, as a deployment does, so a
// holder past the live window still has a record. The publish route's confirmation record is set,
// so a role publish goes through the route as a client's does. The rig is built as the role window
// rig is (role_window_unchanged_test.go, newWindowRig), less the role lane, which no row here
// publishes on.
//
// The session relay's two seams (relayClaimRead, relayCopy) count every call and answer nil unless
// a row refuses them, so a row reads how many claim reads the relay made; beforeCopy stops every
// takeover pass before its first copy while holdPassCopies is set, so a row reads the relay's own
// copy and no pass's.
type delivererRig struct {
	*relayRig
	clock *stateClock

	claimReads      atomic.Int64
	copies          atomic.Int64
	refuseClaimRead atomic.Bool
	refuseCopy      atomic.Bool
	holdPassCopies  atomic.Bool
}

func newDelivererRig(t *testing.T) *delivererRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	clock := &stateClock{}
	consumers := newSessionConsumerRigOn(t, client, url, session.WithSessionTTL(2*session.GoneHorizon), session.WithSessionClock(clock.Now))
	relays, err := startSessionRelays(consumers.deps.consumers, consumers.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	relays.setOwned(endlessTerm())
	consumers.deps.confirmed = newConfirmedRecord(t)
	rig := &delivererRig{relayRig: &relayRig{sessionConsumerRig: consumers, relays: relays, url: url}, clock: clock}
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
		relayClaimRead: func(string) error {
			rig.claimReads.Add(1)
			if rig.refuseClaimRead.Load() {
				return errDelivererRefused
			}
			return nil
		},
		relayCopy: func(string, uint64) error {
			rig.copies.Add(1)
			if rig.refuseCopy.Load() {
				return errDelivererRefused
			}
			return nil
		},
		beforeCopy: func(string, string, uint64) error {
			if rig.holdPassCopies.Load() {
				return errDelivererRefused
			}
			return nil
		},
	})
	return rig
}

// serveOK serves a POST of body to path on the rig's own /v1 routes and fails the test unless it
// answers 200; it returns the answer's body.
func (rig *delivererRig) serveOK(t *testing.T, path string, body any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, postJSON(path, string(encoded)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s %s: status %d, body %s; want 200", path, encoded, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.Bytes()
}

// claim has sessionID claim role outright through POST /v1/roles/set, as the process instance
// names (none when it is empty).
func (rig *delivererRig) claim(t *testing.T, sessionID, role, instance string) {
	t.Helper()
	grantRoleClaim(t, rig.mux, sessionID, role, instance, false)
}

// release has sessionID's process instance hand role back, and fails the test unless no claim of
// role stands after it (releaseRoleThrough).
func (rig *delivererRig) release(t *testing.T, sessionID, role, instance string) {
	t.Helper()
	releaseRoleThrough(t, rig.mux, rig.harness.registry, sessionID, role, instance)
}

// publishToRole publishes message to role's topic through the publish route, from no sender
// session, and returns its event id; it fails the test unless the route answers 200, which it
// does once the stream stored the forward on the holder's own subject.
func (rig *delivererRig) publishToRole(t *testing.T, role, message string) string {
	t.Helper()
	var answer struct {
		EventID string `json:"event_id"`
	}
	raw := rig.serveOK(t, "/v1/messages/publish", roleMessage(role, "", message))
	if err := json.Unmarshal(raw, &answer); err != nil || answer.EventID == "" {
		t.Fatalf("decode the publish's answer %s: %v", raw, err)
	}
	return answer.EventID
}

// storeDispatchMessage stores on sessionID's own subject the message Dispatch's API sends a
// session for a route to role: an envelope of source dispatch on the session's own topic whose
// role mark names role. It returns its event id.
func (rig *delivererRig) storeDispatchMessage(t *testing.T, sessionID, role string) string {
	t.Helper()
	item := listenerTestEnvelope(contracts.LegacyAgentSubject(sessionID), "dispatch-"+id.New())
	item.Source = "dispatch"
	item.Role = role
	rig.storeOn(t, sessionID, item)
	return item.EventID
}

// storeOn stores item on sessionID's own subject and returns its stream sequence.
func (rig *delivererRig) storeOn(t *testing.T, sessionID string, item contracts.Envelope) uint64 {
	t.Helper()
	ack, err := rig.harness.client.JS().Publish(contracts.LegacyAgentSubject(sessionID), marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store a message on %s's own subject: %v", sessionID, err)
	}
	return ack.Sequence
}

// storedOn is every message the stream holds on subject, in stream order (storedEnvelopes).
func (rig *delivererRig) storedOn(t *testing.T, subject string) []storedEnvelope {
	t.Helper()
	return storedEnvelopes(t, rig.harness.client, subject)
}

// lastSeqOf is the highest stream sequence among the messages of eventIDs on sessionID's own
// subject, failing the test when the stream holds any of them nowhere there.
func (rig *delivererRig) lastSeqOf(t *testing.T, sessionID string, eventIDs ...string) uint64 {
	t.Helper()
	held := rig.storedOn(t, contracts.LegacyAgentSubject(sessionID))
	var last uint64
	for _, eventID := range eventIDs {
		index := slices.IndexFunc(held, func(stored storedEnvelope) bool { return stored.item.EventID == eventID })
		if index < 0 {
			t.Fatalf("the stream holds no message of event %s on %s's own subject: %v", eventID, sessionID, held)
		}
		last = max(last, held[index].seq)
	}
	return last
}

// keptCopies is how many copies of eventID role's kept lane holds.
func (rig *delivererRig) keptCopies(t *testing.T, role, eventID string) int {
	t.Helper()
	return copiesOf(rig.storedOn(t, bus.KeptRoleSubject(role)), eventID)
}

// ackedThrough reports whether sessionID's consumer has every message through seq acknowledged.
func (rig *delivererRig) ackedThrough(sessionID string, seq uint64) bool {
	info, err := rig.consumers.Info(context.Background(), sessionID)
	return err == nil && info.AckFloor.Stream >= seq
}

// awaitAckedThrough waits up to within for sessionID's consumer to have every message through seq
// acknowledged, and reports whether it did.
func (rig *delivererRig) awaitAckedThrough(sessionID string, seq uint64, within time.Duration) bool {
	return testwait.Holds(within, 20*time.Millisecond, func() bool { return rig.ackedThrough(sessionID, seq) })
}

// relayLines is every line of the listener's log that names sessionID as its session, eventID as
// its event, and a holder: the session relay's line for a role message it copied to the role's
// kept lane rather than sent (takeOverFrame). Every line is the JSON of the listener's own
// logger, so one that does not decode fails the test.
func (rig *delivererRig) relayLines(t *testing.T, sessionID, eventID string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(rig.harness.logs.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the listener's log line %q: %v", line, err)
		}
		if _, names := record["holder"]; names && record["session_id"] == sessionID && record["event_id"] == eventID {
			lines = append(lines, record)
		}
	}
	return lines
}

// requireOneRelayLineEach fails the test unless the listener logged exactly one relay line per
// event of eventIDs for session, naming role and holder.
func requireOneRelayLineEach(t *testing.T, rig *delivererRig, session, role, holder string, eventIDs ...string) {
	t.Helper()
	for _, eventID := range eventIDs {
		lines := rig.relayLines(t, session, eventID)
		if len(lines) != 1 {
			t.Errorf("the listener logged %d lines naming session %s, event %s and a holder, want exactly one: %v", len(lines), session, eventID, lines)
			continue
		}
		if lines[0]["role"] != role || lines[0]["holder"] != holder {
			t.Errorf("the line for session %s and event %s names role %v and holder %v, want %s and %s: %v",
				session, eventID, lines[0]["role"], lines[0]["holder"], role, holder, lines[0])
		}
	}
}

// aSessionMessageIsDeliveredWithNoClaimRead is row 4 in the setting of the row that runs it: a
// message to sessionID that is not a role message, stored once the row's role messages are
// settled, reaches the session, and the session relay reads no role claim while it delivers it.
func aSessionMessageIsDeliveredWithNoClaimRead(t *testing.T, rig *delivererRig, sessionID string, heard *heardFrames) {
	t.Helper()
	t.Run("4 a message to the session that is not a role message is delivered with no claim read", func(t *testing.T) {
		before := rig.claimReads.Load()
		item, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
		if !heard.await(15*time.Second, item.EventID) {
			t.Fatalf("%s did not receive the message %s to itself within 15s: it received %v", sessionID, item.EventID, heard.all())
		}
		if reads := rig.claimReads.Load() - before; reads != 0 {
			t.Errorf("the session relay read a role claim %d times while it delivered a message that is no role message, want none", reads)
		}
	})
}

// relayCopyLine is the session relay's line for a role message it copied to the role's kept lane
// rather than sent.
const relayCopyLine = "listener session relay copied a role message to the role's kept lane: another session holds the role, or none does"

// requireOneCopyLine fails the test unless the listener logged exactly one relay copy line for
// the message stored at seq on session's own subject, naming role and holder.
func requireOneCopyLine(t *testing.T, rig *delivererRig, session, role, holder string, seq uint64) {
	t.Helper()
	lines := linesIn(rig.harness.logs.String(), relayCopyLine, map[string]any{"session_id": session, "stream_seq": float64(seq)})
	if len(lines) != 1 {
		t.Errorf("the listener logged %d copy lines for session %s's message at %d, want exactly one: %v", len(lines), session, seq, lines)
		return
	}
	if lines[0]["role"] != role || lines[0]["holder"] != holder {
		t.Errorf("the copy line for session %s's message at %d names role %v and holder %v, want %s and %q: %v", session, seq, lines[0]["role"], lines[0]["holder"], role, holder, lines[0])
	}
}

// requireNoKeptCopyEnded fails the test when the kept lane's walk ended a copy because no read
// can tell its role: its line (hourUndecodableLine) and its count (hourUndecodable), which every
// role message a row stores must be spared.
func requireNoKeptCopyEnded(t *testing.T, rig *delivererRig) {
	t.Helper()
	if lines := linesIn(rig.harness.logs.String(), hourUndecodableLine, nil); len(lines) != 0 {
		t.Errorf("the listener logged %q, want no such line: %v", hourUndecodableLine, lines)
	}
	if body := metricsBody(t, rig.harness); counterValue(t, body, hourUndecodable) != -1 {
		t.Errorf("%s has a series, want none:\n%s", hourUndecodable, body)
	}
}

// The listener delivers a role message to a session only while the role's claim names that
// session. A role message that reaches a session's relay while another session holds the role, or
// none does, is copied to the role's kept lane and acknowledged, and the session receives none
// of it; a holder that continues while it holds its role receives its role messages; and a claim
// read or a copy that does not answer leaves the message neither delivered nor acknowledged,
// until it answers. The relay tells a role message by its topic and its role mark alone, each by
// its exact key, so a role message that a read of its whole envelope refuses or misreads is still
// one, and it reaches the holder. A message whose role no read can tell is no role message, and
// the relay delivers it to the session whose subject holds it, as every message to the session
// that is no role message is, with no claim read.
//
// Each row is a listener of its own over an account of its own. A session is a stand-in of its
// process (startSessionStandIn) registered in consumer mode through the listener's own route, and
// a crash is its connection closing. The role messages of a row are two publishes to the role
// through the publish route, which the route stores on the holder's own subject, and one message
// as Dispatch's API sends a holder for a route to the role, whose role mark names the role. Rows 5a
// to 5e store messages on a session's own subject as bytes (storeFrames): role messages of every
// shape a reader of JSON can read wrongly (roleFramesOfEveryShape), and messages whose role no
// read can tell (framesNoReadCanTellTheRoleOf). Their sessions are processes that read each frame
// as bytes (hearMarks), so a frame that reached one is counted by its mark however little of it a
// reader of JSON takes, with the issued_at it carries.
func TestARoleMessageIsNotDeliveredToASessionWhoseRoleWasTaken(t *testing.T) {
	t.Parallel()
	t.Run("1 after a takeover the previous holder registers again and receives none of the role's messages, each reaches the new holder, one relay line per message", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			previous = "ses_deliverer_one_previous"
			next     = "ses_deliverer_one_next"
			role     = "deliverer-one"
		)
		first := startSessionStandIn(t, rig.url, previous, answerReceived)
		heardFirst := hear(t, first)
		rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
		rig.claim(t, previous, role, "")
		events := []string{
			rig.publishToRole(t, role, "first role message"),
			rig.publishToRole(t, role, "second role message"),
			rig.storeDispatchMessage(t, previous, role),
		}
		// The holder takes each and writes none: its consumer holds all three for an ack.
		if !heardFirst.await(15*time.Second, events...) {
			t.Fatalf("the holder %s received %v within 15s, want each of %v", previous, heardFirst.all(), events)
		}
		last := rig.lastSeqOf(t, previous, events...)
		first.close()

		nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
		heardNext := hear(t, nextStandIn)
		rig.subscribe(t, consumerRegistration(next, "epoch-1"))
		rig.claim(t, next, role, "")
		if !heardNext.await(30*time.Second, events...) {
			t.Errorf("the new holder %s received %v within 30s of its claim, want each of %v", next, heardNext.all(), events)
		}

		// The previous holder's session id registers again, with a new epoch, so its relay
		// restarts its consumer at the ack floor and has all three to judge again.
		awaitSessionConsumerStored(t, rig.harness.client.Conn, previous)
		second := startSessionStandIn(t, rig.url, previous, answerDone)
		heardSecond := hear(t, second)
		rig.subscribe(t, consumerRegistration(previous, "epoch-2"))
		if !rig.awaitAckedThrough(previous, last, 30*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 30s of its registration", previous, last)
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, previous, heardSecond)
		// The message to the session came after the three in stream order, so the relay had
		// judged all three when the session received it.
		requireNoneHeard(t, heardSecond, "the previous holder "+previous+" registered again", events...)
		requireOneRelayLineEach(t, rig, previous, role, next, events...)
	})

	t.Run("2 with no takeover a holder that continues after more than 5 minutes without a registration receives its role messages", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			holder = "ses_deliverer_two_holder"
			role   = "deliverer-two"
		)
		first := startSessionStandIn(t, rig.url, holder, answerReceived)
		heardFirst := hear(t, first)
		rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
		rig.claim(t, holder, role, "")
		events := []string{
			rig.publishToRole(t, role, "first role message"),
			rig.publishToRole(t, role, "second role message"),
			rig.storeDispatchMessage(t, holder, role),
		}
		if !heardFirst.await(15*time.Second, events...) {
			t.Fatalf("the holder %s received %v within 15s, want each of %v", holder, heardFirst.all(), events)
		}
		last := rig.lastSeqOf(t, holder, events...)
		first.close()

		rig.clock.advance(session.LiveWindow + 30*time.Second)
		if _, state := rig.harness.sessions.Lookup(holder); state != session.Unresponsive {
			t.Fatalf("the holder %s reads %s with its last registration more than %s old, want %s", holder, state, session.LiveWindow, session.Unresponsive)
		}

		awaitSessionConsumerStored(t, rig.harness.client.Conn, holder)
		second := startSessionStandIn(t, rig.url, holder, answerDone)
		heardSecond := hear(t, second)
		rig.subscribe(t, consumerRegistration(holder, "epoch-2"))
		if !heardSecond.await(30*time.Second, events...) {
			t.Errorf("the holder %s that continued received %v within 30s of its registration, want each of %v", holder, heardSecond.all(), events)
		}
		if !rig.awaitAckedThrough(holder, last, 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 15s of their delivery", holder, last)
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, holder, heardSecond)
	})

	t.Run("2b the new holder received the copies and released the role, the previous holder registers again and receives none of them, the kept lane holds one copy of each", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			previous     = "ses_deliverer_twob_previous"
			next         = "ses_deliverer_twob_next"
			nextInstance = "instance-twob-next"
			role         = "deliverer-twob"
		)
		first := startSessionStandIn(t, rig.url, previous, answerReceived)
		heardFirst := hear(t, first)
		rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
		rig.claim(t, previous, role, "")
		events := []string{
			rig.publishToRole(t, role, "first role message"),
			rig.publishToRole(t, role, "second role message"),
			rig.storeDispatchMessage(t, previous, role),
		}
		if !heardFirst.await(15*time.Second, events...) {
			t.Fatalf("the holder %s received %v within 15s, want each of %v", previous, heardFirst.all(), events)
		}
		last := rig.lastSeqOf(t, previous, events...)
		first.close()

		nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
		heardNext := hear(t, nextStandIn)
		rig.subscribe(t, consumerRegistration(next, "epoch-1"))
		rig.claim(t, next, role, nextInstance)
		if !heardNext.await(30*time.Second, events...) {
			t.Errorf("the new holder %s received %v within 30s of its claim, want each of %v", next, heardNext.all(), events)
		}
		rig.release(t, next, role, nextInstance)

		awaitSessionConsumerStored(t, rig.harness.client.Conn, previous)
		second := startSessionStandIn(t, rig.url, previous, answerDone)
		heardSecond := hear(t, second)
		rig.subscribe(t, consumerRegistration(previous, "epoch-2"))
		if !rig.awaitAckedThrough(previous, last, 30*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 30s of its registration", previous, last)
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, previous, heardSecond)
		requireNoneHeard(t, heardSecond, "the previous holder "+previous+" registered again", events...)
		for _, eventID := range events {
			if copies := rig.keptCopies(t, role, eventID); copies != 1 {
				t.Errorf("role %s's kept lane holds %d copies of event %s, want exactly one", role, copies, eventID)
			}
		}
	})

	t.Run("3a a claim read that fails neither delivers nor acknowledges the role message, which is delivered once the read answers", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			holder = "ses_deliverer_threea_holder"
			role   = "deliverer-threea"
		)
		standIn := startSessionStandIn(t, rig.url, holder, answerDone)
		heard := hear(t, standIn)
		rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
		rig.claim(t, holder, role, "")

		rig.refuseClaimRead.Store(true)
		eventID := rig.publishToRole(t, role, "while the claim cannot be read")
		seq := rig.lastSeqOf(t, holder, eventID)
		// Two refused reads: the frame was given back once and delivered to the relay again.
		if !testwait.Holds(listenerWaitBound(t), 20*time.Millisecond, func() bool { return rig.claimReads.Load() >= 2 }) {
			t.Errorf("the session relay read role %s's claim %d times in time of the message's store, want at least two", role, rig.claimReads.Load())
		}
		requireNoneHeard(t, heard, "the holder "+holder+" while its role's claim could not be read", eventID)
		if rig.ackedThrough(holder, seq) {
			t.Errorf("the consumer of %s acknowledged the role message at sequence %d while its role's claim could not be read", holder, seq)
		}

		rig.refuseClaimRead.Store(false)
		if !heard.await(30*time.Second, eventID) {
			t.Errorf("the holder %s did not receive event %s in time of its role's claim answering again: it received %v", holder, eventID, heard.all())
		}
		if !rig.awaitAckedThrough(holder, seq, 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role message at sequence %d in time of its delivery", holder, seq)
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, holder, heard)
	})

	t.Run("3b a copy whose publish fails neither delivers nor acknowledges the role message, which is copied once the publish answers", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			previous = "ses_deliverer_threeb_previous"
			next     = "ses_deliverer_threeb_next"
			role     = "deliverer-threeb"
		)
		// No takeover pass copies anything in this row, so the only copy of the message is the
		// relay's own.
		rig.holdPassCopies.Store(true)
		previousStandIn := startSessionStandIn(t, rig.url, previous, answerDone)
		heardPrevious := hear(t, previousStandIn)
		rig.subscribe(t, consumerRegistration(previous, "epoch-1"))
		rig.claim(t, previous, role, "")
		nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
		heardNext := hear(t, nextStandIn)
		rig.subscribe(t, consumerRegistration(next, "epoch-1"))
		rig.claim(t, next, role, "")

		// A forward that resolved the previous holder before the claim moved, stored on its own
		// subject after the claim, while the previous holder is still registered and reading.
		rig.refuseCopy.Store(true)
		late := listenerTestEnvelope(contracts.RoleTopicPrefix+role, roleForwardDedupePrefix+"late-forward-"+id.New())
		seq := rig.storeOn(t, previous, late)
		if !testwait.Holds(listenerWaitBound(t), 20*time.Millisecond, func() bool { return rig.copies.Load() >= 2 }) {
			t.Errorf("the session relay tried to copy the role message %d times in time of its store, want at least two", rig.copies.Load())
		}
		requireNoneHeard(t, heardPrevious, "the previous holder "+previous+" while the copy could not be stored", late.EventID)
		if rig.ackedThrough(previous, seq) {
			t.Errorf("the consumer of %s acknowledged the role message at sequence %d while its copy could not be stored", previous, seq)
		}
		if copies := rig.keptCopies(t, role, late.EventID); copies != 0 {
			t.Errorf("role %s's kept lane holds %d copies of event %s while the copy could not be stored, want none", role, copies, late.EventID)
		}

		rig.refuseCopy.Store(false)
		if !rig.awaitAckedThrough(previous, seq, 30*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role message at sequence %d in time of the copy's publish answering again", previous, seq)
		}
		if copies := rig.keptCopies(t, role, late.EventID); copies != 1 {
			t.Errorf("role %s's kept lane holds %d copies of event %s once the copy could be stored, want exactly one", role, copies, late.EventID)
		}
		if !heardNext.await(30*time.Second, late.EventID) {
			t.Errorf("the new holder %s did not receive event %s within 30s: it received %v", next, late.EventID, heardNext.all())
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, previous, heardPrevious)
		requireNoneHeard(t, heardPrevious, "the previous holder "+previous, late.EventID)
	})

	t.Run("5a role messages a strict read refuses or misreads, while another session holds the role, each reach the holder once from the role's kept lane and not the session", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			sessionID = "ses_deliverer_fivea_session"
			holder    = "ses_deliverer_fivea_holder"
			role      = "deliverer-fivea"
		)
		heard := hearMarks(t, rig.url, sessionID)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		heardHolder := hearMarks(t, rig.url, holder)
		rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
		rig.claim(t, holder, role, "")

		frames := roleFramesOfEveryShape(t, role)
		seqs := rig.storeFrames(t, sessionID, frames)
		if !rig.awaitAckedThrough(sessionID, seqs[len(seqs)-1], 30*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 30s of their store", sessionID, seqs[len(seqs)-1])
		}
		// Each copy the walk forwards is stored on the holder's own subject, and the holder's
		// relay reads the role's claim before it sends that frame. The holder has received
		// every copy, the role message stored after them and the message to itself below, before
		// row 4 counts claim reads, so none of those reads falls in its count.
		if !heardHolder.awaitMarks(30*time.Second, frames...) {
			t.Errorf("the holder %s did not receive every role message from role %s's kept lane within 30s: it received %v", holder, role, heardHolder.allMarks())
		}
		// A copy forwarded twice can reach the holder after the first copy of every frame has,
		// so the holder's counts below are read once it received a role message stored after
		// the frames, which takes their path behind them (laterRoleMessage) and so reaches it
		// after any such copy the relay or the walk made before that message's, and then a
		// message to itself stored once the handover ended, which reaches it after any the
		// handover made later (awaitHandoverReceived).
		later := laterRoleMessage(t, role)
		rig.storeFrames(t, sessionID, []markedFrame{later})
		if !heardHolder.awaitMarks(30*time.Second, later) {
			t.Errorf("the holder %s did not receive %q, stored after the frames, within 30s: it received %v", holder, later, heardHolder.allMarks())
		}
		rig.awaitHandoverReceived(t, role, holder, heardHolder)
		// The message to the session came after the role messages in stream order, so the relay
		// had judged each of them when the session received it.
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, sessionID, heard)
		for i, frame := range frames {
			t.Run("case: "+frame.shape, func(t *testing.T) {
				if n := heard.countMark(frame.mark); n != 0 {
					t.Errorf("the session %s, which does not hold role %s, received %q %d times, want none: it received %v", sessionID, role, frame, n, heard.allMarks())
				}
				// The holder answers each frame done at once, so its consumer sends a frame again
				// only after its ack wait (bus.SessionConsumerAckWait, 60s) passes with no
				// acknowledgement: a second frame here is a second copy forwarded, which the
				// copy's message id rules out, and it would have reached the holder before the
				// role message stored after the frames did.
				if n := heardHolder.countMark(frame.mark); n != 1 {
					t.Errorf("the holder %s received %q %d times, want once: it received %v", holder, frame, n, heardHolder.allMarks())
				}
				heardHolder.requireIssuedAt(t, "the holder "+holder, frame)
				if marked, _ := keptMarks(t, rig.harness.client, role, frame.mark); marked != 1 {
					t.Errorf("role %s's kept lane holds %d copies of %q, want exactly one", role, marked, frame)
				}
				requireOneCopyLine(t, rig, sessionID, role, holder, seqs[i])
			})
		}
		requireNoKeptCopyEnded(t, rig)
	})

	t.Run("5b role messages a strict read refuses or misreads, while the session holds the role, are delivered to it as before", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			holder = "ses_deliverer_fiveb_holder"
			role   = "deliverer-fiveb"
		)
		heard := hearMarks(t, rig.url, holder)
		rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
		rig.claim(t, holder, role, "")

		frames := roleFramesOfEveryShape(t, role)
		seqs := rig.storeFrames(t, holder, frames)
		if !heard.awaitMarks(15*time.Second, frames...) {
			t.Errorf("the holder %s did not receive each of its role messages within 15s: it received %v", holder, heard.allMarks())
		}
		if !rig.awaitAckedThrough(holder, seqs[len(seqs)-1], 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 15s of their delivery", holder, seqs[len(seqs)-1])
		}
		if _, held := keptMarks(t, rig.harness.client, role, ""); held != 0 {
			t.Errorf("role %s's kept lane holds %d messages, want none: the claim names the session", role, held)
		}
		if body := metricsBody(t, rig.harness); counterValue(t, body, hourUndecodable) != -1 {
			t.Errorf("%s has a series, want none:\n%s", hourUndecodable, body)
		}
	})

	t.Run("5c messages whose role no read can tell, while another session holds a role, reach the session as every message that is no role message does, with no claim read", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			sessionID = "ses_deliverer_fivec_session"
			holder    = "ses_deliverer_fivec_holder"
			role      = "deliverer-fivec"
		)
		heard := hearMarks(t, rig.url, sessionID)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		heardHolder := hearMarks(t, rig.url, holder)
		rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
		rig.claim(t, holder, role, "")

		frames := framesNoReadCanTellTheRoleOf(t)
		before := rig.claimReads.Load()
		seqs := rig.storeFrames(t, sessionID, frames)
		if !heard.awaitMarks(15*time.Second, frames...) {
			t.Errorf("the session %s did not receive each of its messages within 15s: it received %v", sessionID, heard.allMarks())
		}
		if !rig.awaitAckedThrough(sessionID, seqs[len(seqs)-1], 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the messages through sequence %d within 15s of their delivery", sessionID, seqs[len(seqs)-1])
		}
		if reads := rig.claimReads.Load() - before; reads != 0 {
			t.Errorf("the session relay read a role claim %d times for messages whose role no read can tell, want none", reads)
		}
		if _, held := keptMarks(t, rig.harness.client, role, ""); held != 0 {
			t.Errorf("role %s's kept lane holds %d messages, want none", role, held)
		}
		for _, frame := range frames {
			t.Run("case: "+frame.shape, func(t *testing.T) {
				// The session answers each frame done at once, so its consumer sends a frame
				// again only after its ack wait (bus.SessionConsumerAckWait, 60s) passes with no
				// acknowledgement: the waits above end well inside it.
				if n := heard.countMark(frame.mark); n != 1 {
					t.Errorf("the session %s received %q %d times, want once: it received %v", sessionID, frame, n, heard.allMarks())
				}
				if n := heardHolder.countMark(frame.mark); n != 0 {
					t.Errorf("the holder %s received %q %d times, want none: it received %v", holder, frame, n, heardHolder.allMarks())
				}
			})
		}
	})

	t.Run("5e a session's own frames that are no role message - plain text, a topic that is a JSON object, and one nested 10,001 deep - reach it, with no role claimed anywhere", func(t *testing.T) {
		rig := newDelivererRig(t)
		const sessionID = "ses_deliverer_fivee_session"
		heard := hearMarks(t, rig.url, sessionID)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

		all := framesNoReadCanTellTheRoleOf(t)
		own := listenerTestEnvelope(contracts.AgentTopicPrefix+sessionID, "nested-deep-"+id.New())
		deep := markedFrame{shape: "a message to the session nested 10,001 deep", mark: frameMark()}
		own.PayloadSummary = deep.String()
		deep.data = nestedDeep(marshalListenerEnvelope(t, own))
		frames := []markedFrame{all[0], all[2], deep}
		before := rig.claimReads.Load()
		seqs := rig.storeFrames(t, sessionID, frames)
		if !heard.awaitMarks(15*time.Second, frames...) {
			t.Errorf("the session %s did not receive each of its messages within 15s: it received %v", sessionID, heard.allMarks())
		}
		if !rig.awaitAckedThrough(sessionID, seqs[len(seqs)-1], 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the messages through sequence %d within 15s of their delivery", sessionID, seqs[len(seqs)-1])
		}
		if reads := rig.claimReads.Load() - before; reads != 0 {
			t.Errorf("the session relay read a role claim %d times for messages that are no role message, want none", reads)
		}
		for _, frame := range frames {
			t.Run("case: "+frame.shape, func(t *testing.T) {
				if n := heard.countMark(frame.mark); n != 1 {
					t.Errorf("the session %s received %q %d times, want once: it received %v", sessionID, frame, n, heard.allMarks())
				}
			})
		}
		requireNoKeptCopyEnded(t, rig)
	})

	t.Run("5d role messages of a role no session holds reach no session: the relay copies each to the role's kept lane", func(t *testing.T) {
		rig := newDelivererRig(t)
		const (
			sessionID = "ses_deliverer_fived_session"
			role      = "deliverer-fived"
		)
		heard := hearMarks(t, rig.url, sessionID)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

		valid := listenerTestEnvelope(contracts.RoleTopicPrefix+role, roleForwardDedupePrefix+"no-holder-"+id.New())
		ruled := markedFrame{shape: "a role message the listener's rules take", mark: frameMark()}
		valid.PayloadSummary = ruled.String()
		ruled.data = marshalListenerEnvelope(t, valid)
		frames := []markedFrame{ruled, roleFramesOfEveryShape(t, role)[0]}
		seqs := rig.storeFrames(t, sessionID, frames)
		if !rig.awaitAckedThrough(sessionID, seqs[len(seqs)-1], 15*time.Second) {
			t.Errorf("the consumer of %s did not acknowledge the role messages through sequence %d within 15s of their store", sessionID, seqs[len(seqs)-1])
		}
		aSessionMessageIsDeliveredWithNoClaimRead(t, rig, sessionID, heard)
		for i, frame := range frames {
			t.Run("case: "+frame.shape, func(t *testing.T) {
				if n := heard.countMark(frame.mark); n != 0 {
					t.Errorf("the session %s, while no session holds role %s, received %q %d times, want none: it received %v", sessionID, role, frame, n, heard.allMarks())
				}
				if marked, _ := keptMarks(t, rig.harness.client, role, frame.mark); marked != 1 {
					t.Errorf("role %s's kept lane holds %d copies of %q, want exactly one", role, marked, frame)
				}
				requireOneCopyLine(t, rig, sessionID, role, "", seqs[i])
			})
		}
		requireNoKeptCopyEnded(t, rig)
	})
}

// relayBenchSession and relayBenchRole are the session the relay benchmark delivers to and the
// role that session holds.
const (
	relayBenchSession = "ses_relay_bench"
	relayBenchRole    = "relay-bench"
)

// relayBenchFrameWait bounds one message's wait for its frame; a frame that has not come by then
// fails the benchmark rather than timing a lost frame.
const relayBenchFrameWait = 10 * time.Second

// relayBench is one listener's /v1 routes and session relays over the package's NATS server, as
// newSessionConsumerRigOn and newRelayRig build them for a test, with a stand-in of one session's
// process on its relay subject: it answers every frame received and done as the plugin does, and
// hands on the frame's stream sequence (frames). The session is registered in consumer mode and
// holds relayBenchRole.
type relayBench struct {
	client *bus.Client
	frames chan uint64
}

func newRelayBench(b *testing.B) *relayBench {
	b.Helper()
	url := testnats.URL(b)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		b.Fatalf("connect the listener: %v", err)
	}
	b.Cleanup(client.Close)
	quiet := slog.New(slog.DiscardHandler)
	registry, err := store.Open(b.Context(), client.Conn, store.WithReplicas(1), store.WithLogger(quiet))
	if err != nil {
		b.Fatalf("open the interest registry: %v", err)
	}
	b.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(b.Context(), client.Conn, session.WithSessionReplicas(1), session.WithSessionLogger(quiet))
	if err != nil {
		b.Fatalf("open the session registry: %v", err)
	}
	b.Cleanup(sessions.StopWatch)
	ready, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := registry.WaitForCacheReady(ready); err != nil {
		b.Fatalf("wait for the registry's caches: %v", err)
	}
	if err := sessions.WaitForCacheReady(ready); err != nil {
		b.Fatalf("wait for the session registry's cache: %v", err)
	}
	met := metrics.New()
	logger := logging.NewWithWriter("relay-bench", io.Discard)
	// The configuration newListenerDeliveryHarnessOn builds, which takes a *testing.T.
	cfg := wireRoleForwarder(listenerDeliveryHandlerConfig{
		client:              client,
		forwardRole:         startRoleForward(client),
		registry:            registry,
		sessions:            sessions,
		machineID:           "test-machine",
		ledger:              testDeliveryLedger(b, client.Conn, "test-machine", "boot-bench"),
		logger:              logger,
		messagesReceived:    met.NewCounter("bench_messages_received", "bench"),
		messagesDelivered:   met.NewCounter("bench_messages_delivered", "bench"),
		messagesNAKed:       met.NewCounter("bench_messages_naked", "bench"),
		deliveryDuration:    met.NewHistogram("bench_delivery_duration", "bench", metrics.DefaultBuckets),
		roleKeptExpired:     met.NewCounter("bench_role_kept_expired", "bench"),
		roleDeadLetters:     met.NewCounter("bench_role_dead_letters", "bench"),
		roleKeptUndecodable: met.NewCounter("bench_role_kept_undecodable", "bench"),
		publishDeadLetter:   client.PublishStoredAs,
		keepPublish:         client.PublishStoredAck,
		deferredUnplaceable: newUnplaceableDeferrals(),
		roleKeepDrains:      newRoleKeepGuards(),
		now:                 time.Now,
		consumerGrant:       &consumerGrantAnswer{},
	})
	delivery, err := connectSessionDelivery([]string{url},
		inactiveThresholdFor(sessions, defaultSessionReapInterval, defaultSessionConsumerSweepInterval),
		client, logger)
	if err != nil {
		b.Fatalf("open the session-consumer connection: %v", err)
	}
	b.Cleanup(delivery.close)
	cfg.consumerGrant.set(delivery.holdsConsumerGrant)
	relays, err := startSessionRelays(delivery, cfg)
	if err != nil {
		b.Fatalf("start the session relays: %v", err)
	}
	b.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(stopCtx)
	})
	relays.setOwned(endlessTerm())
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, registry: registry, sessions: sessions, consumers: delivery, delivery: &cfg}, "test-machine", logger)

	bench := &relayBench{client: client, frames: make(chan uint64, 16)}
	standIn := testnats.Connect(b, url)
	b.Cleanup(standIn.Close)
	if _, err := standIn.Subscribe(bus.DeliverSubject(relayBenchSession), func(msg *natsgo.Msg) {
		if msg.Header.Get(bus.ConnectionProbeHeader) == "1" {
			_ = msg.Respond([]byte(relayReplyDone))
			return
		}
		seq, err := strconv.ParseUint(msg.Header.Get(relayHeaderStreamSeq), 10, 64)
		if err != nil {
			b.Errorf("a frame carries stream sequence %q: %v", msg.Header.Get(relayHeaderStreamSeq), err)
			return
		}
		bench.frames <- seq
		_ = standIn.Publish(msg.Reply, []byte(relayReplyReceived))
		_ = standIn.Publish(msg.Reply, []byte(relayReplyDone))
	}); err != nil {
		b.Fatalf("subscribe the stand-in to its relay subject: %v", err)
	}
	if err := standIn.Flush(); err != nil {
		b.Fatalf("flush the stand-in's subscription: %v", err)
	}
	serve := func(path, body string) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, postJSON(path, body))
		if recorder.Code != http.StatusOK {
			b.Fatalf("POST %s %s: status %d, body %s; want 200", path, body, recorder.Code, recorder.Body.String())
		}
	}
	serve("/v1/interests/subscribe", consumerRegistration(relayBenchSession, "epoch-1"))
	if recorder := postRoleClaim(mux, relayBenchSession, relayBenchRole, "", false); recorder.Code != http.StatusOK {
		b.Fatalf("claim %s: status %d, body %s; want 200", relayBenchRole, recorder.Code, recorder.Body.String())
	}
	return bench
}

// deliver times item, stored on the session's own subject, from the start of its store to its
// frame at the session's stand-in. One message is relayed before the timer starts, so the relay is
// pulling and the claim it reads is warm.
func (r *relayBench) deliver(b *testing.B, item contracts.Envelope) {
	data, err := json.Marshal(item)
	if err != nil {
		b.Fatal(err)
	}
	subject := contracts.LegacyAgentSubject(relayBenchSession)
	js := r.client.JS()
	roundTrip := func() {
		ack, err := js.Publish(subject, data)
		if err != nil {
			b.Fatalf("store the message on %s: %v", subject, err)
		}
		select {
		case seq := <-r.frames:
			if seq != ack.Sequence {
				b.Fatalf("the session received the frame of sequence %d, want %d", seq, ack.Sequence)
			}
		case <-time.After(relayBenchFrameWait):
			b.Fatalf("the session received no frame of sequence %d within %s", ack.Sequence, relayBenchFrameWait)
		}
	}
	roundTrip()
	b.ReportAllocs()
	for b.Loop() {
		roundTrip()
	}
}

// BenchmarkTheSessionRelayDeliversAFrame times the session relay's delivery of one message: from
// its store on the session's own subject to its frame at the session's stand-in, through the
// session's consumer and the relay, with the stand-in answering each frame received and done as
// the plugin does. The two kinds are a role message of a role the session holds - an envelope on
// the role's topic, as a forward to the role's holder is stored - and a message to the session
// itself. Each is relayed one at a time, so ns/op is one message's whole delivery.
func BenchmarkTheSessionRelayDeliversAFrame(b *testing.B) {
	bench := newRelayBench(b)
	b.Run("a role message of a role the session holds", func(b *testing.B) {
		bench.deliver(b, listenerTestEnvelope(contracts.RoleTopicPrefix+relayBenchRole, roleForwardDedupePrefix+"relay-bench"))
	})
	b.Run("a message to the session itself", func(b *testing.B) {
		bench.deliver(b, listenerTestEnvelope(contracts.LegacyAgentSubject(relayBenchSession), "relay-bench"))
	})
}
