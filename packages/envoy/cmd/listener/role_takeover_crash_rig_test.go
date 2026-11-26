// The rig and observation helpers of TestATakeoverCopyThatStopsOrRunsAgainLosesNothing
// (role_takeover_crash_test.go): one listener over a stream of its own, and the reads a row makes
// of what it stored.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// crashWatch bounds a row's wait for a session to receive what the row waits for.
const crashWatch = 20 * time.Second

// crashTakeoverLane is the prefix of the listener's takeover records, written out: a row reads a
// record from the stream as the stream holds it, by its subject.
const crashTakeoverLane = "notifications.envoy.takeover."

// crashRig is one listener - its /v1 routes, its session relays, its role lane and the kept lane's
// sweep - over a stream of its own (testnats.URL hands each row an account of its own). Its clock
// (cfg.now) is a stateClock the row moves, set before the relays, the role forwarder and the role
// lane took their copies of the configuration, and its forwards (cfg.forwardRole) pass a hold a
// row can set (holdForward). The session registry keeps the real clock, so a session the row
// registered stays live whatever the row does to the listener's clock.
//
// Its routes answer under two machine ids. mux is the listener's own: a session that registers
// through it is relayed here, to its stand-in. away is the routes of a machine whose listener is
// gone: a session that registers and claims through it has a consumer and a live record, and no
// listener relays it, so nothing reads its own subject but a takeover's pass. A row about a pass
// uses such a previous holder, so the copy it sees is the pass's and not the session relay's.
type crashRig struct {
	t         *testing.T
	url       string
	clock     *stateClock
	harness   listenerDeliveryHarness
	deps      *listenerDeps
	mux       *http.ServeMux
	away      *http.ServeMux
	consumers *bus.SessionConsumers
	// lane is a session's own connection, which publishes on role lanes and stores raw messages.
	lane *natsgo.Conn
	hold atomic.Pointer[func(contracts.Envelope)]
}

func newCrashRig(t *testing.T) *crashRig {
	t.Helper()
	return newCrashRigOn(t, testnats.URL(t))
}

// newCrashRigOn is newCrashRig over the server url names, as the NATS user it carries.
func newCrashRigOn(t *testing.T, url string) *crashRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := &crashRig{t: t, url: url, clock: &stateClock{}}
	rig.harness = newListenerDeliveryHarnessOn(t, client)
	cfg := &rig.harness.config
	cfg.now = rig.clock.Now
	forward := cfg.forwardRole
	cfg.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		if hold := rig.hold.Load(); hold != nil {
			(*hold)(item)
		}
		return forward(subject, item, msgID, timeout)
	}
	*cfg = wireTestRoleForwarder(t, *cfg)
	delivery, err := connectSessionDelivery([]string{url},
		inactiveThresholdFor(rig.harness.sessions, defaultSessionReapInterval, defaultSessionConsumerSweepInterval),
		client, cfg.logger)
	if err != nil {
		t.Fatalf("the session-consumer connection did not open: %v", err)
	}
	t.Cleanup(delivery.close)
	// The takeover grant's answer is the session delivery's, as main.go wires it.
	cfg.consumerGrant.set(delivery.holdsConsumerGrant)
	observer, err := bus.ConnectSessionConsumers([]string{url}, 0, nil)
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(observer.Close)
	rig.consumers = observer
	rig.deps = &listenerDeps{
		client:    client,
		registry:  rig.harness.registry,
		sessions:  rig.harness.sessions,
		consumers: delivery,
		delivery:  cfg,
		confirmed: newConfirmedRecord(t),
	}
	rig.mux = http.NewServeMux()
	registerV1Routes(rig.mux, rig.deps, cfg.machineID, cfg.logger)
	rig.away = http.NewServeMux()
	registerV1Routes(rig.away, rig.deps, "gone-machine", cfg.logger)
	relays, err := startSessionRelays(delivery, *cfg)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	relays.setOwned(true)
	roleLane, _, err := bus.ConnectRoleLane([]string{url}, cfg.machineID, coreNATSDeliveryHandler(*cfg))
	if err != nil {
		t.Fatalf("open the role lane: %v", err)
	}
	t.Cleanup(roleLane.Close)
	rig.lane = testnats.Connect(t, url)
	t.Cleanup(rig.lane.Close)
	return rig
}

// cfg is the listener's configuration, as its routes, its relays and its sweep read it.
func (rig *crashRig) cfg() listenerDeliveryHandlerConfig { return rig.harness.config }

// anotherListener is a second listener of the deployment over the same stream - another task, or
// this one started again - with a connection, caches and guards of its own, on the row's clock. It
// shares the rig's takeover seams, since a row's fault is a point every listener of the deployment
// passes.
func (rig *crashRig) anotherListener() listenerDeliveryHandlerConfig {
	rig.t.Helper()
	cfg := newListenerDeliveryHarnessOn(rig.t, setupPublishTestClient(rig.t)).config
	cfg.now = rig.clock.Now
	cfg.takeoverFaults = rig.harness.config.takeoverFaults
	return wireTestRoleForwarder(rig.t, cfg)
}

func (rig *crashRig) send(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(recorder, request)
	return recorder
}

// register registers sessionID in consumer mode at epoch through mux.
func (rig *crashRig) register(mux *http.ServeMux, sessionID, epoch string) {
	rig.t.Helper()
	if recorder := rig.send(mux, http.MethodPost, "/v1/interests/subscribe", consumerRegistration(sessionID, epoch)); recorder.Code != http.StatusOK {
		rig.t.Fatalf("register %s at %s: status %d %s", sessionID, epoch, recorder.Code, recorder.Body.String())
	}
}

// relayed is a session this listener relays: its stand-in, which answers each frame as answer
// says, and its registration through the listener's own routes.
func (rig *crashRig) relayed(sessionID string, answer func(relayedFrame) []string) *sessionStandIn {
	rig.t.Helper()
	standIn := startSessionStandIn(rig.t, rig.url, sessionID, answer)
	rig.register(rig.mux, sessionID, "epoch-1")
	return standIn
}

// gone registers sessionID through the routes of the machine whose listener is gone.
func (rig *crashRig) gone(sessionID string) {
	rig.t.Helper()
	rig.register(rig.away, sessionID, "epoch-1")
}

// claimThrough is sessionID's outright claim of role through mux, naming instance as its process
// when it is not empty (postRoleClaim). It touches no testing.T, so a row may make it on a
// goroutine of its own.
func (rig *crashRig) claimThrough(mux *http.ServeMux, sessionID, role, instance string) *httptest.ResponseRecorder {
	return postRoleClaim(mux, sessionID, role, instance, false)
}

func (rig *crashRig) claim(mux *http.ServeMux, sessionID, role string) {
	rig.t.Helper()
	grantRoleClaim(rig.t, mux, sessionID, role, "", false)
}

// release hands role back for sessionID's process instance through the unsubscribe route, and
// fails the row unless the role then has no claim (releaseRoleThrough).
func (rig *crashRig) release(sessionID, role, instance string) {
	rig.t.Helper()
	releaseRoleThrough(rig.t, rig.mux, rig.harness.registry, sessionID, role, instance)
}

// holder is the session role's claim record names, as the role bucket holds it.
func (rig *crashRig) holder(role string) string {
	rig.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claim, err := rig.harness.registry.RoleClaim(ctx, role)
	if err != nil {
		rig.t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim.HolderSessionID
}

// crashPublished is the publish route's answer to a role publish.
type crashPublished struct {
	status    int
	raw       string
	EventID   string `json:"event_id"`
	DedupeKey string `json:"dedupe_key"`
	Holder    string `json:"holder"`
	Kept      bool   `json:"kept"`
}

func (rig *crashRig) publish(role, message string) crashPublished {
	rig.t.Helper()
	body, err := json.Marshal(roleMessage(role, "", message))
	if err != nil {
		rig.t.Fatal(err)
	}
	recorder := rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body))
	answer := crashPublished{status: recorder.Code, raw: recorder.Body.String()}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		rig.t.Fatalf("decode the publish answer %s: %v", answer.raw, err)
	}
	return answer
}

// sendTo publishes n messages to role through the publish route, each answered 200 for holder and
// so stored on holder's own subject, and returns their event ids.
func (rig *crashRig) sendTo(holder, role string, n int, what string) []string {
	rig.t.Helper()
	events := make([]string, 0, n)
	for i := range n {
		answer := rig.publish(role, fmt.Sprintf("%s %d", what, i+1))
		if answer.status != http.StatusOK || answer.Holder != holder || answer.Kept {
			rig.t.Fatalf("publish to %s: status %d %s; want 200 stored for %s", role, answer.status, answer.raw, holder)
		}
		events = append(events, answer.EventID)
	}
	return events
}

// onLane publishes a role message on role's lane over core NATS, as a session does, and returns it.
func (rig *crashRig) onLane(role, message string) contracts.Envelope {
	rig.t.Helper()
	item := publishRoleMessage(rig.t, rig.lane, role, message)
	if err := rig.lane.Flush(); err != nil {
		rig.t.Fatalf("flush the role publish: %v", err)
	}
	return item
}

// lateForward stores on previous's own subject a forward of a new role message of role - its
// envelope under the forward mark, under the forward's message id - as a forward that resolved
// previous before the claim moved is stored when the stream takes it late, and returns its event id.
func (rig *crashRig) lateForward(previous, role, message string) string {
	rig.t.Helper()
	item := newEnvelope("agent", contracts.RoleTopicPrefix+role, message)
	forward := item
	forward.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	subject := contracts.LegacyAgentSubject(previous)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.harness.client.PublishStoredAs(ctx, subject, forward, bus.ForwardMsgID(item.EventID, subject)); err != nil {
		rig.t.Fatalf("store the late forward on %s: %v", subject, err)
	}
	return item.EventID
}

// storeToSession stores n messages to sessionID itself on its own subject: no role message among them.
func (rig *crashRig) storeToSession(sessionID string, n int) {
	rig.t.Helper()
	subject := contracts.LegacyAgentSubject(sessionID)
	js, err := rig.lane.JetStream()
	if err != nil {
		rig.t.Fatalf("open JetStream: %v", err)
	}
	for i := range n {
		data, err := json.Marshal(newEnvelope("agent", subject, fmt.Sprintf("to the session %d", i+1)))
		if err != nil {
			rig.t.Fatal(err)
		}
		if _, err := js.PublishAsync(subject, data); err != nil {
			rig.t.Fatalf("store a message to %s: %v", sessionID, err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(listenerWaitBound(rig.t)):
		rig.t.Fatalf("the stream did not acknowledge %d messages to %s in time", n, sessionID)
	}
}

// stored is every envelope the stream holds on subject, in stream order (storedEnvelopes).
func (rig *crashRig) stored(subject string) []storedEnvelope {
	rig.t.Helper()
	return storedEnvelopes(rig.t, rig.harness.client, subject)
}

// events is the event id of each envelope the stream holds on subject, in stream order.
func (rig *crashRig) events(subject string) []string {
	rig.t.Helper()
	held := rig.stored(subject)
	events := make([]string, len(held))
	for i, stored := range held {
		events[i] = stored.item.EventID
	}
	return events
}

// kept is the event id of each copy on role's kept lane, in stream order.
func (rig *crashRig) kept(role string) []string {
	rig.t.Helper()
	return rig.events(bus.KeptRoleSubject(role))
}

// seqOn is the stream sequence of eventID's envelope on subject.
func (rig *crashRig) seqOn(subject, eventID string) uint64 {
	rig.t.Helper()
	for _, stored := range rig.stored(subject) {
		if stored.item.EventID == eventID {
			return stored.seq
		}
	}
	rig.t.Fatalf("the stream holds no event %s on %s", eventID, subject)
	return 0
}

// awaitStored waits for an envelope of eventID on subject and returns it, with the test binary's
// deadline as its only bound.
func (rig *crashRig) awaitStored(subject, eventID, what string) storedEnvelope {
	rig.t.Helper()
	deadline := time.Now().Add(listenerWaitBound(rig.t))
	for {
		held := rig.stored(subject)
		if i := slices.IndexFunc(held, func(stored storedEnvelope) bool { return stored.item.EventID == eventID }); i >= 0 {
			return held[i]
		}
		if time.Now().After(deadline) {
			events := make([]string, len(held))
			for i, stored := range held {
				events[i] = stored.item.EventID
			}
			rig.t.Fatalf("%s: the stream holds %v on %s in the time it was given; want event %s", what, events, subject, eventID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// crashRecord is a takeover record as the stream holds it on its own subject.
type crashRecord struct {
	subject   string
	written   time.Time
	Role      string `json:"role"`
	Previous  string `json:"previous"`
	ClaimedAt int64  `json:"claimed_at"`
	Next      uint64 `json:"next"`
	Finished  bool   `json:"finished"`
}

// records is the newest record on each takeover subject of role taken from previous, read from the
// stream by the subjects' literal prefix, sorted by subject.
func (rig *crashRig) records(role, previous string) []crashRecord {
	rig.t.Helper()
	js := rig.harness.client.JS()
	info, err := js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: crashTakeoverLane + role + "." + previous + ".>"})
	if err != nil {
		rig.t.Fatalf("read the takeover subjects of %s from %s: %v", role, previous, err)
	}
	subjects := slices.Sorted(maps.Keys(info.State.Subjects))
	records := make([]crashRecord, 0, len(subjects))
	for _, subject := range subjects {
		msg, err := js.GetLastMsg(bus.Stream, subject)
		if err != nil {
			rig.t.Fatalf("read the record on %s: %v", subject, err)
		}
		record := crashRecord{subject: subject, written: msg.Time}
		if err := json.Unmarshal(msg.Data, &record); err != nil {
			rig.t.Fatalf("decode the record on %s: %v", subject, err)
		}
		records = append(records, record)
	}
	return records
}

// onlyRecord is the one takeover record of role taken from previous.
func (rig *crashRig) onlyRecord(role, previous string) crashRecord {
	rig.t.Helper()
	records := rig.records(role, previous)
	if len(records) != 1 {
		rig.t.Fatalf("the stream holds %d takeover records of %s from %s: %+v; want one", len(records), role, previous, records)
	}
	return records[0]
}

// idle waits until no handover of role holds cfg's guard: a claim's own handover, say, that has
// not ended yet.
func (rig *crashRig) idle(cfg listenerDeliveryHandlerConfig, role string) {
	rig.t.Helper()
	waitFor(rig.t, "no handover of role "+role+" holding the listener's guard", func() bool {
		release, free := cfg.roleKeepDrains.tryHold(role)
		if free {
			release()
		}
		return free
	})
}

// sweep runs one pass of cfg's kept-lane sweep (sweepOnce) once cfg is idle for role: the sweep
// passes over a role whose guard a handover holds.
func (rig *crashRig) sweep(cfg listenerDeliveryHandlerConfig, role string) {
	rig.t.Helper()
	rig.idle(cfg, role)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sweepOnce(ctx, cfg)
}

// holdForward makes the forward of a role message whose summary is summary wait inside the
// listener's forward, and so at the head of its holder's queue, until release; held is closed
// once one waits. The release runs at the row's end in any case.
func (rig *crashRig) holdForward(summary string) (held <-chan struct{}, release func()) {
	waiting, gate := make(chan struct{}), make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	hold := func(item contracts.Envelope) {
		if item.PayloadSummary != summary {
			return
		}
		heldOnce.Do(func() { close(waiting) })
		<-gate
	}
	rig.hold.Store(&hold)
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	rig.t.Cleanup(release)
	return waiting, release
}

// renewEveryTry sets the seam before each try of a claim of role to write a renewal of the role's
// claim - the record read and written back with a fresh renewed_at, which moves the bucket's
// revision - so the try's write meets a conflict. It returns how many tries there were and the
// first renewal that failed.
func (rig *crashRig) renewEveryTry(role string) (*atomic.Int32, *atomic.Pointer[error]) {
	rig.t.Helper()
	bucket, err := rig.harness.client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		rig.t.Fatalf("open the role bucket: %v", err)
	}
	tries, failed := &atomic.Int32{}, &atomic.Pointer[error]{}
	setTakeoverFaults(rig.t, rig.harness.config, takeoverFaultSet{beforeClaimTry: func(r, _ string) error {
		if r != role {
			return nil
		}
		tries.Add(1)
		if err := crashRenew(bucket, role); err != nil {
			failed.CompareAndSwap(nil, &err)
		}
		time.Sleep(25 * time.Millisecond)
		return nil
	}})
	return tries, failed
}

func crashRenew(bucket natsgo.KeyValue, role string) error {
	entry, err := bucket.Get(role)
	if err != nil {
		return err
	}
	claim, err := store.DecodeRoleClaim(entry.Value())
	if err != nil {
		return err
	}
	claim.RenewedAt = time.Now().UnixMilli()
	record, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	_, err = bucket.Update(role, record, entry.Revision())
	return err
}

// crashAnswerReceived answers a frame received and nothing more: the session took it and has not
// read it, so it stays unacknowledged on the session's consumer.
func crashAnswerReceived(relayedFrame) []string { return []string{relayReplyReceived} }

// crashKeptCopy is a frame that is a kept lane copy's forward, which carries the time it was kept.
func crashKeptCopy(frame relayedFrame) bool { return frame.item.KeptAt != nil }

// crashAwaitEvents reads standIn's frames until a frame of each of want that counts (counts, or
// every frame where it is nil) has arrived, within crashWatch, and returns every frame it read.
func crashAwaitEvents(t *testing.T, standIn *sessionStandIn, want []string, counts func(relayedFrame) bool, what string) []relayedFrame {
	t.Helper()
	missing := map[string]bool{}
	for _, event := range want {
		missing[event] = true
	}
	deadline := time.After(crashWatch)
	var got []relayedFrame
	for len(missing) > 0 {
		select {
		case frame := <-standIn.frames:
			got = append(got, frame)
			if counts == nil || counts(frame) {
				delete(missing, frame.item.EventID)
			}
		case <-deadline:
			t.Fatalf("%s: %d of the %d events %v did not arrive within %s; missing %v; frames that arrived %v",
				what, len(missing), len(want), want, crashWatch, slices.Sorted(maps.Keys(missing)), frameEvents(got))
		}
	}
	return got
}

// crashDrain reads standIn's frames until none arrives for quiet.
func crashDrain(standIn *sessionStandIn, quiet time.Duration) []relayedFrame {
	var got []relayedFrame
	for {
		select {
		case frame := <-standIn.frames:
			got = append(got, frame)
		case <-time.After(quiet):
			return got
		}
	}
}

// crashRequireOneEach fails the row unless events holds each of want exactly once and nothing else.
func crashRequireOneEach(t *testing.T, what string, events, want []string) {
	t.Helper()
	counts := map[string]int{}
	for _, event := range events {
		counts[event]++
	}
	for _, event := range want {
		if counts[event] != 1 {
			t.Fatalf("%s holds %v; want each of %v exactly once", what, events, want)
		}
	}
	if len(events) != len(want) {
		t.Fatalf("%s holds %v; want each of %v exactly once and nothing else", what, events, want)
	}
}

// crashAwait fails the row unless done is closed within the bound.
func crashAwait(t *testing.T, done <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s did not happen within %s", what, within)
	}
}
