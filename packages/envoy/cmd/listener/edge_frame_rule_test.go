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
	rig.relays.setOwned(endlessTerm())
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
	client.sub(1, bus.DeliverSubject(session))
	client.ping()
	select {
	case <-client.pongs:
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("no pong within the time this package gives a wait, after the socket's sub")
	}
	return client
}

// scopeFrame is one msg frame a socket received: the stored subject its relay header names and
// the envelope it carries.
type scopeFrame struct {
	subject string
	item    contracts.Envelope
	raw     string
}

// take reads the socket's next n msg frames within the bound, answering each received and done
// as a session that wrote it does, and fails the test on any other frame or a close.
func takeScopeFrames(t *testing.T, client *relaySocketClient, n int, within time.Duration) []scopeFrame {
	t.Helper()
	deadline := time.After(within)
	var frames []scopeFrame
	for len(frames) < n {
		select {
		case frame := <-client.frames:
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
		case err := <-client.closed:
			t.Fatalf("the socket closed (%v) after %d of %d frames", err, len(frames), n)
		case <-deadline:
			t.Fatalf("the socket sent %d of %d frames within %s: %v", len(frames), n, within, scopeFrameTopics(frames))
		}
	}
	return frames
}

// noScopeFrame fails the test when the socket sends a msg frame within the window.
func noScopeFrame(t *testing.T, client *relaySocketClient, window time.Duration, what string) {
	t.Helper()
	select {
	case frame := <-client.frames:
		t.Fatalf("%s: the socket sent %s within %s, want nothing", what, frame.raw, window)
	case <-time.After(window):
	}
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

// watchRelay subscribes session's relay subject directly, as a session holding a NATS connection
// of its own reads it, so a row sees what the relay itself published apart from what the socket,
// which judges each frame again, forwards.
func watchRelay(t *testing.T, rig *scopeRig, session string) *natsgo.Subscription {
	t.Helper()
	conn := testnats.Connect(t, rig.url)
	sub, err := conn.SubscribeSync(bus.DeliverSubject(session))
	if err != nil {
		t.Fatalf("watch %s: %v", bus.DeliverSubject(session), err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the watch of %s: %v", bus.DeliverSubject(session), err)
	}
	return sub
}

// relayTopics is the envelope topic of every relay frame the watch received so far, in order: a
// connection probe is no frame, and a message whose body is no envelope is named by its body.
func relayTopics(t *testing.T, sub *natsgo.Subscription) []string {
	t.Helper()
	var topics []string
	for {
		msg, err := sub.NextMsg(200 * time.Millisecond)
		if err != nil {
			return topics
		}
		if _, probe := msg.Header[bus.ConnectionProbeHeader]; probe || !strings.HasPrefix(msg.Reply, bus.RelayReplyPrefix) {
			continue
		}
		topic, named, _ := exactFrameTopic(msg.Data)
		if !named {
			topic = string(msg.Data)
		}
		topics = append(topics, topic)
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

// The edge relays a session only the frames of the projects its live token lists. Its own
// traffic arrives: a send in each agent-subject form the edge accepts, a role message of its
// project forwarded to it as the holder, an event of its project, and a GitHub event of its
// project's repository. Three paths that put another project's frame in front of the session are
// each closed at the frame: a frame another NATS user publishes on the session's relay subject, a
// role claim naming the session as the holder of another project's role, and another project's
// subject added to the session's own consumer.
func TestAnEdgeRelaysOnlyItsTokensProjects(t *testing.T) {
	// Not parallel: it holds quiet windows: no further frame reaches the socket within 1 s
	// (noScopeFrame), and the relay's own publishes are read until 200 ms pass with none
	// (relayTopics).
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

		sent := rig.call(t, http.MethodPost, peerToken, "/v1/messages/send",
			fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from a peer of the project","source":"agent"}`, session, peer))
		requireStatus(t, sent, http.StatusOK, "the peer's send")
		var send listenerwire.SendResponse
		if err := json.Unmarshal(sent.Body.Bytes(), &send); err != nil {
			t.Fatalf("decode the send's answer %s: %v", sent.Body.String(), err)
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

		frames := takeScopeFrames(t, client, 5, 15*time.Second)
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
		noScopeFrame(t, client, time.Second, "after its own five messages")
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
		frames := takeScopeFrames(t, client, 1, 10*time.Second)
		if frames[0].item.EventID != direct.EventID {
			t.Fatalf("the socket's first frame is %s, want the message %s to the session: a frame stored before it reached the socket", frames[0].raw, direct.EventID)
		}
		noScopeFrame(t, client, time.Second, "after the message to the session")
		requireWithheld(t, rig, 5)
		// The relay withheld them itself: it published the message to the session alone.
		if published := relayTopics(t, relay); !slices.Equal(published, []string{own}) {
			t.Fatalf("the relay published frames of %v on the relay subject, want only the message to the session on %s", published, own)
		}
	})

	t.Run("a frame another user publishes on its relay subject", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_injected"
		client, _ := registerScopeSession(t, rig, session)
		publisher := testnats.Connect(t, rig.url)
		other := publishScopeFrame(t, publisher, session, otherIssue)
		own := publishScopeFrame(t, publisher, session, ownIssue)
		// One connection's publishes reach a subscriber in the order they were sent, so the
		// first frame being the second publish shows the first never reached the socket.
		frames := takeScopeFrames(t, client, 1, 10*time.Second)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s, want %s's frame %s: the frame of %s published on the relay subject reached it",
				frames[0].item.EventID, frames[0].subject, scopeProject, own.EventID, other.Topic)
		}
		noScopeFrame(t, client, time.Second, "after the frame of "+scopeProject)
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
		frames := takeScopeFrames(t, client, 1, 10*time.Second)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s, its envelope's topic %s under the relay header %s, want %s's frame %s: an event of %s under a header of %s reached it",
				frames[0].item.EventID, frames[0].item.Topic, frames[0].subject, scopeProject, own.EventID, scopeOtherProject, scopeProject)
		}
		noScopeFrame(t, client, time.Second, "after the frame of "+scopeProject)
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
		// The forwards are stored in the order they were made, and the relay sends in stream
		// order, so the first frame being the second forward shows the first was withheld.
		frames := takeScopeFrames(t, client, 1, 10*time.Second)
		if want := contracts.RoleSubject(scopeProject, own); frames[0].item.Topic != want {
			t.Fatalf("the socket's first frame carries %s, want the role message of %s: the role message of %s reached its socket",
				frames[0].item.Topic, want, contracts.RoleSubject(scopeOtherProject, other))
		}
		noScopeFrame(t, client, time.Second, "after the role message of "+scopeProject)
		requireWithheld(t, rig, 1)
		if published := relayTopics(t, relay); !slices.Equal(published, []string{contracts.RoleSubject(scopeProject, own)}) {
			t.Fatalf("the relay published frames of %v on the relay subject, want only the role message of %s", published, contracts.RoleSubject(scopeProject, own))
		}
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
	// requireOnlyOwnIssue fails the test unless the socket's first frame, and the relay's only
	// publish, is own, stored after the other project's forward: the forward was withheld.
	requireOnlyOwnIssue := func(t *testing.T, rig *scopeRig, client *relaySocketClient, relay *natsgo.Subscription, own contracts.Envelope, other string) {
		t.Helper()
		frames := takeScopeFrames(t, client, 1, 15*time.Second)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s (topic %s), want %s's event %s: the role message of %s reached its socket",
				frames[0].item.EventID, frames[0].subject, frames[0].item.Topic, scopeProject, own.EventID, other)
		}
		noScopeFrame(t, client, time.Second, "after the event of "+scopeProject)
		requireWithheld(t, rig, 1)
		if published := relayTopics(t, relay); !slices.Equal(published, []string{ownIssue}) {
			t.Fatalf("the relay published frames of %v on the relay subject, want only %s's event", published, scopeProject)
		}
	}

	t.Run("D a role of another project another writer grants it after its registration", func(t *testing.T) {
		rig := newScopeRig(t)
		const session, infraRole = "ses_scope_late_claim", "legion-infra-infra-8-reviewer"
		client, _ := registerScopeSession(t, rig, session, "notifications.dispatch.issue."+scopeProject+".>")
		relay := watchRelay(t, rig, session)
		if _, err := rig.deps.registry.SetRole(session, "shared-listener", infraRole, false); err != nil {
			t.Fatalf("write the claim of %s naming %s: %v", infraRole, session, err)
		}
		testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
			held, err := rig.deps.registry.RolesHeldBy(session)
			return err == nil && slices.Contains(held, infraRole)
		})
		publishInfraRole(t, rig, infraRole)
		own := storeScopeEvent(t, rig, ownIssue, "")
		requireOnlyOwnIssue(t, rig, client, relay, own, contracts.RoleSubject("INFRA", infraRole))
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
		requireOnlyOwnIssue(t, rig, client, relay, own, contracts.RoleSubject("INFRA", infraRole))
	})

	t.Run("another project's subject added to its consumer", func(t *testing.T) {
		rig := newScopeRig(t)
		const session = "ses_scope_refiltered"
		client, _ := registerScopeSession(t, rig, session, "notifications.dispatch.issue."+scopeProject+".>")
		// The relay restarts the session's consumer on its stored filter when its run starts, so
		// the update below waits for a frame of that run: the restart has happened, and the
		// update is the consumer's filter from then on.
		first := storeScopeEvent(t, rig, ownIssue, "")
		if frames := takeScopeFrames(t, client, 1, 10*time.Second); frames[0].item.EventID != first.EventID {
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
		frames := takeScopeFrames(t, client, 1, 10*time.Second)
		if frames[0].item.EventID != own.EventID {
			t.Fatalf("the socket's first frame is %s on %s, want %s's event %s: the event of %s (%s) the consumer was given reached it",
				frames[0].item.EventID, frames[0].subject, scopeProject, own.EventID, scopeOtherProject, other.EventID)
		}
		noScopeFrame(t, client, time.Second, "after the event of "+scopeProject)
		requireWithheld(t, rig, 1)
		if published := relayTopics(t, relay); !slices.Equal(published, []string{ownIssue}) {
			t.Fatalf("the relay published frames of %v on the relay subject, want only %s's event", published, scopeProject)
		}
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

		got := map[string]bool{}
		deadline := time.Now().Add(listenerWaitBound(t))
		for !got[ownForward.EventID] && time.Now().Before(deadline) {
			for _, frame := range takeScopeFrames(t, client, 1, time.Until(deadline)) {
				got[frame.item.EventID] = true
			}
		}
		noScopeFrame(t, client, time.Second, "after its own project's role messages")
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
				requireFrames := takeScopeFrames(t, client, 2, 15*time.Second)
				if requireFrames[0].item.EventID != github.EventID || requireFrames[1].item.EventID != issue.EventID {
					t.Fatalf("the socket received %v, want the GitHub event %s and then the issue event %s", scopeFrameTopics(requireFrames), github.EventID, issue.EventID)
				}
				return
			}
			first := takeScopeFrames(t, client, 1, 15*time.Second)
			if first[0].item.EventID != issue.EventID {
				t.Fatalf("the cold socket's first frame is %s, want the issue event %s: it judged the GitHub event with no answer", first[0].raw, issue.EventID)
			}
			socketReader.refresh(context.Background())
			again := takeScopeFrames(t, client, 1, 15*time.Second)
			if again[0].item.EventID != github.EventID {
				t.Fatalf("after the socket's replica read the answer, the socket received %s, want the GitHub event %s sent again", again[0].raw, github.EventID)
			}
		})
	}
}
