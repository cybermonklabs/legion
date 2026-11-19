package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A takeover's copy of the previous holder's unread role messages keeps Envoy's at-least-once
// contract when a listener stops between two of its steps, when a pass runs again or beside
// another, and when a forward to the previous holder is stored late: in every row each role
// message of the takeover reaches the session the row names, and the kept lane holds one copy of
// it. Each row is a listener of its own over a stream of its own (crashRig), and forces its fault
// through the takeover copy's test seams (setTakeoverFaults) or the listener's clock.
//
// Rows 6 and 13 are the controls. A build with no takeover copy never moves a message off the
// previous holder's own consumer, so in row 6 the first holder reads its messages there when it
// takes the role back; and it writes no takeover record, so in row 13 the lane's readers read the
// kept lane alone. Every other row is a case: such a build gives the role's next holder none of the
// messages, and it has no seam to force a row's fault, so a row that waits for its seam fails there.
func TestATakeoverCopyThatStopsOrRunsAgainLosesNothing(t *testing.T) {
	t.Run("1 a listener stopped after the claim write leaves the copy to the next sweep", crashStoppedAfterTheClaimWrite)
	t.Run("1b a record whose claim never landed leaves the holder that kept the role its own messages", crashRecordWhoseClaimNeverLanded)
	t.Run("1c an older pass paused before its last write leaves a newer takeover of the holder to copy", crashOlderPassPausedBeforeItsLastWrite)
	t.Run("2 a pass stopped after its first copy is gone on from and the lane holds one copy of each", crashPassStoppedAfterItsFirstCopy)
	t.Run("3 two listeners copying one takeover at once leave one copy of each", crashTwoListenersCopyAtOnce)
	t.Run("4 a release before any pass leaves the copy to the sweep for the next holder", crashReleasedBeforeAnyPass)
	t.Run("4 the reaper deleting the claim before any pass leaves the copy to the sweep for the next holder", crashReapedBeforeAnyPass)
	t.Run("5 a kept copy forwarded to the previous holder and not read reaches the new holder", crashKeptCopyForwardedAndNotRead)
	t.Run("6 control a role taken back from a holder that read none of the copies reaches the first holder", crashRoleTakenBack)
	t.Run("7 a claim that met a concurrent claim's write takes over from that claim's holder too", crashConcurrentClaims)
	t.Run("7b a claim that meets a conflict at every try answers 503 and writes nothing", crashConflictAtEveryTry)
	t.Run("8 a copied forward reaches the new holder with one forward prefix", crashCopiedForwardHasOnePrefix)
	t.Run("9 passes past roleKeepScanMax go on from the record's sequence across a restart", crashPassesAcrossARestart)
	t.Run("10 a release while the record is unfinished and a late forward still reach the next holder", crashReleaseWhileUnfinished)
	t.Run("10b a forward whose resolution is past the fence resolves again and goes to the new holder", crashForwardPastTheFence)
	t.Run("11 a forward stored after the claim's first pass is copied by the next pass", crashForwardStoredAfterTheFirstPass)
	t.Run("12 every listener down past the hour after the takeover and the next sweep still copies", crashListenersDownPastTheHour)
	t.Run("12b a forward stored two hours after the takeover is copied by the next pass", crashForwardTwoHoursLate)
	t.Run("12d a record is held and copies a forward stored 70h55m after the takeover and ends only past 71h", crashRecordHeldToItsLastHour)
	t.Run("12c a claim that meets a renewal at every try leaves the holder that kept the role its own messages", crashRenewalAtEveryTry)
	t.Run("13 control a takeover record ahead of a kept copy changes nothing the lane's readers read", crashRecordAheadOfAKeptCopy)
}

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
	url := testnats.URL(t)
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
	cfg.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
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
// this one started again - with a connection, caches and guards of its own, on the row's clock.
func (rig *crashRig) anotherListener() listenerDeliveryHandlerConfig {
	rig.t.Helper()
	cfg := newListenerDeliveryHarnessOn(rig.t, setupPublishTestClient(rig.t)).config
	cfg.now = rig.clock.Now
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
// when it is not empty. It touches no testing.T, so a row may make it on a goroutine of its own.
func (rig *crashRig) claimThrough(mux *http.ServeMux, sessionID, role, instance string) *httptest.ResponseRecorder {
	fields := map[string]string{"session_id": sessionID, "role": role}
	if instance != "" {
		fields["client_instance_id"] = instance
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return rig.send(mux, http.MethodPost, "/v1/roles/set", string(body))
}

func (rig *crashRig) claim(mux *http.ServeMux, sessionID, role string) {
	rig.t.Helper()
	if recorder := rig.claimThrough(mux, sessionID, role, ""); recorder.Code != http.StatusOK {
		rig.t.Fatalf("%s claims %s: status %d %s; want 200", sessionID, role, recorder.Code, recorder.Body.String())
	}
}

// release hands role back for sessionID's process instance through the unsubscribe route, which
// frees a claim naming that process, and fails the row unless the role then has no claim.
func (rig *crashRig) release(sessionID, role, instance string) {
	rig.t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "topics": []string{contracts.RoleTopicPrefix + role}, "client_instance_id": instance})
	if err != nil {
		rig.t.Fatal(err)
	}
	if recorder := rig.send(rig.mux, http.MethodPost, "/v1/interests/unsubscribe", string(body)); recorder.Code != http.StatusOK {
		rig.t.Fatalf("%s releases %s: status %d %s", sessionID, role, recorder.Code, recorder.Body.String())
	}
	if holder := rig.holder(role); holder != "" {
		rig.t.Fatalf("after %s released %s its claim names %q; want none", sessionID, role, holder)
	}
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
	case <-time.After(30 * time.Second):
		rig.t.Fatalf("the stream did not acknowledge %d messages to %s within 30s", n, sessionID)
	}
}

// crashStored is one envelope the stream holds on a subject, with its stream sequence.
type crashStored struct {
	seq  uint64
	item contracts.Envelope
}

// stored is every envelope the stream holds on subject, in stream order.
func (rig *crashRig) stored(subject string) []crashStored {
	rig.t.Helper()
	var held []crashStored
	for seq := uint64(1); ; {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stored, err := rig.harness.client.NextStoredMessage(ctx, subject, seq)
		cancel()
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return held
		}
		if err != nil {
			rig.t.Fatalf("read %s from %d: %v", subject, seq, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(stored.Data, &item); err != nil {
			rig.t.Fatalf("decode the message at %d on %s: %v", stored.Sequence, subject, err)
		}
		held = append(held, crashStored{seq: stored.Sequence, item: item})
		seq = stored.Sequence + 1
	}
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

// awaitStored waits up to 10 s for an envelope of eventID on subject and returns it.
func (rig *crashRig) awaitStored(subject, eventID, what string) crashStored {
	rig.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held := rig.stored(subject)
		if i := slices.IndexFunc(held, func(stored crashStored) bool { return stored.item.EventID == eventID }); i >= 0 {
			return held[i]
		}
		if time.Now().After(deadline) {
			events := make([]string, len(held))
			for i, stored := range held {
				events[i] = stored.item.EventID
			}
			rig.t.Fatalf("%s: the stream holds %v on %s after 10s; want event %s", what, events, subject, eventID)
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
	waitFor(rig.t, 30*time.Second, "no handover of role "+role+" holding the listener's guard", func() bool {
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
	setTakeoverFaults(rig.t, takeoverFaultSet{beforeClaimTry: func(r, _ string, _ int) error {
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

// (1) The listener stops after the claim write and before the claim's handover runs a pass
// (skipClaimHandover): nothing is copied, and the sweep's next pass copies the three from the
// takeover record the claim wrote before its write.
func crashStoppedAfterTheClaimWrite(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr1_a", "ses_cr1_b", "cr1"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "for the holder that stops")
	next := rig.relayed(b, answerDone)
	setTakeoverFaults(t, takeoverFaultSet{skipClaimHandover: func(r string) bool { return r == role }})
	rig.claim(rig.mux, b, role)
	if kept := rig.kept(role); len(kept) != 0 {
		t.Fatalf("role %s's kept lane holds %v after a claim whose listener stopped before its pass; want nothing copied yet", role, kept)
	}
	next.none(t, time.Second)
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, sent, nil, "the new holder, from the sweep's pass of the takeover record")
}

// (1b) The listener stops after a try's takeover record and before its claim write
// (beforeClaimTry): the claim writes nothing and the previous holder keeps the role. The record's
// passes copy nothing while the claim names that holder, which receives each of its role messages
// from its own consumer (crashKeptByItsHolder).
func crashRecordWhoseClaimNeverLanded(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr1b_a", "ses_cr1b_b", "cr1b"
	keeper := rig.relayed(a, crashAnswerReceived)
	rig.claim(rig.mux, a, role)
	sent := rig.sendTo(a, role, 3, "for the holder that keeps the role")
	crashAwaitEvents(t, keeper, sent, nil, "the holder's own frames, each answered received alone")
	rig.relayed(b, answerDone)
	setTakeoverFaults(t, takeoverFaultSet{beforeClaimTry: func(r, previous string, _ int) error {
		if r == role && previous == a {
			return errors.New("the listener stopped after the takeover record and before the claim write")
		}
		return nil
	}})
	if recorder := rig.claimThrough(rig.mux, b, role, ""); recorder.Code == http.StatusOK {
		t.Fatalf("%s's claim answered 200 %s; want a claim stopped before its write, which writes nothing", b, recorder.Body.String())
	}
	if holder := rig.holder(role); holder != a {
		t.Fatalf("role %s's claim names %q after a claim that stopped before its write; want %s, which kept it", role, holder, a)
	}
	if records := rig.records(role, a); len(records) == 0 {
		t.Errorf("the stream holds no takeover record of %s from %s; want the one the try wrote before it stopped", role, a)
	}
	crashKeptByItsHolder(t, rig, keeper, a, role, sent)
}

// crashKeptByItsHolder is the end of rows 1b and 12c, whose takeover records' claims never landed
// and whose previous holder holds the role. Three sweeps run the records' passes, and after each
// the role's kept lane holds no copy and the holder's own subject holds its three role messages
// and nothing more. The holder's process then stops and comes back - a registration at a new
// epoch restarts its relay at its floor - and it receives each of the three from its own consumer.
func crashKeptByItsHolder(t *testing.T, rig *crashRig, standIn *sessionStandIn, holder, role string, sent []string) {
	t.Helper()
	agent := contracts.LegacyAgentSubject(holder)
	for i := range 3 {
		rig.sweep(rig.cfg(), role)
		if kept := rig.kept(role); len(kept) != 0 {
			t.Fatalf("after sweep %d role %s's kept lane holds %v; want no copy while its claim names %s", i+1, role, kept, holder)
		}
		if events := rig.events(agent); !slices.Equal(events, sent) {
			t.Fatalf("after sweep %d %s's own subject holds %v; want its three role messages alone, %v", i+1, holder, events, sent)
		}
	}
	standIn.conn.Close()
	awaitNoRelaySubscriber(t, rig.url, holder)
	back := startSessionStandIn(t, rig.url, holder, answerDone)
	rig.register(rig.mux, holder, "epoch-2")
	crashAwaitEvents(t, back, sent, nil, holder+", back and holding the role, from its own consumer")
}

// crashOlderClaimAge is how far back the claim of row 1c's older takeover is: past the record's
// life (71 hours), so the pass that runs now reads to the end and marks the record finished, and
// the last write it pauses before is that mark.
const crashOlderClaimAge = 72 * time.Hour

// (1c) An older takeover's pass of A pauses before its last write (beforeRecordWrite), the one
// that marks its record finished. Meanwhile A takes the role back, is sent three more, and C takes
// the role from A. The older write then lands on the older record's own subject and leaves the
// newer record open, and the newer record's passes copy the three A was sent after the older pass
// stopped.
func crashOlderPassPausedBeforeItsLastWrite(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, c, role = "ses_cr1c_a", "ses_cr1c_b", "ses_cr1c_c", "cr1c"
	rig.gone(a)
	rig.gone(b)
	rig.claim(rig.away, a, role)
	rig.sendTo(a, role, 3, "before the older takeover")
	paused, resume := make(chan struct{}), make(chan struct{})
	var pauseOnce, resumeOnce sync.Once
	resumeOlder := func() { resumeOnce.Do(func() { close(resume) }) }
	setTakeoverFaults(t, takeoverFaultSet{
		skipClaimHandover: func(r string) bool { return r == role },
		beforeRecordWrite: func(r, previous string, _ uint64, finished bool) {
			if r != role || previous != a || !finished {
				return
			}
			first := false
			pauseOnce.Do(func() { first = true; close(paused) })
			if first {
				<-resume
			}
		},
	})
	rig.clock.advance(-crashOlderClaimAge)
	rig.claim(rig.away, b, role)
	rig.clock.advance(crashOlderClaimAge)
	olderDone := make(chan struct{})
	go func() {
		defer close(olderDone)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sweepOnce(ctx, rig.cfg())
	}()
	t.Cleanup(func() {
		resumeOlder()
		select {
		case <-olderDone:
		case <-time.After(2 * time.Minute):
		}
	})
	crashAwait(t, paused, 30*time.Second, "the older takeover's pass reaching the write that marks its record finished")
	rig.claim(rig.away, a, role)
	after := rig.sendTo(a, role, 3, "after the older pass stopped")
	next := rig.relayed(c, answerDone)
	rig.claim(rig.mux, c, role)
	resumeOlder()
	crashAwait(t, olderDone, 2*time.Minute, "the older pass's sweep ending once its write went on")
	records := rig.records(role, a)
	finished, open := 0, 0
	for _, record := range records {
		if record.Finished {
			finished++
		} else {
			open++
		}
	}
	if finished != 1 || open != 1 {
		t.Fatalf("the takeover records of %s from %s are %+v; want the older one finished and the newer one open, each on its own subject", role, a, records)
	}
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, after, nil, "the newer holder, from the newer record's passes")
}

// (2) The claim's pass stops after its first copy (beforeCopy fails the second): the walk hands
// the new holder that copy, the sweep's next pass copies the other two from where the record says
// the first stopped, and the kept lane holds one copy of each.
func crashPassStoppedAfterItsFirstCopy(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr2_a", "ses_cr2_b", "cr2"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "copied across a stop")
	second := rig.seqOn(contracts.LegacyAgentSubject(a), sent[1])
	next := rig.relayed(b, answerDone)
	var stoppedAt atomic.Uint64
	setTakeoverFaults(t, takeoverFaultSet{beforeCopy: func(r, _ string, frameSeq uint64) error {
		if r == role && frameSeq == second && stoppedAt.CompareAndSwap(0, frameSeq) {
			return errors.New("the listener stopped after the pass's first copy")
		}
		return nil
	}})
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, sent[:1], nil, "the new holder, from the pass that stopped after its first copy")
	if got := stoppedAt.Load(); got != second {
		t.Fatalf("the pass stopped at sequence %d; want the second message's, %d", got, second)
	}
	if kept := rig.kept(role); !slices.Equal(kept, sent[:1]) {
		t.Fatalf("role %s's kept lane holds %v after the pass that stopped; want the first message alone, %v", role, kept, sent[:1])
	}
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, sent[1:], nil, "the new holder, from the next pass")
	crashRequireOneEach(t, "role "+role+"'s kept lane", rig.kept(role), sent)
}

// (3) Two listeners run the passes of one takeover at the same moment: each pass holds at the
// first copy until the other's reaches it too (beforeCopy), and once both have written the record
// the kept lane holds one copy of each message. The row ends the two sweeps once the new holder has
// the three: the walk that finds the other's claim on the lane would wait that claim out.
func crashTwoListenersCopyAtOnce(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr3_a", "ses_cr3_b", "cr3"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "copied by two listeners")
	first := rig.seqOn(contracts.LegacyAgentSubject(a), sent[0])
	next := rig.relayed(b, answerDone)
	var arrived, written atomic.Int32
	setTakeoverFaults(t, takeoverFaultSet{
		skipClaimHandover: func(r string) bool { return r == role },
		beforeCopy: func(r, _ string, frameSeq uint64) error {
			if r == role && frameSeq == first {
				arrived.Add(1)
				for deadline := time.Now().Add(10 * time.Second); arrived.Load() < 2 && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
			}
			return nil
		},
		beforeRecordWrite: func(r, previous string, _ uint64, _ bool) {
			if r == role && previous == a {
				written.Add(1)
			}
		},
	})
	rig.claim(rig.mux, b, role)
	other := rig.anotherListener()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	var sweeps sync.WaitGroup
	for _, cfg := range []listenerDeliveryHandlerConfig{rig.cfg(), other} {
		sweeps.Go(func() { sweepOnce(ctx, cfg) })
	}
	t.Cleanup(func() {
		cancel()
		sweeps.Wait()
	})
	waitFor(t, 30*time.Second, "both listeners' passes writing the record", func() bool { return written.Load() >= 2 })
	if got := arrived.Load(); got != 2 {
		t.Fatalf("the two listeners' passes reached the first copy %d times; want 2, together", got)
	}
	crashRequireOneEach(t, "role "+role+"'s kept lane", rig.kept(role), sent)
	crashAwaitEvents(t, next, sent, nil, "the new holder")
}

// crashEndedBeforeAnyPass is rows 4: B takes the role from A with its claim's handover stopped
// (skipClaimHandover), end ends B's claim before any pass ran, the sweep's next pass copies A's
// three from the takeover record, and the role's next holder, C, receives them.
func crashEndedBeforeAnyPass(t *testing.T, row, instance string, end func(rig *crashRig, standIn *sessionStandIn, holder, role string)) {
	rig := newCrashRig(t)
	a, b, c, role := "ses_cr"+row+"_a", "ses_cr"+row+"_b", "ses_cr"+row+"_c", "cr"+row
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "for the role's next holder")
	standIn := rig.relayed(b, answerDone)
	var skip atomic.Bool
	skip.Store(true)
	setTakeoverFaults(t, takeoverFaultSet{skipClaimHandover: func(r string) bool { return r == role && skip.Load() }})
	if recorder := rig.claimThrough(rig.mux, b, role, instance); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status %d %s; want 200", b, role, recorder.Code, recorder.Body.String())
	}
	end(rig, standIn, b, role)
	if holder := rig.holder(role); holder != "" {
		t.Fatalf("role %s's claim names %q; want none once %s's claim ended", role, holder, b)
	}
	skip.Store(false)
	rig.sweep(rig.cfg(), role)
	crashRequireOneEach(t, "role "+role+"'s kept lane", rig.kept(role), sent)
	next := rig.relayed(c, answerDone)
	rig.claim(rig.mux, c, role)
	crashAwaitEvents(t, next, sent, nil, "the role's next holder")
}

func crashReleasedBeforeAnyPass(t *testing.T) {
	const instance = "process-of-the-new-holder"
	crashEndedBeforeAnyPass(t, "4r", instance, func(rig *crashRig, _ *sessionStandIn, holder, role string) {
		rig.release(holder, role, instance)
	})
}

// The reaper deletes a claim whose holder's session is gone and whose renewal is past the live
// window: the holder stops and deregisters, and its claim is aged past the window.
func crashReapedBeforeAnyPass(t *testing.T) {
	crashEndedBeforeAnyPass(t, "4p", "", func(rig *crashRig, standIn *sessionStandIn, holder, role string) {
		standIn.conn.Close()
		if recorder := rig.send(rig.mux, http.MethodDelete, "/v1/sessions/"+holder, ""); recorder.Code != http.StatusOK {
			rig.t.Fatalf("DELETE /v1/sessions/%s: status %d %s", holder, recorder.Code, recorder.Body.String())
		}
		waitFor(rig.t, 10*time.Second, holder+" read as not live", func() bool {
			_, state := rig.harness.sessions.Lookup(holder)
			return state != session.Live
		})
		if claimed := rig.holder(role); claimed != holder {
			rig.t.Fatalf("role %s's claim names %q after %s deregistered; want %s, for the reaper to delete", role, claimed, holder, holder)
		}
		ageRoleClaim(rig.t, rig.deps, role, 2*session.LiveWindow)
		isAlive := func(sessionID string) bool {
			return roleHolderMayBeLive(rig.harness.config.logger, rig.harness.sessions, sessionID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if sweep, err := rig.harness.registry.ReapRoleClaims(ctx, isAlive, rig.harness.sessions.TTL(), session.LiveWindow); err != nil || sweep.Reaped != 1 {
			rig.t.Fatalf("reap the role claims = %+v, %v; want role %s's claim deleted", sweep, err, role)
		}
	})
}

// (5) A message kept on the role's kept lane while nobody held the role is forwarded to A when A
// claims, and A does not read it. When B takes the role, the copy of that forward is stored
// beside the earlier kept copy of the same message - its id names the frame it was copied from -
// and reaches B.
func crashKeptCopyForwardedAndNotRead(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr5_a", "ses_cr5_b", "cr5"
	kept := rig.publish(role, "kept while no session holds the role")
	if kept.status != http.StatusAccepted || !kept.Kept {
		t.Fatalf("publish to the unclaimed role %s: status %d %s; want 202 kept", role, kept.status, kept.raw)
	}
	waitForKeptRoleMessage(t, rig.harness.client, role, kept.EventID)
	rig.gone(a)
	rig.claim(rig.away, a, role)
	forwarded := rig.awaitStored(contracts.LegacyAgentSubject(a), kept.EventID, "the claim's handover forwarding the kept copy to "+a)
	if forwarded.item.KeptAt == nil {
		t.Fatalf("the message on %s's own subject carries no kept_at; want the kept lane's forward of the kept copy", a)
	}
	next := rig.relayed(b, answerDone)
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, []string{kept.EventID}, nil, "the new holder, handed the kept message the previous holder was forwarded and did not read")
}

// (6, control) A takes the role, reads none of three role-lane forwards, and stops. B takes the
// role and reads none of what it is handed. A comes back - its relay restarts at its floor - and
// takes the role back: it receives each of the three, and over five sweeps after that its own
// subject gains no message, so no pass copies to A what A already holds. A build that copies at a
// takeover has its relay take A's own three off A's consumer while B holds the role, so A receives
// them through the copies, whose forward to A must not be dropped as the role lane's earlier
// forward of the same message to A. A build that copies nothing leaves them on A's consumer, where
// A reads them on its return, so this row passes there too.
func crashRoleTakenBack(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr6_a", "ses_cr6_b", "cr6"
	first := rig.relayed(a, crashAnswerReceived)
	rig.claim(rig.mux, a, role)
	agent := contracts.LegacyAgentSubject(a)
	sent := make([]string, 0, 3)
	for i := range 3 {
		item := rig.onLane(role, fmt.Sprintf("on the role lane %d", i+1))
		rig.awaitStored(agent, item.EventID, "the role lane's forward to "+a)
		sent = append(sent, item.EventID)
	}
	crashAwaitEvents(t, first, sent, nil, a+"'s frames, each answered received alone")
	first.conn.Close()
	awaitNoRelaySubscriber(t, rig.url, a)
	rig.gone(b)
	rig.claim(rig.away, b, role)
	// Where the build copies at a takeover, the walk forwards the copies to B, which reads none of
	// them: the row waits a while for them, and goes on without them at a build that makes none.
	toB := contracts.LegacyAgentSubject(b)
	deadline := time.Now().Add(10 * time.Second)
	for len(rig.events(toB)) < len(sent) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("%s's own subject holds %v", b, rig.events(toB))
	back := startSessionStandIn(t, rig.url, a, answerDone)
	rig.register(rig.mux, a, "epoch-2")
	awaitAcked(t, rig.consumers, a, rig.seqOn(agent, sent[len(sent)-1]))
	rig.claim(rig.mux, a, role)
	crashAwaitEvents(t, back, sent, nil, a+", which took the role back")
	rig.idle(rig.cfg(), role)
	held := rig.events(agent)
	for i := range 5 {
		rig.sweep(rig.cfg(), role)
		if now := rig.events(agent); len(now) != len(held) {
			t.Fatalf("after sweep %d %s's own subject holds %d messages, %v; want the %d it held once it had each of the three, %v", i+1, a, len(now), now, len(held), held)
		}
	}
}

// (7) B and C claim the role outright at once, and both read A as its holder. C's try holds after
// its record (beforeClaimTry) while B's write lands and B is sent a role message it never reads;
// C's write then meets the conflict, reads again, writes a takeover record for B and lands, so A's
// message and B's both reach C.
func crashConcurrentClaims(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, c, role = "ses_cr7_a", "ses_cr7_b", "ses_cr7_c", "cr7"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	fromA := rig.sendTo(a, role, 1, "for the first holder")
	rig.gone(b)
	next := rig.relayed(c, answerDone)
	var reads atomic.Int32
	cRead, bLanded := make(chan struct{}), make(chan struct{})
	setTakeoverFaults(t, takeoverFaultSet{beforeClaimTry: func(r, previous string, _ int) error {
		if r == role && previous == a && reads.Add(1) == 1 {
			close(cRead)
			select {
			case <-bLanded:
			case <-time.After(3 * time.Second):
			}
		}
		return nil
	}})
	cAnswer := make(chan *httptest.ResponseRecorder, 1)
	go func() { cAnswer <- rig.claimThrough(rig.mux, c, role, "") }()
	t.Cleanup(func() {
		select {
		case <-cAnswer:
		case <-time.After(10 * time.Second):
		}
	})
	crashAwait(t, cRead, 10*time.Second, c+"'s claim reaching its write, having read "+a)
	rig.claim(rig.away, b, role)
	fromB := rig.sendTo(b, role, 1, "for the holder between")
	close(bLanded)
	var answer *httptest.ResponseRecorder
	select {
	case answer = <-cAnswer:
		cAnswer <- answer
	case <-time.After(10 * time.Second):
		t.Fatalf("%s's claim did not answer within 10s", c)
	}
	if answer.Code != http.StatusOK {
		t.Fatalf("%s's claim: status %d %s; want 200", c, answer.Code, answer.Body.String())
	}
	if holder := rig.holder(role); holder != c {
		t.Fatalf("role %s's claim names %q; want %s, whose write landed last", role, holder, c)
	}
	if records := rig.records(role, b); len(records) == 0 {
		t.Fatalf("the stream holds no takeover record of %s from %s; want the one %s's second try wrote", role, b, c)
	}
	crashAwaitEvents(t, next, append(fromA, fromB...), nil, c+", handed what "+a+" and "+b+" were sent")
}

// (7b) A renewal of the claim is written before every try of B's claim (beforeClaimTry), so each
// try's write meets a conflict until the request's deadline: the claim answers 503 with
// Retry-After: 1, the claim still names A, and B's record carries no topic of the role.
func crashConflictAtEveryTry(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr7b_a", "ses_cr7b_b", "cr7b"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	rig.relayed(b, answerDone)
	tries, failed := rig.renewEveryTry(role)
	recorder := rig.claimThrough(rig.mux, b, role, "")
	t.Logf("the claim made %d tries and answered %d (Retry-After %q): %s", tries.Load(), recorder.Code, recorder.Header().Get("Retry-After"), strings.TrimSpace(recorder.Body.String()))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("%s's claim: status %d, Retry-After %q, body %s; want 503 with Retry-After: 1", b, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if err := failed.Load(); err != nil {
		t.Fatalf("a renewal before a try failed: %v", *err)
	}
	if got := tries.Load(); got < 2 {
		t.Fatalf("the claim made %d tries; want it to read and write again after each conflict", got)
	}
	if holder := rig.holder(role); holder != a {
		t.Fatalf("role %s's claim names %q; want %s, since the claim wrote nothing", role, holder, a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	interest, err := rig.harness.registry.Get(ctx, b)
	if err != nil {
		t.Fatalf("read %s's record: %v", b, err)
	}
	if slices.Contains(interest.Topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("%s's record follows %v; want no topic of role %s, which the claim did not take", b, interest.Topics, role)
	}
}

// (8) The message on A's subject is the publish route's forward, its key under the forward mark:
// the copy reaches B under one forward mark, not two.
func crashCopiedForwardHasOnePrefix(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr8_a", "ses_cr8_b", "cr8"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	answer := rig.publish(role, "itself a forward")
	if answer.status != http.StatusOK || answer.Holder != a {
		t.Fatalf("publish to %s: status %d %s; want 200 stored for %s", role, answer.status, answer.raw, a)
	}
	want := roleForwardDedupePrefix + answer.DedupeKey
	if stored := rig.awaitStored(contracts.LegacyAgentSubject(a), answer.EventID, "the forward to "+a); stored.item.DedupeKey != want {
		t.Fatalf("the message on %s's own subject carries dedupe key %q; want the forward's %q", a, stored.item.DedupeKey, want)
	}
	next := rig.relayed(b, answerDone)
	rig.claim(rig.mux, b, role)
	frames := crashAwaitEvents(t, next, []string{answer.EventID}, nil, "the new holder")
	for _, frame := range frames {
		if frame.item.EventID == answer.EventID && frame.item.DedupeKey != want {
			t.Errorf("the new holder received event %s under dedupe key %q; want %q, one forward mark", answer.EventID, frame.item.DedupeKey, want)
		}
	}
}

// (9) A's subject holds more than two passes' worth (roleKeepScanMax) of messages to A itself,
// and three role messages behind them. The claim's handover writes its record after the first
// pass and stops before its second pass's write (beforeRecordWrite), as a listener that stops
// there; the listener started again goes on from the sequence in the record, and the three reach B.
func crashPassesAcrossARestart(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr9_a", "ses_cr9_b", "cr9"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	const toSession = 2*roleKeepScanMax + 50
	rig.storeToSession(a, toSession)
	sent := rig.sendTo(a, role, 3, "behind the session's own messages")
	held := rig.stored(contracts.LegacyAgentSubject(a))
	if len(held) != toSession+len(sent) {
		t.Fatalf("%s's own subject holds %d messages; want %d", a, len(held), toSession+len(sent))
	}
	// A read none of them, so the first pass reads from the first, roleKeepScanMax of them.
	wantNext := held[roleKeepScanMax-1].seq + 1
	next := rig.relayed(b, answerDone)
	var writes atomic.Int32
	var firstNext atomic.Uint64
	stopped, gone := make(chan struct{}), make(chan struct{})
	var goneOnce sync.Once
	t.Cleanup(func() { goneOnce.Do(func() { close(gone) }) })
	setTakeoverFaults(t, takeoverFaultSet{beforeRecordWrite: func(r, previous string, next uint64, _ bool) {
		if r != role || previous != a {
			return
		}
		switch writes.Add(1) {
		case 1:
			firstNext.Store(next)
		case 2:
			close(stopped)
			<-gone
		}
	}})
	rig.claim(rig.mux, b, role)
	crashAwait(t, stopped, 30*time.Second, "the claim's handover reaching its second pass's record write, where its listener stops")
	if got := firstNext.Load(); got != wantNext {
		t.Errorf("the first pass wrote its record at sequence %d; want %d, past the %d messages it read", got, wantNext, roleKeepScanMax)
	}
	if record := rig.onlyRecord(role, a); record.Next != wantNext {
		t.Errorf("the takeover record on the stream names sequence %d; want %d, where the first pass stopped", record.Next, wantNext)
	}
	restarted := rig.anotherListener()
	ctx, cancel := context.WithCancel(context.Background())
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		sweepOnce(ctx, restarted)
	}()
	t.Cleanup(func() {
		cancel()
		<-swept
	})
	crashAwaitEvents(t, next, sent, nil, "the new holder, from the restarted listener's passes")
}

// (10) The claim's pass cannot read A's floor (floorRead), so the record is unfinished when B
// releases the role; a forward to A is then stored late, inside the hour of the claim. The sweep's
// pass copies all four, and the role's next holder, C, receives each.
func crashReleaseWhileUnfinished(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, c, role = "ses_cr10_a", "ses_cr10_b", "ses_cr10_c", "cr10"
	const instance = "process-of-the-new-holder"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "before the release")
	rig.relayed(b, answerDone)
	var failing atomic.Bool
	failing.Store(true)
	var refused atomic.Int32
	setTakeoverFaults(t, takeoverFaultSet{floorRead: func(previous string) error {
		if previous == a && failing.Load() {
			refused.Add(1)
			return errors.New("the read of the previous holder's consumer failed")
		}
		return nil
	}})
	if recorder := rig.claimThrough(rig.mux, b, role, instance); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status %d %s; want 200", b, role, recorder.Code, recorder.Body.String())
	}
	waitFor(t, 10*time.Second, "the claim's pass meeting the failed read", func() bool { return refused.Load() > 0 })
	rig.release(b, role, instance)
	late := rig.lateForward(a, role, "stored after the release")
	failing.Store(false)
	rig.sweep(rig.cfg(), role)
	all := append(slices.Clone(sent), late)
	crashRequireOneEach(t, "role "+role+"'s kept lane", rig.kept(role), all)
	next := rig.relayed(c, answerDone)
	rig.claim(rig.mux, c, role)
	crashAwaitEvents(t, next, all, nil, "the role's next holder")
}

// (10b) A role-lane forward to A waits in A's queue behind another (holdForward) while the clock
// moves past the fence (roleKeepWindow less one roleWakeInterval) and B takes the role: when its
// publish starts it resolves the holder again and is stored for B, and A's subject never holds it.
func crashForwardPastTheFence(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr10b_a", "ses_cr10b_b", "cr10b"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	next := rig.relayed(b, answerDone)
	const blocker = "holds the previous holder's queue"
	held, release := rig.holdForward(blocker)
	rig.onLane(role, blocker)
	crashAwait(t, held, 10*time.Second, "the first forward to "+a+" starting")
	target := rig.onLane(role, "waits in the previous holder's queue")
	waitFor(t, 10*time.Second, "the second forward queued behind the first", func() bool {
		return rig.cfg().roleForwards.outstanding() == 2
	})
	rig.clock.advance(roleKeepWindow - roleWakeInterval + time.Minute)
	rig.claim(rig.mux, b, role)
	release()
	rig.awaitStored(contracts.LegacyAgentSubject(b), target.EventID, "the waiting forward, resolved again, on the new holder's own subject")
	crashAwaitEvents(t, next, []string{target.EventID}, nil, "the new holder")
	if slices.Contains(rig.events(contracts.LegacyAgentSubject(a)), target.EventID) {
		t.Fatalf("%s's own subject holds event %s; want it stored for the new holder alone", a, target.EventID)
	}
}

// (11) A role-lane forward to A waits in A's queue (holdForward) past B's claim and the claim's
// first pass, and is stored on A's subject after it: the next pass copies it, and B receives it.
func crashForwardStoredAfterTheFirstPass(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr11_a", "ses_cr11_b", "cr11"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	first := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	const waiting = "waits in the previous holder's queue"
	held, release := rig.holdForward(waiting)
	target := rig.onLane(role, waiting)
	crashAwait(t, held, 10*time.Second, "the forward to "+a+" starting and waiting")
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, first, nil, "the new holder, from the claim's first pass")
	release()
	rig.awaitStored(contracts.LegacyAgentSubject(a), target.EventID, "the waiting forward stored on "+a+"'s own subject after the first pass")
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, []string{target.EventID}, nil, "the new holder, from the next pass")
}

// (12) The claim is written with the listener's clock two hours back, so the takeover is more than
// roleKeepWindow old when anything runs a pass, and no listener runs one meanwhile
// (skipClaimHandover). A listener started then sweeps, finds the takeover record and copies A's
// three, and B receives them.
func crashListenersDownPastTheHour(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr12_a", "ses_cr12_b", "cr12"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 3, "while every listener is down")
	next := rig.relayed(b, answerDone)
	setTakeoverFaults(t, takeoverFaultSet{skipClaimHandover: func(r string) bool { return r == role }})
	rig.clock.advance(-2 * roleKeepWindow)
	rig.claim(rig.mux, b, role)
	rig.clock.advance(2 * roleKeepWindow)
	if kept := rig.kept(role); len(kept) != 0 {
		t.Fatalf("role %s's kept lane holds %v while no listener ran a pass; want nothing", role, kept)
	}
	if records := rig.records(role, a); len(records) != 1 || time.Since(time.UnixMilli(records[0].ClaimedAt)) <= roleKeepWindow {
		t.Errorf("the takeover records of %s from %s are %+v; want one whose claim is more than %s ago", role, a, records, roleKeepWindow)
	}
	up := rig.anotherListener()
	rig.sweep(up, role)
	crashAwaitEvents(t, next, sent, nil, "the new holder, from the sweep of the listener that is up again")
}

// (12b) The claim is written with the listener's clock two hours back; after its own pass a
// forward to A is stored, two hours after the takeover on that clock. The next pass copies it.
func crashForwardTwoHoursLate(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr12b_a", "ses_cr12b_b", "cr12b"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	first := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	rig.clock.advance(-2 * roleKeepWindow)
	rig.claim(rig.mux, b, role)
	rig.clock.advance(2 * roleKeepWindow)
	crashAwaitEvents(t, next, first, nil, "the new holder, from the claim's own pass")
	late := rig.lateForward(a, role, "stored two hours after the takeover")
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, []string{late}, nil, "the new holder, from the next pass")
}

// crashRecordWrite is one write of a takeover record as the beforeRecordWrite seam reports it.
type crashRecordWrite struct {
	next     uint64
	finished bool
}

// (12d) The claim is written with the listener's clock 70 hours 55 minutes back, and the sweep is
// run pass after pass (its interval shortened). Passes that find nothing new each write the
// record again, unfinished, so the stream's age limit, counted from the last write, never removes
// it; a forward stored 70 hours 55 minutes after the takeover is copied; and only once the clock is
// past 71 hours does a pass mark the record finished, and only the pass that reads to the end:
// with more than one pass's worth of messages behind the floor, the pass before it writes the
// record unfinished.
func crashRecordHeldToItsLastHour(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr12d_a", "ses_cr12d_b", "cr12d"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	first := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	var mu sync.Mutex
	var writes []crashRecordWrite
	setTakeoverFaults(t, takeoverFaultSet{
		skipClaimHandover: func(r string) bool { return r == role },
		beforeRecordWrite: func(r, previous string, n uint64, finished bool) {
			if r == role && previous == a {
				mu.Lock()
				writes = append(writes, crashRecordWrite{next: n, finished: finished})
				mu.Unlock()
			}
		},
	})
	const sinceClaim = 70*time.Hour + 55*time.Minute
	rig.clock.advance(-sinceClaim)
	rig.claim(rig.mux, b, role)
	rig.clock.advance(sinceClaim)
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, first, nil, "the new holder, from the record's first pass")
	record := rig.onlyRecord(role, a)
	if record.Finished {
		t.Fatalf("the record %+v is finished by a pass that started before 71 hours", record)
	}
	for i := range 3 {
		rig.sweep(rig.cfg(), role)
		again := rig.onlyRecord(role, a)
		if again.Finished {
			t.Fatalf("pass %d after the first, before 71 hours, finished the record %+v", i+1, again)
		}
		if !again.written.After(record.written) {
			t.Fatalf("pass %d after the first left the record as written at %s; want it written again, so the stream's age limit starts again", i+1, record.written)
		}
		record = again
	}
	late := rig.lateForward(a, role, "stored 70 hours 55 minutes after the takeover")
	rig.sweep(rig.cfg(), role)
	crashAwaitEvents(t, next, []string{late}, nil, "the new holder, from the pass after the late forward")
	if record := rig.onlyRecord(role, a); record.Finished {
		t.Fatalf("the record %+v is finished before 71 hours", record)
	}
	rig.clock.advance(10 * time.Minute)
	rig.storeToSession(a, roleKeepScanMax+50)
	held := rig.stored(contracts.LegacyAgentSubject(a))
	end := held[len(held)-1].seq + 1
	mu.Lock()
	writes = nil
	mu.Unlock()
	rig.sweep(rig.cfg(), role)
	mu.Lock()
	past := slices.Clone(writes)
	mu.Unlock()
	if len(past) < 2 {
		t.Fatalf("the sweep past 71 hours wrote the record %d times (%+v); want a pass that did not reach the end and the one that did", len(past), past)
	}
	for i, write := range past[:len(past)-1] {
		if write.finished || write.next >= end {
			t.Fatalf("write %d of the sweep past 71 hours is %+v; want it unfinished, short of the end at %d", i+1, write, end)
		}
	}
	if last := past[len(past)-1]; !last.finished || last.next != end {
		t.Fatalf("the last write of the sweep past 71 hours is %+v; want it finished at the end, %d", last, end)
	}
	if record := rig.onlyRecord(role, a); !record.Finished || record.Next != end {
		t.Fatalf("the record on the stream is %+v; want it finished at %d", record, end)
	}
}

// (12c) A renewal of A's claim is written before every try of B's claim (beforeClaimTry): the
// claim answers 503, A keeps the role, each try has written a takeover record of A, and the
// records' passes copy nothing while the claim names A, which receives each of its role messages
// from its own consumer (crashKeptByItsHolder).
func crashRenewalAtEveryTry(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_cr12c_a", "ses_cr12c_b", "cr12c"
	keeper := rig.relayed(a, crashAnswerReceived)
	rig.claim(rig.mux, a, role)
	sent := rig.sendTo(a, role, 3, "for the holder that renews")
	crashAwaitEvents(t, keeper, sent, nil, "the holder's own frames, each answered received alone")
	rig.relayed(b, answerDone)
	tries, failed := rig.renewEveryTry(role)
	recorder := rig.claimThrough(rig.mux, b, role, "")
	t.Logf("the claim made %d tries and answered %d (Retry-After %q): %s", tries.Load(), recorder.Code, recorder.Header().Get("Retry-After"), strings.TrimSpace(recorder.Body.String()))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("%s's claim: status %d, Retry-After %q, body %s; want 503 with Retry-After: 1", b, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if err := failed.Load(); err != nil {
		t.Fatalf("a renewal before a try failed: %v", *err)
	}
	if holder := rig.holder(role); holder != a {
		t.Fatalf("role %s's claim names %q; want %s, which renewed it", role, holder, a)
	}
	if records := rig.records(role, a); len(records) < 2 {
		t.Errorf("the stream holds %d takeover records of %s from %s; want one for each try", len(records), role, a)
	}
	setTakeoverFaults(t, takeoverFaultSet{})
	crashKeptByItsHolder(t, rig, keeper, a, role, sent)
}

// (13, control) B takes the role from A with its handover stopped (skipClaimHandover), so the
// takeover record is on the stream with no pass run, and releases it; a publish to the role is
// then kept behind that record. The wake scan reads no copy while the kept lane holds none and
// one once it holds the kept copy, the read Dispatch's outbox handover makes past the drain cursor
// is that copy, and the walk of the role's next holder, C, hands it the kept copy and nothing that
// is not a role message.
func crashRecordAheadOfAKeptCopy(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, c, role = "ses_cr13_a", "ses_cr13_b", "ses_cr13_c", "cr13"
	const instance = "process-of-the-new-holder"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	fromA := rig.sendTo(a, role, 1, "for the first holder")
	rig.relayed(b, answerDone)
	var skip atomic.Bool
	skip.Store(true)
	setTakeoverFaults(t, takeoverFaultSet{skipClaimHandover: func(r string) bool { return r == role && skip.Load() }})
	if recorder := rig.claimThrough(rig.mux, b, role, instance); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status %d %s; want 200", b, role, recorder.Code, recorder.Body.String())
	}
	rig.release(b, role, instance)
	t.Logf("the takeover records ahead of the kept lane: %+v", rig.records(role, a))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if wakes, err := keptLaneWakesItsRole(ctx, rig.harness.client, role, 1, time.Now()); err != nil || wakes {
		t.Fatalf("the wake scan of role %s's kept lane, which holds no copy: %v, %v; want no wake", role, wakes, err)
	}
	kept := rig.publish(role, "kept behind a takeover record")
	if kept.status != http.StatusAccepted || !kept.Kept {
		t.Fatalf("publish to the unclaimed role %s: status %d %s; want 202 kept", role, kept.status, kept.raw)
	}
	waitForKeptRoleMessage(t, rig.harness.client, role, kept.EventID)
	if wakes, err := keptLaneWakesItsRole(ctx, rig.harness.client, role, 1, time.Now()); err != nil || !wakes {
		t.Fatalf("the wake scan of role %s's kept lane, which holds the kept copy: %v, %v; want a wake", role, wakes, err)
	}
	cursor, err := rig.harness.client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's drain cursor: %v", role, err)
	}
	lead, err := rig.harness.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), cursor.Cursor.Kept+1)
	if err != nil {
		t.Fatalf("read role %s's kept lane past its cursor %d: %v", role, cursor.Cursor.Kept, err)
	}
	var head contracts.Envelope
	if err := json.Unmarshal(lead.Data, &head); err != nil || head.EventID != kept.EventID || head.DedupeKey != kept.DedupeKey {
		t.Fatalf("the first copy past role %s's cursor is %s (%v); want the kept copy %s under %s", role, lead.Data, err, kept.EventID, kept.DedupeKey)
	}
	skip.Store(false)
	next := rig.relayed(c, answerDone)
	rig.claim(rig.mux, c, role)
	frames := crashAwaitEvents(t, next, []string{kept.EventID}, nil, "the role's next holder")
	frames = append(frames, crashDrain(next, 2*time.Second)...)
	known := append(slices.Clone(fromA), kept.EventID)
	for _, frame := range frames {
		if !slices.Contains(known, frame.item.EventID) {
			t.Errorf("the role's next holder received %s (event %q, topic %q); want only role messages: the kept copy and %s's message", frame.msg.Data, frame.item.EventID, frame.item.Topic, a)
		}
	}
}
