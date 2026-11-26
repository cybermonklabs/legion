//go:build natslive

package main

// The edge listener under its rendered grant, on the hub's NATS image: every user of these servers
// is an nkey user held to a grant of natsgrant's, as a hub renders its static users (natsgrant.Edge
// for the edge listener; infra/core.EdgePermissions wraps the same lists, and
// TestEdgePermissionsAreTheEdgeGrant there holds the two equal), and no client is admitted without
// a credential.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// edgeLiveMachine is the edge listener's machine id, the one its user's grant is rendered for.
	edgeLiveMachine = "example-edge-a"
	// edgeLiveSession is the edge session every row registers.
	edgeLiveSession = "ses_edge_a"
	// edgeLiveOtherSession is a second session of the edge's person, which holds a role the edge
	// session then takes, or which holds a role a message on the edge session's subject is of.
	edgeLiveOtherSession = "ses_edge_b"
	// edgeLiveRole is the role the edge session claims: a Legion role of the edge's project, in
	// the new form, which the claim route takes (contracts.RoleProject).
	edgeLiveRole = "PRIV.legion-priv-reviewer"
)

// edgeLiveGrant is the edge listener's grant for edgeLiveMachine and edgeProject (natsgrant.Edge).
func edgeLiveGrant(t *testing.T) natsgrant.Permissions {
	t.Helper()
	grant, err := natsgrant.Edge(edgeLiveMachine, []string{edgeProject})
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	return grant
}

// edgeLiveHub is one hub-image server whose only users are nkey users, each held to a grant of
// natsgrant's as a hub renders its static users, with each user's seed, and the client of
// Dispatch's user on it, which created the stream as Dispatch's server does.
type edgeLiveHub struct {
	server interface {
		Logs(context.Context) (io.ReadCloser, error)
	}
	uri      string
	seeds    map[string]string
	dispatch *bus.Client
}

// startEdgeLiveHub starts a server whose users are grants, by name. The user dispatch names
// creates the stream, as Dispatch's server does, and makes buckets: the edge's grant creates and
// raises no bucket's stream, so an edge listener finds each bucket the shared listeners made at
// their start, and Dispatch's user, whose grant holds each one's create, makes them here before
// any edge starts.
func startEdgeLiveHub(t *testing.T, grants map[string]natsgrant.Permissions, dispatch string, buckets []string) edgeLiveHub {
	t.Helper()
	hub := edgeLiveHub{seeds: map[string]string{}}
	var static []testnats.StaticUser
	for _, name := range slices.Sorted(maps.Keys(grants)) {
		seed, public := testnats.User(t)
		hub.seeds[name] = seed
		grant := grants[name]
		static = append(static, testnats.StaticUser{NKey: public, Publish: grant.Publish, Subscribe: grant.Subscribe})
	}
	hub.server, hub.uri = testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(static...), []string{"-js"}, false)
	dispatchSeed := hub.seeds[dispatch]
	client, err := bus.ConnectOwningStream([]string{hub.uri}, bus.WithName(dispatch), bus.WithEnvironment(testnats.SeedOnly(dispatchSeed)))
	if err != nil {
		t.Fatalf("Dispatch's user creates the stream: %v", err)
	}
	t.Cleanup(client.Close)
	hub.dispatch = client
	for _, bucket := range buckets {
		if _, err := client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("Dispatch's user makes the %s bucket: %v", bucket, err)
		}
	}
	return hub
}

// violations is every permissions violation the server has logged, as kind and subject.
func (h edgeLiveHub) violations(t *testing.T) [][]string {
	t.Helper()
	logs, err := h.server.Logs(context.Background())
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	var found [][]string
	for _, match := range edgeServerViolation.FindAllStringSubmatch(string(text), -1) {
		found = append(found, match[1:])
	}
	return found
}

// edgeLiveRig is one hub of the users a row names (edgeLiveHub) and the broker the edge listener
// asks about its callers' tokens.
type edgeLiveRig struct {
	edgeLiveHub
	broker *ciphertokentest.StubBroker
	caller *ciphertokentest.Caller
	token  string
	// otherToken is edgeLiveOtherSession's broker token, as token is edgeLiveSession's.
	otherToken string
	binary     string
}

// newEdgeLiveRig starts a server whose users are the edge listener's, holding edge (natsgrant.Edge
// unless a row changes it), Dispatch's, a shared listener's (natsgrant.ListenerHost) and the edge
// session's observer, which reads the session's relay subject and answers its frames as the
// relay socket would. Dispatch's user creates the stream, as Dispatch's server does, and every
// listener bucket. The broker says the edge session's token is a named token of the edge's project.
func newEdgeLiveRig(t *testing.T, edge natsgrant.Permissions) *edgeLiveRig {
	t.Helper()
	return newEdgeLiveRigWithBuckets(t, edge, bus.ListenerBuckets())
}

// newEdgeLiveRigWithBuckets is newEdgeLiveRig with Dispatch's user making buckets alone of the
// listener buckets, so a row can leave one for a shared listener's start to make.
func newEdgeLiveRigWithBuckets(t *testing.T, edge natsgrant.Permissions, buckets []string) *edgeLiveRig {
	t.Helper()
	rig := &edgeLiveRig{binary: cmdtest.Build(t, "envoy-listener")}
	rig.edgeLiveHub = startEdgeLiveHub(t, map[string]natsgrant.Permissions{
		"edge":             edge,
		"example-dispatch": natsgrant.Dispatch("example-dispatch"),
		"shared":           natsgrant.ListenerHost("example-shared"),
		// The observer stands in for the relay socket's serving replica: it reads the session's
		// relay subject and answers on whatever reply subject a frame names. An edge listener's
		// frames name a relay receipt (bus.RelayReplyPrefix), which the serving replica's grant
		// publishes, and its connection probe names the edge's own inbox.
		"observer": {Publish: []string{natsgo.InboxPrefix + ">", natsgrant.EdgeInboxPrefix(edgeLiveMachine) + ".>", bus.RelayReplyPrefix + ">"}, Subscribe: []string{bus.DeliverSubjectPrefix + "*"}},
	}, "example-dispatch", buckets)

	rig.broker = ciphertokentest.NewStubBroker(t)
	rig.caller = ciphertokentest.NewCaller(t)
	rig.token = "access-token-of-the-edge-session"
	rig.otherToken = "access-token-of-the-other-edge-session"
	for token, session := range map[string]string{rig.token: edgeLiveSession, rig.otherToken: edgeLiveOtherSession} {
		rig.broker.Says(token, ciphertoken.Introspection{
			Active:       true,
			Session:      session,
			Person:       "outside-person@example.test",
			ProjectScope: ciphertoken.ProjectScopeNamed,
			Projects:     []string{edgeProject},
			ClientID:     ciphertoken.ExchangeClientID,
			Thumbprint:   rig.caller.JKT(),
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
	}
	return rig
}

// startEdge starts the edge listener as its user, an edge listener of edgeProject.
func (r *edgeLiveRig) startEdge(t *testing.T) *listenerProcess {
	t.Helper()
	return startEdgeListenerProcess(t, r.binary, r.uri, edgeLiveMachine, edgeProject, r.broker.Server.URL, edgeAccountToken, bus.NkeySeedEnvVar+"="+r.seeds["edge"])
}

// startShared starts a shared listener as its own user, one no session shares.
func (r *edgeLiveRig) startShared(t *testing.T) *listenerProcess {
	t.Helper()
	return startListenerProcess(t, r.binary, r.uri, "example-shared", bus.NkeySeedEnvVar+"="+r.seeds["shared"])
}

// connect connects to the server as the user name names.
func (r *edgeLiveRig) connect(t *testing.T, name string) *natsgo.Conn {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, r.uri, r.seeds[name])
	if err != nil {
		t.Fatalf("connect as the %s user: %v", name, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// call sends a /v1 request to the edge listener under the edge session's broker token, signed as
// its real client signs one, and returns the status and body it answered.
func (r *edgeLiveRig) call(t *testing.T, edge *listenerProcess, method, path, body string) (int, string) {
	t.Helper()
	return r.callAs(t, edge, r.token, method, path, body)
}

// callAs is call under the broker token token.
func (r *edgeLiveRig) callAs(t *testing.T, edge *listenerProcess, token, method, path, body string) (int, string) {
	t.Helper()
	origin := "http://127.0.0.1:" + strconv.Itoa(edge.port)
	signed := r.caller.Signed(t, method, origin+path, token)
	request, err := http.NewRequest(method, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(answer)
}

// mustCall is call, requiring a 2xx.
func (r *edgeLiveRig) mustCall(t *testing.T, edge *listenerProcess, method, path, body string) string {
	t.Helper()
	return r.mustCallAs(t, edge, r.token, method, path, body)
}

// mustCallAs is callAs, requiring a 2xx.
func (r *edgeLiveRig) mustCallAs(t *testing.T, edge *listenerProcess, token, method, path, body string) string {
	t.Helper()
	status, answer := r.callAs(t, edge, token, method, path, body)
	if status < 200 || status > 299 {
		t.Fatalf("%s %s: %d %s\n%s", method, path, status, answer, edge.Output.String())
	}
	return answer
}

// register registers the edge session through the edge listener with consumer delivery, and
// requires the answer to be consumer delivery on the session's relay subject.
func (r *edgeLiveRig) register(t *testing.T, edge *listenerProcess) {
	t.Helper()
	r.registerAs(t, edge, edgeLiveSession, r.token)
}

// registerAs is register for session, under its broker token token.
func (r *edgeLiveRig) registerAs(t *testing.T, edge *listenerProcess, session, token string) {
	t.Helper()
	answer := r.mustCallAs(t, edge, token, http.MethodPost, "/v1/interests/subscribe",
		`{"session_id":"`+session+`","topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"edge-epoch"}}`)
	if !strings.Contains(answer, `"mode":"consumer"`) || !strings.Contains(answer, `"subject":"`+bus.DeliverSubject(session)+`"`) {
		t.Fatalf("session %s's registration was not answered consumer delivery on its relay subject: %s", session, answer)
	}
}

// requireRoleHolder requires the edge listener's role lookup to name holder as edgeLiveRole's live
// holder.
func (r *edgeLiveRig) requireRoleHolder(t *testing.T, edge *listenerProcess, holder string) {
	t.Helper()
	answer := r.mustCall(t, edge, http.MethodGet, "/v1/roles/"+edgeLiveRole, "")
	var read struct {
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal([]byte(answer), &read); err != nil {
		t.Fatalf("decode the role lookup %s: %v", answer, err)
	}
	if read.Holder != holder {
		t.Fatalf("role %s is held by %q, want %s: %s", edgeLiveRole, read.Holder, holder, answer)
	}
}

// nextRelayFrame is the next relay frame sub receives within within, answered done, with its
// envelope: the connection probe is answered done and passed over, as a session answers it. ok is
// false when no frame came.
func nextRelayFrame(t *testing.T, sub *natsgo.Subscription, within time.Duration) (contracts.Envelope, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		msg, err := sub.NextMsg(time.Until(deadline))
		if err != nil {
			return contracts.Envelope{}, false
		}
		if msg.Reply != "" {
			if err := msg.Respond([]byte("done")); err != nil {
				t.Fatalf("answer the frame on %s: %v", msg.Reply, err)
			}
		}
		if msg.Header.Get(bus.ConnectionProbeHeader) == "1" {
			continue
		}
		var envelope contracts.Envelope
		if err := json.Unmarshal(msg.Data, &envelope); err != nil {
			t.Fatalf("decode the relay frame: %v", err)
		}
		return envelope, true
	}
}

// The edge listener connects as its user, rendered from natsgrant.Edge, starts, registers a
// session under a broker token of its project, gives it a consumer and relays a send to it on the
// session's relay subject, whose frame the observer answers on the reply subject the frame names.
// The session sends to itself and names itself the sender (source_session), as a send made under
// its token names it.
// The server refuses the edge nothing: a publish the edge's own user is refused, made last, is the
// one violation the server logs, which shows the log is read. Without the edge's own inbox prefix
// in its grant the server refuses the listener's subscription to it, which every reply the
// listener asks for comes under: the start waits for the grant, up and not ready, and each try
// logs the refusal at ERROR naming a subject under the prefix (streamRefusedLine). Only the grant
// lifts that refusal, so an exit would send every pod of the machine into a crash loop.
func TestTheEdgeStartsAndServesUnderItsRenderedGrant(t *testing.T) {
	t.Run("under its grant", func(t *testing.T) {
		rig := newEdgeLiveRig(t, edgeLiveGrant(t))
		edge := rig.startEdge(t)
		edge.waitHealthy(t)
		frames, err := rig.connect(t, "observer").SubscribeSync(bus.DeliverSubject(edgeLiveSession))
		if err != nil {
			t.Fatalf("the observer reads the relay subject: %v", err)
		}
		rig.register(t, edge)
		rig.mustCall(t, edge, http.MethodPost, "/v1/messages/send", `{"target_session":"`+edgeLiveSession+`","source_session":"`+edgeLiveSession+`","message":"example message to the edge"}`)
		envelope, ok := nextRelayFrame(t, frames, 15*time.Second)
		if !ok {
			t.Fatalf("no relay frame reached the edge session within 15s:\n%s", edge.Output.String())
		}
		if envelope.PayloadSummary != "example message to the edge" || !slices.Contains(contracts.TopicForms(contracts.LegacyAgentSubject(edgeLiveSession)), envelope.Topic) {
			t.Fatalf("the relay frame carries %q on %s, want the send on the session's agent subject", envelope.PayloadSummary, envelope.Topic)
		}

		// The violation provoked last, by the edge's own user: the server logs it, so a reading that
		// finds it and nothing else saw the server refuse the edge listener nothing.
		const provoked = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
		probe := rig.connect(t, "edge")
		if err := probe.Publish(provoked, nil); err != nil {
			t.Fatal(err)
		}
		_ = probe.Flush()
		deadline := time.Now().Add(listenerWaitBound(t))
		for {
			logged := rig.violations(t)
			if slices.ContainsFunc(logged, func(v []string) bool { return v[1] == provoked }) {
				for _, v := range logged {
					if v[1] != provoked {
						t.Fatalf("the server refused %s %s under the edge's grant:\n%s", v[0], v[1], edge.Output.String())
					}
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the server logged no violation naming %s in time, so its log was not read", provoked)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	t.Run("without its inbox prefix", func(t *testing.T) {
		grant := edgeLiveGrant(t)
		prefix := natsgrant.EdgeInboxPrefix(edgeLiveMachine)
		grant.Subscribe = slices.DeleteFunc(slices.Clone(grant.Subscribe), func(entry string) bool { return entry == prefix+".>" })
		if len(grant.Subscribe) != 2 {
			t.Fatalf("the edge's grant does not subscribe %s.>: %v", prefix, grant.Subscribe)
		}
		rig := newEdgeLiveRig(t, grant)
		edge := rig.startEdge(t)
		refusals := func() []map[string]any {
			return linesIn(edge.Output.String(), streamRefusedLine, map[string]any{"level": "ERROR", "stream": bus.Stream})
		}
		// Two tries each log the refusal: the start went on trying, and did not end on the first.
		waitFor(t, "the edge listener to name the refused inbox prefix at ERROR on two tries", func() bool {
			select {
			case <-edge.Exited:
				t.Fatalf("the edge listener exited %d under a grant without its inbox prefix, want it up and trying again:\n%s",
					edge.Cmd.ProcessState.ExitCode(), edge.Output.String())
			default:
			}
			return len(refusals()) >= 2
		})
		for _, line := range refusals() {
			if subject, _ := line["subject"].(string); !strings.HasPrefix(subject, prefix+".") {
				t.Fatalf("a try's refusal names %q, want a subject under %s:\n%s", subject, prefix, edge.Output.String())
			}
		}
		requireLiveAndStarting(t, edge, "its grant refused the inbox prefix")
	})
}

// edgeRoleEnvelope is a role message on topic, as a publisher sends one.
func edgeRoleEnvelope(topic, summary string) contracts.Envelope {
	eventID := id.New()
	return contracts.Envelope{
		EventID:        eventID,
		Source:         "envoy",
		SourceEventID:  eventID,
		Topic:          topic,
		DedupeKey:      "edge-role-" + eventID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: summary,
		Payload:        `{"text":"` + summary + `"}`,
		TraceID:        id.New(),
	}
}

// A shared listener that takes no broker tokens makes the spent-proof bucket at its start, so an
// edge listener, whose grant creates no bucket, starts against it. The rig's Dispatch user makes
// every listener bucket but that one; the shared listener starts first, with no broker URL, and
// leaves bus.DPoPProofBucket in place at the configuration a broker-token listener makes
// (ensureSpentProofBucket): file storage, one copy at this rig's count, one value per key and
// spentProofRetention as its age. The edge then starts, and registers its session under a broker
// token, which records the token's proof in that bucket.
func TestASharedListenerMakesTheSpentProofBucketAnEdgeListenerStartsAgainst(t *testing.T) {
	buckets := slices.DeleteFunc(bus.ListenerBuckets(), func(bucket string) bool { return bucket == bus.DPoPProofBucket })
	rig := newEdgeLiveRigWithBuckets(t, edgeLiveGrant(t), buckets)
	shared := rig.startShared(t)
	shared.waitHealthy(t)
	info, err := rig.dispatch.JS().StreamInfo("KV_" + bus.DPoPProofBucket)
	if err != nil {
		t.Fatalf("the shared listener's start left no %s bucket: %v\n%s", bus.DPoPProofBucket, err, shared.Output.String())
	}
	got := fmt.Sprintf("storage %s, %d copies, %d per key, max age %s", info.Config.Storage, info.Config.Replicas, info.Config.MaxMsgsPerSubject, info.Config.MaxAge)
	want := fmt.Sprintf("storage %s, 1 copies, 1 per key, max age %s", natsgo.FileStorage, spentProofRetention)
	if got != want {
		t.Fatalf("the %s bucket the shared listener made is %s, want %s", bus.DPoPProofBucket, got, want)
	}

	edge := rig.startEdge(t)
	edge.waitHealthy(t)
	rig.register(t, edge)
}

// An edge session claims a role of its project. A message published on the role's subject while
// no shared listener runs is taken by no one: the edge listener subscribes no role lane, so it
// takes no share of any role's messages. Once a shared listener runs, as its own user, the next
// message is taken from the role lane by it, which resolves the edge session as the holder and
// forwards the message to the session's agent subject, and the edge listener relays it to the
// session. So the first frame the session is relayed is that second message: had the edge taken
// the first, the first frame would be the first message.
func TestARoleHeldAtTheEdgeIsReachedThroughASharedListener(t *testing.T) {
	rig := newEdgeLiveRig(t, edgeLiveGrant(t))
	edge := rig.startEdge(t)
	edge.waitHealthy(t)
	frames, err := rig.connect(t, "observer").SubscribeSync(bus.DeliverSubject(edgeLiveSession))
	if err != nil {
		t.Fatalf("the observer reads the relay subject: %v", err)
	}
	rig.register(t, edge)
	rig.mustCall(t, edge, http.MethodPost, "/v1/roles/set", `{"session_id":"`+edgeLiveSession+`","role":"`+edgeLiveRole+`"}`)

	if err := rig.dispatch.Publish(edgeRoleEnvelope(contracts.RoleTopicPrefix+edgeLiveRole, "before a shared listener runs")); err != nil {
		t.Fatalf("publish the role message: %v", err)
	}

	shared := rig.startShared(t)
	shared.waitHealthy(t)
	shared.WaitForOutput(t, `"msg":"role lane subscribed"`)
	const summary = "through the shared listener"
	if err := rig.dispatch.Publish(edgeRoleEnvelope(contracts.RoleTopicPrefix+edgeLiveRole, summary)); err != nil {
		t.Fatalf("publish the role message: %v", err)
	}
	envelope, ok := nextRelayFrame(t, frames, 20*time.Second)
	if !ok {
		t.Fatalf("the role message %q did not reach the edge session through the shared listener within 20s:\nshared:\n%s\nedge:\n%s", summary, shared.Output.String(), edge.Output.String())
	}
	if envelope.PayloadSummary != summary || envelope.Topic != contracts.RoleTopicPrefix+edgeLiveRole {
		t.Fatalf("the edge session was relayed %q on %s, want %q on %s", envelope.PayloadSummary, envelope.Topic, summary, contracts.RoleTopicPrefix+edgeLiveRole)
	}
}

// edgeRoleClaim is the body of a hard claim of edgeLiveRole by session.
func edgeRoleClaim(session string) string {
	return `{"session_id":"` + session + `","role":"` + edgeLiveRole + `"}`
}

// An edge session takes a role another session of the edge holds. A claim that takes a role from
// another session writes a takeover record on the takeover lane first (publishTakeover), and the
// edge's grant publishes no takeover lane (natsgrant.Edge), so an edge listener writes none: the
// claim lands, answered 200, and the role lookup names the claimant. A record the edge had tried to
// write would be refused by the server, and the claim answered 503 with nothing written
// (errTakeoverUnrecorded).
func TestAnEdgeSessionTakesARoleAnotherSessionHolds(t *testing.T) {
	rig := newEdgeLiveRig(t, edgeLiveGrant(t))
	edge := rig.startEdge(t)
	edge.waitHealthy(t)
	rig.register(t, edge)
	rig.registerAs(t, edge, edgeLiveOtherSession, rig.otherToken)
	rig.mustCallAs(t, edge, rig.otherToken, http.MethodPost, "/v1/roles/set", edgeRoleClaim(edgeLiveOtherSession))
	rig.requireRoleHolder(t, edge, edgeLiveOtherSession)

	status, answer := rig.call(t, edge, http.MethodPost, "/v1/roles/set", edgeRoleClaim(edgeLiveSession))
	if status != http.StatusOK {
		t.Fatalf("session %s's claim of role %s from %s answered %d %s, want 200:\n%s", edgeLiveSession, edgeLiveRole, edgeLiveOtherSession, status, answer, edge.Output.String())
	}
	rig.requireRoleHolder(t, edge, edgeLiveSession)
}

// A role message on an edge session's own subject whose role another session holds reaches the
// edge session. A shared listener's session relay copies such a frame to the role's kept lane and
// acknowledges it rather than sending it (sessionRelays.takeOverFrame), and the edge's grant
// publishes no kept role lane (natsgrant.Edge), so an edge listener takes no frame: the frame goes
// to the session as every frame does. A copy the edge had tried to make would be refused by the
// server, and the frame given back to it at every delivery, never sent. Dispatch's user stands in
// for the shared listener that forwarded the message to the session's subject while the session
// held the role (TestARoleHeldAtTheEdgeIsReachedThroughASharedListener), on the subject and with
// the role topic that forward carries.
func TestAnEdgeSessionIsRelayedARoleMessageWhoseRoleAnotherSessionHolds(t *testing.T) {
	rig := newEdgeLiveRig(t, edgeLiveGrant(t))
	edge := rig.startEdge(t)
	edge.waitHealthy(t)
	frames, err := rig.connect(t, "observer").SubscribeSync(bus.DeliverSubject(edgeLiveSession))
	if err != nil {
		t.Fatalf("the observer reads the relay subject: %v", err)
	}
	rig.register(t, edge)
	rig.registerAs(t, edge, edgeLiveOtherSession, rig.otherToken)
	rig.mustCallAs(t, edge, rig.otherToken, http.MethodPost, "/v1/roles/set", edgeRoleClaim(edgeLiveOtherSession))
	rig.requireRoleHolder(t, edge, edgeLiveOtherSession)

	const summary = "forwarded while the edge session held the role"
	item := edgeRoleEnvelope(contracts.RoleTopicPrefix+edgeLiveRole, summary)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rig.dispatch.PublishStoredAck(ctx, contracts.LegacyAgentSubject(edgeLiveSession), item, item.DedupeKey); err != nil {
		t.Fatalf("store the role message on session %s's subject: %v", edgeLiveSession, err)
	}
	envelope, ok := nextRelayFrame(t, frames, 20*time.Second)
	if !ok {
		t.Fatalf("the role message %q did not reach session %s within 20s:\n%s", summary, edgeLiveSession, edge.Output.String())
	}
	if envelope.PayloadSummary != summary || envelope.Topic != contracts.RoleTopicPrefix+edgeLiveRole {
		t.Fatalf("session %s was relayed %q on %s, want %q on %s", edgeLiveSession, envelope.PayloadSummary, envelope.Topic, summary, contracts.RoleTopicPrefix+edgeLiveRole)
	}
	if output := edge.Output.String(); strings.Contains(output, "gave a role message back to the server") {
		t.Fatalf("the edge listener gave the role message back to the server:\n%s", output)
	}
}
