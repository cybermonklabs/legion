package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The lines and the counters the tests of a kept copy's hour read, by the text and the series name
// a listener writes, so a test reads what an operator reads. The counters are the harness's own
// names for the listener's envoy_role_kept_expired_total, envoy_role_dead_letters_total and
// envoy_role_kept_undecodable_total; envoy_messages_delivered_total is read as deliveredSeries
// names it.
const (
	hourDropLine        = "listener dropped a kept role message older than the keep window"
	hourDeadLetterLine  = "listener stored a dead letter for a kept role message"
	hourRefusedLine     = "listener could not send a kept role message's dead letter"
	hourForwardLine     = "listener role forwarded"
	hourUndecodableLine = "listener kept role message cannot be decoded"
	hourModeLine        = "listener kept role message's new holder does not advertise delivery mode"
	hourRoleSendLine    = "listener publish role send"
	hourWakeStoredLine  = "legion wake report was already stored"
	hourWakeLine        = "legion wake report published"

	hourExpired         = `test_role_kept_expired{kept_by="listener",stale="false"}`
	hourExpiredStale    = `test_role_kept_expired{kept_by="listener",stale="true"}`
	hourExpiredOutbox   = `test_role_kept_expired{kept_by="dispatch_outbox",stale="false"}`
	hourLettersStored   = `test_role_dead_letters{result="stored"}`
	hourLettersRefused  = `test_role_dead_letters{result="refused"}`
	hourUndecodable     = "test_role_kept_undecodable"
	hourExpiryExplained = "No session held the role in the hour that Envoy kept the message."
)

// hourRig is two listeners of one deployment over one test's stream: testnats.URL gives a test one
// JetStream account however often it asks, so both reach the same streams and buckets. Each is a
// delivery harness whose configuration reads the rig's clock (cfg.now), with its role forwarder
// wired again for that seam (wireTestRoleForwarder), the publish route and the role lane over
// that configuration, and the metrics and the log lines a test reads.
//
// The clock is the real time moved on by an offset the test sets, held in an atomic because the
// walks read it from goroutines of their own. A copy's age is that clock against the time the
// stream stored the copy, which no test may choose, so moving the clock is how a test crosses a
// copy's hour.
type hourRig struct {
	t         *testing.T
	uri       string
	offset    atomic.Int64
	listeners [2]*hourListener
}

// hourListener is one listener of an hourRig.
type hourListener struct {
	harness listenerDeliveryHarness
	cfg     listenerDeliveryHandlerConfig
	// route is the publish route, the role-claim route and the session routes over cfg, as main
	// hands them its delivery configuration.
	route *listenerDeps
	// lane is the role lane's handler over cfg: a raw role publish handed to it is one the lane
	// took on its own subscription.
	lane natsgo.MsgHandler
	// sweepPasses counts the passes its sweep has ended (startSweeps), and sweepStarted says
	// whether startSweeps has started that sweep.
	sweepPasses  atomic.Int64
	sweepStarted bool
}

func newHourRig(t *testing.T, sessionOptions ...session.SessionRegistryOption) *hourRig {
	t.Helper()
	rig := &hourRig{t: t, uri: testnats.URL(t)}
	for i := range rig.listeners {
		harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t), sessionOptions...)
		l := &hourListener{harness: harness, cfg: harness.config}
		l.cfg.now = rig.now
		l.route = harness.publishDeps(t)
		l.route.delivery = &l.cfg
		l.rewire(t)
		rig.listeners[i] = l
	}
	return rig
}

// rewire wires l's role forwarder again from cfg and builds the role lane over it, which a seam
// set on cfg needs, since the forwarder keeps the configuration it was built from. The route reads
// cfg itself.
func (l *hourListener) rewire(t *testing.T) {
	t.Helper()
	l.cfg = wireTestRoleForwarder(t, l.cfg)
	l.lane = coreNATSDeliveryHandler(l.cfg)
}

// now is the listeners' clock.
func (r *hourRig) now() time.Time { return time.Now().Add(time.Duration(r.offset.Load())) }

// moveClock sets the listeners' clock to the real time plus by.
func (r *hourRig) moveClock(by time.Duration) { r.offset.Store(int64(by)) }

// setClock sets the listeners' clock so that it reads at as of this moment.
func (r *hourRig) setClock(at time.Time) { r.offset.Store(int64(time.Until(at))) }

// client is the first listener's connection, which the tests read the stream through.
func (r *hourRig) client() *bus.Client { return r.listeners[0].harness.client }

// hourSweepInterval is how often the rig's listeners sweep their kept lanes, where main.go sweeps
// every roleWakeInterval. It is short because no row waits out an interval: letSweepsRun waits for
// a count of passes, and every other wait polls for the state a pass makes.
const hourSweepInterval = 200 * time.Millisecond

// startSweeps starts each listener's sweep of the kept lanes, the loop startKeptRoleSweep runs over
// its pool of roleKeepSweepHandovers slots, at hourSweepInterval and counting each pass whose scan
// of the lanes has ended (sweepKeptRoleLanes returned), and stops both when the test ends, waiting
// for the pass each loop has in flight. A count does not wait for the handovers a pass started: a
// handover runs on, under roleKeepHandoverWindow (10 minutes), for as long as its lane has a holder
// to give copies to, holding its role's guard and its slot, and a pass after it passes that role
// over (roleKeepGuards.tryHold). A wait for that work would wait for the window, not for the sweep.
func (r *hourRig) startSweeps() {
	ctx, cancel := context.WithCancel(context.Background())
	var loops []<-chan struct{}
	for _, l := range r.listeners {
		cfg, slots, passes := l.cfg, make(chan struct{}, roleKeepSweepHandovers), &l.sweepPasses
		l.sweepStarted = true
		loops = append(loops, startSweep(ctx, hourSweepInterval, func(ctx context.Context) {
			sweepKeptRoleLanes(ctx, cfg, slots)
			passes.Add(1)
		}))
	}
	r.t.Cleanup(func() {
		cancel()
		for _, done := range loops {
			<-done
		}
	})
}

// letSweepsRun waits until each listener's sweep has scanned its lanes twice more, in two passes
// that began after the call (startSweeps). It is the wait these rows make for nothing more to
// happen, which only the sweeps could still make happen, as the 2.5 s sleep it replaces was, and it
// counts passes rather than waiting out a stretch of time.
func (r *hourRig) letSweepsRun() {
	r.t.Helper()
	for i, l := range r.listeners {
		if !l.sweepStarted {
			r.t.Fatalf("listener %d's sweep was waited for before startSweeps started it", i)
		}
		// The pass in flight now may have begun before the call; the two after it did not. Three
		// passes need about 600 ms (hourSweepInterval), so the package's own bound leaves this
		// wait a hundred times what it needs, far past the ten times a wait of its own would take.
		through := l.sweepPasses.Load() + 3
		waitFor(r.t, fmt.Sprintf("two more passes of listener %d's kept-lane sweep", i), func() bool {
			return l.sweepPasses.Load() >= through
		})
	}
}

// hourAnswer is the publish route's answer as these tests read it: the envelope it names and the
// fields of a 202 kept.
type hourAnswer struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	DedupeKey      string `json:"dedupe_key"`
	PayloadSummary string `json:"payload_summary"`
	Holder         string `json:"holder"`
	HolderState    string `json:"holder_state"`
	Kept           bool   `json:"kept"`
	KeptReason     string `json:"kept_reason"`
	KeptUntil      int64  `json:"kept_until"`
}

// roleMessage is the body of a publish of message to role's topic from sender, which names no
// source_session when sender is empty.
func roleMessage(role, sender, message string) map[string]any {
	fields := map[string]any{"topic": contracts.RoleTopicPrefix + role, "source": "agent", "message": message}
	if sender != "" {
		fields["source_session"] = sender
	}
	return fields
}

// publish sends fields to l's publish route and returns its answer, decoded for a 200 or a 202.
func (r *hourRig) publish(l *hourListener, fields map[string]any) (*httptest.ResponseRecorder, hourAnswer) {
	r.t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatalf("encode the publish %v: %v", fields, err)
	}
	recorder, _ := publishThrough(l.route, string(body))
	var answer hourAnswer
	if recorder.Code == http.StatusOK || recorder.Code == http.StatusAccepted {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			r.t.Fatalf("decode the publish answer %s: %v", recorder.Body.String(), err)
		}
	}
	return recorder, answer
}

// kept publishes fields through l's route and fails unless the route answered 202 kept.
func (r *hourRig) kept(l *hourListener, fields map[string]any) hourAnswer {
	r.t.Helper()
	recorder, answer := r.publish(l, fields)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		r.t.Fatalf("publish %v: status %d, body %s; want 202 kept", fields, recorder.Code, recorder.Body.String())
	}
	return answer
}

// claim registers sessionID with entry and claims role for it through l's POST /v1/roles/set,
// whose answer starts a handover of the role's kept lane on l's configuration.
func (r *hourRig) claim(l *hourListener, sessionID, role string, entry session.SessionEntry) {
	r.t.Helper()
	if err := l.harness.sessions.Put(sessionID, entry); err != nil {
		r.t.Fatalf("register %s: %v", sessionID, err)
	}
	recorder := httptest.NewRecorder()
	roleSetHandler(l.route, "test-machine", logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"`+sessionID+`","role":"`+role+`"}`)))
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("claim of %s by %s: status %d, body %s", role, sessionID, recorder.Code, recorder.Body.String())
	}
}

// register makes sessionID register through l's POST /v1/interests/subscribe, the heartbeat a
// running client repeats (registerThrough), on l's route given the session-consumer connection
// main's listener holds, without which that route refuses every registration
// (setupSessionConsumers). The registration states instance as the process making it, empty for
// none. A registration stating none renews a claim that names none, as claimRoleWith's does
// (store.Registry.SetRole), the way a client's heartbeat renews the claim naming its own process;
// one stating a process the claim does not name renews nothing, as a new process that resumed the
// session renews nothing of the claim the process before it made (store.Registry.RenewRoleClaims).
func (r *hourRig) register(l *hourListener, sessionID, instance string) {
	r.t.Helper()
	l.route.consumers = setupSessionConsumers(r.t, l.harness.client)
	registerThrough(r.t, subscribeHandler(l.route, "test-machine", logging.New("test")), sessionID, instance)
}

// deregister deletes sessionID's record through l's DELETE /v1/sessions/<id> and waits until both
// listeners read the session as not live, so neither forwards to it from a session cache that has
// not seen the delete yet.
func (r *hourRig) deregister(l *hourListener, sessionID string) {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	deleteSessionHandler(l.route, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil))
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("DELETE /v1/sessions/%s: status %d, body %s", sessionID, recorder.Code, recorder.Body.String())
	}
	waitFor(r.t, "both listeners read "+sessionID+" as not live", func() bool {
		for _, l := range r.listeners {
			if _, state := l.harness.sessions.Lookup(sessionID); state == session.Live {
				return false
			}
		}
		return true
	})
}

// goneHolder makes holder claim role through the first listener's claim route and then delete its
// session record, so the claim names a gone session.
func (r *hourRig) goneHolder(holder, role string) {
	r.t.Helper()
	r.claim(r.listeners[0], holder, role, liveRoleActEntry)
	r.deregister(r.listeners[0], holder)
}

// reapClaim frees role's claim, whose holder's session is gone, through a pass of the first
// listener's role-claim reaper. The claim is aged past the live window first, since a holder that
// left stopped renewing it; the registry opened before the claim was made, so the claim is not one
// it restored and no restart grace holds it.
func (r *hourRig) reapClaim(role string) {
	r.t.Helper()
	first := r.listeners[0]
	ageRoleClaim(r.t, first.route, role, 2*session.LiveWindow)
	isAlive := func(sessionID string) bool {
		return roleHolderMayBeLive(logging.New("test"), first.harness.sessions, sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if sweep, err := first.harness.registry.ReapRoleClaims(ctx, isAlive, first.harness.sessions.TTL(), session.LiveWindow); err != nil || sweep.Reaped != 1 {
		r.t.Fatalf("reap the role claims = %+v, %v; want role %s's claim freed", sweep, err, role)
	}
}

// laneCopy is one copy on a role's kept lane as the stream holds it.
type laneCopy struct {
	sequence uint64
	stored   time.Time
	item     contracts.Envelope
}

// listenerCopy is a copy as the listener's keep stores one for a publish of summary from sender
// (no source_session when sender is empty): the route's envelope, under a publish.<id> key as the
// route mints one.
func listenerCopy(role, sender, summary string) contracts.Envelope {
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "publish."+id.New())
	item.SourceSession = sender
	item.PayloadSummary = summary
	return item
}

// outboxCopy is a copy as Dispatch's outbox stores it for a role-routed event: source dispatch,
// the event's dispatch-<id> key, and the session the event names.
func outboxCopy(role, sender, summary string) contracts.Envelope {
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "dispatch-"+id.New())
	item.Source = "dispatch"
	item.SourceSession = sender
	item.PayloadSummary = summary
	return item
}

// keep writes item to role's kept lane as Dispatch's outbox writes a copy, under
// bus.KeptMsgID, and returns the copy the stream holds for it.
func (r *hourRig) keep(role string, item contracts.Envelope) laneCopy {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client().PublishStoredAs(ctx, bus.KeptRoleSubject(role), item, bus.KeptMsgID(item.DedupeKey, role)); err != nil {
		r.t.Fatalf("write event %s to role %s's kept lane: %v", item.EventID, role, err)
	}
	stored, err := r.client().LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if err != nil {
		r.t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	held := laneCopy{sequence: stored.Sequence, stored: stored.Time}
	if err := json.Unmarshal(stored.Data, &held.item); err != nil || held.item.EventID != item.EventID {
		r.t.Fatalf("role %s's newest copy is %s (%v), want event %s", role, stored.Data, err, item.EventID)
	}
	return held
}

// readStored is every message the stream holds on subject, in stream order.
func readStored(ctx context.Context, client *bus.Client, subject string) ([]bus.StoredMessage, error) {
	var found []bus.StoredMessage
	for next := uint64(1); ; {
		stored, err := client.NextStoredMessage(ctx, subject, next)
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return found, nil
		}
		if err != nil {
			return found, fmt.Errorf("read %s from %d: %w", subject, next, err)
		}
		found = append(found, stored)
		next = stored.Sequence + 1
	}
}

// stored is readStored for the test's own goroutine.
func (r *hourRig) stored(subject string) []bus.StoredMessage {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	found, err := readStored(ctx, r.client(), subject)
	if err != nil {
		r.t.Fatal(err)
	}
	return found
}

// envelopesOn is every envelope the stream holds on subject, in stream order.
func (r *hourRig) envelopesOn(subject string) []contracts.Envelope {
	r.t.Helper()
	var found []contracts.Envelope
	for _, stored := range r.stored(subject) {
		var item contracts.Envelope
		if err := json.Unmarshal(stored.Data, &item); err != nil {
			r.t.Fatalf("decode the message stored at %d on %s: %v", stored.Sequence, stored.Subject, err)
		}
		found = append(found, item)
	}
	return found
}

// laneCopies is every copy on role's kept lane.
func (r *hourRig) laneCopies(role string) []laneCopy {
	r.t.Helper()
	var copies []laneCopy
	for _, stored := range r.stored(bus.KeptRoleSubject(role)) {
		held := laneCopy{sequence: stored.Sequence, stored: stored.Time}
		if err := json.Unmarshal(stored.Data, &held.item); err != nil {
			r.t.Fatalf("decode role %s's copy at %d: %v", role, stored.Sequence, err)
		}
		copies = append(copies, held)
	}
	return copies
}

// copyOf is the copy of eventID on role's kept lane.
func (r *hourRig) copyOf(role, eventID string) laneCopy {
	r.t.Helper()
	for _, held := range r.laneCopies(role) {
		if held.item.EventID == eventID {
			return held
		}
	}
	r.t.Fatalf("role %s's kept lane holds no copy of event %s", role, eventID)
	return laneCopy{}
}

// deadLetters is every envelope from Envoy on sender's agent subject that names eventID as its
// source event.
func (r *hourRig) deadLetters(sender, eventID string) []contracts.Envelope {
	r.t.Helper()
	var found []contracts.Envelope
	for _, item := range r.envelopesOn(contracts.LegacyAgentSubject(sender)) {
		if item.Source == "envoy" && item.SourceEventID == eventID {
			found = append(found, item)
		}
	}
	return found
}

// envoyLetters is every envelope from Envoy on any session's agent subject.
func (r *hourRig) envoyLetters() []contracts.Envelope {
	r.t.Helper()
	var found []contracts.Envelope
	for _, item := range r.envelopesOn(contracts.AgentTopicPrefix + ">") {
		if item.Source == "envoy" {
			found = append(found, item)
		}
	}
	return found
}

// cursor is role's drain cursor.
func (r *hourRig) cursor(role string) bus.KeptRoleCursor {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := r.client().ReadKeptRoleCursor(ctx, role)
	if err != nil {
		r.t.Fatalf("read role %s's drain cursor: %v", role, err)
	}
	return stored.Cursor
}

// waitPassed waits until role's cursor is past the copy stored at seq: the last write a walk makes
// for a copy it ended or forwarded, after the dead letter or the forward a test may already read.
func (r *hourRig) waitPassed(role string, seq uint64) {
	r.t.Helper()
	waitFor(r.t, fmt.Sprintf("role %s's cursor passing its copy at %d", role, seq), func() bool {
		return r.cursor(role).Kept >= seq
	})
}

// waitDeadLetter waits until the stream holds the dead letter of held on its sender's agent
// subject.
func (r *hourRig) waitDeadLetter(held laneCopy) {
	r.t.Helper()
	waitFor(r.t, "the dead letter of event "+held.item.EventID, func() bool {
		return len(r.deadLetters(held.item.SourceSession, held.item.EventID)) > 0
	})
}

// count is the sum of series over both listeners' metrics, 0 where neither has counted it.
func (r *hourRig) count(series string) int64 {
	r.t.Helper()
	var total int64
	for _, l := range r.listeners {
		total += max(0, counterValue(r.t, metricsBody(r.t, l.harness), series))
	}
	return total
}

// linesIn is every JSON record of logs named name whose attributes hold each value of where, as
// the record decodes it: a string, a float64 or a bool.
func linesIn(logs, name string, where map[string]any) []map[string]any {
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["msg"] != name {
			continue
		}
		matches := true
		for key, value := range where {
			if record[key] != value {
				matches = false
				break
			}
		}
		if matches {
			found = append(found, record)
		}
	}
	return found
}

// lines is linesIn over both listeners' logs.
func (r *hourRig) lines(name string, where map[string]any) []map[string]any {
	var found []map[string]any
	for _, l := range r.listeners {
		found = append(found, linesIn(l.harness.logs.String(), name, where)...)
	}
	return found
}

// returnTo checks the copy of eventID kept for holder, a session that is not live: the sweeps
// hand it nothing while it is not live, and once it registers again the copy reaches its agent
// subject once, which frames, its own subscription of that subject, receives once.
func (r *hourRig) returnTo(holder, eventID string, frames <-chan contracts.Envelope) {
	r.t.Helper()
	r.letSweepsRun()
	if held := r.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 0 {
		r.t.Fatalf("%s's agent subject holds %d messages while it is not live, want none", holder, len(held))
	}
	if err := r.listeners[1].harness.sessions.Put(holder, liveRoleActEntry); err != nil {
		r.t.Fatalf("register %s again: %v", holder, err)
	}
	waitFor(r.t, "the kept copy reaching "+holder, func() bool {
		return len(r.envelopesOn(contracts.LegacyAgentSubject(holder))) > 0
	})
	r.letSweepsRun()
	if held := r.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 1 || held[0].EventID != eventID {
		r.t.Fatalf("%s's agent subject holds %+v, want the kept copy of event %s once", holder, held, eventID)
	}
	if got := expectFrames(r.t, frames, 1, 100*time.Millisecond); got[0].EventID != eventID {
		r.t.Fatalf("%s received event %s, want the kept %s", holder, got[0].EventID, eventID)
	}
}

// assertDeadLetterOf fails unless letter is the dead letter of held, a copy of role's lane: from
// Envoy on its sender's agent subject, naming the copy as its source event, with a summary that
// names the role, and a text that names the role, the copy's event id, the time Envoy kept it, the
// time Envoy dropped it - no earlier than droppedAfter and no later than droppedBy - and each of
// says, and that gives the copy's first line for a copy from any source but dispatch and none of
// its summary for a Dispatch frame (keptCopyNamed).
func assertDeadLetterOf(t *testing.T, letter contracts.Envelope, role string, held laneCopy, droppedAfter, droppedBy time.Time, says ...string) {
	t.Helper()
	if letter.Source != "envoy" || letter.SourceSession != "" || letter.Topic != contracts.LegacyAgentSubject(held.item.SourceSession) || letter.SourceEventID != held.item.EventID {
		t.Fatalf("the dead letter is %+v, want one from envoy on %s naming event %s", letter, contracts.LegacyAgentSubject(held.item.SourceSession), held.item.EventID)
	}
	if !strings.HasPrefix(letter.PayloadSummary, "Envoy dropped your message to role "+role+": ") {
		t.Fatalf("the dead letter's summary is %q, want it to name role %s", letter.PayloadSummary, role)
	}
	kept := "Envoy kept the message at " + held.stored.UTC().Format(time.RFC3339)
	for _, want := range append([]string{role, held.item.EventID, kept}, says...) {
		if !strings.Contains(letter.Payload, want) {
			t.Fatalf("the dead letter's text does not say %q:\n%s", want, letter.Payload)
		}
	}
	if quoted := strings.Contains(letter.Payload, held.item.PayloadSummary); quoted != (held.item.Source != "dispatch") {
		t.Fatalf("the dead letter of a copy from source %s quotes its summary %q: %t, want %t:\n%s",
			held.item.Source, held.item.PayloadSummary, quoted, !quoted, letter.Payload)
	}
	_, rest, found := strings.Cut(letter.Payload, "Envoy dropped the message at ")
	stamp, _, _ := strings.Cut(rest, ".")
	dropped, err := time.Parse(time.RFC3339, stamp)
	if !found || err != nil {
		t.Fatalf("the dead letter's text names no time it was dropped (%v):\n%s", err, letter.Payload)
	}
	if dropped.Before(droppedAfter.Truncate(time.Second)) || dropped.After(droppedBy) {
		t.Fatalf("the dead letter says the copy was dropped at %s, want between %s and %s", dropped, droppedAfter, droppedBy)
	}
}

// deadLetterSeam is the dead letter's publish of both listeners (cfg.publishDeadLetter): the first
// call fails with bus.ErrStreamFull, an error that can pass, after running failed; every later
// call publishes as main.go does, through that listener's bus.Client.PublishStoredAs. Each call
// records role's cursor and how many messages the dead letter's subject held when it was made.
// The cursor writes go to the stream as they do in the listener.
type deadLetterSeam struct {
	role   string
	failed func()
	mu     sync.Mutex
	calls  []deadLetterCall
}

// deadLetterCall is what one call of a deadLetterSeam found.
type deadLetterCall struct {
	// at is the listeners' clock when the call was made.
	at time.Time
	// cursor is the role's drain cursor when the call was made.
	cursor bus.KeptRoleCursor
	// held is how many messages the dead letter's subject held when the call was made.
	held int
	// err is a read of the lane or the subject the call could not make.
	err error
}

// install sets the seam on both of rig's listeners. A test installs it before it starts the
// sweeps, which copy the configuration they walk with.
func (s *deadLetterSeam) install(t *testing.T, rig *hourRig) {
	t.Helper()
	for _, l := range rig.listeners {
		client := l.harness.client
		l.cfg.publishDeadLetter = func(ctx context.Context, subject string, item contracts.Envelope, msgID string) error {
			call := deadLetterCall{at: rig.now()}
			cursor, err := client.ReadKeptRoleCursor(ctx, s.role)
			call.cursor, call.err = cursor.Cursor, err
			if err == nil {
				var held []bus.StoredMessage
				held, call.err = readStored(ctx, client, subject)
				call.held = len(held)
			}
			s.mu.Lock()
			s.calls = append(s.calls, call)
			first := len(s.calls) == 1
			s.mu.Unlock()
			if first {
				if s.failed != nil {
					s.failed()
				}
				return bus.ErrStreamFull
			}
			return client.PublishStoredAs(ctx, subject, item, msgID)
		}
		l.rewire(t)
	}
}

// made is every call the seam has had.
func (s *deadLetterSeam) made() []deadLetterCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// assertTakenOver fails unless the seam had two calls - the one that failed and the one that sent
// the dead letter again - each made with the cursor holding a claim of held and not past it, and
// the second made while the stream held no dead letter yet.
func (s *deadLetterSeam) assertTakenOver(t *testing.T, held laneCopy) []deadLetterCall {
	t.Helper()
	calls := s.made()
	if len(calls) != 2 {
		t.Fatalf("the dead letter was published %d times, want twice: the call that failed and one walk after it: %+v", len(calls), calls)
	}
	for i, call := range calls {
		if call.err != nil {
			t.Fatalf("call %d of the dead letter's publish could not read the lane: %v", i+1, call.err)
		}
		if call.cursor.Claiming != held.sequence || call.cursor.Kept >= held.sequence {
			t.Fatalf("call %d of the dead letter's publish found the cursor %+v, want a claim of the copy at %d with kept short of it", i+1, call.cursor, held.sequence)
		}
	}
	if calls[1].held != 0 {
		t.Fatalf("the stream held %d dead letters before the second publish, want none: a dead letter was stored outside cfg.publishDeadLetter", calls[1].held)
	}
	return calls
}

// TestAKeptRoleCopyIsForwardedOrReturned holds the rows of check k1 about one copy's hour, and
// rows (p), (q) and (r) for the parts of R1 and R3 no check covers: a kept copy is forwarded
// within its hour or dropped at its end, its sender gets one dead letter where it is owed one, and
// every outcome is counted and logged once however many listeners and sweeps reach the copy. Each
// row runs two listeners on a stream of its own, sweeping every hourSweepInterval where the row sweeps, and
// moves their clock rather than waiting an hour.
func TestAKeptRoleCopyIsForwardedOrReturned(t *testing.T) {
	t.Parallel()
	t.Run("a a publish to a role with no claim is answered 202 with kept_until one hour after the keep", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-unclaimed"
		answer := rig.kept(rig.listeners[0], roleMessage(role, "ses_hour_unclaimed_sender", "nobody holds this role yet"))
		if answer.KeptReason != "unclaimed" {
			t.Fatalf("the answer's kept_reason is %q, want unclaimed", answer.KeptReason)
		}
		held := rig.copyOf(role, answer.EventID)
		if want := held.stored.Add(time.Hour).UnixMilli(); answer.KeptUntil != want {
			t.Fatalf("the answer's kept_until is %d, want %d: one hour after the stream stored the copy at %s", answer.KeptUntil, want, held.stored)
		}
	})

	t.Run("b a claim made 50 minutes after the keep gets the copy once with kept_at and the forward line names the sender's session", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-claimed-late"
			holder = "ses_hour_late_holder"
			sender = "ses_hour_late_sender"
		)
		answer := rig.kept(rig.listeners[0], roleMessage(role, sender, "hand this to the role's next holder"))
		held := rig.copyOf(role, answer.EventID)
		// The clock moves 50 minutes on below, so every claim reads older than
		// bus.KeptRoleClaimWindow to the listeners the moment it is written: the first listener's
		// sweep could take over the claim the claim's handover writes on the second listener while
		// that forward is in flight, and forward the copy again. The first listener's guard is
		// held for the whole row, so its sweep passes the lane over; the second listener's sweep
		// walks it before the claim, and the claim's handover makes the one forward.
		release, taken := rig.listeners[0].cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the first listener", role)
		}
		defer release()
		rig.startSweeps()
		rig.letSweepsRun()
		if forwards := rig.lines(hourForwardLine, map[string]any{"role": role}); len(forwards) != 0 {
			t.Fatalf("the copy was forwarded with no claim: %v", forwards)
		}

		rig.moveClock(50 * time.Minute)
		frames := startStandIn(t, rig.uri, holder)
		rig.claim(rig.listeners[1], holder, role, liveRoleActEntry)
		got := expectFrames(t, frames, 1, 2500*time.Millisecond)[0]
		if got.EventID != answer.EventID {
			t.Fatalf("the claimant received event %s, want the kept %s", got.EventID, answer.EventID)
		}
		if got.KeptAt == nil || *got.KeptAt != held.stored.UnixMilli() {
			t.Fatalf("the forwarded copy carries kept_at %v, want %d, when the stream stored it", got.KeptAt, held.stored.UnixMilli())
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != 1 {
			t.Fatalf("the claimant's agent subject holds %d copies, want one", len(stored))
		}
		forwards := rig.lines(hourForwardLine, map[string]any{"role": role})
		if len(forwards) != 1 || forwards[0]["event_id"] != answer.EventID || forwards[0]["session_id"] != holder || forwards[0]["source_session"] != sender {
			t.Fatalf("the forward lines are %v, want one naming event %s, holder %s and the sender's session %s", forwards, answer.EventID, holder, sender)
		}
	})

	t.Run("c the first sweep after the hour drops the copy and tells its sender", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-dropped"
			sender = "ses_hour_dropped_sender"
		)
		answer := rig.kept(rig.listeners[0], roleMessage(role, sender, "this waits for a holder that never comes"))
		held := rig.copyOf(role, answer.EventID)
		rig.startSweeps()
		for _, by := range []time.Duration{0, 59 * time.Minute} {
			rig.moveClock(by)
			rig.letSweepsRun()
			if dropped, letters := rig.count(hourExpired), rig.deadLetters(sender, held.item.EventID); dropped != 0 || len(letters) != 0 {
				t.Fatalf("with the clock %s on, inside the copy's hour, the sweeps counted %d drops and stored %d dead letters, want none", by, dropped, len(letters))
			}
		}

		rig.moveClock(time.Hour + time.Minute)
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()
		if dropped := rig.count(hourExpired); dropped != 1 {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want one", dropped)
		}
		drops := rig.lines(hourDropLine, map[string]any{"role": role})
		if len(drops) != 1 || drops[0]["event_id"] != held.item.EventID || drops[0]["source_session"] != sender {
			t.Fatalf("the drop lines are %v, want one naming role %s, event %s and the sender's session %s", drops, role, held.item.EventID, sender)
		}
		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want its one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored.Add(time.Hour), rig.now(), hourExpiryExplained)
		stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role})
		if len(stored) != 1 || stored[0]["event_id"] != held.item.EventID || stored[0]["reason"] != "expired" || stored[0]["dead_letter_event_id"] != letters[0].EventID {
			t.Fatalf("the dead letter lines are %v, want one naming event %s, reason expired and dead letter %s", stored, held.item.EventID, letters[0].EventID)
		}
	})

	t.Run("d copies from two senders that end before one sweep each get a dead letter on their own sender's subject", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-two-senders"
		senders := []string{"ses_hour_first_sender", "ses_hour_second_sender"}
		var held []laneCopy
		for i, sender := range senders {
			answer := rig.kept(rig.listeners[i], roleMessage(role, sender, "a message from "+sender))
			held = append(held, rig.copyOf(role, answer.EventID))
		}
		rig.startSweeps()
		rig.letSweepsRun()
		for i, sender := range senders {
			if dropped, letters := rig.count(hourExpired), rig.deadLetters(sender, held[i].item.EventID); dropped != 0 || len(letters) != 0 {
				t.Fatalf("inside the copies' hour, the sweeps counted %d drops and stored %d dead letters for %s, want none", dropped, len(letters), sender)
			}
		}

		rig.moveClock(time.Hour + time.Minute)
		rig.waitPassed(role, held[1].sequence)
		rig.letSweepsRun()
		for i, sender := range senders {
			letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
			if len(letters) != 1 {
				t.Fatalf("%s's agent subject holds %d messages, want one dead letter: %+v", sender, len(letters), letters)
			}
			assertDeadLetterOf(t, letters[0], role, held[i], held[i].stored.Add(time.Hour), rig.now(), hourExpiryExplained)
		}
		if dropped := rig.count(hourExpired); dropped != 2 {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want two", dropped)
		}
	})

	t.Run("e a copy that named no session and a copy the outbox wrote are counted and logged once with no dead letter", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-no-letter"
		answer := rig.kept(rig.listeners[0], roleMessage(role, "", "a publish that names no session"))
		unnamed := rig.copyOf(role, answer.EventID)
		outbox := rig.keep(role, outboxCopy(role, "ses_hour_outbox_sender", "a Dispatch event routed to the role"))
		rig.startSweeps()
		rig.letSweepsRun()
		if dropped, letters := rig.count(hourExpired)+rig.count(hourExpiredOutbox), rig.envoyLetters(); dropped != 0 || len(letters) != 0 {
			t.Fatalf("inside the copies' hour, the sweeps counted %d drops and stored dead letters %+v, want none", dropped, letters)
		}

		rig.moveClock(time.Hour + time.Minute)
		rig.waitPassed(role, outbox.sequence)
		rig.letSweepsRun()
		for _, tc := range []struct {
			held   laneCopy
			keptBy string
			series string
		}{
			{held: unnamed, keptBy: "listener", series: hourExpired},
			{held: outbox, keptBy: "dispatch_outbox", series: hourExpiredOutbox},
		} {
			if dropped := rig.count(tc.series); dropped != 1 {
				t.Fatalf("%s is %d, want one", tc.series, dropped)
			}
			drops := rig.lines(hourDropLine, map[string]any{"role": role, "event_id": tc.held.item.EventID})
			if len(drops) != 1 || drops[0]["kept_by"] != tc.keptBy || drops[0]["stale"] != false {
				t.Fatalf("the drop lines of event %s are %v, want one with kept_by %s and stale false", tc.held.item.EventID, drops, tc.keptBy)
			}
		}
		if lines := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(lines) != 0 {
			t.Fatalf("the listeners logged dead letters %v, want none", lines)
		}
		if stored := rig.count(hourLettersStored); stored != 0 {
			t.Fatalf("envoy_role_dead_letters_total counts %d stored, want none", stored)
		}
		if letters := rig.envoyLetters(); len(letters) != 0 {
			t.Fatalf("the stream holds dead letters %+v, want none", letters)
		}
	})

	t.Run("f a claim made after the hour before any sweep stores the copy's dead letter and does not forward it", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-claimed-after"
			holder = "ses_hour_after_holder"
			sender = "ses_hour_after_sender"
		)
		held := rig.keep(role, listenerCopy(role, sender, "a copy whose hour ends before any claim"))
		rig.moveClock(time.Hour + time.Minute)
		frames := startStandIn(t, rig.uri, holder)
		rig.claim(rig.listeners[0], holder, role, liveRoleActEntry)
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)

		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want its one dead letter: %+v", len(letters), letters)
		}
		// The claim's walk is for the claimant, which holds the role live, so the dead letter
		// names it and does not say that no session held the role.
		assertDeadLetterOf(t, letters[0], role, held, held.stored.Add(time.Hour), rig.now(),
			"The hour ended before session "+holder+", which holds the role now, took the message.")
		if want := "Envoy dropped your message to role " + role + ": its hour ended before the session that holds the role now took it."; letters[0].PayloadSummary != want {
			t.Fatalf("the dead letter's summary is %q, want %q", letters[0].PayloadSummary, want)
		}
		if strings.Contains(letters[0].Payload, hourExpiryExplained) {
			t.Fatalf("the dead letter says no session held the role, while %s held it live:\n%s", holder, letters[0].Payload)
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != 0 {
			t.Fatalf("the claimant's agent subject holds %+v, want nothing: a copy past its hour is not forwarded", stored)
		}
		select {
		case frame := <-frames:
			t.Fatalf("the claimant received event %s, want nothing", frame.EventID)
		default:
		}
		if forwards := rig.lines(hourForwardLine, map[string]any{"role": role}); len(forwards) != 0 {
			t.Fatalf("the forward lines are %v, want none", forwards)
		}
	})

	t.Run("g both listeners walk the same lane over several sweeps and each copy gets one dead letter one count and one drop line", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-walked-twice"
			copies = 4
		)
		var held []laneCopy
		for i := range copies {
			sender := fmt.Sprintf("ses_hour_walked_sender_%d", i)
			held = append(held, rig.keep(role, listenerCopy(role, sender, "copy "+sender)))
		}
		rig.startSweeps()
		rig.moveClock(time.Hour + time.Minute)
		rig.waitPassed(role, held[copies-1].sequence)
		rig.letSweepsRun()
		rig.letSweepsRun()

		for _, each := range held {
			event, sender := each.item.EventID, each.item.SourceSession
			if letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender)); len(letters) != 1 || letters[0].SourceEventID != event {
				t.Fatalf("%s's agent subject holds %+v, want one dead letter of event %s", sender, letters, event)
			}
			if drops := rig.lines(hourDropLine, map[string]any{"role": role, "event_id": event}); len(drops) != 1 {
				t.Fatalf("event %s's drop is logged %d times, want once: %v", event, len(drops), drops)
			}
			if stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role, "event_id": event}); len(stored) != 1 {
				t.Fatalf("event %s's dead letter is logged %d times, want once: %v", event, len(stored), stored)
			}
		}
		if dropped := rig.count(hourExpired); dropped != copies {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want %d", dropped, copies)
		}
		if stored := rig.count(hourLettersStored); stored != copies {
			t.Fatalf("envoy_role_dead_letters_total counts %d stored, want %d", stored, copies)
		}
	})

	t.Run("h a dead letter whose first publish fails is stored once by a walk after the claim ages out", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-letter-retried"
			sender = "ses_hour_retried_sender"
		)
		held := rig.keep(role, listenerCopy(role, sender, "a copy whose dead letter fails once"))
		seam := &deadLetterSeam{role: role}
		seam.install(t, rig)
		// The clock is past the copy's hour, so every claim reads older than
		// bus.KeptRoleClaimWindow to the listeners the moment it is written: the second listener's
		// sweep could take over the first walk's claim while that walk's publish is in flight, and
		// publish the dead letter a third time. The second listener's guard is held for the whole
		// row, so its sweep passes the lane over, and the walk that takes the claim over is the
		// first listener's next one.
		release, taken := rig.listeners[1].cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the second listener", role)
		}
		defer release()
		rig.moveClock(time.Hour + time.Minute)
		rig.startSweeps()
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		seam.assertTakenOver(t, held)
		if letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender)); len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		if dropped := rig.count(hourExpired); dropped != 1 {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want one", dropped)
		}
		if drops := rig.lines(hourDropLine, map[string]any{"role": role}); len(drops) != 1 {
			t.Fatalf("the copy's drop is logged %d times, want once: %v", len(drops), drops)
		}
		if stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(stored) != 1 || rig.count(hourLettersStored) != 1 {
			t.Fatalf("the dead letter is logged %d times and counted %d times, want once each: %v", len(stored), rig.count(hourLettersStored), stored)
		}
	})

	t.Run("i a dead letter the stream refuses for good ends its copy with one line and a later copy reaches the live holder", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-letter-refused"
			holder = "ses_hour_refused_holder"
		)
		// The live holder does not take the delivery mode steer, so the walk drops the first copy
		// and owes its sender a dead letter, which no subject of bad.token's own can hold.
		refusedItem := listenerCopy(role, "bad.token", "a message whose sender names no subject of its own")
		refusedItem.Payload = `{"delivery":{"mode":"steer"}}`
		refused := rig.keep(role, refusedItem)
		later := rig.keep(role, listenerCopy(role, "ses_hour_refused_sender", "the copy after it"))
		frames := startStandIn(t, rig.uri, holder)
		claimRoleWith(t, rig.listeners[0].harness, holder, role,
			session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
		rig.startSweeps()

		got := expectFrames(t, frames, 1, 2500*time.Millisecond)
		if got[0].EventID != later.item.EventID {
			t.Fatalf("the live holder received event %s, want the later copy %s", got[0].EventID, later.item.EventID)
		}
		rig.waitPassed(role, later.sequence)
		if lines := rig.lines(hourRefusedLine, map[string]any{"role": role, "event_id": refused.item.EventID}); len(lines) != 1 {
			t.Fatalf("the refused dead letter is logged %d times, want once: %v", len(lines), lines)
		}
		if lines := rig.lines(hourModeLine, map[string]any{"role": role, "event_id": refused.item.EventID}); len(lines) != 1 {
			t.Fatalf("the first copy's drop is logged %d times, want once: %v", len(lines), lines)
		}
		if refusedCount, storedCount := rig.count(hourLettersRefused), rig.count(hourLettersStored); refusedCount != 1 || storedCount != 0 {
			t.Fatalf("envoy_role_dead_letters_total counts %d refused and %d stored, want one refused", refusedCount, storedCount)
		}
		if letters := rig.envoyLetters(); len(letters) != 0 {
			t.Fatalf("the stream holds dead letters %+v, want none", letters)
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != 1 || stored[0].EventID != later.item.EventID {
			t.Fatalf("the holder's agent subject holds %+v, want the later copy once", stored)
		}
	})

	t.Run("j a holder that does not take the copy's delivery mode claims the role and the copy is dropped with a dead letter naming that holder and the mode", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-mode-refused"
			holder = "ses_hour_mode_holder"
			sender = "ses_hour_mode_sender"
		)
		message := roleMessage(role, sender, "steer the next holder")
		message["payload"] = `{"delivery":{"mode":"steer"}}`
		answer := rig.kept(rig.listeners[0], message)
		held := rig.copyOf(role, answer.EventID)
		rig.startSweeps()
		rig.claim(rig.listeners[1], holder, role,
			session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored, rig.now(),
			"Session "+holder+" held the role.", "does not take the delivery mode steer.")
		if stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(stored) != 1 || stored[0]["reason"] != "delivery_mode" {
			t.Fatalf("the dead letter lines are %v, want one with reason delivery_mode", stored)
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != 0 {
			t.Fatalf("the holder's agent subject holds %+v, want nothing", stored)
		}
	})

	t.Run("m a copy whose hour ended more than an hour before the first sweep is dropped as stale with no dead letter", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-stale"
			sender = "ses_hour_stale_sender"
		)
		held := rig.keep(role, listenerCopy(role, sender, "a copy no listener walked for two hours"))
		rig.moveClock(2*time.Hour + time.Minute)
		rig.startSweeps()
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		if stale, fresh := rig.count(hourExpiredStale), rig.count(hourExpired); stale != 1 || fresh != 0 {
			t.Fatalf("envoy_role_kept_expired_total counts %d stale and %d not stale, want one stale", stale, fresh)
		}
		drops := rig.lines(hourDropLine, map[string]any{"role": role})
		if len(drops) != 1 || drops[0]["event_id"] != held.item.EventID || drops[0]["stale"] != true {
			t.Fatalf("the drop lines are %v, want one naming event %s with stale true", drops, held.item.EventID)
		}
		if letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender)); len(letters) != 0 {
			t.Fatalf("the sender's agent subject holds %+v, want no dead letter", letters)
		}
		if lines := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(lines) != 0 || rig.count(hourLettersStored) != 0 {
			t.Fatalf("the listeners logged dead letters %v, want none", lines)
		}
	})

	t.Run("n a dead letter whose first publish fails while the clock moves two hours on is stored once and the copy is not stale", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-letter-late"
			sender = "ses_hour_late_letter_sender"
		)
		held := rig.keep(role, listenerCopy(role, sender, "a copy whose dead letter fails until two hours on"))
		seam := &deadLetterSeam{role: role, failed: func() { rig.setClock(held.stored.Add(2*time.Hour + time.Minute)) }}
		seam.install(t, rig)
		// The second listener's sweep passes the lane over, as in row (h): otherwise it can take
		// the first walk's claim over before that walk's failed publish moves the clock on, and
		// drop the copy at the age the clock had then.
		release, taken := rig.listeners[1].cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the second listener", role)
		}
		defer release()
		rig.moveClock(time.Hour + time.Minute)
		rig.startSweeps()
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		seam.assertTakenOver(t, held)
		if letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender)); len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		if fresh, stale := rig.count(hourExpired), rig.count(hourExpiredStale); fresh != 1 || stale != 0 {
			t.Fatalf("envoy_role_kept_expired_total counts %d not stale and %d stale, want one not stale", fresh, stale)
		}
		drops := rig.lines(hourDropLine, map[string]any{"role": role})
		if len(drops) != 1 || drops[0]["stale"] != false {
			t.Fatalf("the drop lines are %v, want one with stale false", drops)
		}
		if age, err := time.ParseDuration(fmt.Sprint(drops[0]["age"])); err != nil || age <= 2*time.Hour {
			t.Fatalf("the drop line names the copy's age %v (%v), want over two hours: a copy a walk claimed is not stale at any age", drops[0]["age"], err)
		}
	})

	t.Run("o a route publish naming source dispatch and a source_session is kept and gets its dead letter when its hour ends", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-dispatch-source"
			sender = "ses_hour_dispatch_sender"
		)
		message := roleMessage(role, sender, "a publish that names source dispatch")
		message["source"] = "dispatch"
		answer := rig.kept(rig.listeners[1], message)
		if answer.Source != "dispatch" || !strings.HasPrefix(answer.DedupeKey, "publish.") {
			t.Fatalf("the kept envelope names source %q and dedupe key %q, want dispatch and a key the route minted", answer.Source, answer.DedupeKey)
		}
		held := rig.copyOf(role, answer.EventID)
		rig.startSweeps()
		rig.letSweepsRun()
		if dropped, letters := rig.count(hourExpired), rig.deadLetters(sender, held.item.EventID); dropped != 0 || len(letters) != 0 {
			t.Fatalf("inside the copy's hour, the sweeps counted %d drops and stored %d dead letters, want none", dropped, len(letters))
		}

		rig.moveClock(time.Hour + time.Minute)
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()
		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored.Add(time.Hour), rig.now(), hourExpiryExplained)
		if listener, outbox := rig.count(hourExpired), rig.count(hourExpiredOutbox); listener != 1 || outbox != 0 {
			t.Fatalf("envoy_role_kept_expired_total counts %d kept by the listener and %d by the outbox, want one by the listener", listener, outbox)
		}
		if drops := rig.lines(hourDropLine, map[string]any{"role": role}); len(drops) != 1 || drops[0]["kept_by"] != "listener" {
			t.Fatalf("the drop lines are %v, want one with kept_by listener", drops)
		}
	})

	t.Run("p a copy that does not decode is ended with its sequence logged once and counted once", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-undecodable"
		ack, err := rig.client().JS().Publish(bus.KeptRoleSubject(role), []byte("not an envelope"), natsgo.MsgId("hour-undecodable-"+id.New()))
		if err != nil {
			t.Fatalf("write bytes no read decodes to role %s's kept lane: %v", role, err)
		}
		rig.startSweeps()
		rig.waitPassed(role, ack.Sequence)
		rig.letSweepsRun()

		if ended := rig.count(hourUndecodable); ended != 1 {
			t.Fatalf("envoy_role_kept_undecodable_total is %d, want one", ended)
		}
		lines := rig.lines(hourUndecodableLine, map[string]any{"role": role})
		if len(lines) != 1 || lines[0]["sequence"] != float64(ack.Sequence) {
			t.Fatalf("the lines of the copy no read decodes are %v, want one naming sequence %d", lines, ack.Sequence)
		}
	})

	t.Run("q 250 copies whose hours ended are all ended over the sweeps that follow each counted once", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-many-ended"
			copies = 250
		)
		var last laneCopy
		events := map[string]bool{}
		for i := range copies {
			last = rig.keep(role, listenerCopy(role, "", fmt.Sprintf("ended copy %d", i)))
			events[last.item.EventID] = true
		}
		rig.moveClock(time.Hour + time.Minute)
		rig.startSweeps()
		waitFor(t, "every copy ended", func() bool { return rig.count(hourExpired) >= copies })
		rig.waitPassed(role, last.sequence)
		rig.letSweepsRun()

		if dropped := rig.count(hourExpired); dropped != copies {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want %d", dropped, copies)
		}
		drops := rig.lines(hourDropLine, map[string]any{"role": role})
		logged := map[string]int{}
		for _, drop := range drops {
			event, _ := drop["event_id"].(string)
			logged[event]++
		}
		for event := range events {
			if logged[event] != 1 {
				t.Fatalf("event %s's drop is logged %d times, want once (%d drop lines for %d copies)", event, logged[event], len(drops), copies)
			}
		}
		if len(drops) != copies {
			t.Fatalf("%d drop lines, want %d", len(drops), copies)
		}
	})

	t.Run("r with two listeners and a live holder each forward is counted once", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-forwarded"
			holder = "ses_hour_forward_holder"
			copies = 3
		)
		var held []laneCopy
		for i := range copies {
			held = append(held, rig.keep(role, listenerCopy(role, "ses_hour_forward_sender", fmt.Sprintf("forwarded copy %d", i))))
		}
		frames := startStandIn(t, rig.uri, holder)
		claimRoleWith(t, rig.listeners[0].harness, holder, role, liveRoleActEntry)
		rig.startSweeps()

		received := map[string]int{}
		for _, frame := range expectFrames(t, frames, copies, 2500*time.Millisecond) {
			received[frame.EventID]++
		}
		rig.waitPassed(role, held[copies-1].sequence)
		for _, each := range held {
			if received[each.item.EventID] != 1 {
				t.Fatalf("the holder received event %s %d times, want once: %v", each.item.EventID, received[each.item.EventID], received)
			}
			if forwards := rig.lines(hourForwardLine, map[string]any{"role": role, "event_id": each.item.EventID}); len(forwards) != 1 {
				t.Fatalf("event %s's forward is logged %d times, want once: %v", each.item.EventID, len(forwards), forwards)
			}
		}
		if delivered := rig.count(deliveredSeries("delivered")); delivered != copies {
			t.Fatalf("envoy_messages_delivered_total counts %d delivered, want %d", delivered, copies)
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != copies {
			t.Fatalf("the holder's agent subject holds %d copies, want %d", len(stored), copies)
		}
	})

	// The tester's red test of round 3 (INFRA-2846): the review's R2 finding was a dead-letter
	// sentence that is false for some walk. A walk drops a copy for its delivery mode and claims
	// it to send the dead letter, the first publish of that dead letter fails with an error that
	// can pass, and the copy's hour ends before the walk that takes the claim over sends it again.
	// The copy was dropped because the session that held the role does not take its mode, so
	// the dead letter that is stored says that, and does not say that no session held the role or
	// that the hour ended before the holder took it.
	t.Run("s a delivery mode dead letter sent again after the copy's hour still names the delivery mode", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-mode-letter-late"
			holder = "ses_hour_mode_late_holder"
			sender = "ses_hour_mode_late_sender"
		)
		message := roleMessage(role, sender, "steer the next holder, whose dead letter fails once")
		message["payload"] = `{"delivery":{"mode":"steer"}}`
		answer := rig.kept(rig.listeners[0], message)
		held := rig.copyOf(role, answer.EventID)
		seam := &deadLetterSeam{role: role, failed: func() { rig.setClock(held.stored.Add(time.Hour + time.Minute)) }}
		seam.install(t, rig)
		rig.startSweeps()
		rig.claim(rig.listeners[1], holder, role,
			session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored, rig.now(), "does not take the delivery mode steer.")
		for _, wrong := range []string{"No session held the role", "no session held the role", "The hour ended before session", "its hour ended before"} {
			if strings.Contains(letters[0].Payload, wrong) || strings.Contains(letters[0].PayloadSummary, wrong) {
				t.Fatalf("the dead letter of a copy dropped for its delivery mode says %q:\n%s\n%s", wrong, letters[0].PayloadSummary, letters[0].Payload)
			}
		}
		if stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(stored) != 1 || stored[0]["reason"] != "delivery_mode" {
			t.Fatalf("the dead letter lines are %v, want one with reason delivery_mode", stored)
		}
	})

	// The tester's red test of round 4 (INFRA-2846), in the class of round 3's: a claim whose
	// reason is lost when another walk takes it over. A walk claimed the copy to forward it and
	// ended before it confirmed the forward, so the stream may hold that forward. A walk that
	// takes the claim over after the copy's hour owes the sender a dead letter that says the
	// message may have reached the holder. When that walk's own dead-letter publish fails with an
	// error that can pass, the walk that takes its claim over sends the same dead letter, which
	// must still say so, and not that no session held the role.
	t.Run("t a forwarded copy's dead letter sent again by a second takeover still says the message may have reached the holder", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-forward-letter-late"
			sender = "ses_hour_forward_late_sender"
		)
		held := rig.keep(role, listenerCopy(role, sender, "a copy whose forward was never confirmed"))
		// The walk that claimed the copy to forward it ended there: its claim stands on a
		// cursor that has passed nothing yet.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, err := json.Marshal(bus.KeptRoleCursor{Claiming: held.sequence})
		if err != nil {
			t.Fatalf("encode the forward's claim: %v", err)
		}
		if _, err := rig.client().PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), body, 0); err != nil {
			t.Fatalf("write the forward's claim: %v", err)
		}
		seam := &deadLetterSeam{role: role}
		seam.install(t, rig)
		rig.moveClock(time.Hour + time.Minute)
		rig.startSweeps()
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		if calls := seam.made(); len(calls) < 2 {
			t.Fatalf("the dead letter was published %d times, want the failed one and the one sent again: %+v", len(calls), calls)
		}
		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored, rig.now(), "The message may have reached that session.")
		for _, wrong := range []string{"No session held the role", "no session held the role", "The hour ended before session"} {
			if strings.Contains(letters[0].Payload, wrong) || strings.Contains(letters[0].PayloadSummary, wrong) {
				t.Fatalf("the dead letter of a copy whose forward was claimed says %q:\n%s\n%s", wrong, letters[0].PayloadSummary, letters[0].Payload)
			}
		}
	})

	// One more instance of the class rows (s) and (t) hold (INFRA-2846): what a claim says is
	// lost when another walk takes it over. A walk claimed the copy to forward it and ended
	// before it confirmed the forward, so the stream may hold that forward. The walk that takes
	// the claim over finds the copy inside its hour and the role held by a live session that
	// does not take the copy's delivery mode, so it drops the copy for its delivery mode, and
	// its first dead-letter publish fails with an error that can pass. The dead letter the walk
	// after it stores names the delivery mode, and it still says that the message may have
	// reached the session that held the role when the forward was claimed: the dead letter's
	// claim carries that through both takeovers (delivery_mode_unconfirmed).
	t.Run("u a delivery mode dead letter of a copy whose forward was claimed still says the message may have reached the holder", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-forward-mode-letter"
			holder = "ses_hour_forward_mode_holder"
			sender = "ses_hour_forward_mode_sender"
		)
		item := listenerCopy(role, sender, "steer the holder, whose forward was never confirmed")
		item.Payload = `{"delivery":{"mode":"steer"}}`
		held := rig.keep(role, item)
		// The walk that claimed the copy to forward it ended there: its claim stands on a
		// cursor that has passed nothing yet.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, err := json.Marshal(bus.KeptRoleCursor{Claiming: held.sequence})
		if err != nil {
			t.Fatalf("encode the forward's claim: %v", err)
		}
		if _, err := rig.client().PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), body, 0); err != nil {
			t.Fatalf("write the forward's claim: %v", err)
		}
		seam := &deadLetterSeam{role: role}
		seam.install(t, rig)
		// The clock moves 30 minutes on below, which leaves the copy inside its hour and makes
		// every claim read older than bus.KeptRoleClaimWindow to the listeners the moment it is
		// written. That is what lets a walk take the forward's claim over, and it would let the
		// first listener's sweep take over the dead letter's claim the second listener's walk
		// writes while that walk's publish is in flight, and publish the dead letter a third
		// time. The first listener's guard is held for the whole row, so its sweep passes the
		// lane over, and both walks are the second listener's, where the holder claims the role.
		release, taken := rig.listeners[0].cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the first listener", role)
		}
		defer release()
		rig.moveClock(30 * time.Minute)
		rig.startSweeps()
		rig.claim(rig.listeners[1], holder, role,
			session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
		rig.waitDeadLetter(held)
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()

		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 1 {
			t.Fatalf("the sender's agent subject holds %d messages, want one dead letter: %+v", len(letters), letters)
		}
		assertDeadLetterOf(t, letters[0], role, held, held.stored, rig.now(),
			"does not take the delivery mode steer.", "The message may have reached that session.")
		for _, wrong := range []string{"No session held the role", "no session held the role", "The hour ended before", "its hour ended before"} {
			if strings.Contains(letters[0].Payload, wrong) || strings.Contains(letters[0].PayloadSummary, wrong) {
				t.Fatalf("the dead letter of a copy dropped inside its hour for its delivery mode says %q:\n%s\n%s", wrong, letters[0].PayloadSummary, letters[0].Payload)
			}
		}
		if stored := rig.lines(hourDeadLetterLine, map[string]any{"role": role}); len(stored) != 1 || stored[0]["reason"] != "delivery_mode" {
			t.Fatalf("the dead letter lines are %v, want one with reason delivery_mode", stored)
		}
		if dropped := rig.count(hourExpired); dropped != 0 {
			t.Fatalf("envoy_role_kept_expired_total rose by %d, want none: the copy was dropped inside its hour", dropped)
		}
		for i, call := range seam.assertTakenOver(t, held) {
			if call.cursor.DeadLetter != "delivery_mode_unconfirmed" {
				t.Fatalf("call %d of the dead letter's publish found the claim %+v, want its dead_letter to be delivery_mode_unconfirmed", i+1, call.cursor)
			}
		}
	})
}

// TestADeadLetterClaimIsTakenOverForTheReasonItWasMade holds the rule a walk that takes over a
// dead letter's claim sends that dead letter again by (keptCopyClaimOf, judgeKeptRoleCopy): the
// claim names the reason the walk that made it ended the copy for, and whether the copy's forward
// may have reached the holder (bus.KeptRoleCursor.DeadLetter), and the walk taking it over sends
// the dead letter again for that reason, with that qualifier, and for no other, whatever the
// copy's age on its own clock. Rows (h), (n), (s), (t) and (u) above take claims over that the
// walks of their listeners wrote; this one judges claims at ages those rows' clocks do not reach,
// an expiry claim on a copy that names a delivery mode among them, and reads the claim each drop
// is written under (keptRoleDrop.claimReason) back as that drop, so a claim taken over again and
// again is judged as the first takeover judged it.
func TestADeadLetterClaimIsTakenOverForTheReasonItWasMade(t *testing.T) {
	t.Parallel()
	stored := bus.StoredMessage{Sequence: 7, Time: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(contracts.Envelope{EventID: "evt_claimed", SourceSession: "ses_claim_sender", Payload: `{"delivery":{"mode":"steer"}}`})
	if err != nil {
		t.Fatalf("encode the copy: %v", err)
	}
	stored.Data = data
	forward := bus.KeptRoleCursor{Kept: 6, Claiming: 7}
	mode := bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: string(deadLetterDeliveryMode)}
	expiry := bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: string(deadLetterExpired)}
	for _, tc := range []struct {
		name   string
		cursor bus.KeptRoleCursor
		want   keptCopyClaim
	}{
		{"a dead letter's claim of the copy before it", bus.KeptRoleCursor{Kept: 5, Claiming: 6, DeadLetter: string(deadLetterDeliveryMode)}, keptCopyClaim{kind: keptCopyUnclaimed}},
		{"a forward's claim", forward, keptCopyClaim{kind: keptCopyForwardClaimed}},
		{"a dead letter's claim for the copy's delivery mode", mode, keptCopyClaim{kind: keptCopyModeClaimed}},
		{"a dead letter's claim for the copy's hour", expiry, keptCopyClaim{kind: keptCopyExpiryClaimed}},
		{"a dead letter's claim for the hour of a copy whose forward was claimed",
			bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: "expired_unconfirmed"}, keptCopyClaim{kind: keptCopyExpiryClaimed, unconfirmed: true}},
		{"a dead letter's claim for the delivery mode of a copy whose forward was claimed",
			bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: "delivery_mode_unconfirmed"}, keptCopyClaim{kind: keptCopyModeClaimed, unconfirmed: true}},
		{"a dead letter's claim for a reason no walk writes",
			bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: "retired_unconfirmed"}, keptCopyClaim{kind: keptCopyExpiryClaimed}},
	} {
		if got := keptCopyClaimOf(tc.cursor, stored.Sequence); got != tc.want {
			t.Fatalf("%s: the claim is %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// refuser is a live holder that takes btw alone, and so not the copy's steer. judged is the
	// judgement of a walk for holder - refuser, or none - of the copy under cursor's claim, by a
	// clock at after the keep.
	const refuser = "ses_claim_holder"
	judged := func(holder string, cursor bus.KeptRoleCursor, at time.Duration) keptCopyVerdict {
		return judgeKeptRoleCopy(listenerDeliveryHandlerConfig{}, "claimed-role", holder, session.SessionEntry{Capabilities: []string{"btw"}}, stored,
			keptCopyClaimOf(cursor, stored.Sequence), func() time.Time { return stored.Time.Add(at) }, func(string, string) bool { return false })
	}
	// A mode claim taken over three hours on is the delivery mode drop it was, with no expired
	// report.
	if verdict := judged("", mode, 3*time.Hour); verdict.step != keptCopyDeadLetter || verdict.drop.reason != deadLetterDeliveryMode ||
		verdict.drop.mode != "steer" || verdict.report != nil {
		t.Fatalf("a mode claim taken over after the hour is judged %+v, want the steer delivery mode's dead letter and no expired report", verdict)
	}
	// An expiry claim taken over by a walk whose clock reads the copy inside its hour is the
	// expired drop it was, reported as one, though the copy names a delivery mode, and it says
	// nothing of a forward.
	if verdict := judged("", expiry, 59*time.Minute); verdict.step != keptCopyDeadLetter || verdict.drop.reason != deadLetterExpired ||
		verdict.drop.forwarded || verdict.report == nil {
		t.Fatalf("an expiry claim taken over by a clock inside the hour is judged %+v, want the expired dead letter with its report and no forward", verdict)
	}
	// A forward's claim taken over inside the hour for a holder that does not take the copy's
	// delivery mode is a delivery mode drop, and its dead letter names that holder and the mode
	// first and the forward no walk confirmed after them.
	const refusedAfterForward = "Session " + refuser + " held the role. Session " + refuser + " does not take the delivery mode steer. " +
		"Before that, Envoy started to send the message to the session that held the role then. " +
		"Envoy did not confirm that send. The message may have reached that session."
	if verdict := judged(refuser, forward, 30*time.Minute); verdict.step != keptCopyDeadLetter || verdict.drop.explanation() != refusedAfterForward {
		t.Fatalf("a forward's claim taken over for a holder that does not take the copy's delivery mode is judged %+v, which explains %q, want a dead letter that explains %q",
			verdict, verdict.drop.explanation(), refusedAfterForward)
	}

	// The claim a walk writes for each drop is read back as that drop, by every walk that takes
	// it over after it: a copy whose forward was claimed stays a drop that may have reached the
	// holder however many times its dead letter's publish fails, whether it was dropped at the
	// end of its hour, which row (t) holds on a stream, or inside it for its delivery mode, which
	// row (u) holds there.
	for _, tc := range []struct {
		name string
		// holder is the live holder the walk that drops the copy is for, empty for a walk with
		// none. The walks that take the claim over have none.
		holder    string
		cursor    bus.KeptRoleCursor
		at        time.Duration
		reason    deadLetterReason
		forwarded bool
		// claim is the dead_letter value the claim of that drop carries.
		claim string
	}{
		{"a forward's claim taken over after the hour", "", forward, time.Hour + time.Minute, deadLetterExpired, true, "expired_unconfirmed"},
		{"a copy no walk claimed, read after the hour", "", bus.KeptRoleCursor{Kept: 6}, time.Hour + time.Minute, deadLetterExpired, false, "expired"},
		{"a mode claim taken over after the hour", "", mode, 3 * time.Hour, deadLetterDeliveryMode, false, "delivery_mode"},
		{"a forward's claim taken over inside the hour for a holder that does not take the copy's delivery mode",
			refuser, forward, 30 * time.Minute, deadLetterDeliveryMode, true, "delivery_mode_unconfirmed"},
		{"a copy no walk claimed, read inside the hour for a holder that does not take its delivery mode",
			refuser, bus.KeptRoleCursor{Kept: 6}, 30 * time.Minute, deadLetterDeliveryMode, false, "delivery_mode"},
	} {
		first := judged(tc.holder, tc.cursor, tc.at)
		if first.step != keptCopyDeadLetter || first.drop.reason != tc.reason || first.drop.forwarded != tc.forwarded {
			t.Fatalf("%s: judged %+v, want the %s dead letter with forwarded %t", tc.name, first, tc.reason, tc.forwarded)
		}
		claimed := bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: first.drop.claimReason()}
		if claimed.DeadLetter != tc.claim {
			t.Fatalf("%s: the dead letter's claim names %q, want %q", tc.name, claimed.DeadLetter, tc.claim)
		}
		for _, at := range []time.Duration{59 * time.Minute, 3 * time.Hour} {
			again := judged("", claimed, at)
			if again.step != keptCopyDeadLetter || again.drop.reason != tc.reason || again.drop.forwarded != tc.forwarded ||
				again.drop.claimReason() != tc.claim {
				t.Fatalf("%s: its claim taken over by a clock %s after the keep is judged %+v, want the %s dead letter with forwarded %t under the claim %q",
					tc.name, at, again, tc.reason, tc.forwarded, tc.claim)
			}
		}
	}
}

// TestADeadLetterNamesADispatchPostAndQuotesNoneOfIt holds the line a dead letter names its
// message by (keptCopyNamed), and that the letter keeps every other line it had. A Dispatch
// frame's summary is the head of what its event carries, a post's body included, and the letter
// is written as late as the end of the copy's hour, after the post's author may have deleted the
// post, so the letter of a Dispatch frame gives none of the summary. It names a comment or a
// message by its id and its dispatch:// reference, and says that is because its author can edit
// or delete it; it names any other Dispatch frame - an ask's, an issue's, a delivery receipt, one
// that names no post - by the event id alone, with no such reason, since no author edits or
// deletes those. A message from any other source keeps its first line. Each Dispatch frame is
// built as the outbox builds one (internal/dispatch/outbox envelope), from the event types
// Dispatch writes: the whole event as its payload, and the head of what it carries in its summary.
func TestADeadLetterNamesADispatchPostAndQuotesNoneOfIt(t *testing.T) {
	const (
		role         = "letter-role"
		sender       = "ses_letter_sender"
		phrase       = "zephyr-quokka-7731 is what the author deleted"
		messageID    = "0b5e7c1e-2f4a-4c0e-9d3b-6a1f8e2c4d71"
		commentID    = "7d2c9a40-5b1e-4f3a-8c6d-1e9b0a7f3c52"
		askID        = "3c8e1f5a-6b2d-4e9c-a7f0-5d1b8c3e2a96"
		artifactID   = "4f1a6b2c-8d3e-4a5f-9b0c-2e7d1a3f6c84"
		postUnquoted = "Envoy names a comment or a message from Dispatch by its ID and its reference and quotes none of it, because its author can edit or delete it."
		unquoted     = "Envoy does not quote a message from Dispatch, and names this one by its event ID alone."
	)
	stored := bus.StoredMessage{Sequence: 11, Time: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	dropped := stored.Time.Add(time.Hour + time.Minute)
	drop := keptRoleDrop{reason: deadLetterExpired}
	alice := model.Actor{Kind: "user", ID: "alice"}
	body := phrase + "\n\nThe rest of the post."
	// frame is the copy of event that the publish route kept from sender, with summary.
	frame := func(t *testing.T, event model.Event, summary string) contracts.Envelope {
		t.Helper()
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("encode event %d: %v", event.ID, err)
		}
		return contracts.Envelope{
			EventID: fmt.Sprintf("dispatch-%d", event.ID), Source: "dispatch", SourceSession: sender, SourceEventID: fmt.Sprint(event.ID),
			Topic: contracts.RoleTopicPrefix + role, DedupeKey: publishDedupePrefix + id.New(), PayloadSummary: summary, Payload: string(payload),
		}
	}
	// named is the line of item's dead letter that names the message, and the letter. It fails t
	// unless every other line of the letter, and its summary, are the ones every dead letter of an
	// expired copy gives.
	named := func(t *testing.T, item contracts.Envelope) (string, contracts.Envelope) {
		t.Helper()
		letter, _ := keptRoleDeadLetter(role, stored, item, dropped, drop)
		lines := strings.Split(letter.Payload, "\n")
		want := []string{
			"Envoy dropped a message that you published to role " + role + ".",
			"The event ID of the message is " + item.EventID + ".",
			"",
			"Envoy kept the message at 2026-11-20T09:00:00Z. Envoy dropped the message at 2026-11-20T10:01:00Z.",
			drop.explanation(),
		}
		if len(lines) != len(want) {
			t.Fatalf("the dead letter has %d lines, want %d:\n%s", len(lines), len(want), letter.Payload)
		}
		for i := range want {
			if i != 2 && lines[i] != want[i] {
				t.Fatalf("line %d of the dead letter is %q, want %q", i+1, lines[i], want[i])
			}
		}
		if want := "Envoy dropped your message to role " + role + ": no session held the role in the hour that Envoy kept it."; letter.PayloadSummary != want {
			t.Fatalf("the dead letter's summary is %q, want %q", letter.PayloadSummary, want)
		}
		return lines[2], letter
	}
	// quotesNothing fails t if letter gives any of the post's body.
	quotesNothing := func(t *testing.T, letter contracts.Envelope) {
		t.Helper()
		if strings.Contains(letter.Payload, "zephyr") || strings.Contains(letter.PayloadSummary, "zephyr") {
			t.Fatalf("the dead letter of a Dispatch frame quotes the post's body:\n%s\n%s", letter.PayloadSummary, letter.Payload)
		}
	}

	t.Run("a a Dispatch message's dead letter names the message by its reference and its id and gives none of its body", func(t *testing.T) {
		item := frame(t, model.Event{
			ID: 41, IssueKey: new("CORE-12"), Type: "message.created", Actor: alice, Notify: true, CreatedAt: stored.Time,
			Payload: model.MessageEventPayload{Message: model.Message{ID: messageID, IssueKey: new("CORE-12"), Author: alice, Body: body, CreatedAt: stored.Time}},
		}, "CORE-12 message created: "+phrase+" The rest of the post.")
		line, letter := named(t, item)
		if want := "The message is Dispatch message " + messageID + ": dispatch://CORE-12/message/" + messageID + ". " + postUnquoted +
			" Read the reference to see the message as it is now."; line != want {
			t.Fatalf("the dead letter names the message as %q, want %q", line, want)
		}
		quotesNothing(t, letter)
	})

	t.Run("b a Dispatch comment's dead letter names the comment by its reference and its id and gives none of its body", func(t *testing.T) {
		item := frame(t, model.Event{
			ID: 42, IssueKey: new("CORE-12"), Type: "comment.created", Actor: alice, Notify: true, CreatedAt: stored.Time,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("CORE-12"), Author: alice, Body: body, CreatedAt: stored.Time}},
		}, "CORE-12 comment created: "+phrase+" The rest of the post.")
		line, letter := named(t, item)
		if want := "The message is Dispatch comment " + commentID + ": dispatch://CORE-12/comment/" + commentID + ". " + postUnquoted +
			" Read the reference to see the comment as it is now."; line != want {
			t.Fatalf("the dead letter names the comment as %q, want %q", line, want)
		}
		quotesNothing(t, letter)
	})

	t.Run("c a Dispatch comment on a project document is named by the document's reference to it", func(t *testing.T) {
		item := frame(t, model.Event{
			ID: 43, ArtifactID: new(artifactID), Project: "CORE", Type: "comment.created", Actor: alice, Notify: true, CreatedAt: stored.Time,
			Payload: model.CommentEventPayload{
				Comment:      model.Comment{ID: commentID, ArtifactID: new(artifactID), Author: alice, Body: body, CreatedAt: stored.Time},
				ArtifactName: "notes.md", ProjectKey: "CORE", ArtifactSlug: "notes-md",
			},
		}, "CORE/notes-md comment created: "+phrase+" The rest of the post.")
		line, letter := named(t, item)
		if want := "The message is Dispatch comment " + commentID + ": dispatch://CORE/artifact/notes-md/comment/" + commentID + ". " + postUnquoted +
			" Read the reference to see the comment as it is now."; line != want {
			t.Fatalf("the dead letter names the comment as %q, want %q", line, want)
		}
		quotesNothing(t, letter)
	})

	t.Run("d a Dispatch frame that names no post is named by its event id alone, with no reason of an author's, and gives none of its body", func(t *testing.T) {
		noEvent := frame(t, model.Event{ID: 44}, phrase)
		noEvent.Payload = body
		for _, tc := range []struct {
			name string
			item contracts.Envelope
		}{
			{"a frame whose payload is no event", noEvent},
			{"a comment on a project document whose event, an older server's, names no slug", frame(t, model.Event{
				ID: 45, ArtifactID: new(artifactID), Project: "CORE", Type: "comment.created", Actor: alice, Notify: true, CreatedAt: stored.Time,
				Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, ArtifactID: new(artifactID), Author: alice, Body: body, CreatedAt: stored.Time}},
			}, "CORE/notes-md comment created: "+phrase)},
			{"a comment's delivery receipt, whose payload names no post's id", frame(t, model.Event{
				ID: 46, IssueKey: new("CORE-12"), Type: "comment.delivery", Actor: alice, CreatedAt: stored.Time,
				Payload: model.CommentDeliveryEventPayload{CommentID: commentID, Target: "session:ses_letter_target", Attempt: 1, State: "sent"},
			}, "CORE-12 comment delivery")},
			{"an ask's frame", frame(t, model.Event{
				ID: 48, IssueKey: new("CORE-12"), Type: "ask.opened", Actor: alice, Notify: true, CreatedAt: stored.Time,
				Payload: model.AskEventPayload{Ask: model.Ask{ID: askID, IssueKey: new("CORE-12"), Author: alice, Kind: "question", Question: phrase + "?"}},
			}, "CORE-12 ask opened: "+phrase+"?")},
			{"an issue's frame", frame(t, model.Event{
				ID: 49, IssueKey: new("CORE-12"), Type: "issue.created", Actor: alice, Notify: true, CreatedAt: stored.Time,
				Payload: model.IssueEventPayload{Issue: model.Issue{Key: "CORE-12", Project: "CORE", Number: 12, Title: phrase, Status: "todo"}},
			}, "CORE-12 issue created: "+phrase)},
		} {
			line, letter := named(t, tc.item)
			if line != unquoted || strings.Contains(letter.Payload, "dispatch://") {
				t.Fatalf("%s: the dead letter names the message as %q, want %q and no reference:\n%s", tc.name, line, unquoted, letter.Payload)
			}
			if strings.Contains(letter.Payload, "edit or delete") {
				t.Fatalf("%s: the dead letter gives the reason of a post its author can edit or delete:\n%s", tc.name, letter.Payload)
			}
			quotesNothing(t, letter)
		}
	})

	t.Run("e a raw role publish keeps its first line, whatever its payload holds", func(t *testing.T) {
		event, err := json.Marshal(model.Event{
			ID: 47, IssueKey: new("CORE-12"), Type: "message.created", Actor: alice, CreatedAt: stored.Time,
			Payload: model.MessageEventPayload{Message: model.Message{ID: messageID, IssueKey: new("CORE-12"), Author: alice, Body: body}},
		})
		if err != nil {
			t.Fatalf("encode the event: %v", err)
		}
		eventPayload := string(event)
		for _, tc := range []struct {
			name    string
			payload *string
		}{
			{"a message of two lines", nil},
			{"a message whose payload is a Dispatch event", &eventPayload},
		} {
			item := contracts.MessageEnvelope(contracts.Message{Source: "agent", SourceSession: sender, Text: phrase + "\nThe second line.", Payload: tc.payload},
				contracts.RoleTopicPrefix+role, publishDedupePrefix+id.New())
			if line, _ := named(t, item); line != "The first line of the message is: "+phrase {
				t.Fatalf("%s: the dead letter names the message as %q, want its first line %q", tc.name, line, phrase)
			}
		}
	})
}

// TestARepeatedKeepIsOneCopyUntilItEnds holds rows (k) and (l) of check k1: a publish that repeats
// the dedupe key of a copy still on the lane inside its hour stores nothing and is answered with
// that copy, and one that repeats the key of a copy that was forwarded or whose hour ended is kept
// as a copy of its own, however many times.
func TestARepeatedKeepIsOneCopyUntilItEnds(t *testing.T) {
	t.Parallel()
	t.Run("k a repeat inside the copy's hour is answered with the first copy and the lane holds one copy", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-repeat-inside"
			sender = "ses_hour_repeat_inside_sender"
		)
		key := "hour-repeat-" + id.New()
		message := func(text string) map[string]any {
			fields := roleMessage(role, sender, text)
			fields["dedupe_key"] = key
			return fields
		}
		first := rig.kept(rig.listeners[0], message("the first publish of the key"))
		second := rig.kept(rig.listeners[1], message("the same key published again"))
		if first.KeptUntil == 0 || second.EventID != first.EventID || second.KeptUntil != first.KeptUntil || second.PayloadSummary != first.PayloadSummary {
			t.Fatalf("the repeat was answered %+v, want the first copy %+v with its event id and kept_until", second, first)
		}
		sends := linesIn(rig.listeners[1].harness.logs.String(), hourRoleSendLine, map[string]any{"topic": contracts.RoleTopicPrefix + role})
		if len(sends) != 1 || sends[0]["event_id"] != first.EventID || sends[0]["reason"] != "kept" {
			t.Fatalf("the repeat's role send lines are %v, want one naming the first copy's event %s as kept", sends, first.EventID)
		}
		if copies := rig.laneCopies(role); len(copies) != 1 || copies[0].item.EventID != first.EventID {
			t.Fatalf("the lane holds %d copies, want the first copy alone", len(copies))
		}
	})

	t.Run("l a repeat after a copy was forwarded or its hour ended is kept as a new copy each time", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			forwarded   = "hour-repeat-forwarded"
			ended       = "hour-repeat-ended"
			sender      = "ses_hour_repeat_sender"
			firstHolder = "ses_hour_repeat_first_holder"
			nextHolder  = "ses_hour_repeat_next_holder"
			endedHolder = "ses_hour_repeat_ended_holder"
		)
		first, second := rig.listeners[0], rig.listeners[1]
		publishKey := func(l *hourListener, role, key, text string) hourAnswer {
			t.Helper()
			fields := roleMessage(role, sender, text)
			fields["dedupe_key"] = key
			return rig.kept(l, fields)
		}
		rig.startSweeps()

		// The first lane: its first copy is forwarded, its holder leaves and the role-claim reaper
		// frees its claim, and the key is published again to the role nobody claims; the session
		// that claims the role next is handed the second copy.
		forwardedKey := "hour-forwarded-" + id.New()
		firstCopy := publishKey(first, forwarded, forwardedKey, "the first copy, which a holder takes")
		firstFrames := startStandIn(t, rig.uri, firstHolder)
		rig.claim(first, firstHolder, forwarded, liveRoleActEntry)
		if got := expectFrames(t, firstFrames, 1, 500*time.Millisecond); got[0].EventID != firstCopy.EventID {
			t.Fatalf("the first holder received event %s, want the first copy %s", got[0].EventID, firstCopy.EventID)
		}
		rig.waitPassed(forwarded, rig.copyOf(forwarded, firstCopy.EventID).sequence)
		rig.deregister(first, firstHolder)
		rig.reapClaim(forwarded)
		secondCopy := publishKey(second, forwarded, forwardedKey, "the key again, after its copy was forwarded")
		if secondCopy.EventID == firstCopy.EventID {
			t.Fatalf("the repeat after the forward was answered with the forwarded copy %s, want a new copy", firstCopy.EventID)
		}
		if copies := rig.laneCopies(forwarded); len(copies) != 2 || copies[1].item.EventID != secondCopy.EventID {
			t.Fatalf("the lane holds %d copies, want the forwarded copy and the new one", len(copies))
		}
		nextFrames := startStandIn(t, rig.uri, nextHolder)
		rig.claim(second, nextHolder, forwarded, liveRoleActEntry)
		if got := expectFrames(t, nextFrames, 1, 2500*time.Millisecond); got[0].EventID != secondCopy.EventID {
			t.Fatalf("the next holder received event %s, want the second copy %s", got[0].EventID, secondCopy.EventID)
		}
		select {
		case frame := <-firstFrames:
			t.Fatalf("the first holder, which left, received event %s as well", frame.EventID)
		default:
		}

		// The second lane: each copy's hour ends before the key is published again. The stream
		// stamps a copy with the real time it stored it, so once a copy has ended the clock goes
		// back to the real time, and the copy the next publish stores is as young as its own store
		// time says.
		endedKey := "hour-ended-" + id.New()
		answers := []hourAnswer{publishKey(first, ended, endedKey, "the first copy, whose hour ends")}
		for i, l := range []*hourListener{second, first} {
			held := rig.copyOf(ended, answers[i].EventID)
			rig.moveClock(time.Hour + time.Minute)
			rig.waitDeadLetter(held)
			rig.waitPassed(ended, held.sequence)
			rig.moveClock(0)
			next := publishKey(l, ended, endedKey, fmt.Sprintf("the key again, after copy %d ended", i+1))
			for _, earlier := range answers {
				if next.EventID == earlier.EventID {
					t.Fatalf("the repeat after copy %d ended was answered with copy %s, want a new copy", i+1, earlier.EventID)
				}
			}
			if next.KeptUntil <= answers[i].KeptUntil {
				t.Fatalf("the new copy's kept_until is %d, want an hour of its own after the ended copy's %d", next.KeptUntil, answers[i].KeptUntil)
			}
			answers = append(answers, next)
			if copies := rig.laneCopies(ended); len(copies) != i+2 {
				t.Fatalf("the lane holds %d copies after publish %d, want %d", len(copies), i+2, i+2)
			}
		}
		endedFrames := startStandIn(t, rig.uri, endedHolder)
		rig.claim(second, endedHolder, ended, liveRoleActEntry)
		if got := expectFrames(t, endedFrames, 1, 2500*time.Millisecond); got[0].EventID != answers[2].EventID {
			t.Fatalf("the holder of the second lane received event %s, want the third copy %s", got[0].EventID, answers[2].EventID)
		}
		letters := rig.envelopesOn(contracts.LegacyAgentSubject(sender))
		if len(letters) != 2 || letters[0].SourceEventID != answers[0].EventID || letters[1].SourceEventID != answers[1].EventID || letters[0].EventID == letters[1].EventID {
			t.Fatalf("the sender's agent subject holds %+v, want one dead letter of its own for each of the two ended copies %s and %s",
				letters, answers[0].EventID, answers[1].EventID)
		}
	})
}

// TestALapsedHolderIsKeptFor holds rows (a) to (e) of check k2: a publish, or a raw role lane
// message, to a role whose claim names a session that is gone or unresponsive is kept for it as
// for a role nobody claims, and reaches that session once it registers again, or the session that
// claims the role once the reaper has freed it. A publish with keep false to such a claim is
// refused 404 holder_lapsed and keeps nothing. Row (g) is R4's holder that answers again: its
// registration through POST /v1/interests/subscribe, which renews its claim and claims nothing,
// hands it the copy kept for it with no sweep running (subscribeHandler). Row (h) is that holder
// resumed by a new process after a clean stop: its registration states a process its claim does
// not name, so it renews nothing, and it is handed the copy all the same. Row (i) is a resumed
// process that claims the role before it registers: the claim route's refresh of its session
// makes it live, so its registration would not read it as returning, and it is handed the copy
// at that refresh though the claim is refused (roleSetHandler).
func TestALapsedHolderIsKeptFor(t *testing.T) {
	t.Parallel()
	t.Run("a a publish to a role whose holder deleted its record is kept and reaches that session once it registers again", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-lapsed-gone"
			holder = "ses_hour_gone_holder"
		)
		rig.startSweeps()
		rig.goneHolder(holder, role)
		frames := startStandIn(t, rig.uri, holder)
		answer := rig.kept(rig.listeners[1], roleMessage(role, "ses_hour_gone_sender", "are you coming back?"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder || answer.HolderState != "gone" {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s as gone", answer, holder)
		}
		rig.returnTo(holder, answer.EventID, frames)
	})

	t.Run("b a copy kept for a gone holder reaches the session that claims the role after the reaper frees it", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role      = "hour-lapsed-reaped"
			holder    = "ses_hour_reaped_holder"
			successor = "ses_hour_reaped_successor"
		)
		rig.startSweeps()
		rig.goneHolder(holder, role)
		answer := rig.kept(rig.listeners[1], roleMessage(role, "ses_hour_reaped_sender", "for whoever holds the role next"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s", answer, holder)
		}
		rig.reapClaim(role)
		frames := startStandIn(t, rig.uri, successor)
		rig.claim(rig.listeners[1], successor, role, liveRoleActEntry)
		waitFor(t, "the kept copy reaching "+successor, func() bool {
			return len(rig.envelopesOn(contracts.LegacyAgentSubject(successor))) > 0
		})
		rig.letSweepsRun()
		if held := rig.envelopesOn(contracts.LegacyAgentSubject(successor)); len(held) != 1 || held[0].EventID != answer.EventID {
			t.Fatalf("%s's agent subject holds %+v, want the kept copy of event %s once", successor, held, answer.EventID)
		}
		if got := expectFrames(t, frames, 1, 100*time.Millisecond); got[0].EventID != answer.EventID {
			t.Fatalf("%s received event %s, want the kept %s", successor, got[0].EventID, answer.EventID)
		}
		if held := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 0 {
			t.Fatalf("the gone holder's agent subject holds %+v, want nothing", held)
		}
	})

	t.Run("c a publish with keep false to a gone holder is refused 404 holder_lapsed and keeps nothing", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-lapsed-opted-out"
			holder = "ses_hour_opted_out_holder"
		)
		rig.goneHolder(holder, role)
		message := roleMessage(role, "ses_hour_opted_out_sender", "the sender queues this itself")
		message["keep"] = false
		recorder, _ := rig.publish(rig.listeners[1], message)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"reason":"holder_lapsed"`) {
			t.Fatalf("publish with keep false to a gone holder: status %d, body %s; want 404 holder_lapsed", recorder.Code, recorder.Body.String())
		}
		assertNothingKept(t, rig.client(), role)
	})

	t.Run("d a raw role lane message to a gone holder is kept and reaches that session once it registers again", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-lapsed-lane"
			holder = "ses_hour_lane_holder"
		)
		rig.startSweeps()
		rig.goneHolder(holder, role)
		frames := startStandIn(t, rig.uri, holder)
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "lane-"+id.New())
		item.SourceSession = "ses_hour_lane_sender"
		rig.listeners[1].lane(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
		waitForKeptRoleMessage(t, rig.client(), role, item.EventID)
		rig.returnTo(holder, item.EventID, frames)
	})

	t.Run("e a publish to an unresponsive holder is kept and reaches that session once it registers again", func(t *testing.T) {
		var registered atomic.Int64
		rig := newHourRig(t, session.WithSessionClock(func() time.Time { return time.Now().Add(time.Duration(registered.Load())) }))
		const (
			role   = "hour-lapsed-unresponsive"
			holder = "ses_hour_unresponsive_holder"
		)
		rig.startSweeps()
		frames := startStandIn(t, rig.uri, holder)
		// The holder's record says it last registered past the live window and well inside the
		// gone horizon: it claimed the role and stopped renewing.
		registered.Store(int64(-(session.LiveWindow + 2*time.Minute)))
		claimRoleWith(t, rig.listeners[0].harness, holder, role, liveRoleActEntry)
		registered.Store(0)
		answer := rig.kept(rig.listeners[1], roleMessage(role, "ses_hour_unresponsive_sender", "are you still there?"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder || answer.HolderState != "unresponsive" {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s as unresponsive", answer, holder)
		}
		rig.returnTo(holder, answer.EventID, frames)
	})

	t.Run("g a holder that answers again gets its kept copy at its registration with no sweep", func(t *testing.T) {
		var registered atomic.Int64
		rig := newHourRig(t, session.WithSessionClock(func() time.Time { return time.Now().Add(time.Duration(registered.Load())) }))
		const (
			role   = "hour-lapsed-returning"
			holder = "ses_hour_returning_holder"
		)
		frames := startStandIn(t, rig.uri, holder)
		// The holder's record says it last registered past the live window and well inside the
		// gone horizon: it claimed the role and stopped renewing.
		registered.Store(int64(-(session.LiveWindow + 2*time.Minute)))
		claimRoleWith(t, rig.listeners[0].harness, holder, role, liveRoleActEntry)
		registered.Store(0)
		// The keep starts a handover of its own on the listener that kept the copy
		// (keepForNextHolder). Its guard is held for the whole row, so that handover waits and
		// resolves no holder, and the one handover that can hand the copy over is the one the
		// holder's registration starts on the other listener.
		keeper := rig.listeners[1]
		release, taken := keeper.cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the listener that keeps its copy", role)
		}
		defer release()
		answer := rig.kept(keeper, roleMessage(role, "ses_hour_returning_sender", "are you back yet?"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder || answer.HolderState != "unresponsive" {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s as unresponsive", answer, holder)
		}

		rig.register(rig.listeners[0], holder, "")
		waitFor(t, "the kept copy reaching "+holder+" at its registration", func() bool {
			return len(rig.envelopesOn(contracts.LegacyAgentSubject(holder))) > 0
		})
		if held := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 1 || held[0].EventID != answer.EventID {
			t.Fatalf("%s's agent subject holds %+v, want the kept copy of event %s once", holder, held, answer.EventID)
		}
		if got := expectFrames(t, frames, 1, 100*time.Millisecond); got[0].EventID != answer.EventID {
			t.Fatalf("%s received event %s, want the kept %s", holder, got[0].EventID, answer.EventID)
		}
	})

	t.Run("h a holder resumed by a new process gets its kept copy at its registration with no sweep", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role    = "hour-lapsed-resumed"
			holder  = "ses_hour_resumed_holder"
			stopped = "hour-process-that-stopped"
			resumed = "hour-process-that-resumed"
		)
		frames := startStandIn(t, rig.uri, holder)
		// The process that stopped registered, claimed the role under its own client instance, and
		// stopped cleanly: its record is deleted and its claim stands, naming that process.
		first := rig.listeners[0]
		entry := liveRoleActEntry
		entry.ClientInstanceID = stopped
		if err := first.harness.sessions.Put(holder, entry); err != nil {
			t.Fatalf("register %s under process %s: %v", holder, stopped, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := first.harness.registry.SetRoleWithPrevious(ctx, store.RoleClaimRequest{
			SessionID: holder, MachineID: "test-machine", ClientInstanceID: stopped, Role: role,
		}); err != nil {
			t.Fatalf("claim %s for %s under process %s: %v", role, holder, stopped, err)
		}
		rig.deregister(first, holder)
		// The keep's own handover waits on its guard for the whole row, as in row (g).
		keeper := rig.listeners[1]
		release, taken := keeper.cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the listener that keeps its copy", role)
		}
		defer release()
		answer := rig.kept(keeper, roleMessage(role, "ses_hour_resumed_sender", "are you back under a new process?"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder || answer.HolderState != "gone" {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s as gone", answer, holder)
		}

		// A new process resumes the session under the same id and registers, stating itself.
		rig.register(first, holder, resumed)
		waitFor(t, "the kept copy reaching "+holder+" at its resumed process's registration", func() bool {
			return len(rig.envelopesOn(contracts.LegacyAgentSubject(holder))) > 0
		})
		if held := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 1 || held[0].EventID != answer.EventID {
			t.Fatalf("%s's agent subject holds %+v, want the kept copy of event %s once", holder, held, answer.EventID)
		}
		if got := expectFrames(t, frames, 1, 100*time.Millisecond); got[0].EventID != answer.EventID {
			t.Fatalf("%s received event %s, want the kept %s", holder, got[0].EventID, answer.EventID)
		}
		// The registration renewed nothing: the claim still names the process that stopped, so
		// the handover was started for a role the registration read and did not renew.
		if claim := storedRoleClaim(t, first.harness.registry, role); claim.HolderSessionID != holder || claim.ClientInstanceID != stopped {
			t.Fatalf("role %s's claim is %+v, want it still naming %s under process %s", role, claim, holder, stopped)
		}
	})

	t.Run("i a holder whose resumed process claims before it registers gets its kept copy with no sweep, even when the claim is refused", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role    = "hour-lapsed-claimed-again"
			holder  = "ses_hour_claimed_again_holder"
			stalled = "hour-process-that-stalled"
			resumed = "hour-process-that-claims"
		)
		frames := startStandIn(t, rig.uri, holder)
		// The process that stalled registered and claimed the role under its own client instance.
		// Its record says it last registered past the live window and well inside the gone
		// horizon, written into the bucket as another writer's record arrives
		// (storeSessionHeartbeat), so both listeners read it from there and the claim route finds
		// it; and its claim is inside the live window on the registry's own clock, so another
		// process's claim of the role is refused while the session is not live.
		storeSessionHeartbeat(t, rig.client(), holder, time.Now().Add(-(session.LiveWindow + 2*time.Minute)))
		first := rig.listeners[0]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := first.harness.registry.SetRoleWithPrevious(ctx, store.RoleClaimRequest{
			SessionID: holder, MachineID: "test-machine", ClientInstanceID: stalled, Role: role,
		}); err != nil {
			t.Fatalf("claim %s for %s under process %s: %v", role, holder, stalled, err)
		}
		waitFor(t, "both listeners to read "+holder+" as unresponsive and the first to read its claim of "+role, func() bool {
			for _, l := range rig.listeners {
				if _, state := l.harness.sessions.Lookup(holder); state != session.Unresponsive {
					return false
				}
			}
			held, err := first.harness.registry.RolesHeldBy(holder)
			return err == nil && slices.Contains(held, role)
		})
		// The keep's own handover waits on its guard for the whole row, as in row (g).
		keeper := rig.listeners[1]
		release, taken := keeper.cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the listener that keeps its copy", role)
		}
		defer release()
		answer := rig.kept(keeper, roleMessage(role, "ses_hour_claimed_again_sender", "are you back before you register?"))
		if answer.KeptReason != "holder_lapsed" || answer.Holder != holder || answer.HolderState != "unresponsive" {
			t.Fatalf("the answer is %+v, want kept_reason holder_lapsed naming %s as unresponsive", answer, holder)
		}

		// A new process resumes the session under the same id and claims the role, stating
		// itself, before it registers. The claim's renewal is inside the live window, so the claim
		// is refused 409 as one the role is held under this session id by another process.
		logs := &strings.Builder{}
		claimed := claimThrough(t, roleSetHandler(first.route, "test-machine", logging.NewWithWriter("test", logs)), holder, role, resumed)
		if claimed.Code != http.StatusConflict || !strings.Contains(claimed.Body.String(), `"holder":"`+holder+`"`) {
			t.Fatalf("the resumed process's claim: status %d %s, want 409 naming %s", claimed.Code, claimed.Body.String(), holder)
		}
		refusal := "listener role claim refused: the role is held under this session id by another process"
		if refused := linesIn(logs.String(), refusal, map[string]any{"role": role, "holder_client_instance_id": stalled, "client_instance_id": resumed}); len(refused) != 1 {
			t.Fatalf("the claim route's lines are %s, want one refusal of process %s naming process %s as the holder", logs.String(), resumed, stalled)
		}
		waitFor(t, "the kept copy reaching "+holder+" after its resumed process's refused claim", func() bool {
			return len(rig.envelopesOn(contracts.LegacyAgentSubject(holder))) > 0
		})
		if held := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(held) != 1 || held[0].EventID != answer.EventID {
			t.Fatalf("%s's agent subject holds %+v, want the kept copy of event %s once", holder, held, answer.EventID)
		}
		if got := expectFrames(t, frames, 1, 100*time.Millisecond); got[0].EventID != answer.EventID {
			t.Fatalf("%s received event %s, want the kept %s", holder, got[0].EventID, answer.EventID)
		}
		if claim := storedRoleClaim(t, first.harness.registry, role); claim.HolderSessionID != holder || claim.ClientInstanceID != stalled {
			t.Fatalf("role %s's claim is %+v, want it still naming %s under process %s", role, claim, holder, stalled)
		}
	})
}

// wakeKey is the dedupe key of the sweep's wake report of role in the 5-minute interval at falls
// in, as the sweep names it: legion.wake.role.<role>.<the interval's start in Unix seconds>.
func wakeKey(role string, at time.Time) string {
	return fmt.Sprintf("legion.wake.role.%s.%d", role, at.Unix()/300*300)
}

// wakeStepShift is where TestAKeptCopyWakesItsRoleUntilTaken sets the clock in each of its steps
// through the hour of a copy stored at kept: kept, plus whole steps of five minutes, plus the
// shift returned, between one and four minutes short of a whole step. So the copy's age at a step
// is at least a minute clear of the forty minutes after which it wakes its role no more and of the
// hour after which it is dropped, and the clock lands at least 30 s into an interval and stays in
// it for a minute at least, so each step's reports fall in an interval of their own.
func wakeStepShift(t *testing.T, kept time.Time) time.Duration {
	t.Helper()
	const interval = 5 * time.Minute
	phase := time.Duration(kept.UnixNano()) % interval
	for shift := -time.Minute; shift >= -4*time.Minute; shift -= 10 * time.Second {
		if at := (phase + shift + interval) % interval; at >= 30*time.Second && at <= 4*time.Minute {
			return shift
		}
	}
	t.Fatalf("no shift of the steps lands inside an interval for a copy stored %s into one", phase)
	return 0
}

// hourWake is one report on bus.LegionWakeTopic as these tests read it.
type hourWake struct {
	key  string
	kind string
	role string
}

// wakes is every report the stream holds on bus.LegionWakeTopic, in stream order.
func (r *hourRig) wakes() []hourWake {
	r.t.Helper()
	var found []hourWake
	for _, item := range r.envelopesOn(bus.LegionWakeTopic) {
		var report struct {
			Kind string `json:"kind"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal([]byte(item.Payload), &report); err != nil {
			r.t.Fatalf("decode the wake report %s: %v", item.Payload, err)
		}
		found = append(found, hourWake{key: item.DedupeKey, kind: report.Kind, role: report.Role})
	}
	return found
}

// sweptWakes is the dedupe key of every report a sweep raised for role, in stream order.
func (r *hourRig) sweptWakes(role string) []string {
	r.t.Helper()
	var keys []string
	for _, wake := range r.wakes() {
		if strings.HasPrefix(wake.key, "legion.wake.role."+role+".") {
			keys = append(keys, wake.key)
		}
	}
	return keys
}

// keepWakes is the dedupe key of every role report for role that no sweep raised.
func (r *hourRig) keepWakes(role string) []string {
	r.t.Helper()
	var keys []string
	for _, wake := range r.wakes() {
		if wake.role == role && wake.kind == "role_message" && !strings.HasPrefix(wake.key, "legion.wake.role.") {
			keys = append(keys, wake.key)
		}
	}
	return keys
}

// TestAKeptCopyWakesItsRoleUntilTaken holds row (f) of check k2: a copy the route keeps raises
// its role's wake report at the keep, and while a copy the listener kept has at least twenty
// minutes of its hour left and no live session holds the role, the sweeps raise the report again
// once in each 5-minute interval across both listeners. A copy that is taken, or whose hour has
// ended, and a copy Dispatch's outbox wrote, wake nothing.
func TestAKeptCopyWakesItsRoleUntilTaken(t *testing.T) {
	t.Parallel()
	t.Run("f a kept copy wakes its role once in each interval of its first forty minutes until it is taken or ends", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			routed = "hour-wake-routed"
			laned  = "hour-wake-laned"
			taken  = "hour-wake-taken"
			outbox = "hour-wake-outbox"
			taker  = "ses_hour_wake_taker"
			sender = "ses_hour_wake_sender"
		)
		first := rig.listeners[0]
		reporting, stopReporting := context.WithCancel(context.Background())
		t.Cleanup(stopReporting)
		// The route raises the keep's report through the reporter main.go gives it.
		first.route.wakes = newWakeReporter(reporting, first.harness.client)

		stored := map[string]laneCopy{}
		for _, role := range []string{routed, taken} {
			answer := rig.kept(first, roleMessage(role, sender, "wake role "+role))
			stored[role] = rig.copyOf(role, answer.EventID)
		}
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+laned, "lane-"+id.New())
		item.SourceSession = sender
		rig.listeners[1].lane(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
		waitForKeptRoleMessage(t, rig.client(), laned, item.EventID)
		stored[laned] = rig.copyOf(laned, item.EventID)
		stored[outbox] = rig.keep(outbox, outboxCopy(outbox, sender, "a Dispatch event routed to the role"))
		waitFor(t, "the wake reports of the route's two keeps", func() bool {
			return len(rig.keepWakes(routed)) == 1 && len(rig.keepWakes(taken)) == 1
		})

		kept := stored[routed].stored
		for role, held := range stored {
			if spread := held.stored.Sub(kept); spread < 0 || spread > 30*time.Second {
				t.Fatalf("role %s's copy was stored %s after the first, want the four copies in time of each other", role, spread)
			}
		}
		shift := wakeStepShift(t, kept)
		expected := map[string][]string{}
		waiting := map[string]bool{routed: true, laned: true, taken: true}
		for step := 1; step <= 14; step++ {
			rig.setClock(kept.Add(time.Duration(step)*5*time.Minute + shift))
			if step == 1 {
				rig.startSweeps()
			}
			at := rig.now()
			woken := 0
			for _, role := range []string{routed, laned, taken} {
				if !waiting[role] || at.Sub(stored[role].stored) > 40*time.Minute {
					continue
				}
				key := wakeKey(role, at)
				waitFor(t, fmt.Sprintf("step %d's wake report of role %s", step, role), func() bool {
					return slices.Contains(rig.sweptWakes(role), key)
				})
				waitFor(t, fmt.Sprintf("a listener logging step %d's report of role %s as already stored", step, role), func() bool {
					return len(rig.lines(hourWakeStoredLine, map[string]any{"role": role, "dedupe_key": key})) > 0
				})
				expected[role] = append(expected[role], key)
				woken++
			}
			if woken == 0 {
				rig.letSweepsRun()
			}
			if step == 3 {
				// A live session takes the copy, then leaves the role with no live holder: the
				// lane holds nothing more that waits for one.
				claimRoleWith(t, first.harness, taker, taken, liveRoleActEntry)
				waitFor(t, "the copy reaching "+taker, func() bool {
					return len(rig.envelopesOn(contracts.LegacyAgentSubject(taker))) == 1
				})
				rig.waitPassed(taken, stored[taken].sequence)
				if err := first.harness.sessions.Delete(context.Background(), taker); err != nil {
					t.Fatalf("delete %s's record: %v", taker, err)
				}
				waiting[taken] = false
			}
			if at.Sub(kept) > time.Hour {
				for _, role := range []string{routed, laned, outbox} {
					rig.waitPassed(role, stored[role].sequence)
				}
			}
			for _, role := range []string{routed, laned, taken, outbox} {
				if got := rig.sweptWakes(role); !slices.Equal(got, expected[role]) {
					t.Fatalf("after step %d, with the clock %s after the keep, role %s's lane has raised wake reports %v, want %v",
						step, at.Sub(kept), role, got, expected[role])
				}
			}
		}

		if len(expected[routed]) != 8 || len(expected[taken]) != 3 {
			t.Fatalf("the steps woke role %s %d times and role %s %d times, want once in each of the eight steps of the copy's first forty minutes and in the three before the copy was taken",
				routed, len(expected[routed]), taken, len(expected[taken]))
		}
		for _, role := range []string{routed, laned, taken} {
			for _, key := range expected[role] {
				if published := rig.lines(hourWakeLine, map[string]any{"role": role, "dedupe_key": key}); len(published) != 1 {
					t.Fatalf("report %s is logged published %d times across both listeners, want once: %v", key, len(published), published)
				}
			}
		}
		for role, want := range map[string]int{routed: 1, taken: 1, laned: 0, outbox: 0} {
			if got := rig.keepWakes(role); len(got) != want {
				t.Fatalf("role %s has %d wake reports no sweep raised, want %d: %v", role, len(got), want, got)
			}
		}
	})
}
