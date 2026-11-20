package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// copiesHolder is the role's holder whose process stops (A), copiesTaker the different
	// session that takes the role from it (B), copiesRole the role it takes (R) and copiesOther a
	// role the holder keeps (S).
	copiesHolder = "ses_copies_holder"
	copiesTaker  = "ses_copies_taker"
	copiesRole   = "copies-role"
	copiesOther  = "copies-other"
	// copiesSender is the session a row's role messages name as their sender: a dead letter of
	// one of them would be stored on its own subject.
	copiesSender = "ses_copies_sender"
	// copiesReadMark begins the text of a role message the holder's stand-in acknowledges
	// (copiesHolderAnswer): a message the holder read.
	copiesReadMark = "read by the holder: "
	// copiesWatchBound bounds every wait of a row for a delivery: long enough for a claim's
	// handover on a loaded box, and at main, where no copy is made, the whole wait of a case row.
	copiesWatchBound = 30 * time.Second
	// copiesSettle is how long a watch goes on once the role's handover has ended, so a frame that
	// handover sent after the ones a row waits for is counted too.
	copiesSettle = 2 * time.Second
	// copiesSignalBound bounds a row's wait for a seam it set to be reached.
	copiesSignalBound = 15 * time.Second
)

// errCopiesRelayCopyHeld is what the session relay's own copy of a frame answers in these rows
// (copiesRelayCopyHeld).
var errCopiesRelayCopyHeld = errors.New("this row holds the session relay's own copy back, so every copy the new holder receives is the takeover pass's")

// copiesRelayCopyHeld is the relayCopy seam of a row about the takeover copy alone. The session
// relay copies a frame of role from a session whose role another session holds
// (sessionRelays.takeOverFrame); here that copy is refused, so the relay gives each such frame
// back to the server, and what the new holder receives is what the claim's pass and the sweep's
// passes copied (runTakeoverPasses). The relay's copy is the one-deliverer check's to show. The
// new holder's own frames are not touched: the claim names it, and a frame whose role's claim
// names its session is delivered with no copy.
func copiesRelayCopyHeld(role string) func(string, uint64) error {
	return func(of string, _ uint64) error {
		if of == role {
			return errCopiesRelayCopyHeld
		}
		return nil
	}
}

// copiesClock is the listener's clock (listenerDeliveryHandlerConfig.now): the wall clock, or the
// instant a row pins it to while a claim writes its takeover record.
type copiesClock struct {
	mu     sync.Mutex
	pinned time.Time
}

func (c *copiesClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinned.IsZero() {
		return time.Now()
	}
	return c.pinned
}

func (c *copiesClock) pin(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned = at
}

func (c *copiesClock) unpin() {
	c.pin(time.Time{})
}

// copiesRig is a relayRig - the listener's /v1 routes, its session consumers relayed to the
// stand-ins of the sessions it serves as main relays them - with two clocks a row can move: the
// session registry's (sessionClock), which judges a record live, unresponsive or gone on the
// compiled windows, and the listener's (listenerClock), which the relays copy when they start, so
// it is set before they do (startSessionRelays). The session bucket keeps a record past the gone
// horizon, as a deployment does, so a holder past the live window reads unresponsive and keeps
// its claim.
type copiesRig struct {
	*relayRig
	sessionClock  *stateClock
	listenerClock *copiesClock
}

func newCopiesRig(t *testing.T) *copiesRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	sessionClock := &stateClock{}
	consumers := newSessionConsumerRigOn(t, client, url, session.WithSessionTTL(2*session.GoneHorizon), session.WithSessionClock(sessionClock.Now))
	listenerClock := &copiesClock{}
	// The routes read the deps they were built over at each request, so the clock and the
	// publish route's confirmation record set here are the ones they use.
	consumers.deps.delivery.now = listenerClock.Now
	consumers.harness.config.now = listenerClock.Now
	consumers.deps.confirmed = newConfirmedRecord(t)
	relays, err := startSessionRelays(consumers.deps.consumers, *consumers.deps.delivery)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	// Set before the first relay starts, which gaining the binding is what does.
	relays.pauseFirst, relays.pauseMax, relays.receiptWait = relayPauseFirst, relayPauseMax, relayReceiptWait
	relays.setOwned(true)
	return &copiesRig{
		relayRig:      &relayRig{sessionConsumerRig: consumers, relays: relays, url: url},
		sessionClock:  sessionClock,
		listenerClock: listenerClock,
	}
}

// copiesHolderAnswer is how the holder's process answers its frames: received to each, so the
// relay never takes it for unreachable, and done as well to a frame whose text begins
// copiesReadMark, so its consumer acknowledges that one and holds every other unacknowledged.
func copiesHolderAnswer(frame relayedFrame) []string {
	if strings.HasPrefix(frame.item.PayloadSummary, copiesReadMark) {
		return []string{relayReplyReceived, relayReplyDone}
	}
	return []string{relayReplyReceived}
}

// startCopiesStandIn is startSessionStandIn whose stand-in answers nothing more, and whose
// connection drains, once t ends and before the stand-in's own cleanup closes that connection: a
// frame that arrives as a row ends - the holder's consumer goes on handing out what it holds - is
// taken with no reply, so no callback writes to a closed connection or reports to a test that has
// ended.
func startCopiesStandIn(t *testing.T, url, sessionID string, answer func(relayedFrame) []string) *sessionStandIn {
	t.Helper()
	standIn := startSessionStandIn(t, url, sessionID, answer)
	t.Cleanup(func() {
		standIn.silent.Store(true)
		if err := standIn.conn.Drain(); err == nil {
			testwait.Holds(5*time.Second, testwait.Poll, standIn.conn.IsClosed)
		}
	})
	return standIn
}

// startHolder starts sessionID's process, a stand-in on its relay subject answering as
// copiesHolderAnswer does, and registers it in consumer mode through the listener's own route.
func (rig *copiesRig) startHolder(t *testing.T, sessionID string) *sessionStandIn {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, sessionID, copiesHolderAnswer)
	rig.register(t, sessionID, nil)
	return standIn
}

// startTaker starts sessionID's process, a stand-in that writes every frame it receives (done),
// and registers it in consumer mode.
func (rig *copiesRig) startTaker(t *testing.T, sessionID string) *sessionStandIn {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, sessionID, answerDone)
	rig.register(t, sessionID, nil)
	return standIn
}

// crash stops sessionID's process as a crash stops it: its stand-in's connection closes and it
// registers no more, so its record stays as it last wrote it, inside the live window.
func (rig *copiesRig) crash(t *testing.T, sessionID string, standIn *sessionStandIn) {
	t.Helper()
	standIn.silent.Store(true)
	standIn.conn.Close()
	awaitNoRelaySubscriber(t, rig.url, sessionID)
}

// claim has sessionID claim role through POST /v1/roles/set, outright or soft, and fails the row
// unless it is granted.
func (rig *copiesRig) claim(t *testing.T, sessionID, role string, soft bool) {
	t.Helper()
	grantRoleClaim(t, rig.mux, sessionID, role, "", soft)
}

// publish publishes message to role's topic through the publish route, from sender (none when it
// is empty), and returns its event id. It fails the row unless the route answered 200 naming
// holder and stored the message for it, on holder's own subject.
func (rig *copiesRig) publish(t *testing.T, role, sender, message, holder string) string {
	t.Helper()
	body, err := json.Marshal(roleMessage(role, sender, message))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", string(body)))
	var answer struct {
		EventID string `json:"event_id"`
		Holder  string `json:"holder"`
		Kept    bool   `json:"kept"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the publish answer %s: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusOK || answer.Holder != holder || answer.Kept || answer.EventID == "" {
		t.Fatalf("publish %q to role %s: status %d, body %s; want 200 stored for its holder %s", message, role, recorder.Code, recorder.Body.String(), holder)
	}
	return answer.EventID
}

// sendToSession stores a message to sessionID itself on its own subject, as a send stores one,
// and returns its event id.
func (rig *copiesRig) sendToSession(t *testing.T, sessionID string) string {
	t.Helper()
	item, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	return item.EventID
}

// stored is every message the stream holds on subject, in stream order (storedEnvelopes).
func (rig *copiesRig) stored(t *testing.T, subject string) []storedEnvelope {
	t.Helper()
	return storedEnvelopes(t, rig.harness.client, subject)
}

// copiesStoredEvent is the message of eventID among held, and whether held has one.
func copiesStoredEvent(held []storedEnvelope, eventID string) (storedEnvelope, bool) {
	for _, message := range held {
		if message.item.EventID == eventID {
			return message, true
		}
	}
	return storedEnvelope{}, false
}

// keptCopiesOf is how many copies of eventID role's kept lane holds.
func (rig *copiesRig) keptCopiesOf(t *testing.T, role, eventID string) int {
	t.Helper()
	return copiesOf(rig.stored(t, bus.KeptRoleSubject(role)), eventID)
}

// resumeRecord is the floor the listener recorded for sessionID's consumer when it deleted it
// (the session resume lane), and whether the stream holds one.
func (rig *copiesRig) resumeRecord(t *testing.T, sessionID string) (bus.SessionResumeRecord, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := rig.harness.client.LastStoredMessage(ctx, bus.SessionResumeSubject(sessionID))
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return bus.SessionResumeRecord{}, false
	}
	if err != nil {
		t.Fatalf("read %s's resume record: %v", sessionID, err)
	}
	var record bus.SessionResumeRecord
	if err := json.Unmarshal(stored.Data, &record); err != nil {
		t.Fatalf("decode %s's resume record %s: %v", sessionID, stored.Data, err)
	}
	return record, true
}

// copiesSeqs is the stream sequence of each frame's event, failing the row unless frames carry
// each of eventIDs.
func copiesSeqs(t *testing.T, frames []relayedFrame, eventIDs ...string) map[string]uint64 {
	t.Helper()
	seqs := map[string]uint64{}
	for _, frame := range frames {
		seqs[frame.item.EventID] = frame.seq
	}
	for _, eventID := range eventIDs {
		if seqs[eventID] == 0 {
			t.Fatalf("the holder's process received no frame of %s: %v", eventID, frameEvents(frames))
		}
	}
	return seqs
}

// requireUnacknowledged fails the row unless sessionID's consumer stands and has acknowledged none
// of eventIDs: its ack floor is below each one's stream sequence (seqs).
func (rig *copiesRig) requireUnacknowledged(t *testing.T, sessionID string, seqs map[string]uint64, eventIDs ...string) {
	t.Helper()
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read %s's consumer: %v; want it standing", sessionID, err)
	}
	for _, eventID := range eventIDs {
		if info.AckFloor.Stream >= seqs[eventID] {
			t.Errorf("%s's consumer acknowledged everything through %d, so event %s at %d among it; want it held", sessionID, info.AckFloor.Stream, eventID, seqs[eventID])
		}
	}
}

// copiesReceived counts the frames a session received, by event id.
type copiesReceived map[string]int

// missing is each of want the session has not received.
func (r copiesReceived) missing(want []string) []string {
	var missing []string
	for _, eventID := range want {
		if r[eventID] == 0 {
			missing = append(missing, eventID)
		}
	}
	return missing
}

// takeOne counts into r the next frame standIn receives before until, and reports whether one
// came.
func (r copiesReceived) takeOne(standIn *sessionStandIn, until time.Time) bool {
	timer := time.NewTimer(max(time.Until(until), 0))
	defer timer.Stop()
	select {
	case frame := <-standIn.frames:
		r[frame.item.EventID]++
		return true
	case <-timer.C:
		return false
	}
}

// tallyFor counts every frame standIn receives for d.
func tallyFor(standIn *sessionStandIn, d time.Duration) copiesReceived {
	received := copiesReceived{}
	for until := time.Now().Add(d); received.takeOne(standIn, until); {
	}
	return received
}

// add counts more's frames into r.
func (r copiesReceived) add(more copiesReceived) {
	for eventID, n := range more {
		r[eventID] += n
	}
}

// handoverIdle reports whether no handover of role runs on this listener: none holds the role's
// guard (roleKeepGuards), which every handover - the claim route's, a keep's, a registration's,
// the sweep's - holds while it runs.
func (rig *copiesRig) handoverIdle(role string) bool {
	release, held := rig.deps.delivery.roleKeepDrains.tryHold(role)
	if held {
		release()
	}
	return held
}

// watch counts by event id every frame standIn receives: until each of want has arrived, then
// until no handover of role runs on this listener, then for copiesSettle more, all within
// copiesWatchBound. It is a watch that outlasts the handover, so a frame that handover sends after
// the ones a row waits for is counted too. At main, where nothing arrives, it watches the whole
// bound.
func (rig *copiesRig) watch(t *testing.T, standIn *sessionStandIn, role string, want ...string) copiesReceived {
	t.Helper()
	received := copiesReceived{}
	deadline := time.Now().Add(copiesWatchBound)
	for len(received.missing(want)) > 0 && received.takeOne(standIn, deadline) {
	}
	for !rig.handoverIdle(role) && time.Now().Before(deadline) {
		next := time.Now().Add(50 * time.Millisecond)
		if next.After(deadline) {
			next = deadline
		}
		received.takeOne(standIn, next)
	}
	received.add(tallyFor(standIn, copiesSettle))
	t.Logf("%s received, by event id: %v", role, received)
	return received
}

// awaitHandoverIdle waits for no handover of role to run on this listener, failing the row when
// one still runs after copiesWatchBound.
func (rig *copiesRig) awaitHandoverIdle(t *testing.T, role string) {
	t.Helper()
	if !testwait.Holds(copiesWatchBound, testwait.Poll, func() bool { return rig.handoverIdle(role) }) {
		t.Fatalf("a handover of role %s still runs after %s", role, copiesWatchBound)
	}
}

// returnAndReceive registers sessionID again under a new epoch, as its process coming back does,
// with a stand-in that answers each frame received, and returns what its consumer hands it: each
// of want, or what arrived within copiesWatchBound.
func (rig *copiesRig) returnAndReceive(t *testing.T, sessionID string, want ...string) copiesReceived {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, sessionID, func(relayedFrame) []string { return []string{relayReplyReceived} })
	rig.register(t, sessionID, map[string]any{"delivery": map[string]any{"consumer": true, "epoch": "epoch-returned-" + sessionID}})
	received := copiesReceived{}
	deadline := time.Now().Add(copiesWatchBound)
	for len(received.missing(want)) > 0 && received.takeOne(standIn, deadline) {
	}
	return received
}

// copiesAwait reports whether signal fired within copiesSignalBound.
func copiesAwait(signal <-chan struct{}) bool {
	timer := time.NewTimer(copiesSignalBound)
	defer timer.Stop()
	select {
	case <-signal:
		return true
	case <-timer.C:
		return false
	}
}

// copiesSignal is a seam's signal to the row: a send that never blocks, which a row reads once.
func copiesSignal(signal chan struct{}) {
	select {
	case signal <- struct{}{}:
	default:
	}
}

// sweepPass runs one pass of the kept lane's sweep over the listener's configuration, as the
// sweep's loop runs each (sweepOnce), which also runs the passes of every takeover record that is
// not finished.
func (rig *copiesRig) sweepPass() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sweepOnce(ctx, *rig.deps.delivery)
}

// copiesRequireEach fails the row for each of eventIDs who did not receive.
func copiesRequireEach(t *testing.T, received copiesReceived, who string, eventIDs ...string) {
	t.Helper()
	if missing := received.missing(eventIDs); len(missing) > 0 {
		t.Errorf("%s received none of %v; it received %v; want each of %v at least once", who, missing, received, eventIDs)
	}
}

// copiesRequireNone fails the row for each of eventIDs who received.
func copiesRequireNone(t *testing.T, received copiesReceived, who string, eventIDs ...string) {
	t.Helper()
	for _, eventID := range eventIDs {
		if n := received[eventID]; n != 0 {
			t.Errorf("%s received event %s %d times; want none", who, eventID, n)
		}
	}
}

// copiesHeldForward is the role lane's handler over a copy of the listener's delivery
// configuration whose forward of one event waits, once its publish has started (forwardRole),
// until the row releases it: a forward that resolved the role's holder and that the stream stores
// later.
type copiesHeldForward struct {
	deliver natsgo.MsgHandler
	started chan struct{}
	release func()
}

func (rig *copiesRig) holdRoleLaneForward(t *testing.T, eventID string) copiesHeldForward {
	t.Helper()
	cfg := *rig.deps.delivery
	forward := cfg.forwardRole
	started, released := make(chan struct{}), make(chan struct{})
	var start, release sync.Once
	cfg.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
		if item.EventID == eventID {
			start.Do(func() { close(started) })
			<-released
		}
		return forward(subject, item, msgID, timeout)
	}
	cfg = wireTestRoleForwarder(t, cfg)
	releaseForward := func() { release.Do(func() { close(released) }) }
	t.Cleanup(releaseForward)
	return copiesHeldForward{deliver: coreNATSDeliveryHandler(cfg), started: started, release: releaseForward}
}

// A holder of role R crashes inside its live window, and a different session B takes R. Each
// role message of R stored for the holder that its consumer had not acknowledged reaches B at
// least once, counted by event id over a watch that outlasts the claim's handover; B receives no
// message the holder acknowledged, no role message of another role S the holder keeps, and no
// message to the holder itself, and the holder's consumer still holds those.
//
// Every message reaches the holder's own subject as production stores it: a role message
// through the publish route, while the holder is the role's live holder, a message to the session
// on that subject, and a role-lane forward through the role lane's handler. The holder's process
// is a stand-in that answers each frame received, and done to the ones it read; a crash closes
// its connection. The session relay's own copy of a frame whose role another session holds is
// held back in each row where the holder's relay runs (copiesRelayCopyHeld), so what B receives
// is what the takeover passes copied.
//
// The case rows fail at main, where no copy is made and B receives none of the role messages;
// the control rows - what B must not receive, what the holder's consumer still holds, what is
// not copied and the dead letter that is not sent - pass there and with the change. Row 5 as the
// verification writes it holds only controls; its case is the role message stored inside the
// hour before the same takeover, which reaches B, so the row shows the pass ran. Row 8 stores on
// the holder's subject a role message the listener's wire rules refuse and one JSON does not read
// as an envelope: the pass copies the first, which reaches B, and not the second.
func TestADifferentSessionThatTakesARoleReceivesTheUnreadRoleMessages(t *testing.T) {
	t.Run("1 an outright claim by a different session after the holder crashed inside its live window", func(t *testing.T) {
		copiesUnreadRoleMessagesRow(t, false)
	})

	t.Run("2 a soft claim by a different session after the holder's renewal lapsed", func(t *testing.T) {
		copiesUnreadRoleMessagesRow(t, true)
	})

	t.Run("3 a forward that resolved the previous holder before the claim moved and was stored after the claim's pass", func(t *testing.T) {
		rig := newCopiesRig(t)
		passWrote := make(chan struct{}, 1)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			relayCopy: copiesRelayCopyHeld(copiesRole),
			beforeRecordWrite: func(role, previous string, _ uint64, _ bool) {
				if role == copiesRole && previous == copiesHolder {
					copiesSignal(passWrote)
				}
			},
		})
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		rig.crash(t, copiesHolder, holder)

		// The role lane resolves the holder, which is live, and starts its forward; the forward
		// waits there until the claim's pass and its handover have ended.
		late := listenerTestEnvelope(contracts.RoleTopicPrefix+copiesRole, "copies-late-"+id.New())
		lane := rig.holdRoleLaneForward(t, late.EventID)
		lane.deliver(&natsgo.Msg{Data: marshalListenerEnvelope(t, late)})
		if !copiesAwait(lane.started) {
			t.Fatalf("the role lane started no forward of %s to %s within %s", late.EventID, copiesHolder, copiesSignalBound)
		}

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		passRan := copiesAwait(passWrote)
		if passRan {
			rig.awaitHandoverIdle(t, copiesRole)
		}
		lane.release()
		agent := contracts.LegacyAgentSubject(copiesHolder)
		if !testwait.Holds(copiesWatchBound, 50*time.Millisecond, func() bool {
			_, ok := copiesStoredEvent(rig.stored(t, agent), late.EventID)
			return ok
		}) {
			t.Fatalf("the stream holds no forward of %s on %s after %s", late.EventID, agent, copiesWatchBound)
		}
		if before := tallyFor(taker, copiesSettle); before[late.EventID] != 0 {
			t.Fatalf("%s received %s before the sweep's next pass ran, %v: the row's order did not hold", copiesTaker, late.EventID, before)
		}

		rig.sweepPass()
		received := rig.watch(t, taker, copiesRole, late.EventID)

		t.Run("case: it reaches the new holder by the sweep's next pass", func(t *testing.T) {
			if !passRan {
				t.Errorf("no takeover pass of %s from %s wrote its record within %s of the claim, so the forward was not stored after one", copiesRole, copiesHolder, copiesSignalBound)
			}
			copiesRequireEach(t, received, copiesTaker, late.EventID)
		})
	})

	t.Run("4 more than roleKeepScanMax messages to the session with role messages of the role behind them", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(copiesRole)})
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		ahead := roleKeepScanMax + 50
		for range ahead {
			rig.sendToSession(t, copiesHolder)
		}
		var behind []string
		for i := range 3 {
			behind = append(behind, rig.publish(t, copiesRole, "", fmt.Sprintf("a role message behind %d messages to the session, %d", ahead, i+1), copiesHolder))
		}
		copiesSeqs(t, holder.collect(t, ahead+len(behind), copiesWatchBound), behind...)
		rig.crash(t, copiesHolder, holder)

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		received := rig.watch(t, taker, copiesRole, behind...)

		t.Run("case: each role message reaches the new holder", func(t *testing.T) {
			copiesRequireEach(t, received, copiesTaker, behind...)
		})
	})

	t.Run("5 a role message stored more than roleKeepWindow before the takeover", func(t *testing.T) {
		rig := newCopiesRig(t)
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		old := rig.publish(t, copiesRole, copiesSender, "a role message stored more than an hour before the takeover", copiesHolder)
		// Only so the two store times are apart by more than a millisecond, the claim time's
		// resolution.
		time.Sleep(50 * time.Millisecond)
		fresh := rig.publish(t, copiesRole, copiesSender, "a role message stored inside the hour before the takeover", copiesHolder)
		copiesSeqs(t, holder.collect(t, 2, copiesWatchBound), old, fresh)
		rig.crash(t, copiesHolder, holder)

		// The takeover's time is read on the listener's clock as its record is written
		// (publishTakeover): the clock is pinned one roleKeepWindow after the moment halfway
		// between the two store times, so the old message was stored more than roleKeepWindow
		// before the takeover and the fresh one less, and back on the wall clock before the claim
		// write, so every walk after it judges its copies as production does.
		held := rig.stored(t, contracts.LegacyAgentSubject(copiesHolder))
		oldStored, okOld := copiesStoredEvent(held, old)
		freshStored, okFresh := copiesStoredEvent(held, fresh)
		if !okOld || !okFresh || freshStored.at.Sub(oldStored.at) < 10*time.Millisecond {
			t.Fatalf("the holder's subject holds the old message at %v (%v) and the fresh one at %v (%v); want both, 10ms or more apart", oldStored.at, okOld, freshStored.at, okFresh)
		}
		takeoverAt := oldStored.at.Add(freshStored.at.Sub(oldStored.at) / 2).Add(roleKeepWindow)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			relayCopy: copiesRelayCopyHeld(copiesRole),
			beforeClaimTry: func(role, _ string) error {
				if role == copiesRole {
					rig.listenerClock.unpin()
				}
				return nil
			},
		})
		taker := rig.startTaker(t, copiesTaker)
		rig.listenerClock.pin(takeoverAt)
		rig.claim(t, copiesTaker, copiesRole, false)
		rig.listenerClock.unpin()
		received := rig.watch(t, taker, copiesRole, fresh)
		// A walk that drops a copy past its hour sends its dead letter; the sweep's pass is one
		// more walk of the lane, and one more pass of the takeover record.
		rig.sweepPass()
		received.add(tallyFor(taker, copiesSettle))

		t.Run("case: the role message stored inside the hour before the takeover reaches the new holder", func(t *testing.T) {
			copiesRequireEach(t, received, copiesTaker, fresh)
		})
		t.Run("control: the role message stored more than roleKeepWindow before the takeover is not copied", func(t *testing.T) {
			copiesRequireNone(t, received, copiesTaker, old)
			if copies := rig.keptCopiesOf(t, copiesRole, old); copies != 0 {
				t.Errorf("role %s's kept lane holds %d copies of %s; want none", copiesRole, copies, old)
			}
		})
		t.Run("control: no dead letter is sent", func(t *testing.T) {
			subject := contracts.LegacyAgentSubject(copiesSender)
			for _, message := range rig.stored(t, subject) {
				t.Errorf("the stream holds %s (dedupe key %q, %q) on the sender's subject %s; want no dead letter", message.item.EventID, message.item.DedupeKey, message.item.PayloadSummary, subject)
			}
		})
	})

	t.Run("6 the previous holder's consumer was deleted at a clean deregistration after it acknowledged two role messages", func(t *testing.T) {
		rig := newCopiesRig(t)
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		read := []string{
			rig.publish(t, copiesRole, "", copiesReadMark+"the first role message the holder acknowledged", copiesHolder),
			rig.publish(t, copiesRole, "", copiesReadMark+"the second role message the holder acknowledged", copiesHolder),
		}
		var unread []string
		for i := range 3 {
			unread = append(unread, rig.publish(t, copiesRole, "", fmt.Sprintf("unread role message %d", i+1), copiesHolder))
		}
		seqs := copiesSeqs(t, holder.collect(t, 5, copiesWatchBound), append(read, unread...)...)
		awaitAckState(t, rig.consumers, copiesHolder, len(unread), seqs[read[1]])
		// A clean stop: the session deregisters, and its process ends.
		rig.deregister(t, copiesHolder, "")
		holder.conn.Close()
		rig.awaitNoConsumer(t, copiesHolder)
		record, recorded := rig.resumeRecord(t, copiesHolder)
		if !recorded || record.Floor != seqs[read[1]] || record.StreamID != rig.streamID(t) {
			t.Fatalf("%s's resume record is %+v (recorded %v); want the floor %d of the stream %s", copiesHolder, record, recorded, seqs[read[1]], rig.streamID(t))
		}

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		received := rig.watch(t, taker, copiesRole, unread...)

		t.Run("case: the new holder receives each role message the previous holder did not acknowledge", func(t *testing.T) {
			copiesRequireEach(t, received, copiesTaker, unread...)
		})
		t.Run("control: the new holder receives neither of the two the previous holder acknowledged", func(t *testing.T) {
			copiesRequireNone(t, received, copiesTaker, read...)
		})
	})

	t.Run("6b the previous holder's consumer was removed by the server with no resume record", func(t *testing.T) {
		rig := newCopiesRig(t)
		passWrote := make(chan struct{}, 1)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			beforeRecordWrite: func(role, previous string, _ uint64, _ bool) {
				if role == copiesRole && previous == copiesHolder {
					copiesSignal(passWrote)
				}
			},
		})
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		read := rig.publish(t, copiesRole, "", copiesReadMark+"the role message the holder acknowledged", copiesHolder)
		var unread []string
		for i := range 2 {
			unread = append(unread, rig.publish(t, copiesRole, "", fmt.Sprintf("unread role message %d", i+1), copiesHolder))
		}
		seqs := copiesSeqs(t, holder.collect(t, 3, copiesWatchBound), append([]string{read}, unread...)...)
		awaitAckState(t, rig.consumers, copiesHolder, len(unread), seqs[read])
		rig.crash(t, copiesHolder, holder)
		// The server's removal of an inactive consumer: a delete of the consumer itself, which
		// records no floor.
		name, _ := bus.SessionConsumerName(copiesHolder)
		if err := rig.harness.client.JS().DeleteConsumer(bus.Stream, name); err != nil {
			t.Fatalf("delete %s's consumer %s: %v", copiesHolder, name, err)
		}
		rig.awaitNoConsumer(t, copiesHolder)
		if record, recorded := rig.resumeRecord(t, copiesHolder); recorded {
			t.Fatalf("%s has a resume record %+v; want none", copiesHolder, record)
		}

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		// The claim's pass finds neither the consumer nor a record of its floor, writes its record
		// so, and leaves the next read of the consumer to the sweep's next pass, which starts from
		// the hour before the claim: the floor is unknown. That pass takes the role's guard only
		// once the claim's handover has given it back.
		if copiesAwait(passWrote) {
			rig.awaitHandoverIdle(t, copiesRole)
		}
		rig.sweepPass()
		all := append([]string{read}, unread...)
		received := rig.watch(t, taker, copiesRole, all...)

		t.Run("case: every role message of the last hour reaches the new holder, the one the previous holder read included", func(t *testing.T) {
			copiesRequireEach(t, received, copiesTaker, all...)
		})
	})

	t.Run("7 the read of the previous holder's consumer is refused", func(t *testing.T) {
		rig := newCopiesRig(t)
		var refusing atomic.Bool
		refusing.Store(true)
		refused := make(chan struct{}, 1)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			relayCopy: copiesRelayCopyHeld(copiesRole),
			floorRead: func(previous string) error {
				if previous != copiesHolder || !refusing.Load() {
					return nil
				}
				copiesSignal(refused)
				return fmt.Errorf("the row refuses the read of %s's session consumer: %w", previous, bus.ErrConsumerNotGranted)
			},
		})
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		var unread []string
		for i := range 3 {
			unread = append(unread, rig.publish(t, copiesRole, "", fmt.Sprintf("unread role message %d", i+1), copiesHolder))
		}
		copiesSeqs(t, holder.collect(t, 3, copiesWatchBound), unread...)
		rig.crash(t, copiesHolder, holder)

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		refusedRead := copiesAwait(refused)
		if refusedRead {
			rig.awaitHandoverIdle(t, copiesRole)
		}
		first := tallyFor(taker, copiesSettle)

		t.Run("control: the claim's pass, its read refused, copies nothing", func(t *testing.T) {
			copiesRequireNone(t, first, copiesTaker, unread...)
			for _, eventID := range unread {
				if copies := rig.keptCopiesOf(t, copiesRole, eventID); copies != 0 {
					t.Errorf("role %s's kept lane holds %d copies of %s after the refused pass; want none", copiesRole, copies, eventID)
				}
			}
		})

		refusing.Store(false)
		rig.sweepPass()
		received := rig.watch(t, taker, copiesRole, unread...)

		t.Run("case: the sweep's next pass, with the read allowed, copies them and the new holder receives each", func(t *testing.T) {
			if !refusedRead {
				t.Errorf("no takeover pass read %s's consumer within %s of the claim, so none met the refused read", copiesHolder, copiesSignalBound)
			}
			copiesRequireEach(t, received, copiesTaker, unread...)
		})
	})

	t.Run("8 a role message JSON reads and the wire rules refuse, and one JSON does not read, both unread by the holder", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(copiesRole)})
		holder := rig.startHolder(t, copiesHolder)
		rig.claim(t, copiesHolder, copiesRole, false)
		rig.crash(t, copiesHolder, holder)
		// Stored once the holder's process stopped, as messages it never read: no process takes
		// them, and its consumer holds both unacknowledged when the takeover's pass reads it.
		refused, refusedData := roleFrameTheWireRulesRefuse(t, copiesRole)
		unreadable, unreadableData := roleFrameNoEnvelopeReads(t, copiesRole)
		for _, data := range [][]byte{refusedData, unreadableData} {
			if _, err := rig.harness.client.JS().Publish(contracts.LegacyAgentSubject(copiesHolder), data); err != nil {
				t.Fatalf("store a role message on %s's own subject: %v", copiesHolder, err)
			}
		}

		taker := rig.startTaker(t, copiesTaker)
		rig.claim(t, copiesTaker, copiesRole, false)
		received := rig.watch(t, taker, copiesRole, refused)

		t.Run("case: the role message the wire rules refuse reaches the new holder", func(t *testing.T) {
			copiesRequireEach(t, received, copiesTaker, refused)
		})
		t.Run("control: the role message JSON does not read is not copied, and the new holder does not receive it", func(t *testing.T) {
			copiesRequireNone(t, received, copiesTaker, unreadable)
			if copies := rig.keptCopiesOf(t, copiesRole, unreadable); copies != 0 {
				t.Errorf("role %s's kept lane holds %d copies of %s, whose envelope JSON does not read; want none", copiesRole, copies, unreadable)
			}
		})
	})
}

// copiesUnreadRoleMessagesRow is rows 1 and 2. The holder claims R and S; its subject holds, in
// this order, a role message of R it acknowledged, a role message of S, two messages to the
// session and three role messages of R it did not acknowledge; it crashes inside its live window.
// A different session then takes R: outright, or - soft - once the session registry's clock has
// moved the holder past the live window, which a soft claim needs to take a role from another
// session.
func copiesUnreadRoleMessagesRow(t *testing.T, soft bool) {
	rig := newCopiesRig(t)
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(copiesRole)})
	holder := rig.startHolder(t, copiesHolder)
	rig.claim(t, copiesHolder, copiesRole, false)
	rig.claim(t, copiesHolder, copiesOther, false)
	read := rig.publish(t, copiesRole, "", copiesReadMark+"the role message the holder acknowledged", copiesHolder)
	other := rig.publish(t, copiesOther, "", "a role message of the other role the holder keeps", copiesHolder)
	direct := []string{rig.sendToSession(t, copiesHolder), rig.sendToSession(t, copiesHolder)}
	var unread []string
	for i := range 3 {
		unread = append(unread, rig.publish(t, copiesRole, "", fmt.Sprintf("unread role message %d", i+1), copiesHolder))
	}
	kept := append([]string{other}, direct...)
	seqs := copiesSeqs(t, holder.collect(t, 7, copiesWatchBound), append(append([]string{read}, kept...), unread...)...)
	awaitAckState(t, rig.consumers, copiesHolder, len(kept)+len(unread), seqs[read])
	rig.crash(t, copiesHolder, holder)

	if soft {
		rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
		if _, state := rig.harness.sessions.Lookup(copiesHolder); state != session.Unresponsive {
			t.Fatalf("%s reads %s past its live window; want %s", copiesHolder, state, session.Unresponsive)
		}
	}
	taker := rig.startTaker(t, copiesTaker)
	rig.claim(t, copiesTaker, copiesRole, soft)
	received := rig.watch(t, taker, copiesRole, unread...)

	t.Run("case: the new holder receives each of the three unread role messages at least once", func(t *testing.T) {
		copiesRequireEach(t, received, copiesTaker, unread...)
	})
	t.Run("control: the new holder receives neither the acknowledged role message, nor the other role's message, nor the two messages to the session, and the role's kept lane holds no copy of them", func(t *testing.T) {
		copiesRequireNone(t, received, copiesTaker, append([]string{read}, kept...)...)
		// The new holder's own relay copies a role message of S rather than deliver it, since
		// S's claim names the previous holder (sessionRelays.takeOverFrame), so what it receives
		// alone cannot show that the takeover copied nothing but R's role messages; R's kept lane
		// can.
		for _, eventID := range append([]string{read}, kept...) {
			if copies := rig.keptCopiesOf(t, copiesRole, eventID); copies != 0 {
				t.Errorf("role %s's kept lane holds %d copies of %s; want none", copiesRole, copies, eventID)
			}
		}
	})
	t.Run("control: the previous holder's consumer still holds the other role's message and the two messages to the session", func(t *testing.T) {
		rig.requireUnacknowledged(t, copiesHolder, seqs, kept...)
		back := rig.returnAndReceive(t, copiesHolder, kept...)
		copiesRequireEach(t, back, copiesHolder+", registered again,", kept...)
	})
}
