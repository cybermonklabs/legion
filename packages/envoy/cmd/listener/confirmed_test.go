package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// publishTo publishes item to subject over core NATS and flushes, as a router that forwards an
// envelope while it keeps its own topic does (bus.Client.PublishCoreContext). The stream captures
// agent subjects, so a forward copy published to one is stored there.
func publishTo(t *testing.T, client *bus.Client, subject string, item contracts.Envelope) {
	t.Helper()
	ctx := testwait.Context(t)
	if err := client.PublishCoreContext(ctx, subject, item); err != nil {
		t.Fatalf("publish %s to %s: %v", item.Topic, subject, err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publish to %s: %v", subject, err)
	}
}

// publishDeps is the harness's stores as main hands them to the /v1 routes, with the delivery
// handlers' own config and a confirmation record of the route's own, as main wires the publish
// route.
func (h listenerDeliveryHarness) publishDeps(t *testing.T) *listenerDeps {
	t.Helper()
	return &listenerDeps{
		client:    h.client,
		registry:  h.registry,
		sessions:  h.sessions,
		delivery:  &h.config,
		confirmed: newConfirmedRecord(t),
	}
}

// newConfirmedRecord is the publish route's own confirmation record as main makes it, stopped
// when the test ends.
func newConfirmedRecord(t *testing.T) *dedupe.Cache {
	t.Helper()
	confirmed := dedupe.New(10 * time.Minute)
	t.Cleanup(confirmed.Stop)
	return confirmed
}

// withRoleClaimDrain gives d what POST /v1/roles/set needs beyond its stores, as main wires it.
//
// The delivery configuration a route runs a kept-lane drain on: a config over d's stores with
// the dedupe cache and the attempt cache the drain judges a forward against (beforeForward),
// startRoleForward(client) as the forward, a logger, a delivered counter, the drain's
// kept-expired, dead-letter and undecodable counters, the dead letter's publish as main.go sets
// it (client.PublishStoredAs), the guards two drains of one role take (roleKeepGuards) and the
// listener's clock, time.Now as main.go sets it. POST /v1/roles/set hands this config to the
// handovers it starts (runKeptRoleDrain) - a granted claim's, and one for each role of a claimant
// it read not live before its refresh - and so does POST /v1/interests/subscribe for a
// registration that makes its session live again while its row or the role claim copy names a
// role for it, so a test that claims a role through the one route, or registers a role lane or a
// session that holds a claim through the other, needs it.
//
// Neither route needs anything of the live window here: each reads that window from d's own
// session registry, the claim route when it weighs a standing claim's renewal (roleSetHandler,
// through session.SessionRegistry.Windows) and the registration route when it reads whether the
// session was live (listenerDeps.registerPass), so a test's routes and its registry are held to
// one window with nothing to keep in step by hand.
func withRoleClaimDrain(t *testing.T, d *listenerDeps) *listenerDeps {
	t.Helper()
	cache := dedupe.New(10 * time.Minute)
	t.Cleanup(cache.Stop)
	attempts := dedupe.New(5 * time.Minute)
	t.Cleanup(attempts.Stop)
	logs := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("publish route log:\n%s", logs.String())
		}
	})
	d.delivery = &listenerDeliveryHandlerConfig{
		client: d.client,
		// The delivery config holds the deps' clock, so a test that drives one drives the
		// bounds of both the route and the kept-lane drain behind it.
		clock:               d.clock,
		forwardRole:         startRoleForward(d.client),
		registry:            d.registry,
		sessions:            d.sessions,
		dedupeCache:         cache,
		attemptCache:        attempts,
		logger:              logging.NewWithWriter("test", logs),
		messagesDelivered:   metrics.New().NewCounter("test_messages_delivered", "test"),
		roleKeptExpired:     metrics.New().NewCounter("test_role_kept_expired", "test"),
		roleDeadLetters:     metrics.New().NewCounter("test_role_dead_letters", "test"),
		roleKeptUndecodable: metrics.New().NewCounter("test_role_kept_undecodable", "test"),
		publishDeadLetter:   d.client.PublishStoredAs,
		roleKeepDrains:      newRoleKeepGuards(),
		now:                 time.Now,
	}
	return d
}

// withPublishRoute gives d what the publish route needs beyond its stores, as main wires it: the
// delivery config a kept copy's drain runs on (withRoleClaimDrain) and a confirmation record.
func withPublishRoute(t *testing.T, d *listenerDeps) *listenerDeps {
	t.Helper()
	withRoleClaimDrain(t, d)
	d.confirmed = newConfirmedRecord(t)
	return d
}

// startStandIn is a session process on a NATS connection of its own to the test's server, as
// pi-envoy and claude-envoy hold one: it subscribes to its agent subject and hands over every frame
// it receives. It answers nothing, as those hosts answer nothing: a role forward counts as
// delivered once the stream acknowledged storing it (startRoleForward).
func startStandIn(t *testing.T, uri, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	frames := make(chan contracts.Envelope, 16)
	subscription, err := conn.Subscribe(contracts.LegacyAgentSubject(sessionID), func(message *natsgo.Msg) {
		var frame contracts.Envelope
		if err := json.Unmarshal(message.Data, &frame); err != nil {
			t.Errorf("stand-in %s decode frame: %v", sessionID, err)
			return
		}
		frames <- frame
	})
	if err != nil {
		t.Fatalf("stand-in %s subscribe: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("stand-in %s flush: %v", sessionID, err)
	}
	return frames
}

// claimRoleFor registers sessionID as a live self-subscribed session advertising capabilities and
// gives it role.
func claimRoleFor(t *testing.T, registry *store.Registry, sessions *session.SessionRegistry, sessionID, role string, capabilities []string) {
	t.Helper()
	if err := sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: capabilities}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	if _, err := registry.SetRole(sessionID, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, sessionID, err)
	}
}

// publishThrough sends body to the publish route and returns its answer and how long it took.
func publishThrough(d *listenerDeps, body string) (*httptest.ResponseRecorder, time.Duration) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	publishHandler(d, logging.New("test")).ServeHTTP(recorder, request)
	return recorder, time.Since(started)
}

// expectFramesQuiet is the span expectFrames holds for a frame it wants none of: the frames come
// from a session stand-in reading its own consumer, which this test reaches through neither its
// state nor the effect of a later action of its own, since a frame the route already forwarded
// and one it never forwards look alike until the stand-in has had a moment to read.
const expectFramesQuiet = 300 * time.Millisecond

// expectFrames reads want frames from frames, then fails on any further one inside
// expectFramesQuiet.
func expectFrames(t *testing.T, frames <-chan contracts.Envelope, want int) []contracts.Envelope {
	t.Helper()
	var got []contracts.Envelope
	for len(got) < want {
		got = append(got, testwait.Receive(t, fmt.Sprintf("the stand-in's frame %d of %d", len(got)+1, want), frames))
	}
	testwait.Quiet(t, fmt.Sprintf("the stand-in's frames past the %d wanted", want), frames, expectFramesQuiet,
		"the frames are a session stand-in's reads of its own consumer, which neither this test's state nor a later action of its own reaches")
	return got
}

// noException fails when the probe holds a delivery exception: the code under test published
// none. It reads what the probe holds and waits for nothing, the proof of an absence from state
// (R2, docs/testing.md), so its caller calls it once two things hold. The code that would
// publish the exception has returned: a route answered, a handler called in line returned and
// the role forwarder settled, or, for a handler a subscription runs, a marker sent behind the
// message through that subscription was handled (keptBehind). And a round trip on the probe's
// own connection followed: a flush, or a request on it such as a read of the stream. The round
// trip orders the probe after the exception because every exception the listener raises reached
// the server before its publish returned - publishDeliveryException and publishRefusalException
// publish through bus.Client.Publish, which waits for the stream's acknowledgement on a stored
// lane and for publishConfirmed's flush on a core one - and the server writes the probe's copy
// of a message before it answers a later round trip on the probe's connection. A refusal's
// exception goes out on a goroutine of its own after the 404 (publishRefusalException), which
// nothing a caller can wait for orders, so no caller reads a refusal with this.
func noException(t *testing.T, probe *natsgo.Subscription) {
	t.Helper()
	// A timeout of zero reads what the subscription already holds and waits for nothing.
	if message, err := probe.NextMsg(0); err == nil {
		t.Fatalf("a delivery exception was published on %s: %s", message.Subject, message.Data)
	} else if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("read the exception probe: %v", err)
	}
}

// keptBehind publishes a marker on role's lane, on the harness's client, and waits for the lane
// to keep it for the role's next holder. A subscription hands its messages to the lane's handler
// one at a time, so once the marker is kept the handler has returned from every message
// published on that lane before it. It is the later action a test of a role nobody holds live
// takes before it reads the probe (noException): the kept copy of the message under test is
// stored before the handler that stored it returns.
func keptBehind(t *testing.T, harness listenerDeliveryHarness, role string) {
	t.Helper()
	marker := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "marker-"+id.New())
	if err := harness.client.Publish(marker); err != nil {
		t.Fatalf("publish the marker on role %s's lane: %v", role, err)
	}
	waitForKeptRoleMessage(t, harness.client, role, marker.EventID)
}

func exceptionProbe(t *testing.T, client *bus.Client) *natsgo.Subscription {
	t.Helper()
	probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the exception lanes: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}
	return probe
}

// A live holder answers no receipt, so its forward counts as delivered once the stream stored it:
// the sender is answered 200 after the one forward, and the holder is not sent the envelope again.
func TestPublishRouteAnswersStoredOnceTheHoldersForwardIsKept(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "stored-forward"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder")
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"take this","idempotency_key":"stored-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" || answer.DedupeKey != "publish.stored-1" {
		t.Fatalf("answer %s, want holder ses_holder and dedupe key publish.stored-1", recorder.Body.String())
	}
	for i, frame := range expectFrames(t, frames, 1) {
		if frame.EventID != answer.EventID || frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-1" || frame.Topic != contracts.RoleTopicPrefix+role {
			t.Fatalf("frame %d: event %s dedupe key %s topic %s, want the answered event %s under %s on its role topic", i+1, frame.EventID, frame.DedupeKey, frame.Topic, answer.EventID, roleForwardDedupePrefix+"publish.stored-1")
		}
	}
	if err := harness.client.Conn.FlushWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("a round trip on the exception probe's connection: %v", err)
	}
	noException(t, probe)
}

// A holder whose session holds no NATS connection - its process is down, or it reads its messages
// from its own JetStream consumer - is answered 200 all the same: the stream holds the forward on
// the holder's agent subject, where the holder's consumer delivers it with acknowledgement and
// redelivery. Nothing answers a receipt there, and the send waits for none.
func TestPublishRouteStoresTheForwardForAHolderThatIsNotConnected(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "offline-holder"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_offline", role, nil)
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"while nobody listens","idempotency_key":"offline-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_offline" || answer.DedupeKey != "publish.offline-1" {
		t.Fatalf("answer %s, want holder ses_offline and dedupe key publish.offline-1", recorder.Body.String())
	}
	subject := contracts.LegacyAgentSubject("ses_offline")
	stored, err := harness.client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the stream's forward on %s: %v", subject, err)
	}
	var forwarded contracts.Envelope
	if err := json.Unmarshal(stored.Data, &forwarded); err != nil {
		t.Fatalf("decode the stored forward %s: %v", stored.Data, err)
	}
	if forwarded.EventID != answer.EventID || forwarded.DedupeKey != roleForwardDedupePrefix+"publish.offline-1" || forwarded.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the stream holds event %s dedupe key %s topic %s on %s, want the answered event %s under %s on its role topic",
			forwarded.EventID, forwarded.DedupeKey, forwarded.Topic, subject, answer.EventID, roleForwardDedupePrefix+"publish.offline-1")
	}
	if count := storedForwards(t, harness.client, subject); count != 1 {
		t.Fatalf("the stream holds %d messages on %s, want the one forward", count, subject)
	}
	if err := harness.client.Conn.FlushWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("a round trip on the exception probe's connection: %v", err)
	}
	noException(t, probe)
}

// The publish route names its role forward with the forward's own Nats-Msg-Id (bus.ForwardMsgID
// of the forward's dedupe key and the holder's agent subject), so the stream, which captures
// agent subjects, keeps one copy of the message for its holder however often the route sends it.
// Two sends of one idempotency key through two routes - a second listener, which holds no record
// of the first one's confirmation - forward the envelope to the holder twice, and the stream
// stores that one copy.
func TestPublishRouteStoresOneCopyOfARoleForwardSentTwice(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "stored-once"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_one_copy", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_one_copy")
	probe := exceptionProbe(t, harness.client)
	body := `{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"twice over","idempotency_key":"stored-once-1"}`

	for send, deps := range []*listenerDeps{harness.publishDeps(t), harness.publishDeps(t)} {
		recorder, elapsed := publishThrough(deps, body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("send %d: status %d after %s, want 200; body %s", send+1, recorder.Code, elapsed, recorder.Body.String())
		}
	}
	// Both sends forwarded, so the stream took the message twice and stored it under one name.
	for i, frame := range expectFrames(t, frames, 2) {
		if frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-once-1" {
			t.Fatalf("forward %d: dedupe key %s, want %s", i+1, frame.DedupeKey, roleForwardDedupePrefix+"publish.stored-once-1")
		}
	}
	subject := contracts.LegacyAgentSubject("ses_one_copy")
	if count := storedForwards(t, harness.client, subject); count != 1 {
		t.Fatalf("the stream holds %d messages on %s after two forwards of publish.stored-once-1, want the one copy their Nats-Msg-Id keeps", count, subject)
	}
	if err := harness.client.Conn.FlushWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("a round trip on the exception probe's connection: %v", err)
	}
	noException(t, probe)
}

// A role-claim read that cannot finish, the server having stopped answering under a live
// connection, ends the send's first resolution at sendRequestBudget with the answer GET
// /v1/roles/<role> gives the same stalled read - the bus-unavailable 503 with Retry-After: 1,
// naming the read - rather than holding the sender past its window.
func TestPublishRouteAnswersBusUnavailableWhenTheClaimReadStalls(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	const role = "frozen-hub"
	claimRoleFor(t, registry, sessions, "ses_holder", role, nil)
	startStandIn(t, uri, "ses_holder")
	// The route's own bounds run on a fake clock, so the send's first resolution ends when this
	// test moves the clock past sendRequestBudget rather than when the machine's clock reaches
	// it: a route that left the stalled read to nats.go's own timeout would arm nothing here.
	now := clocktest.Held()
	deps := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions, clock: now})

	thaw := srv.Freeze(t)
	t.Cleanup(thaw)
	answers := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder, _ := publishThrough(deps, `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"through a frozen hub"}`)
		answers <- recorder
	}()
	awaitArmedBeforeReturn(t, now, "the publish route's own bound on its reads of the frozen hub", timersArmed(1), answerReturned(answers))
	select {
	case recorder := <-answers:
		t.Fatalf("the route answered %d before its budget was spent: %s", recorder.Code, recorder.Body.String())
	default:
	}
	now.Advance(sendRequestBudget)
	recorder := testwait.Receive(t, "the publish route's answer once its budget was spent", answers)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d at sendRequestBudget (%s), want 503; body %s", recorder.Code, sendRequestBudget, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, "read role "+role+"'s claim") {
		t.Fatalf("answer %s, want it to begin %q and name the claim read", recorder.Body.String(), busUnansweredPrefix)
	}
}

// A holder that does not advertise the payload's delivery mode is refused as /v1/messages/send
// refuses it, naming the holder, and is sent nothing.
func TestPublishRouteRefusesADeliveryModeTheHolderDoesNotAdvertise(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "no-steer"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_aside_only", role, []string{"aside"})
	frames := startStandIn(t, testnats.URL(t), "ses_aside_only")

	recorder, _ := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"steer now","payload":"{\"delivery\":{\"mode\":\"steer\"}}"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != `{"error":"session ses_aside_only () does not advertise steer","reason":"capability","holder":"ses_aside_only"}`+"\n" {
		t.Fatalf("body %q", body)
	}
	expectFrames(t, frames, 0)
}

// A sender that holds the role itself is answered 200 and nothing is sent, to the holder or on the
// role lane, as the role lane skips an echo.
func TestPublishRouteSendsNothingToASenderHoldingTheRole(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "self-held"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_self", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_self")
	lane, err := harness.client.Conn.SubscribeSync(contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role lane: %v", err)
	}
	t.Cleanup(func() { _ = lane.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the lane probe: %v", err)
	}

	recorder, _ := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"note to self","source_session":"ses_self"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_self" {
		t.Fatalf("answer %s (%v), want holder ses_self", recorder.Body.String(), err)
	}
	expectFrames(t, frames, 0)
	// A marker the test publishes on the role lane, on the client the route published on, after
	// the send was answered, is the later action that proves the route published nothing there:
	// anything the route sent on the lane reaches the probe before the marker.
	if err := harness.client.Conn.Publish(contracts.RoleTopicPrefix+role, []byte("marker")); err != nil {
		t.Fatalf("publish the marker on the role lane: %v", err)
	}
	if err := harness.client.Conn.FlushWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("flush the marker: %v", err)
	}
	message, err := lane.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("read the role lane up to the marker: %v", err)
	}
	if string(message.Data) != "marker" {
		t.Fatalf("the send went out on the role lane: the probe read %s before the marker", message.Data)
	}
}

// A re-send under the key of a message its holder already confirmed is answered 200 and not
// forwarded again: the holder took it once.
func TestPublishRouteDoesNotForwardARepeatKeyItsHolderConfirmed(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "repeat-key"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder")
	deps := harness.publishDeps(t)
	body := `{"topic":"` + contracts.RoleTopicPrefix + role + `","message":"once","idempotency_key":"repeat-1"}`

	first, _ := publishThrough(deps, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first send: status %d, want 200; body %s", first.Code, first.Body.String())
	}
	expectFrames(t, frames, 1)
	repeat, _ := publishThrough(deps, body)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat send: status %d, want 200; body %s", repeat.Code, repeat.Body.String())
	}
	var answer publishResponse
	if err := json.Unmarshal(repeat.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_holder" || answer.DedupeKey != "publish.repeat-1" {
		t.Fatalf("repeat answer %s (%v), want holder ses_holder under publish.repeat-1", repeat.Body.String(), err)
	}
	expectFrames(t, frames, 0)
}

// A forward copy the stream stored on a session's agent subject reaches every listener's durable;
// the durable acknowledges it and delivers it to nobody, so a session subscribed to the copy's
// topic is not given the message a second time.
func TestDurableAcknowledgesAStoredForwardCopyAndDeliversNothing(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const topic = "notifications.team.stored-copy"
	harness.registerFanoutTarget(t, "ses_subscriber", topic, nil)
	consumer := "listener-forward-copy-" + id.New()
	subscription, err := startListenerSubscription(harness.client, consumer, harness.handler)
	if err != nil {
		t.Fatalf("start the durable: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })

	forwardCopy := listenerTestEnvelope(topic, "publish.stored-copy")
	publishTo(t, harness.client, contracts.LegacyAgentSubject("ses_self_subscribed"), forwardCopy)
	testwait.Until(t, "the durable to acknowledge the stored copy", func() (string, bool) {
		info, err := harness.client.JS().ConsumerInfo(bus.Stream, consumer)
		if err != nil {
			t.Fatalf("read the durable: %v", err)
		}
		acknowledged := info.Delivered.Stream >= 1 && info.AckFloor.Stream >= info.Delivered.Stream && info.NumAckPending == 0
		if acknowledged && info.Delivered.Consumer != 1 {
			t.Fatalf("the durable delivered the copy %d times, want once", info.Delivered.Consumer)
		}
		return fmt.Sprintf("%+v", info), acknowledged
	})
	if harness.config.dedupeCache.Seen(forwardCopy.DedupeKey, "ses_subscriber") {
		t.Fatalf("the durable delivered the stored copy to the topic's subscriber, which is sent nothing for it")
	}
}
