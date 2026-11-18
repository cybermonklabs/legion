package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sendStateSender is the session every send below names as its source, so no answer here turns on
// the sender's own record.
const sendStateSender = "ses_state_sender"

// knownSessionRig is a listener's /v1 routes over one NATS server with a session-consumer
// connection of its own (sessionConsumerRig), so a registration gets the consumer every
// registration needs, and with the known-session store open, as main wires the routes with every
// store open. clock is what that store stamps each record with and reads its 30-day retention and
// its refresh age against; a test moves it.
type knownSessionRig struct {
	*sessionConsumerRig
	known *session.KnownSessions
	url   string
	clock time.Time
}

// newKnownSessionRig is the rig on a server of its own, with watch added to the options of the
// four bucket watchers it opens on the listener's connection (newKnownSessionRigOn).
func newKnownSessionRig(t *testing.T, watch ...kvwatch.Option) *knownSessionRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	return newKnownSessionRigOn(t, client, url, watch...)
}

// newKnownSessionRigOn is newKnownSessionRig over a listener's bus client the test connected
// itself: to a server whose grant it set, or on a clock it chose. watch is added to the options of
// the four bucket watchers the rig opens on client's connection: the three its harness opens
// (newSessionConsumerRigWatching) and the known-session store's.
func newKnownSessionRigOn(t *testing.T, client *bus.Client, url string, watch ...kvwatch.Option) *knownSessionRig {
	t.Helper()
	rig := &knownSessionRig{sessionConsumerRig: newSessionConsumerRigWatching(t, client, url, watch), url: url, clock: time.Now()}
	rig.known = openKnownSessionsOn(t, client.Conn, func() time.Time { return rig.clock }, watch...)
	// The routes read the deps they were built over at each request, so the store set here is the
	// one every route below answers from. The confirmation record is the publish route's own, as
	// main wires it, for a role send.
	rig.deps.known = rig.known
	rig.deps.confirmed = newConfirmedRecord(t)
	return rig
}

// openKnownSessionsOn opens the known-session store on conn with clock, its watcher built with
// watch beside its own options (session.WithKnownSessionWatch), and waits for its first read of
// the bucket, as main's start waits for it.
func openKnownSessionsOn(t *testing.T, conn *natsgo.Conn, clock func() time.Time, watch ...kvwatch.Option) *session.KnownSessions {
	t.Helper()
	known, err := session.OpenKnownSessions(conn,
		session.WithKnownSessionReplicas(1),
		session.WithKnownSessionClock(clock),
		session.WithKnownSessionWatch(watch...))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	return known
}

// register runs one registration of sessionID through the rig's own /v1 routes, as a plugin in
// consumer mode sends it: that writes the interest row, the session entry with consumer delivery,
// the session's consumer and the known-session record. fields overrides or adds to the
// registration's defaults, "capabilities", "topics" and "delivery" among them.
func (rig *sessionConsumerRig) register(t *testing.T, sessionID string, fields map[string]any) {
	t.Helper()
	body := map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"title":           "a checked session",
		"topics":          []string{},
		"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + sessionID},
	}
	maps.Copy(body, fields)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rig.post(t, "/v1/interests/subscribe", string(encoded))
}

// stop ends sessionID the way a session that shuts down cleanly does, through the real route: its
// session entry goes, its consumer after it, and its known-session record stays, moved to the
// store's clock.
func (rig *knownSessionRig) stop(t *testing.T, sessionID string) {
	t.Helper()
	stopSession(t, rig.mux, sessionID, "")
	if _, err := rig.deps.sessions.Get(sessionID); err == nil {
		t.Fatalf("%s still holds a session entry, so it did not stop", sessionID)
	}
}

// record is sessionID's known-session record, which must be there.
func (rig *knownSessionRig) record(t *testing.T, sessionID string) session.KnownSession {
	t.Helper()
	stored, known := rig.known.Get(sessionID)
	if !known {
		t.Fatalf("the known-session store holds no record of %s", sessionID)
	}
	return stored
}

// sendAnswer is one send's answer as these tests read it: the status, the raw body (what an older
// client's keys are asserted on), and the fields the send answers - state and last_seen beside
// recipient and recipient_state.
type sendAnswer struct {
	status int
	raw    string

	EventID        string `json:"event_id"`
	Recipient      string `json:"recipient"`
	RecipientState string `json:"recipient_state"`
	State          string `json:"state"`
	LastSeen       int64  `json:"last_seen"`
	Error          string `json:"error"`
}

// sendRequest is one send of a message to target, as a listener's /v1 routes take it. fields
// overrides or adds to the request's defaults, "connected_only" and "payload" among them.
func sendRequest(t *testing.T, target string, fields map[string]any) *http.Request {
	t.Helper()
	body := map[string]any{
		"target_session": target,
		"source":         "agent",
		"source_session": sendStateSender,
		"message":        "are you there?",
	}
	maps.Copy(body, fields)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return postJSON("/v1/messages/send", string(encoded))
}

// readSendAnswer decodes the answer a send's route wrote.
func readSendAnswer(t *testing.T, recorder *httptest.ResponseRecorder) sendAnswer {
	t.Helper()
	answer := sendAnswer{status: recorder.Code, raw: recorder.Body.String()}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the send answer %s: %v", answer.raw, err)
	}
	return answer
}

// sendThroughRoutes sends one message to target through a listener's own /v1 routes
// (sendRequest).
func sendThroughRoutes(t *testing.T, mux *http.ServeMux, target string, fields map[string]any) sendAnswer {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, sendRequest(t, target, fields))
	return readSendAnswer(t, recorder)
}

func (rig *knownSessionRig) send(t *testing.T, target string, fields map[string]any) sendAnswer {
	t.Helper()
	return sendThroughRoutes(t, rig.mux, target, fields)
}

// sendInBackground sends one message to target through the rig's routes on a goroutine of its
// own, and answers the channel its recorder arrives on once the route has answered.
func (rig *knownSessionRig) sendInBackground(t *testing.T, target string, fields map[string]any) <-chan *httptest.ResponseRecorder {
	t.Helper()
	request := sendRequest(t, target, fields)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, request)
		answered <- recorder
	}()
	return answered
}

// stillClock puts the rig's routes on a fake clock the row never moves (listenerDeps.clock), so
// no bound the routes arm on it - the probe's second, the sender stamp's, the send's budget - can
// end: a send answered on it (sendOnStillClock) is answered by no wait of the route's own. It is
// called before the row's first request.
func (rig *knownSessionRig) stillClock() *clocktest.Fake {
	fake := clocktest.New(time.Now())
	rig.deps.clock = fake
	return fake
}

// sendOnStillClock sends one message to target with the rig on its still clock (stillClock) and
// waits for the answer through the shared wait, so a route that waited on a bound of its own is
// named by that wait rather than read as slow.
func (rig *knownSessionRig) sendOnStillClock(t *testing.T, target string, fields map[string]any) sendAnswer {
	t.Helper()
	return readSendAnswer(t, testwait.Receive(t, "the send to "+target+" to answer with the routes' clock unmoved",
		rig.sendInBackground(t, target, fields)))
}

// requireAnswer fails the test unless answer is a 200 carrying state and recipientState.
func (a sendAnswer) requireAnswer(t *testing.T, what, state string, recipientState session.State) {
	t.Helper()
	if a.status != http.StatusOK || a.State != state || a.RecipientState != string(recipientState) {
		t.Fatalf("%s: status %d, body %s; want 200 state %s recipient_state %s",
			what, a.status, a.raw, state, recipientState)
	}
}

// registryRoute reads GET /v1/registry/<session_id>, with query appended as it stands ("" for no
// selector at all).
func (rig *knownSessionRig) registryRoute(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/registry/"+sessionID+query, nil))
	return recorder
}

// storedOn is how many messages the stream holds on subject.
func (rig *knownSessionRig) storedOn(t *testing.T, subject string) uint64 {
	t.Helper()
	info, err := rig.harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// startSecondListener starts a second listener binary on the server uri names, as machine
// another-machine, and waits until it reports healthy: it holds every subscription a listener
// holds - its role lane, its relay reply inbox, its durable and its wake reports - so a probe that
// any of them answered would read connected for a session nothing of which subscribes its relay
// subject. env is added to its environment.
func startSecondListener(t *testing.T, uri string, env ...string) {
	t.Helper()
	second := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "another-machine", env...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the second listener's output:\n%s", second.Output.String())
		}
	})
	second.waitHealthy(t)
}

// awaitNoRelaySubscriber waits until no subscription on the server uri names holds sessionID's
// relay subject: a probe of its own, carrying the connection probe's header so no plugin would
// read it as a frame, is answered no-responders. A closed client connection leaves its
// subscriptions for the moment the server reads the close, so a test that closed one waits for
// this rather than for a fixed time. Each probe is a read bounded by its own 200 ms, since a
// subscription that lingers answers nothing: the read ends there and the wait asks again.
func awaitNoRelaySubscriber(t *testing.T, uri, sessionID string) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	testwait.Until(t, "nothing to subscribe "+sessionID+"'s relay subject", func() (error, bool) {
		msg := natsgo.NewMsg(bus.DeliverSubject(sessionID))
		msg.Header.Set(bus.ConnectionProbeHeader, "1")
		_, err := conn.RequestMsg(msg, 200*time.Millisecond)
		return err, errors.Is(err, natsgo.ErrNoResponders)
	})
}

// A session whose NATS connection closed with its entry still in place reads stopped, with
// last_seen its entry's and recipient_state its entry's age: the probe wins over age. The routes
// run on a clock the row never moves (stillClock), so the stopped answer is the no-responders one
// and comes with no bound of the route's ended, the probe's second among them.
//
// A second listener runs on the same server throughout, holding every subscription a listener
// holds, and the first send is the negative control: the same route, the same session and the
// same entry, with the plugin still subscribed, reads connected - so the stopped answer below
// can only come from the closed connection. The plugin's connection runs through a proxy, whose
// cut closes it as a network that ends it does.
func TestASendReadsASessionWhoseConnectionClosedAsStopped(t *testing.T) {
	rig := newKnownSessionRig(t)
	rig.stillClock()
	startSecondListener(t, rig.url)

	const checked = "ses_cut_connection"
	proxy := testnats.StartProxy(t, rig.url)
	plugin := startSessionStandInOn(t, testnats.Connect(t, proxy.URL()), checked, answerDone)
	rig.register(t, checked, nil)

	connected := rig.sendOnStillClock(t, checked, nil)
	connected.requireAnswer(t, "a send to a session whose plugin holds its relay subject", sessionStateConnected, session.Live)
	if probed := plugin.probed.Load(); probed != 1 {
		t.Fatalf("the plugin was probed %d times, want once", probed)
	}
	entry := rig.entry(t, checked)

	proxy.Cut()
	awaitNoRelaySubscriber(t, rig.url, checked)
	stopped := rig.sendOnStillClock(t, checked, nil)

	stopped.requireAnswer(t, "a send to a session whose connection closed", sessionStateStopped, session.Live)
	if stopped.LastSeen != entry.UpdatedAt {
		t.Fatalf("the stopped answer's last_seen = %d, want the entry's updated_at %d: %s",
			stopped.LastSeen, entry.UpdatedAt, stopped.raw)
	}
	t.Logf("the send to a session whose connection closed answered stopped with the routes' clock unmoved")
}

// asListenerUser is uri with the credentials listenerGrant's configuration gives the listener's
// NATS user. They are set through net/url rather than written into the string, so no literal of
// the form scheme://user:password@host stands in the tree.
func asListenerUser(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the server's URL %q: %v", uri, err)
	}
	parsed.User = url.UserPassword("listener", "listener")
	return parsed.String()
}

// A listener whose NATS user may not publish the probe answers connected, at once, and logs why.
// The denial is the server's own - the user may publish every subject but the relay subjects - so
// the probe comes back as bus.ErrPublishDenied from internal/bus's own detection and not from an
// error a seam injected. A listener that cannot ask must not call every session of its deployment
// stopped. "At once" is the routes' clock left unmoved (stillClock): the answer ends no bound of
// the route's, the probe's second among them.
//
// The negative control is the plugin subscribed to its relay subject throughout: it is never
// probed, because the publish never leaves this listener, so connected here is the denial's
// answer and not a reply's.
func TestASendAnswersConnectedWhenTheListenerMayNotProbe(t *testing.T) {
	_, uri := testnats.StartConfig(t, listenerGrant(bus.DeliverSubjectPrefix+">"), []string{"-js"}, false)
	uri = asListenerUser(t, uri)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, uri)
	rig.stillClock()
	startSecondListener(t, uri)

	const denied = "ses_denied_probe"
	plugin := startSessionStandIn(t, uri, denied, answerDone)
	rig.register(t, denied, nil)

	answer := rig.sendOnStillClock(t, denied, nil)

	answer.requireAnswer(t, "a send whose probe the server denies", sessionStateConnected, session.Live)
	if probed := plugin.probed.Load(); probed != 0 {
		t.Fatalf("the plugin was probed %d times, so the publish was not denied", probed)
	}
	if logs := rig.harness.logs.String(); !strings.Contains(logs, "listener connection probe did not complete; the session counts as connected") {
		t.Fatalf("the denial was not logged:\n%s", logs)
	}
	t.Logf("the send whose probe was denied answered connected with the routes' clock unmoved")
}

// An entry that records no consumer delivery - one a listener older than consumer delivery wrote,
// here written straight into the bucket - is not probed, and reads connected while it stands.
// Nothing subscribes its relay subject, so a probe would have read it stopped, and the routes run
// on a clock the row never moves (stillClock), so a send that waited on a probe's second would
// not answer at all. The negative control is the same session registered in consumer mode, its
// entry now recording consumer delivery, with still nothing on its relay subject: that one is
// probed, and reads stopped.
func TestASendToAnEntryWithoutConsumerDeliveryIsNotProbed(t *testing.T) {
	rig := newKnownSessionRig(t)
	rig.stillClock()
	startSecondListener(t, rig.url)

	const older = "ses_older_entry"
	storeSessionHeartbeat(t, rig.harness.client, older, time.Now())
	if entry, err := rig.harness.sessions.Refresh(context.Background(), older); err != nil || entry.Delivery != "" {
		t.Fatalf("the entry written straight into the bucket reads %+v (error %v), want one recording no delivery", entry, err)
	}

	answer := rig.sendOnStillClock(t, older, nil)
	answer.requireAnswer(t, "a send to an entry without consumer delivery", sessionStateConnected, session.Live)

	rig.register(t, older, nil)
	if entry := rig.entry(t, older); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("the registration left the entry %+v, want consumer delivery", entry)
	}
	probed := rig.sendOnStillClock(t, older, nil)
	probed.requireAnswer(t, "the same session in consumer mode, nothing on its relay subject", sessionStateStopped, session.Live)
}

// A subscriber on the session's relay subject that never answers reads connected, after the
// probe's second of silence and inside the send's budget. The subscriber heard the probe as the
// probe is sent: the one header Envoy-Probe: 1, none of a relay frame's headers - which would have
// it decode as a frame and move the session's resume point - and no body. The routes and the
// listener's bus client run on one fake the row moves: the send has not answered while the probe's
// second and RequestCore's own bound, both armed on it, are still to run, and answers once the row
// moves the fake through that second alone, short of the send's budget. The probe's context ends
// on the routes' clock and so reports no deadline (a fake's deadline context reports none), and
// RequestCore arms its bound for such a request on the client's clock (bus.WithClock): on the real
// clock that bound, not the row, could end the probe while a slow machine runs the row.
func TestASilentRelaySubscriberReadsConnected(t *testing.T) {
	fake := clocktest.New(time.Now())
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithClock(fake))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url)
	rig.deps.clock = fake
	startSecondListener(t, rig.url)

	const quiet = "ses_silent_subscriber"
	rig.register(t, quiet, nil)
	conn := testnats.Connect(t, rig.url)
	t.Cleanup(conn.Close)
	heard := make(chan *natsgo.Msg, 4)
	var count atomic.Int32
	if _, err := conn.Subscribe(bus.DeliverSubject(quiet), func(msg *natsgo.Msg) {
		count.Add(1)
		heard <- msg
	}); err != nil {
		t.Fatalf("subscribe the silent subscriber: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the silent subscriber: %v", err)
	}

	started := fake.Now()
	probeEnds, budgetEnds := started.Add(connectionProbeTimeout), started.Add(sendRequestBudget)
	// pastBudget is the deadlines pending on the fake and how many of them lie past the send's
	// budget, which no bound of the route's reaches: before the send, the client's own timers from
	// its connect (its keeper's period, its refresh of the stream's subjects); while the probe
	// waits, RequestCore's own bound besides.
	pastBudget := func() ([]time.Time, int) {
		armed, past := fake.Armed(), 0
		for _, deadline := range armed {
			if deadline.After(budgetEnds) {
				past++
			}
		}
		return armed, past
	}
	_, standing := pastBudget()
	answered := rig.sendInBackground(t, quiet, nil)
	msg := testwait.Receive(t, "the silent subscriber to hear the send's probe", heard)
	awaitArmedBeforeReturn(t, fake, "the probe's second, and RequestCore's own bound past the send's budget, to be armed on the fake", func(armed []time.Time) bool {
		_, past := pastBudget()
		return slices.ContainsFunc(armed, probeEnds.Equal) && past > standing
	}, answerReturned(answered))
	select {
	case recorder := <-answered:
		t.Fatalf("the send answered %s while the probe's second was still to run", recorder.Body.String())
	default:
	}
	fake.Advance(connectionProbeTimeout)
	answer := readSendAnswer(t, testwait.Receive(t, "the send to answer once the probe's second has run", answered))

	answer.requireAnswer(t, "a send to a session whose relay subscriber never answers", sessionStateConnected, session.Live)
	if connectionProbeTimeout >= sendRequestBudget {
		t.Fatalf("the probe's %s is not inside the send's %s budget, so this row cannot tell an answer at the probe's end from one at the budget's", connectionProbeTimeout, sendRequestBudget)
	}
	if got := msg.Header.Get(bus.ConnectionProbeHeader); got != "1" {
		t.Fatalf("the probe carried %s: %q, want 1", bus.ConnectionProbeHeader, got)
	}
	if len(msg.Header) != 1 {
		t.Fatalf("the probe carried the headers %v, want %s alone", msg.Header, bus.ConnectionProbeHeader)
	}
	for _, frameHeader := range []string{relayHeaderSubject, relayHeaderStreamSeq, relayHeaderStreamID, relayHeaderDelivered, relayHeaderStoredAt} {
		if value := msg.Header.Get(frameHeader); value != "" {
			t.Fatalf("the probe carried the relay frame header %s: %q", frameHeader, value)
		}
	}
	if len(msg.Data) != 0 {
		t.Fatalf("the probe carried a body of %d bytes, want none", len(msg.Data))
	}
	if msg.Reply == "" {
		t.Fatal("the probe carried no reply subject, so nothing could answer it")
	}
	if heardCount := count.Load(); heardCount != 1 {
		t.Fatalf("the silent subscriber heard %d messages, want the one probe", heardCount)
	}
	t.Logf("the send to a silent relay subscriber answered connected once the probe's second had run")
}

// A send to a session neither store knows is the 404 naming the id: Dispatch classifies a
// delivery failure on that prefix. A known-session record older than the 30-day retention is no
// record at all, so a session the store has aged out is that same 404 and not a stopped answer
// with a month-old last_seen.
func TestASendToASessionNeitherStoreKnowsIsTheNotFound(t *testing.T) {
	rig := newKnownSessionRig(t)

	unknown := rig.send(t, "ses_never_registered", nil)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("a send to a session neither store knows: status %d, body %s; want 404", unknown.status, unknown.raw)
	}
	if !strings.HasPrefix(unknown.Error, "no live session ses_never_registered") {
		t.Fatalf("the 404 reads %q, want it to open with the session it names", unknown.Error)
	}

	// The negative control for the aged-out case: while the record is young the same send is the
	// 200 that proves the rig can answer stopped at all.
	const aged = "ses_aged_out"
	rig.register(t, aged, nil)
	rig.stop(t, aged)
	young := rig.send(t, aged, nil)
	young.requireAnswer(t, "a send to a session with a fresh record", sessionStateStopped, session.Gone)

	rig.clock = rig.clock.Add(session.KnownSessionRetention + time.Hour)
	if _, known := rig.known.Get(aged); known {
		t.Fatalf("the store still answers a record of %s past its retention, so the route's answer shows nothing", aged)
	}
	old := rig.send(t, aged, nil)
	if old.status != http.StatusNotFound {
		t.Fatalf("a send to a session whose record aged out: status %d, body %s; want 404", old.status, old.raw)
	}
	if !strings.HasPrefix(old.Error, "no live session "+aged) {
		t.Fatalf("the 404 reads %q, want it to open with the session it names", old.Error)
	}
}

// A send to a session with no entry is judged by the capabilities its known-session record
// carries, which are the ones that session last registered with: a mode the record advertises is
// stored, one it does not is the 403 this route answers a live session that will not take the
// mode. A record carrying no capabilities at all - a plugin that declared none - is not a session
// that advertises nothing, so its send is accepted and the mode is left to the session to judge
// when it runs again.
func TestASendToAStoppedSessionIsJudgedByItsRecordsCapabilities(t *testing.T) {
	rig := newKnownSessionRig(t)
	const steerFrame = `{"event":{"id":0,"issue_key":"CORE-1"},"delivery":{"attempt":1,"mode":"steer"}}`

	for _, each := range []struct {
		name         string
		sessionID    string
		capabilities []string
		wantStatus   int
		wantError    string
	}{
		{"the record advertises the mode", "ses_record_steers", []string{"aside", "steer"}, http.StatusOK, ""},
		{"the record advertises another mode", "ses_record_btw", []string{"btw"}, http.StatusForbidden,
			"session ses_record_btw (a checked session) does not advertise steer"},
		{"the record carries no capabilities", "ses_record_silent", nil, http.StatusOK, ""},
	} {
		t.Run(each.name, func(t *testing.T) {
			fields := map[string]any{}
			if each.capabilities != nil {
				fields["capabilities"] = each.capabilities
			}
			rig.register(t, each.sessionID, fields)
			rig.stop(t, each.sessionID)

			answer := rig.send(t, each.sessionID, map[string]any{"payload": steerFrame})

			if answer.status != each.wantStatus {
				t.Fatalf("a steer send to %s: status %d, body %s; want %d",
					each.sessionID, answer.status, answer.raw, each.wantStatus)
			}
			if each.wantError != "" && answer.Error != each.wantError {
				t.Fatalf("the refusal reads %q, want %q", answer.Error, each.wantError)
			}
			if each.wantStatus == http.StatusOK && answer.State != sessionStateStopped {
				t.Fatalf("the accepted send answers state %q, want stopped: %s", answer.State, answer.raw)
			}
		})
	}
}

// connected_only is the flag Dispatch sets on every send of a model or thinking switch, and of a
// message whose agents-route request set it: applied at an unknown later time such a switch is
// worse than none, so a stopped target is answered 409 and nothing is stored for it, whichever way
// it is stopped - its entry stands and nothing holds its relay subject, or it stopped cleanly and
// only its record is left. A connected target is the ordinary 200, and a send with the flag absent
// stores for a stopped target.
func TestConnectedOnlyRefusesAStoppedTargetAndStoresNothing(t *testing.T) {
	rig := newKnownSessionRig(t)

	const cleanlyStopped = "ses_switch_stopped"
	rig.register(t, cleanlyStopped, nil)
	rig.stop(t, cleanlyStopped)
	const unheld = "ses_switch_unheld"
	rig.register(t, unheld, nil)

	for _, target := range []string{cleanlyStopped, unheld} {
		subject := contracts.LegacyAgentSubject(target)
		if held := rig.storedOn(t, subject); held != 0 {
			t.Fatalf("the stream already holds %d messages on %s", held, subject)
		}

		refused := rig.send(t, target, map[string]any{"connected_only": true})

		if refused.status != http.StatusConflict {
			t.Fatalf("a connected_only send to the stopped %s: status %d, body %s; want 409", target, refused.status, refused.raw)
		}
		if refused.raw != `{"error":"the session is stopped"}`+"\n" {
			t.Fatalf("the 409 reads %q, want {\"error\":\"the session is stopped\"}", refused.raw)
		}
		if held := rig.storedOn(t, subject); held != 0 {
			t.Fatalf("the refused send stored %d messages on %s, want none", held, subject)
		}
		// The negative control: the same session, the same route, the flag left off. It stores,
		// so the 409 above is the flag's doing and not the rig's.
		if stored := rig.send(t, target, nil); stored.status != http.StatusOK || stored.State != sessionStateStopped {
			t.Fatalf("the same send to %s without the flag: status %d, body %s; want 200 stopped", target, stored.status, stored.raw)
		}
		if held := rig.storedOn(t, subject); held != 1 {
			t.Fatalf("the stream holds %d messages on %s after the send without the flag, want 1", held, subject)
		}
	}

	const live = "ses_switch_connected"
	plugin := startSessionStandIn(t, rig.url, live, answerDone)
	rig.register(t, live, nil)
	answer := rig.send(t, live, map[string]any{"connected_only": true})
	answer.requireAnswer(t, "a connected_only send to a connected session", sessionStateConnected, session.Live)
	if probed := plugin.probed.Load(); probed != 1 {
		t.Fatalf("the plugin was probed %d times, want once", probed)
	}
}

// awaitWake waits on wakes for the wake report of kind naming name (wakeNamed), and answers the
// reports it passed over on the way, in the order they arrived. It fails the test when none
// arrives before the test binary's deadline less testwait's margin.
func awaitWake(t *testing.T, wakes *natsgo.Subscription, kind, name string) []wakeReport {
	t.Helper()
	ctx := testwait.Context(t)
	var passed []wakeReport
	for {
		msg, err := wakes.NextMsgWithContext(ctx)
		if err != nil {
			t.Fatalf("no %s wake report named %s (passed over %+v): %v", kind, name, passed, err)
		}
		report := wakeReportIn(t, msg)
		if wakeNamed(report, kind, name) {
			return passed
		}
		passed = append(passed, report)
	}
}

// wakeReportIn is the wake report msg carries.
func wakeReportIn(t *testing.T, msg *natsgo.Msg) wakeReport {
	t.Helper()
	var item contracts.Envelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		t.Fatalf("decode a wake report: %v", err)
	}
	var report wakeReport
	if err := json.Unmarshal([]byte(item.Payload), &report); err != nil {
		t.Fatalf("decode a wake report's payload %s: %v", item.Payload, err)
	}
	return report
}

// wakeNamed reports whether report is of kind and names name in the field that kind names: the
// session of an agent wake, and the role of a role wake. A report of any other kind names neither.
func wakeNamed(report wakeReport, kind, name string) bool {
	if report.Kind != kind {
		return false
	}
	switch kind {
	case wakeKindAgent:
		return report.Session == name
	case wakeKindRole:
		return report.Role == name
	}
	return false
}

// A send to a session with no entry within the gone horizon raises the wake report, on the 200
// stopped answer as on a 404, since a released Legion pod's sessions leave the session bucket with
// the pod and this send is the only sight of the message meant for them. Both ways of having no
// such entry are covered - a clean stop, and an entry past the gone horizon - and both answer
// state stopped, recipient_state gone and the record's last_seen. A session whose entry stands
// and whose probe met no responders raises none, as no lingering entry does, and answers stopped
// with its entry's age and last_seen.
func TestASendRaisesTheWakeWhereTheEntryIsGoneAndNotForAProbeStoppedEntry(t *testing.T) {
	rig := newKnownSessionRig(t)
	wakesCtx, stopWakes := context.WithCancel(context.Background())
	t.Cleanup(stopWakes)
	rig.deps.wakes = newWakeReporter(wakesCtx, rig.harness.client)
	conn := testnats.Connect(t, rig.url)
	t.Cleanup(conn.Close)
	wakes, err := conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", bus.LegionWakeTopic, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the wake subscription: %v", err)
	}

	const unheld = "ses_wake_probe_stopped"
	rig.register(t, unheld, nil)
	entry := rig.entry(t, unheld)
	probeStopped := rig.send(t, unheld, nil)
	probeStopped.requireAnswer(t, "a send to an entry nothing holds the relay subject of", sessionStateStopped, session.Live)
	if probeStopped.LastSeen != entry.UpdatedAt {
		t.Fatalf("the probe-stopped answer's last_seen = %d, want the entry's %d: %s", probeStopped.LastSeen, entry.UpdatedAt, probeStopped.raw)
	}

	const cleanlyStopped = "ses_wake_clean_stop"
	rig.register(t, cleanlyStopped, nil)
	rig.clock = rig.clock.Add(time.Minute)
	rig.stop(t, cleanlyStopped)
	record := rig.record(t, cleanlyStopped)
	stopped := rig.send(t, cleanlyStopped, nil)
	stopped.requireAnswer(t, "a send to a session that stopped cleanly", sessionStateStopped, session.Gone)
	if stopped.LastSeen != record.LastSeen {
		t.Fatalf("the stopped answer's last_seen = %d, want the record's %d: %s", stopped.LastSeen, record.LastSeen, stopped.raw)
	}
	passed := awaitWake(t, wakes, wakeKindAgent, cleanlyStopped)

	const pastHorizon = "ses_wake_past_horizon"
	storeSessionHeartbeat(t, rig.harness.client, pastHorizon, time.Now().Add(-(session.GoneHorizon + time.Minute)))
	if err := rig.known.RecordWithin(context.Background(), session.KnownSession{SessionID: pastHorizon, Title: "a session past its horizon"}); err != nil {
		t.Fatalf("record %s: %v", pastHorizon, err)
	}
	horizonRecord := rig.record(t, pastHorizon)
	gone := rig.send(t, pastHorizon, nil)
	gone.requireAnswer(t, "a send to an entry past the gone horizon with a record", sessionStateStopped, session.Gone)
	if gone.LastSeen != horizonRecord.LastSeen {
		t.Fatalf("the past-horizon answer's last_seen = %d, want the record's %d: %s", gone.LastSeen, horizonRecord.LastSeen, gone.raw)
	}
	passed = append(passed, awaitWake(t, wakes, wakeKindAgent, pastHorizon)...)

	// Each wake report is published off the route's path in the order the sends raised them, so a
	// report the first send raised would have arrived before the two the later sends raised, and
	// been passed over on the way to them.
	if slices.ContainsFunc(passed, func(report wakeReport) bool { return wakeNamed(report, wakeKindAgent, unheld) }) {
		t.Fatalf("the send to the probe-stopped %s raised a wake report: the waits passed over %+v", unheld, passed)
	}
}

// A send to a session that stopped cleanly answers from the caches it already holds and the bucket
// read a send to a missing entry makes. Over warm caches 100 of them send no JetStream API request
// on the consumer or stream-info subjects, and the probe of the API shows it is watching by seeing
// the one call the test makes itself (watchJetStreamAPI, which
// TestAHundredPublishesSendNoJetStreamAPIRequest uses too).
//
// The four bucket watchers the rig opens on the listener's connection - the interest registry's,
// the role claims', the session registry's and the known-session store's - each read their
// bucket's stream every 2 s (kvwatch's stream probe) on that same connection, so on time.AfterFunc
// a run of sends that spanned one of those moments would count their reads as the route's. Their
// timers run on a fake the row never moves, and so does the clock they measure a confirmation of
// their stream by, so the gate on a record delivered while the row counts finds the confirmation
// fresh and sends no read of its own either. The listener's bus client runs on the same fake
// (bus.WithClock): its keeper's period and its refresh of the stream's subjects would each read the
// notification stream on that connection, the request the row's own control call makes, and one
// that came mid-window would end the count before the sends after it.
func TestAHundredSendsToAStoppedSessionSendNoJetStreamAPIRequest(t *testing.T) {
	held := clocktest.New(time.Now())
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithClock(held))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url,
		kvwatch.WithAfterFunc(func(d time.Duration, f func()) kvwatch.Timer { return held.AfterFunc(d, f) }),
		kvwatch.WithNow(held.Now))
	const gone = "ses_stopped_api_free"
	rig.register(t, gone, nil)
	rig.stop(t, gone)
	rig.awaitNoConsumer(t, gone)
	// One send before the watch starts, so anything a first send would set up is already set up
	// and the window below holds the steady state.
	if warm := rig.send(t, gone, nil); warm.status != http.StatusOK || warm.State != sessionStateStopped {
		t.Fatalf("the warming send: status %d, body %s; want 200 stopped", warm.status, warm.raw)
	}

	watch := watchJetStreamAPI(t, rig.url, rig.harness.client.Conn, "$JS.API.CONSUMER.>", "$JS.API.STREAM.INFO.>")
	for i := range 100 {
		answer := rig.send(t, gone, map[string]any{"idempotency_key": "stopped-api-free-" + strconv.Itoa(i)})
		if answer.status != http.StatusOK || answer.State != sessionStateStopped {
			t.Fatalf("send %d: status %d, body %s; want 200 stopped", i, answer.status, answer.raw)
		}
	}

	control := "$JS.API.STREAM.INFO." + bus.Stream
	if _, err := rig.harness.client.JS().StreamInfo(bus.Stream); err != nil {
		t.Fatalf("the watch's control call: %v", err)
	}
	if calls := watch.awaitCall(t, control); len(calls) > 0 {
		t.Fatalf("100 sends sent %d JetStream API requests: %v", len(calls), calls)
	}
}

// GET /v1/registry/<session_id> answers a stopped session only when its include list asks for
// one. Without stopped in the list the route answers the 404 for a session that stopped cleanly,
// and a live session's ordinary body, which is what the Agents page, issue_claim.go's holder
// liveness and every other reader of it read. With it, a stopped session is 200 with the record's
// own fields and no port and no host at all, a live session is its ordinary body, and a session
// neither store knows is still the 404.
func TestTheRegistryRouteAnswersAStoppedSessionOnlyWhenAsked(t *testing.T) {
	rig := newKnownSessionRig(t)
	const gone = "ses_registry_stopped"
	rig.register(t, gone, map[string]any{
		"title":        "the stopped one",
		"capabilities": []string{"aside", "btw"},
		"topics":       []string{"notifications.team.registry-state"},
	})
	stored := rig.record(t, gone)
	rig.stop(t, gone)

	for _, query := range []string{"", "?include=unresponsive"} {
		if plain := rig.registryRoute(t, gone, query); plain.Code != http.StatusNotFound {
			t.Fatalf("GET /v1/registry/%s%s: status %d, body %s; want 404", gone, query, plain.Code, plain.Body.String())
		}
	}

	for _, query := range []string{"?include=unresponsive,stopped", "?include=stopped"} {
		t.Run(query, func(t *testing.T) {
			answer := rig.registryRoute(t, gone, query)
			if answer.Code != http.StatusOK {
				t.Fatalf("GET /v1/registry/%s%s: status %d, body %s; want 200", gone, query, answer.Code, answer.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(answer.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode %s: %v", answer.Body.String(), err)
			}
			for _, key := range []string{"port", "host"} {
				if value, carried := raw[key]; carried {
					t.Fatalf("the stopped answer carries %s: %s; want the key left out entirely: %s",
						key, value, answer.Body.String())
				}
			}
			var info stoppedSessionInfo
			if err := json.Unmarshal(answer.Body.Bytes(), &info); err != nil {
				t.Fatalf("decode %s: %v", answer.Body.String(), err)
			}
			want := stoppedSessionInfo{
				SessionID:    gone,
				State:        sessionStateStopped,
				LastSeen:     stored.LastSeen,
				Title:        "the stopped one",
				Capabilities: []string{"aside", "btw"},
				Topics:       stored.Topics,
			}
			if !reflect.DeepEqual(info, want) {
				t.Fatalf("the stopped answer is %+v, want %+v: %s", info, want, answer.Body.String())
			}
		})
	}

	const live = "ses_registry_live"
	rig.register(t, live, nil)
	plainLive := rig.registryRoute(t, live, "?include=unresponsive")
	if plainLive.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/%s?include=unresponsive: status %d, body %s; want 200", live, plainLive.Code, plainLive.Body.String())
	}
	askedLive := rig.registryRoute(t, live, "?include=unresponsive,stopped")
	if askedLive.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/%s?include=unresponsive,stopped: status %d, body %s; want 200", live, askedLive.Code, askedLive.Body.String())
	}
	var plainInfo, liveInfo sessionInfo
	if err := json.Unmarshal(plainLive.Body.Bytes(), &plainInfo); err != nil {
		t.Fatalf("decode %s: %v", plainLive.Body.String(), err)
	}
	if err := json.Unmarshal(askedLive.Body.Bytes(), &liveInfo); err != nil {
		t.Fatalf("decode %s: %v", askedLive.Body.String(), err)
	}
	if liveInfo.State != session.Live || !reflect.DeepEqual(plainInfo, liveInfo) {
		t.Fatalf("a live session asked with stopped in the list answers %+v, want its ordinary live body %+v",
			liveInfo, plainInfo)
	}

	unknown := rig.registryRoute(t, "ses_registry_unknown", "?include=unresponsive,stopped")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/registry/ses_registry_unknown?include=unresponsive,stopped: status %d, body %s; want 404",
			unknown.Code, unknown.Body.String())
	}
}
