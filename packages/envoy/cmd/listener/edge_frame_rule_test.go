//go:build natslive

package main

// edge_frame_rule_test.go holds the edge's per-frame rule (edge_frames.go) to what an outside
// session receives through its relay socket: a frame reaches the socket only for a project its
// live token lists, as the session's own message, or as a GitHub event of one of its projects'
// repositories, whatever a stored record, a role claim or the session's consumer says. Each
// attack row first shows the channel it uses is live (a frame of the token's own project sent the
// same way arrives), then that the other project's frame does not, and that it was counted.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// scopeProject is the project the scope rig's edge serves and its sessions' tokens list, and
// scopeOtherProject the project no token of the rig lists.
const (
	scopeProject      = "PRIV"
	scopeOtherProject = "PUB"
)

// scopeRig is one edge replica as a deployment runs it: its routes behind the broker-token
// credential (edgeRig, built here with the edge relay main starts in edge mode, whose receipts
// are under bus.RelayReplyPrefix), and the same replica's relay socket route, so a session's
// frames reach the socket the row reads.
type scopeRig struct {
	*edgeRig
	socketServer *httptest.Server
}

func newScopeRig(t *testing.T) *scopeRig {
	t.Helper()
	rig, _ := newScopeRigSocketReader(t, false)
	return rig
}

// newScopeRigSocketReader is newScopeRig whose socket route, when cold is set, judges GitHub
// frames by a repository reader of its own that has read no answer yet: the other replica of a
// deployment, before its first read. It returns that reader, the relay's own when cold is unset.
func newScopeRigSocketReader(t *testing.T, cold bool) (*scopeRig, *edgeRepositories) {
	t.Helper()
	projects := []string{scopeProject}
	met := metrics.New()
	grant, err := newEdgeGrant("edge-a", projects, met)
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	rig := newRelayRigUnownedAs(t, relayPauseFirst, relayPauseMax, grant)
	withPublishRoute(t, rig.deps)
	stand := newRepositoryStandIn(t, []string{scopeProject}, edgeRepositoryFixture())
	clock := &edgeClock{}
	logger := rig.harness.config.logger
	reader := newEdgeRepositories(stand.server.URL, listenerBrokerAccount(edgeAccountToken), projects, newEdgeWithheldCounter(met), logger)
	reader.now = clock.now
	edge := &edgeRig{relayRig: rig, projects: projects, met: met, repositories: stand, reader: reader, clock: clock, ticks: make(chan time.Time)}
	rig.deps.repositories = reader
	rig.relays.repositories = reader
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEdgeRepositories(ctx, rig.deps, "test-machine", edge.ticks)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	rig.relays.setOwned(true)
	edge.broker = ciphertokentest.NewStubBroker(t)
	edge.caller = ciphertokentest.NewCaller(t)
	edge.handler = apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, edge.broker), edgeProjects: projects, logger: logger}, rig.mux)

	socketReader := reader
	if cold {
		socketReader = newEdgeRepositories(stand.server.URL, listenerBrokerAccount(edgeAccountToken), projects, newEdgeWithheldCounter(metrics.New()), logger)
		socketReader.now = clock.now
	}
	tokens := listenerBrokerTokens(t, edge.broker)
	sockets := newRelaySockets(rig.harness.client, tokens, logger)
	sockets.repositories = socketReader
	routes := http.NewServeMux()
	registerV1Routes(routes, &listenerDeps{client: rig.harness.client, relaySockets: sockets, repositories: socketReader}, "test-machine", logger)
	server := httptest.NewServer(apiAuth(listenerAPIAuth{brokers: tokens, edgeProjects: projects, logger: logger}, routes))
	t.Cleanup(server.Close)
	t.Cleanup(sockets.stop)
	// One whole pass of the repository refresh, so a GitHub frame of the project's repository
	// meets a fresh answer.
	edge.tick(t)
	return &scopeRig{edgeRig: edge, socketServer: server}, socketReader
}

// openSocket opens session's relay socket with token, subscribes its relay subject and returns
// once the replica holds that subscription (the pong after the sub).
func (r *scopeRig) openSocket(t *testing.T, session, token string) *relaySocketClient {
	t.Helper()
	signed := r.caller.Signed(t, http.MethodGet, listenerBrokerOrigin+relaySocketPath(session), token)
	header := http.Header{"Authorization": {signed.Header.Get("Authorization")}, "DPoP": {signed.Header.Get("DPoP")}}
	dialer := websocket.Dialer{Subprotocols: []string{relaySubprotocol}, HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(r.socketServer.URL, "http")+relaySocketPath(session), header)
	if err != nil {
		t.Fatalf("open the relay socket of %s: %v (%s)", session, err, describeUpgrade(response))
	}
	client := startRelaySocketClient(t, conn)
	client.subscribeOwn(session)
	return client
}

// scopeFrame is one msg frame a socket received: the stored subject its relay header names and
// the envelope it carries.
type scopeFrame struct {
	subject string
	item    contracts.Envelope
	raw     string
}

// takeScopeFrames reads the socket's next n msg frames, answering each received and done as a
// session that wrote it does, and fails the test on any other frame or a close. It waits for each
// through testwait (relaySocketClient.frame), with no bound of its own.
func takeScopeFrames(t *testing.T, client *relaySocketClient, n int) []scopeFrame {
	t.Helper()
	frames := make([]scopeFrame, 0, n)
	for len(frames) < n {
		frame := client.frame(fmt.Sprintf("frame %d of %d from the socket, after %v", len(frames)+1, n, scopeFrameTopics(frames)))
		if frame.Op != "msg" {
			t.Fatalf("the socket sent %s, want a msg frame", frame.raw)
		}
		read := scopeFrame{raw: frame.raw}
		if subjects := frame.Headers[relayHeaderSubject]; len(subjects) > 0 {
			read.subject = subjects[0]
		}
		_ = json.Unmarshal(frame.data(t), &read.item)
		if strings.HasPrefix(frame.Reply, bus.RelayReplyPrefix) {
			client.pub(frame.Reply, relayReplyReceived)
			client.pub(frame.Reply, relayReplyDone)
		}
		frames = append(frames, read)
	}
	return frames
}

func scopeFrameTopics(frames []scopeFrame) []string {
	topics := make([]string, len(frames))
	for i, frame := range frames {
		topics[i] = frame.item.Topic + " (" + frame.item.EventID + ")"
	}
	return topics
}

// storeScopeEvent stores a new envelope on subject, its payload payload when not empty, and
// returns it.
func storeScopeEvent(t *testing.T, rig *scopeRig, subject, payload string) contracts.Envelope {
	t.Helper()
	item := listenerTestEnvelope(subject, "edge-scope-"+id.New())
	if payload != "" {
		item.Source = "github"
		item.Payload = payload
	}
	if _, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a message on %s: %v", subject, err)
	}
	return item
}

// registerScopeSession registers session at the edge under a token of scopeProject, following
// topics, and opens its relay socket first, so every frame its relay sends finds the socket.
func registerScopeSession(t *testing.T, rig *scopeRig, session string, topics ...string) (*relaySocketClient, string) {
	t.Helper()
	token := rig.personToken(session, scopeProject)
	client := rig.openSocket(t, session, token)
	requireStatus(t, rig.subscribe(t, token, session, topics...), http.StatusOK, "the registration of "+session)
	return client, token
}

// publishScopeFrame publishes a frame on session's relay subject as a NATS user that is no relay
// would: under a receipt subject of the relay family, naming subject as the stored subject, and
// carrying an envelope of that subject.
func publishScopeFrame(t *testing.T, conn *natsgo.Conn, session, subject string) contracts.Envelope {
	t.Helper()
	item := listenerTestEnvelope(subject, "edge-scope-forged-"+id.New())
	header := natsgo.Header{}
	header.Set(relayHeaderSubject, subject)
	header.Set(relayHeaderStreamSeq, "1")
	msg := &natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: relayFrameReceipt(session), Header: header, Data: marshalListenerEnvelope(t, item)}
	if err := conn.PublishMsg(msg); err != nil {
		t.Fatalf("publish a frame on %s: %v", msg.Subject, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the frame on %s: %v", msg.Subject, err)
	}
	return item
}

// relayWatch is session's relay subject as a session holding a NATS connection of its own reads it
// (watchRelay): every message the server routes there, in the order it routes them, so a row sees
// what the relay itself published apart from what the socket, which judges each frame again,
// forwards.
type relayWatch struct {
	session string
	msgs    chan *natsgo.Msg
}

// watchRelay subscribes session's relay subject directly, on a connection of the row's own, and
// returns once the server holds the subscription.
func watchRelay(t *testing.T, rig *scopeRig, session string) *relayWatch {
	t.Helper()
	conn := testnats.Connect(t, rig.url)
	t.Cleanup(conn.Close)
	watch := &relayWatch{session: session, msgs: make(chan *natsgo.Msg, 64)}
	if _, err := conn.ChanSubscribe(bus.DeliverSubject(session), watch.msgs); err != nil {
		t.Fatalf("watch %s: %v", bus.DeliverSubject(session), err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the watch of %s: %v", bus.DeliverSubject(session), err)
	}
	return watch
}

// watchedFrame is one relay frame the watch read: its envelope's topic and event id, or its body as
// its topic when the body names no topic.
type watchedFrame struct{ topic, eventID string }

// next waits for the next relay frame the watch received, passing over a connection probe, which
// is no frame, and a message under no relay receipt. what names what the row waits for. It waits
// through testwait, with no bound of its own.
func (w *relayWatch) next(t *testing.T, what string) watchedFrame {
	t.Helper()
	for {
		msg := testwait.Receive(t, what, w.msgs)
		if _, probe := msg.Header[bus.ConnectionProbeHeader]; probe || !strings.HasPrefix(msg.Reply, bus.RelayReplyPrefix) {
			continue
		}
		topic, named, _ := exactFrameTopic(msg.Data)
		if !named {
			topic = string(msg.Data)
		}
		var item contracts.Envelope
		_ = json.Unmarshal(msg.Data, &item)
		return watchedFrame{topic: topic, eventID: item.EventID}
	}
}

// requireNext fails the test unless the relay's next frame on the watch carries want: the envelope
// of want's event id, or, for a want that names none, of want's topic. why says what a frame of
// another envelope there shows.
func (w *relayWatch) requireNext(t *testing.T, want contracts.Envelope, why string) {
	t.Helper()
	subject := bus.DeliverSubject(w.session)
	got := w.next(t, fmt.Sprintf("the relay's next publish on %s, %s (%s)", subject, want.Topic, want.EventID))
	if got.topic != want.Topic || (want.EventID != "" && got.eventID != want.EventID) {
		t.Fatalf("the relay's next publish on %s is %s (%s), want %s (%s): %s", subject, got.topic, got.eventID, want.Topic, want.EventID, why)
	}
}

// awaitProbe waits for the next connection probe the watch received, passing over every frame
// before it. It waits through testwait, with no bound of its own.
func (w *relayWatch) awaitProbe(t *testing.T, what string) {
	t.Helper()
	for {
		msg := testwait.Receive(t, what, w.msgs)
		if _, probe := msg.Header[bus.ConnectionProbeHeader]; probe {
			return
		}
	}
}

// requireWithheld fails the test unless the edge counted want frames withheld for a project its
// token does not list.
func requireWithheld(t *testing.T, rig *scopeRig, want int64) {
	t.Helper()
	if got := rig.withheld(t, "project_not_held"); got != want {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="project_not_held"} = %d, want %d`, got, want)
	}
}

// requireNextScopeFrame reads the socket's next msg frame (takeScopeFrames) and fails the test
// unless it carries want, the marker a row stored or published after the frame after names, by its
// event id: one marker frame proves that nothing reached the socket between the two, since the
// socket is sent its frames in the order they were stored or published.
func requireNextScopeFrame(t *testing.T, client *relaySocketClient, want contracts.Envelope, after string) {
	t.Helper()
	if frame := takeScopeFrames(t, client, 1)[0]; frame.item.EventID != want.EventID {
		t.Fatalf("the socket's frame after %s is %s, want the marker %s, stored or published after it: a frame reached the socket after %s", after, frame.raw, want.EventID, after)
	}
}

// The edge relays a session only the frames of the projects its live token lists. Its own
// traffic arrives: a send in each agent-subject form the edge accepts, a role message of its
// project forwarded to it as the holder, an event of its project, and a GitHub event of its
// project's repository. Three paths that put another project's frame in front of the session are
// each closed at the frame: a frame another NATS user publishes on the session's relay subject, a
// role claim naming the session as the holder of another project's role, and another project's
// subject added to the session's own consumer.
func TestAnEdgeRelaysOnlyItsTokensProjects(t *testing.T) {
	t.Parallel()
	const ownIssue = "notifications.dispatch.issue." + scopeProject + ".PRIV-1.comment.created"
	const otherIssue = "notifications.dispatch.issue." + scopeOtherProject + ".PUB-1.comment.created"

	t.Run("its own traffic arrives", func(t *testing.T) {
		rig := newScopeRig(t)
		const session, peer = "ses_scope_own", "ses_scope_peer"
		newForm := contracts.AgentSubject(scopeProject, session)
		client, token := registerScopeSession(t, rig, session,
			"notifications.github.acme.pub.>", newForm, "notifications.dispatch.issue."+scopeProject+".>")
		peerToken := rig.namedToken(peer, scopeProject)
		requireStatus(t, rig.subscribe(t, peerToken, peer), http.StatusOK, "the peer's registration")
		const role = "legion-priv-priv-1-tester"
		requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, session, role)),
			http.StatusOK, "the claim of "+role)

		// The send probes the session's relay subject (probeSessionConnection), which the
		// socket's replica subscribes and neither forwards nor answers, so the probe ends only at
		// its bound (connectionProbeTimeout). The routes run on a fake clock (listenerDeps.clock)
		// from the send on, set once the registrations and the claim are answered, so the send
		// alone runs on it: its budget, its arrival plus sendRequestBudget, and the probe's bound
		// are armed there. A fake's context reports its parent's deadline and none of its own, so
		// the send's request carries a context that reports the test binary's deadline
		// (testnats.Context), and no call under the send arms a wait of its own on the wall clock
		// for want of one: nothing but the fake's bound or the binary's end can end the probe. The
		// row moves the clock to the probe's bound once the bound is armed and the probe is on the
		// relay subject (a watch of the row's own sees it there), and no further: the probe ends
		// at its second while the send's budget, a timer still pending on the fake, has not ended
		// the send's context, and the send that stores the message under that context is answered
		// 200. A probe bound left on the wall clock fails the row at the wait for it to be armed,
		// naming it.
		routes := clocktest.New(time.Now())
		rig.deps.clock = routes
		arrived := routes.Now()
		probes := watchRelay(t, rig, session)
		request := brokerTokenRequest(t, rig.caller, http.MethodPost, "/v1/messages/send", peerToken,
			fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from a peer of the project","source":"agent"}`, session, peer)).
			WithContext(testnats.Context(t))
		answers := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			rig.handler.ServeHTTP(recorder, request)
			answers <- recorder
		}()
		armed := routes.AwaitArmedAt(t, "the send's probe of "+session+" to arm its bound, connectionProbeTimeout past the send's arrival on the routes' clock",
			arrived.Add(connectionProbeTimeout))
		if budget := arrived.Add(sendRequestBudget); !slices.Contains(armed, budget) {
			t.Fatalf("the routes' clock holds %v armed while the send probes, want the send's budget, its arrival plus sendRequestBudget (%s), among them", armed, budget)
		}
		probes.awaitProbe(t, "the send's probe on "+bus.DeliverSubject(session))
		select {
		case recorder := <-answers:
			t.Fatalf("the send answered %d %s while its probe was on the relay subject and short of its bound, want it held by the probe", recorder.Code, recorder.Body.String())
		default:
		}
		routes.Advance(connectionProbeTimeout)
		sent := testwait.Receive(t, "the send's answer once its probe's bound passed, its budget still running", answers)
		requireStatus(t, sent, http.StatusOK, "the peer's send, its probe ended at its bound with the send's budget still running")
		var send listenerwire.SendResponse
		if err := json.Unmarshal(sent.Body.Bytes(), &send); err != nil {
			t.Fatalf("decode the send's answer %s: %v", sent.Body.String(), err)
		}
		if send.State == sessionStateStopped {
			t.Fatalf("the send answered state %s: a probe that met only silence read the session stopped", send.State)
		}
		roleSubject := contracts.RoleSubject(scopeProject, role)
		published := rig.call(t, http.MethodPost, peerToken, "/v1/messages/publish",
			fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":%q}`, roleSubject, peer))
		if published.Code != http.StatusOK && published.Code != http.StatusAccepted {
			t.Fatalf("the publish to %s: status %d (%s), want it taken", roleSubject, published.Code, published.Body.String())
		}
		direct := storeScopeEvent(t, rig, newForm, "")
		issue := storeScopeEvent(t, rig, ownIssue, "")
		github := storeScopeEvent(t, rig, "notifications.github.acme.pub.pr.1", `{"kind":"pr","action":"opened","repo":"acme/pub"}`)
		// An event of its project stored after the five: the relay sends the session's messages in
		// stream order, so the socket's frames before it are the five and nothing else.
		marker := storeScopeEvent(t, rig, ownIssue, "")

		var frames []scopeFrame
		for len(frames) == 0 || frames[len(frames)-1].item.EventID != marker.EventID {
			frames = append(frames, takeScopeFrames(t, client, 1)...)
		}
		frames = frames[:len(frames)-1]
		var events, topics []string
		for _, frame := range frames {
			events = append(events, frame.item.EventID)
			topics = append(topics, frame.item.Topic)
		}
		for _, want := range []contracts.Envelope{send.Envelope, direct, issue, github} {
			if !slices.Contains(events, want.EventID) {
				t.Errorf("the message on %s (%s) never reached the socket; it received %v", want.Topic, want.EventID, scopeFrameTopics(frames))
			}
		}
		if !slices.Contains(topics, roleSubject) {
			t.Errorf("the role message on %s never reached its holder's socket; it received %v", roleSubject, scopeFrameTopics(frames))
		}
		if len(frames) != 5 {
			t.Errorf("the socket received %v before the event %s stored after its own five messages, want those five alone", scopeFrameTopics(frames), marker.EventID)
		}
		requireWithheld(t, rig, -1)
	})

	t.Run("a message on its own agent subject of no project its token lists", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_forwards"
		client, _ := registerScopeSession(t, rig, session)
		own := contracts.LegacyAgentSubject(session)
		relay := watchRelay(t, rig, session)
		store := func(data []byte) {
			t.Helper()
			if _, err := rig.harness.client.JS().Publish(own, data); err != nil {
				t.Fatalf("store a message on %s: %v", own, err)
			}
		}
		// Each is stored on the session's own agent subject, where a forward lands, so only the
		// topic its envelope names, or an envelope that does not decode, can withhold it.
		for _, topic := range []string{
			"notifications.github.acme.pub.pr.7",                   // a forward of a GitHub event
			contracts.LegacyAgentSubject("ses_scope_someone_else"), // a forward of another session's message
			contracts.AgentSubject(scopeOtherProject, session),     // its own agent subject under a project it does not hold
			"notifications.team.priv",                              // a forward of a topic of no project
		} {
			store(marshalListenerEnvelope(t, listenerTestEnvelope(topic, "edge-scope-forward-"+id.New())))
		}
		store([]byte("not an envelope"))
		direct := storeScopeEvent(t, rig, own, "")
		marker := storeScopeEvent(t, rig, own, "")
		frames := takeScopeFrames(t, client, 1)
		if frames[0].item.EventID != direct.EventID {
			t.Fatalf("the socket's first frame is %s, want the message %s to the session: a frame stored before it reached the socket", frames[0].raw, direct.EventID)
		}
		requireNextScopeFrame(t, client, marker, "the message to the session")
		requireWithheld(t, rig, 5)
		// The relay withheld them itself: it published the message to the session first, and the
		// message stored after it next.
		relay.requireNext(t, direct, "the relay published a frame stored before the message to the session, which it was to withhold")
		relay.requireNext(t, marker, "the relay published a frame after the message to the session, before the one stored after it")
	})

	t.Run("a frame another user publishes on its relay subject", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_injected"
		client, _ := registerScopeSession(t, rig, session)
		publisher := testnats.Connect(t, rig.url)
		other := publishScopeFrame(t, publisher, session, otherIssue)
		own := publishScopeFrame(t, publisher, session, ownIssue)
		marker := publishScopeFrame(t, publisher, session, ownIssue)
		// One connection's publishes reach a subscriber in the order they were sent, so the
		// first frame being the second publish shows the first never reached the socket, and the
		// next being the third shows nothing reached it after the second.
		frames := takeScopeFrames(t, client, 1)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s, want %s's frame %s: the frame of %s published on the relay subject reached it",
				frames[0].item.EventID, frames[0].subject, scopeProject, own.EventID, other.Topic)
		}
		requireNextScopeFrame(t, client, marker, "the frame of "+scopeProject)
		requireWithheld(t, rig, 1)
	})

	t.Run("a frame whose envelope names another project than its relay header", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_relabelled"
		client, _ := registerScopeSession(t, rig, session)
		publisher := testnats.Connect(t, rig.url)
		// The relay header names an issue of the token's project and the envelope is an event of
		// another project: both are what a publisher on the relay subject writes.
		relabelled := listenerTestEnvelope(otherIssue, "edge-scope-relabelled-"+id.New())
		header := natsgo.Header{}
		header.Set(relayHeaderSubject, ownIssue)
		header.Set(relayHeaderStreamSeq, "1")
		if err := publisher.PublishMsg(&natsgo.Msg{Subject: bus.DeliverSubject(session), Reply: relayFrameReceipt(session), Header: header, Data: marshalListenerEnvelope(t, relabelled)}); err != nil {
			t.Fatalf("publish the relabelled frame: %v", err)
		}
		own := publishScopeFrame(t, publisher, session, ownIssue)
		marker := publishScopeFrame(t, publisher, session, ownIssue)
		frames := takeScopeFrames(t, client, 1)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s, its envelope's topic %s under the relay header %s, want %s's frame %s: an event of %s under a header of %s reached it",
				frames[0].item.EventID, frames[0].item.Topic, frames[0].subject, scopeProject, own.EventID, scopeOtherProject, scopeProject)
		}
		requireNextScopeFrame(t, client, marker, "the frame of "+scopeProject)
		requireWithheld(t, rig, 1)
	})

	t.Run("a role claim naming it for another project's role", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_claimed"
		client, token := registerScopeSession(t, rig, session)
		relay := watchRelay(t, rig, session)
		const own, other = "legion-priv-priv-2-tester", "legion-pub-pub-2-tester"
		requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, session, own)),
			http.StatusOK, "the claim of "+own)
		// The claim of the other project's role is written to the role bucket as a forged
		// record would be: the edge refuses the claim itself (edgeRoleRefusal).
		if _, err := rig.deps.registry.SetRole(session, "test-machine", other, false); err != nil {
			t.Fatalf("write the claim of %s naming %s: %v", other, session, err)
		}
		publishRole := func(role string) {
			t.Helper()
			subject := contracts.RoleSubject(contracts.RoleProject(role), role)
			recorder := httptest.NewRecorder()
			// A publish through a listener in no edge mode: the shared listener another
			// project's sender uses, which forwards the role's message to the claim's holder.
			rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(
				fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":"ses_scope_sender"}`, subject))))
			if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
				t.Fatalf("the publish to %s: status %d (%s), want it taken", subject, recorder.Code, recorder.Body.String())
			}
		}
		publishRole(other)
		publishRole(own)
		// A message to the session stored after the two forwards.
		marker := storeScopeEvent(t, rig, contracts.LegacyAgentSubject(session), "")
		// The forwards are stored in the order they were made, and the relay sends in stream
		// order, so the first frame being the second forward shows the first was withheld.
		frames := takeScopeFrames(t, client, 1)
		if want := contracts.RoleSubject(scopeProject, own); frames[0].item.Topic != want {
			t.Fatalf("the socket's first frame carries %s, want the role message of %s: the role message of %s reached its socket",
				frames[0].item.Topic, want, contracts.RoleSubject(scopeOtherProject, other))
		}
		requireNextScopeFrame(t, client, marker, "the role message of "+scopeProject)
		requireWithheld(t, rig, 1)
		relay.requireNext(t, contracts.Envelope{Topic: contracts.RoleSubject(scopeProject, own)},
			"the relay published the role message of "+contracts.RoleSubject(scopeOtherProject, other)+" ahead of it, which it was to withhold")
		relay.requireNext(t, marker, "the relay published a frame after the role message of "+scopeProject+", before the message stored after it")
	})

	// publishInfraRole publishes a message to role through a listener in no edge mode, the shared
	// listener another project's sender uses, which forwards it to the claim's holder on the
	// holder's own agent subject, the role lane its envelope's topic.
	publishInfraRole := func(t *testing.T, rig *scopeRig, role string) {
		t.Helper()
		subject := contracts.RoleSubject(contracts.RoleProject(role), role)
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(
			fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":"ses_scope_sender"}`, subject))))
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
			t.Fatalf("the publish to %s: status %d (%s), want it taken", subject, recorder.Code, recorder.Body.String())
		}
	}
	// requireOnlyOwnIssue fails the test unless the socket's first frame, and the relay's first
	// publish, is own, stored after the other project's forward, and the next of each is marker,
	// stored after own: the forward was withheld, by the relay itself, and nothing came after own.
	requireOnlyOwnIssue := func(t *testing.T, rig *scopeRig, client *relaySocketClient, relay *relayWatch, own, marker contracts.Envelope, other string) {
		t.Helper()
		frames := takeScopeFrames(t, client, 1)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s (topic %s), want %s's event %s: the role message of %s reached its socket",
				frames[0].item.EventID, frames[0].subject, frames[0].item.Topic, scopeProject, own.EventID, other)
		}
		requireNextScopeFrame(t, client, marker, "the event of "+scopeProject)
		requireWithheld(t, rig, 1)
		relay.requireNext(t, own, "the relay published the role message of "+other+" ahead of "+scopeProject+"'s event, which it was to withhold")
		relay.requireNext(t, marker, "the relay published a frame after "+scopeProject+"'s event, before the event stored after it")
	}

	t.Run("D a role of another project another writer grants it after its registration", func(t *testing.T) {
		rig := newScopeRig(t)
		const session, infraRole = "ses_scope_late_claim", "legion-infra-infra-8-reviewer"
		client, _ := registerScopeSession(t, rig, session, "notifications.dispatch.issue."+scopeProject+".>")
		relay := watchRelay(t, rig, session)
		if _, err := rig.deps.registry.SetRole(session, "shared-listener", infraRole, false); err != nil {
			t.Fatalf("write the claim of %s naming %s: %v", infraRole, session, err)
		}
		testwait.Until(t, "the role claim copy to name the claim", func() ([]string, bool) {
			held, err := rig.deps.registry.RolesHeldBy(session)
			return held, err == nil && slices.Contains(held, infraRole)
		})
		publishInfraRole(t, rig, infraRole)
		own := storeScopeEvent(t, rig, ownIssue, "")
		marker := storeScopeEvent(t, rig, ownIssue, "")
		requireOnlyOwnIssue(t, rig, client, relay, own, marker, contracts.RoleSubject("INFRA", infraRole))
	})

	t.Run("E a role message of another project forwarded while it was away, then its registration here", func(t *testing.T) {
		rig := newScopeRig(t)
		const session, infraRole = "ses_scope_backlog", "legion-infra-infra-7-reviewer"
		tokenOf := func(token string, projects ...string) string {
			answer := sessionAnswer(session, "outside@example.com", rig.caller.JKT())
			answer.Projects = projects
			rig.broker.Says(token, answer)
			return token
		}
		// A listener in no edge mode, as the session's earlier process registered through: its
		// registration and its claim of an INFRA role under a token listing INFRA.
		plain := apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, rig.broker), logger: rig.harness.config.logger}, rig.mux)
		infraToken := tokenOf("scope-infra-token", "INFRA")
		for _, call := range []struct{ path, body string }{
			{"/v1/interests/subscribe", consumerRegistration(session, "epoch-1")},
			{"/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, session, infraRole)},
		} {
			recorder := httptest.NewRecorder()
			plain.ServeHTTP(recorder, brokerTokenRequest(t, rig.caller, http.MethodPost, call.path, infraToken, call.body))
			requireStatus(t, recorder, http.StatusOK, "the earlier process's "+call.path)
		}
		// The role's message is forwarded to the session's own subject while no process of it
		// reads its consumer, which holds the forward for its next registration.
		publishInfraRole(t, rig, infraRole)

		privToken := tokenOf("scope-priv-token", scopeProject)
		client := rig.openSocket(t, session, privToken)
		relay := watchRelay(t, rig, session)
		requireStatus(t, rig.call(t, http.MethodPost, privToken, "/v1/interests/subscribe",
			consumerRegistration(session, "epoch-2", "notifications.dispatch.issue."+scopeProject+".>")), http.StatusOK, "the registration at the edge")
		own := storeScopeEvent(t, rig, ownIssue, "")
		marker := storeScopeEvent(t, rig, ownIssue, "")
		requireOnlyOwnIssue(t, rig, client, relay, own, marker, contracts.RoleSubject("INFRA", infraRole))
	})

	t.Run("another project's subject added to its consumer", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_refiltered"
		client, _ := registerScopeSession(t, rig, session, "notifications.dispatch.issue."+scopeProject+".>")
		// The relay restarts the session's consumer on its stored filter when its run starts, so
		// the update below waits for a frame of that run: the restart has happened, and the
		// update is the consumer's filter from then on.
		first := storeScopeEvent(t, rig, ownIssue, "")
		if frames := takeScopeFrames(t, client, 1); frames[0].item.EventID != first.EventID {
			t.Fatalf("the socket's first frame is %s, want the event %s", frames[0].raw, first.EventID)
		}
		relay := watchRelay(t, rig, session)
		name, ok := bus.SessionConsumerName(session)
		if !ok {
			t.Fatalf("no consumer name for %s", session)
		}
		js := rig.harness.client.JS()
		info, err := js.ConsumerInfo(bus.Stream, name)
		if err != nil {
			t.Fatalf("read %s's consumer %s: %v", session, name, err)
		}
		config := info.Config
		if config.FilterSubject != "" {
			config.FilterSubjects = []string{config.FilterSubject}
			config.FilterSubject = ""
		}
		config.FilterSubjects = append(config.FilterSubjects, "notifications.dispatch.issue."+scopeOtherProject+".>")
		// The update a listener-grant user can send (CONSUMER.CREATE carries updates), with no
		// record of the session's changed at all.
		if _, err := js.UpdateConsumer(bus.Stream, &config); err != nil {
			t.Fatalf("add %s's issues to %s's consumer: %v", scopeOtherProject, session, err)
		}
		other := storeScopeEvent(t, rig, otherIssue, "")
		own := storeScopeEvent(t, rig, ownIssue, "")
		marker := storeScopeEvent(t, rig, ownIssue, "")
		frames := takeScopeFrames(t, client, 1)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s, want %s's event %s: the event of %s (%s) the consumer was given reached it",
				frames[0].item.EventID, frames[0].subject, scopeProject, own.EventID, scopeOtherProject, other.EventID)
		}
		requireNextScopeFrame(t, client, marker, "the event of "+scopeProject)
		requireWithheld(t, rig, 1)
		relay.requireNext(t, own, "the relay published the event of "+scopeOtherProject+" ahead of "+scopeProject+"'s, which it was to withhold")
		relay.requireNext(t, marker, "the relay published a frame after "+scopeProject+"'s event, before the event stored after it")
	})

	t.Run("a role-marked send forwarded to it, of another project and of its own", func(t *testing.T) {
		// A role-marked send (item.Role set) is a role's message forwarded to the claim's holder
		// on the holder's own agent subject, which Dispatch's API sends and the kept lane and a
		// takeover hand over. Its topic is an agent subject, not the role lane, so the rule reads
		// its role mark: a forged claim can name an outside session for another project's role and
		// have the send reach it on its own subject (F1), and a copy kept or taken for the
		// session's own project's role keeps the previous holder's subject (F2). Both are judged
		// by the role, not by the subject they are stored on.
		rig := newScopeRig(t)
		const session = "ses_scope_role_marked"
		client, _ := registerScopeSession(t, rig, session)
		own := contracts.LegacyAgentSubject(session)
		const otherRole, ownRole = "legion-pub-pub-3-tester", "legion-priv-priv-3-tester"
		// Both claims are written to the role bucket as a forged record would be: the edge
		// refuses the claim of the other project's role itself (edgeRoleRefusal).
		for _, role := range []string{otherRole, ownRole} {
			if _, err := rig.deps.registry.SetRole(session, "test-machine", role, false); err != nil {
				t.Fatalf("write the claim of %s naming %s: %v", role, session, err)
			}
		}
		store := func(item contracts.Envelope) contracts.Envelope {
			t.Helper()
			if _, err := rig.harness.client.JS().Publish(own, marshalListenerEnvelope(t, item)); err != nil {
				t.Fatalf("store a frame on %s: %v", own, err)
			}
			return item
		}
		// 1 CONTROL other project's role as a role-lane forward (topic = the role lane): withheld.
		otherForward := store(listenerTestEnvelope(contracts.RoleTopicPrefix+otherRole, "scope-other-forward-"+id.New()))
		// 2 F1 other project's role as a role-marked send (topic = the session's own subject): withheld.
		f1 := listenerTestEnvelope(own, "scope-f1-"+id.New())
		f1.Role = otherRole
		f1 = store(f1)
		// 3 F2 its own project's role as a kept role-marked send (topic = the previous holder's subject): delivered.
		f2 := listenerTestEnvelope(contracts.LegacyAgentSubject("ses_scope_previous_holder"), "scope-f2-"+id.New())
		f2.Role = ownRole
		f2 = store(f2)
		// 4 CONTROL its own project's role as a role-lane forward (topic = the role lane): delivered.
		ownForward := store(listenerTestEnvelope(contracts.RoleTopicPrefix+ownRole, "scope-own-forward-"+id.New()))
		// 5 MARKER a message to the session with no role mark, stored after the four: delivered,
		// and the next frame after its own project's role-lane forward, since the relay sends in
		// stream order.
		marker := store(listenerTestEnvelope(own, "scope-marker-"+id.New()))

		var received []scopeFrame
		for len(received) == 0 || received[len(received)-1].item.EventID != marker.EventID {
			received = append(received, takeScopeFrames(t, client, 1)...)
		}
		got := map[string]bool{}
		for _, frame := range received {
			got[frame.item.EventID] = true
		}
		if len(received) < 2 || received[len(received)-2].item.EventID != ownForward.EventID {
			t.Errorf("the socket received %v up to the message %s stored after the four, want its own project's role-lane forward (%s) right before it: a frame reached the socket after its own project's role messages",
				scopeFrameTopics(received), marker.EventID, ownForward.EventID)
		}
		if got[otherForward.EventID] {
			t.Errorf("the other project's role-lane forward (%s) reached the socket", otherForward.EventID)
		}
		if got[f1.EventID] {
			t.Errorf("F1: the other project's role-marked send (%s) reached the socket", f1.EventID)
		}
		if !got[f2.EventID] {
			t.Errorf("F2: its own project's kept role-marked send (%s) never reached the socket", f2.EventID)
		}
		if !got[ownForward.EventID] {
			t.Errorf("its own project's role-lane forward (%s) never reached the socket", ownForward.EventID)
		}
		requireWithheld(t, rig, 2)
	})
}

// A relay socket on a replica that has read no repository answer yet cannot say whether a GitHub
// event is its session's, so it answers the frame nak and the relay sends it again: once the
// socket's replica reads the answer, the event reaches the session. Acking it away instead, as a
// done answer does, loses it with no gap mark. The control is the socket whose replica has the
// answer from the start: the event reaches it at once, ahead of the event stored after it.
func TestARelaySocketWithNoFreshAnswerHoldsAGitHubEventForItsSession(t *testing.T) {
	t.Parallel()
	const ownIssue = "notifications.dispatch.issue." + scopeProject + ".PRIV-1.comment.created"
	for _, cold := range []bool{false, true} {
		name := "the socket's replica has the answer (control)"
		if cold {
			name = "the socket's replica has read no answer yet"
		}
		t.Run(name, func(t *testing.T) {
			rig, socketReader := newScopeRigSocketReader(t, cold)
			const session = "ses_scope_cold_socket"
			client, _ := registerScopeSession(t, rig, session, "notifications.github.acme.pub.>", "notifications.dispatch.issue."+scopeProject+".>")
			github := storeScopeEvent(t, rig, "notifications.github.acme.pub.pr.1", `{"kind":"pr","action":"opened","repo":"acme/pub"}`)
			issue := storeScopeEvent(t, rig, ownIssue, "")
			if !cold {
				requireFrames := takeScopeFrames(t, client, 2)
				if requireFrames[0].item.EventID != github.EventID || requireFrames[1].item.EventID != issue.EventID {
					t.Fatalf("the socket received %v, want the GitHub event %s and then the issue event %s", scopeFrameTopics(requireFrames), github.EventID, issue.EventID)
				}
				return
			}
			first := takeScopeFrames(t, client, 1)
			if first[0].item.EventID != issue.EventID {
				t.Fatalf("the cold socket's first frame is %s, want the issue event %s: it judged the GitHub event with no answer", first[0].raw, issue.EventID)
			}
			socketReader.refresh(context.Background())
			again := takeScopeFrames(t, client, 1)
			if again[0].item.EventID != github.EventID {
				t.Fatalf("after the socket's replica read the answer, the socket received %s, want the GitHub event %s sent again", again[0].raw, github.EventID)
			}
		})
	}
}
