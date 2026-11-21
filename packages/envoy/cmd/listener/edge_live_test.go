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
	"io"
	"net/http"
	"regexp"
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
	// edgeLiveRole is the role the edge session claims: a Legion role of the edge's project, in
	// the new form, which the claim route takes (contracts.RoleProject).
	edgeLiveRole = "PRIV.legion-priv-reviewer"
)

// edgeServerViolation matches a permissions violation nats-server logs, capturing its kind and
// the subject it names.
var edgeServerViolation = regexp.MustCompile(`(Publish|Subscription) Violation - .*?Subject "([^"]*)"`)

// edgeLiveGrant is the edge listener's grant for edgeLiveMachine and edgeProject (natsgrant.Edge).
func edgeLiveGrant(t *testing.T) natsgrant.Permissions {
	t.Helper()
	grant, err := natsgrant.Edge(edgeLiveMachine, []string{edgeProject})
	if err != nil {
		t.Fatalf("the edge's grant: %v", err)
	}
	return grant
}

// edgeLiveRig is one hub-image server with the users a row names, the client of Dispatch's user
// that created the stream on it, as Dispatch's server does, and the broker the edge listener asks
// about its callers' tokens.
type edgeLiveRig struct {
	server interface {
		Logs(context.Context) (io.ReadCloser, error)
	}
	uri      string
	seeds    map[string]string
	dispatch *bus.Client
	broker   *ciphertokentest.StubBroker
	caller   *ciphertokentest.Caller
	token    string
	binary   string
}

// newEdgeLiveRig starts a server whose users are the edge listener's, holding edge (natsgrant.Edge
// unless a row changes it), Dispatch's, a shared listener's (natsgrant.ListenerHost) and the edge
// session's observer, which reads the session's relay subject and answers its frames as the
// relay socket would. Dispatch's user creates the stream, as Dispatch's server does. The broker
// says the edge session's token is a named token of the edge's project.
func newEdgeLiveRig(t *testing.T, edge natsgrant.Permissions) *edgeLiveRig {
	t.Helper()
	rig := &edgeLiveRig{seeds: map[string]string{}, binary: cmdtest.Build(t, "envoy-listener")}
	users := map[string]natsgrant.Permissions{
		"edge":     edge,
		"dispatch": natsgrant.Dispatch("example-dispatch"),
		"shared":   natsgrant.ListenerHost("example-shared"),
		// The observer stands in for the relay socket's serving replica: it reads the session's
		// relay subject and answers on whatever reply subject a frame names. An edge listener's
		// frames name a relay receipt (bus.RelayReplyPrefix), which the serving replica's grant
		// publishes, and its connection probe names the edge's own inbox.
		"observer": {Publish: []string{natsgo.InboxPrefix + ">", natsgrant.EdgeInboxPrefix(edgeLiveMachine) + ".>", bus.RelayReplyPrefix + ">"}, Subscribe: []string{bus.DeliverSubjectPrefix + "*"}},
	}
	var static []testnats.StaticUser
	for _, name := range []string{"edge", "dispatch", "shared", "observer"} {
		seed, public := testnats.User(t)
		rig.seeds[name] = seed
		grant := users[name]
		static = append(static, testnats.StaticUser{NKey: public, Publish: grant.Publish, Subscribe: grant.Subscribe})
	}
	rig.server, rig.uri = testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(static...), []string{"-js"}, false)
	dispatchSeed := rig.seeds["dispatch"]
	client, err := bus.ConnectOwningStream([]string{rig.uri}, bus.WithName("example-dispatch"), bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return dispatchSeed, true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("Dispatch's user creates the stream: %v", err)
	}
	t.Cleanup(client.Close)
	rig.dispatch = client

	rig.broker = ciphertokentest.NewStubBroker(t)
	rig.caller = ciphertokentest.NewCaller(t)
	rig.token = "access-token-of-the-edge-session"
	rig.broker.Says(rig.token, ciphertoken.Introspection{
		Active:       true,
		Session:      edgeLiveSession,
		Person:       "outside-person@example.test",
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{edgeProject},
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   rig.caller.JKT(),
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})
	return rig
}

// startEdge starts the edge listener as its user, an edge listener of edgeProject.
func (r *edgeLiveRig) startEdge(t *testing.T) *listenerProcess {
	t.Helper()
	return startEdgeListenerProcess(t, r.binary, r.uri, edgeLiveMachine, r.broker, bus.NkeySeedEnvVar+"="+r.seeds["edge"])
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
	origin := "http://127.0.0.1:" + strconv.Itoa(edge.port)
	signed := r.caller.Signed(t, method, origin+path, r.token)
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
	status, answer := r.call(t, edge, method, path, body)
	if status < 200 || status > 299 {
		t.Fatalf("%s %s: %d %s\n%s", method, path, status, answer, edge.Output.String())
	}
	return answer
}

// register registers the edge session through the edge listener with consumer delivery, and
// requires the answer to be consumer delivery on the session's relay subject.
func (r *edgeLiveRig) register(t *testing.T, edge *listenerProcess) {
	t.Helper()
	answer := r.mustCall(t, edge, http.MethodPost, "/v1/interests/subscribe",
		`{"session_id":"`+edgeLiveSession+`","topics":[],"self_subscribed":true,"delivery":{"consumer":true,"epoch":"edge-epoch"}}`)
	if !strings.Contains(answer, `"mode":"consumer"`) || !strings.Contains(answer, `"subject":"`+bus.DeliverSubject(edgeLiveSession)+`"`) {
		t.Fatalf("the edge session's registration was not answered consumer delivery on its relay subject: %s", answer)
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

// violations is every permissions violation the server has logged, as kind and subject.
func (r *edgeLiveRig) violations(t *testing.T) [][]string {
	t.Helper()
	logs, err := r.server.Logs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	var found [][]string
	for _, match := range edgeServerViolation.FindAllStringSubmatch(string(text), -1) {
		found = append(found, match[1:])
	}
	return found
}

// The edge listener connects as its user, rendered from natsgrant.Edge, starts, registers a
// session under a broker token of its project, gives it a consumer and relays a send to it on the
// session's relay subject, whose frame the observer answers on the reply subject the frame names.
// The server refuses the edge nothing: a publish the edge's own user is refused, made last, is the
// one violation the server logs, which shows the log is read. Without the edge's own inbox prefix
// in its grant the start ends, naming the prefix, since every reply the listener asks for comes
// under it.
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
		rig.mustCall(t, edge, http.MethodPost, "/v1/messages/send", `{"target_session":"`+edgeLiveSession+`","message":"example message to the edge"}`)
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
		deadline := time.Now().Add(10 * time.Second)
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
				t.Fatalf("the server logged no violation naming %s within 10s, so its log was not read", provoked)
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
		edge.WaitExit(t, "its start")
		output := edge.Output.String()
		if code := edge.Cmd.ProcessState.ExitCode(); code != 1 {
			t.Fatalf("the edge listener exited %d under a grant without its inbox prefix, want 1:\n%s", code, output)
		}
		// The start's own refusal, wherever it falls in the output: a closing client can log its
		// recovery's end after it.
		refusal := "connect to NATS and reconcile the " + bus.Stream + " stream: "
		var line string
		for candidate := range strings.SplitSeq(output, "\n") {
			if strings.Contains(candidate, refusal) {
				line = candidate
			}
		}
		if !strings.Contains(line, prefix) {
			t.Fatalf("the edge listener's start did not end naming %s; its refusal was %q:\n%s", prefix, line, output)
		}
		t.Logf("the start ended: %s", line)
	})
}

// edgeRoleEnvelope is a role message on the role subject of edgeLiveRole, as a publisher sends one.
func edgeRoleEnvelope(summary string) contracts.Envelope {
	eventID := id.New()
	return contracts.Envelope{
		EventID:        eventID,
		Source:         "envoy",
		SourceEventID:  eventID,
		Topic:          contracts.RoleTopicPrefix + edgeLiveRole,
		DedupeKey:      "edge-role-" + eventID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: summary,
		Payload:        `{"text":"` + summary + `"}`,
		TraceID:        id.New(),
	}
}

// An edge session claims a role of its project. A message published on the role's subject while
// no shared listener runs reaches no one: the edge listener subscribes no role lane, so it takes
// no share of any role's messages. Once a shared listener runs, as its own user, the next message
// is taken from the role lane by it, which resolves the edge session as the holder and forwards
// the message to the session's agent subject, and the edge listener relays it to the session.
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

	if err := rig.dispatch.Publish(edgeRoleEnvelope("before a shared listener runs")); err != nil {
		t.Fatalf("publish the role message: %v", err)
	}
	if envelope, ok := nextRelayFrame(t, frames, 5*time.Second); ok {
		t.Fatalf("a role message reached the edge session with no shared listener running: %q on %s:\n%s", envelope.PayloadSummary, envelope.Topic, edge.Output.String())
	}

	shared := rig.startShared(t)
	shared.waitHealthy(t)
	shared.WaitForOutput(t, `"msg":"role lane subscribed"`)
	const summary = "through the shared listener"
	if err := rig.dispatch.Publish(edgeRoleEnvelope(summary)); err != nil {
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
