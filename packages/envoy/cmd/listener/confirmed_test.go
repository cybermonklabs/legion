package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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
	"github.com/sjawhar/envoy/listenerwire"
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
		client:              d.client,
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
		publishExpiryNotice: d.client.PublishKeptCopyExpiry,
		keepPublish:         d.client.PublishStoredAck,
		roleKeepDrains:      newRoleKeepGuards(),
		now:                 time.Now,
		// Whether the rig's NATS user holds the session consumers' calls, asked of the server at
		// each call: the rig opens no session delivery, whose answer main.go wires.
		consumerGrant: answeredBy(func(ctx context.Context) bool { return d.client.ProbeConsumerCalls(ctx) == nil }),
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

// expectFrames reads want frames from frames, each through the shared wait, and fails on a frame
// past them that has already come. A caller proves that no later frame comes either by a state
// read or by a marker published after the code under test's forwards (noFurtherFrame), and
// passes a quiet of 0, which holds no span. A quiet above 0 is a window in which no further frame
// may come (testwait.Quiet), kept for the callers that pass one, in files whose rows have not yet
// moved to such a proof.
func expectFrames(t *testing.T, frames <-chan contracts.Envelope, want int, quiet time.Duration) []contracts.Envelope {
	t.Helper()
	got := make([]contracts.Envelope, 0, want)
	for len(got) < want {
		got = append(got, testwait.Receive(t, fmt.Sprintf("frame %d of the %d the stand-in is sent, after %d", len(got)+1, want, len(got)), frames))
	}
	if quiet > 0 {
		testwait.Quiet(t, fmt.Sprintf("a frame past the %d the stand-in is sent", want), frames, quiet,
			"the caller has not moved to a state read or to a marker published after the code under test's forwards (noFurtherFrame), so its quiet window is still its proof that no further frame comes")
		return got
	}
	select {
	case frame := <-frames:
		t.Fatalf("the stand-in received frame %d (event %s, dedupe key %s), want %d", want+1, frame.EventID, frame.DedupeKey, want)
	default:
	}
	return got
}

// noFurtherFrame fails when the stand-in for sessionID, whose frames come on frames, has been sent
// a frame past those its caller has read. It proves the absence with a marker rather than a
// window: once the code under test has made every forward it makes - a route once it has
// answered, a handover once its listener is idle for the role - it publishes a marker envelope on
// sessionID's agent subject on publisher, the connection that code forwards on, and waits through
// the shared wait for the stand-in's next frame, which must be the marker. NATS hands one
// connection's publishes to a subscriber in the order they were made, so every forward published
// before the marker reaches the stand-in before it. The stream captures agent subjects and stores
// the marker as well, so a caller that counts what that subject holds counts it first.
func noFurtherFrame(t *testing.T, publisher *natsgo.Conn, sessionID string, frames <-chan contracts.Envelope) {
	t.Helper()
	subject := contracts.LegacyAgentSubject(sessionID)
	marker := listenerTestEnvelope(subject, "frame-marker-"+id.New())
	if err := publisher.Publish(subject, marshalListenerEnvelope(t, marker)); err != nil {
		t.Fatalf("publish the marker frame to %s: %v", subject, err)
	}
	frame := testwait.Receive(t, fmt.Sprintf("the marker frame published to %s after every forward of the code under test", sessionID), frames)
	if frame.EventID != marker.EventID {
		t.Fatalf("the stand-in %s received frame event %s (dedupe key %s, topic %s) before the marker published after every forward, want nothing past the frames it was sent",
			sessionID, frame.EventID, frame.DedupeKey, frame.Topic)
	}
}

// nothingHeardBefore fails when sub has been handed a message by the time it is called. sub is
// subscribed on the connection the code under test publishes on, and that code has returned. It
// proves the absence with a marker rather than a window: it publishes a marker, on sub's own
// connection (natsgo.Msg.Respond publishes on the connection its Sub is bound to), to a subject
// sub hears (markerSubject), and waits for sub's next message through the shared wait's context,
// which must be the marker. One connection keeps the order of its publishes, so a message the code
// published on it before this call reaches sub before the marker. what names what sub must not
// have been handed, for the failure.
func nothingHeardBefore(t *testing.T, sub *natsgo.Subscription, what string) {
	t.Helper()
	token := "marker " + id.New()
	marker := &natsgo.Msg{Sub: sub, Reply: markerSubject(sub.Subject)}
	if err := marker.Respond([]byte(token)); err != nil {
		t.Fatalf("publish the marker to %s: %v", marker.Reply, err)
	}
	heard, err := sub.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("the marker published to %s after the code under test returned did not reach the probe of %s: %v", marker.Reply, sub.Subject, err)
	}
	if heard.Subject != marker.Reply || string(heard.Data) != token {
		t.Fatalf("%s was published: the probe of %s was handed %s on %s before the marker published after the code under test returned",
			what, sub.Subject, heard.Data, heard.Subject)
	}
}

// markerSubject is a subject that a subscription to subscribed hears: subscribed with each
// wildcard token replaced by a token of the marker's own. A probe of one literal subject gets its
// marker on that subject, and the marker's body tells it apart from anything else published there.
func markerSubject(subscribed string) string {
	tokens := strings.Split(subscribed, ".")
	for i, token := range tokens {
		if token == "*" || token == ">" {
			tokens[i] = "test-marker"
		}
	}
	return strings.Join(tokens, ".")
}

// noException fails when any delivery exception was published before it is called: the route
// answers its sender and publishes none. probe is subscribed on the connection the code under test
// publishes its exceptions on (exceptionProbe's is on that client's own), and the code has
// returned, so a marker published on that connection (nothingHeardBefore) reaches probe after any
// exception it published.
func noException(t *testing.T, probe *natsgo.Subscription) {
	t.Helper()
	nothingHeardBefore(t, probe, "a delivery exception")
}

// exceptionProbe subscribes to every exception lane on client's own connection, the one the code
// under test publishes its exceptions on when it holds client, and flushes, so the probe hears
// every exception published after it returns.
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
	t.Parallel()
	harness := newListenerDeliveryHarness(t)
	const role = "stored-forward"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_holder", role, nil)
	frames := startStandIn(t, testnats.URL(t), "ses_holder")
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"take this","idempotency_key":"stored-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" || answer.DedupeKey != "publish.stored-1" {
		t.Fatalf("answer %s, want holder ses_holder and dedupe key publish.stored-1", recorder.Body.String())
	}
	for i, frame := range expectFrames(t, frames, 1, 0) {
		if frame.EventID != answer.EventID || frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-1" || frame.Topic != contracts.RoleTopicPrefix+role {
			t.Fatalf("frame %d: event %s dedupe key %s topic %s, want the answered event %s under %s on its role topic", i+1, frame.EventID, frame.DedupeKey, frame.Topic, answer.EventID, roleForwardDedupePrefix+"publish.stored-1")
		}
	}
	// The route has answered, so it has made every forward it makes.
	noFurtherFrame(t, harness.client.Conn, "ses_holder", frames)
	noException(t, probe)
}

// A holder whose session holds no NATS connection - its process is down, or it reads its messages
// from its own JetStream consumer - is answered 200 all the same: the stream holds the forward on
// the holder's agent subject, where the holder's consumer delivers it with acknowledgement and
// redelivery. Nothing answers a receipt there, and the send waits for none.
func TestPublishRouteStoresTheForwardForAHolderThatIsNotConnected(t *testing.T) {
	t.Parallel()
	harness := newListenerDeliveryHarness(t)
	const role = "offline-holder"
	claimRoleFor(t, harness.registry, harness.sessions, "ses_offline", role, nil)
	probe := exceptionProbe(t, harness.client)

	recorder, elapsed := publishThrough(harness.publishDeps(t), `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"while nobody listens","idempotency_key":"offline-1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d after %s, want 200; body %s", recorder.Code, elapsed, recorder.Body.String())
	}
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_offline" || answer.DedupeKey != "publish.offline-1" {
		t.Fatalf("answer %s, want holder ses_offline and dedupe key publish.offline-1", recorder.Body.String())
	}
	subject := contracts.LegacyAgentSubject("ses_offline")
	stored, err := harness.client.JS().GetLastMsg(bus.Stream, subject, natsgo.Context(testwait.Context(t)))
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
	noException(t, probe)
}

// The publish route names its role forward with the forward's own Nats-Msg-Id (bus.ForwardMsgID
// of the forward's dedupe key and the holder's agent subject), so the stream, which captures
// agent subjects, keeps one copy of the message for its holder however often the route sends it.
// Two sends of one idempotency key through two routes - a second listener, which holds no record
// of the first one's confirmation - forward the envelope to the holder twice, and the stream
// stores that one copy.
func TestPublishRouteStoresOneCopyOfARoleForwardSentTwice(t *testing.T) {
	t.Parallel()
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
	for i, frame := range expectFrames(t, frames, 2, 0) {
		if frame.DedupeKey != roleForwardDedupePrefix+"publish.stored-once-1" {
			t.Fatalf("forward %d: dedupe key %s, want %s", i+1, frame.DedupeKey, roleForwardDedupePrefix+"publish.stored-once-1")
		}
	}
	subject := contracts.LegacyAgentSubject("ses_one_copy")
	if count := storedForwards(t, harness.client, subject); count != 1 {
		t.Fatalf("the stream holds %d messages on %s after two forwards of publish.stored-once-1, want the one copy their Nats-Msg-Id keeps", count, subject)
	}
	// Both routes have answered, so they have made every forward they make.
	noFurtherFrame(t, harness.client.Conn, "ses_one_copy", frames)
	noException(t, probe)
}

// A role-claim read that cannot finish, the server having stopped answering under a live
// connection, ends the send's first resolution at sendRequestBudget with the answer GET
// /v1/roles/<role> gives the same stalled read - the bus-unavailable 503 with Retry-After: 1,
// naming the read - rather than holding the sender past its window.
func TestPublishRouteAnswersBusUnavailableWhenTheClaimReadStalls(t *testing.T) {
	// Not parallel: it requires the 503 at sendRequestBudget (3.5 s) and inside 4.5 s, a bound
	// one second past what it waits for.
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	const role = "frozen-hub"
	claimRoleFor(t, registry, sessions, "ses_holder", role, nil)
	startStandIn(t, uri, "ses_holder")
	deps := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})

	thaw := srv.Freeze(t)
	t.Cleanup(thaw)
	recorder, elapsed := publishThrough(deps, `{"topic":"`+contracts.RoleTopicPrefix+role+`","message":"through a frozen hub"}`)
	if recorder.Code != http.StatusServiceUnavailable || elapsed < sendRequestBudget || elapsed > 4500*time.Millisecond {
		t.Fatalf("status %d after %s, want 503 at sendRequestBudget (%s) and inside 4.5s; body %s", recorder.Code, elapsed, sendRequestBudget, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var refusal listenerwire.APIError
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
	t.Parallel()
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
	// The route has answered, so it has made every forward it makes.
	noFurtherFrame(t, harness.client.Conn, "ses_aside_only", frames)
}

// A sender that holds the role itself is answered 200 and nothing is sent, to the holder or on the
// role lane, as the role lane skips an echo.
func TestPublishRouteSendsNothingToASenderHoldingTheRole(t *testing.T) {
	t.Parallel()
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
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_self" {
		t.Fatalf("answer %s (%v), want holder ses_self", recorder.Body.String(), err)
	}
	// The route has answered, so it has published all it publishes, on the client the lane probe
	// listens on too.
	noFurtherFrame(t, harness.client.Conn, "ses_self", frames)
	nothingHeardBefore(t, lane, "the send on the role lane")
}

// A re-send under the key of a message its holder already confirmed is answered 200 and not
// forwarded again: the holder took it once.
func TestPublishRouteDoesNotForwardARepeatKeyItsHolderConfirmed(t *testing.T) {
	t.Parallel()
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
	expectFrames(t, frames, 1, 0)
	repeat, _ := publishThrough(deps, body)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat send: status %d, want 200; body %s", repeat.Code, repeat.Body.String())
	}
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(repeat.Body.Bytes(), &answer); err != nil || answer.Holder != "ses_holder" || answer.DedupeKey != "publish.repeat-1" {
		t.Fatalf("repeat answer %s (%v), want holder ses_holder under publish.repeat-1", repeat.Body.String(), err)
	}
	// Both sends have been answered, so the route has made every forward it makes for either: a
	// second forward of the first send, or one of the repeat, comes before the marker.
	noFurtherFrame(t, harness.client.Conn, "ses_holder", frames)
}

// A forward copy the stream stored on a session's agent subject reaches every listener's durable;
// the durable acknowledges it and delivers it to nobody, so a session subscribed to the copy's
// topic is not given the message a second time.
func TestDurableAcknowledgesAStoredForwardCopyAndDeliversNothing(t *testing.T) {
	t.Parallel()
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
	ctx := testwait.Context(t)
	info := testwait.Until(t, "the durable to acknowledge the stored copy", func() (*natsgo.ConsumerInfo, bool) {
		info, err := harness.client.JS().ConsumerInfo(bus.Stream, consumer, natsgo.Context(ctx))
		if err != nil {
			t.Fatalf("read the durable: %v", err)
		}
		return info, info.Delivered.Stream >= 1 && info.AckFloor.Stream >= info.Delivered.Stream && info.NumAckPending == 0
	})
	if info.Delivered.Consumer != 1 {
		t.Fatalf("the durable delivered the copy %d times, want once", info.Delivered.Consumer)
	}
	if harness.config.dedupeCache.Seen(forwardCopy.DedupeKey, "ses_subscriber") {
		t.Fatalf("the durable delivered the stored copy to the topic's subscriber, which is sent nothing for it")
	}
}
