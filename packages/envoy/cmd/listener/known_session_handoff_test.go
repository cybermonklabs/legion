package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// legionAgentEntry is the registry's entry for a phase worker of the Legion project infra.
func legionAgentEntry(sessionID, issue, role string) legionagents.Entry {
	return legionagents.Entry{
		SessionID: sessionID, LegionProject: "infra", Issue: issue, Role: role,
		RoleToken: legionagents.RoleToken("infra", issue, role), Title: "Legion " + role + " · " + issue,
		Harness: "omp", StartedAt: "2026-10-26T03:00:00Z",
	}
}

// putLegionAgent writes entry to the Legion agent registry, as the daemon writes it.
func putLegionAgent(t *testing.T, agents *legionagents.Store, entry legionagents.Entry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := agents.Put(ctx, entry); err != nil {
		t.Fatalf("record %s: %v", entry.SessionID, err)
	}
}

// stateAnswering writes the project infra's answering record, as the daemon writes it.
func stateAnswering(t *testing.T, agents *legionagents.Store, answering bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: answering, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project's answering is %t: %v", answering, err)
	}
}

// withLegionAgent puts a Legion agent registry on d holding entry, states that the project infra
// answers, and puts the hand-off on d over the registry as main wires it. It answers the registry,
// so a test can write the entry or the project's flag again.
func withLegionAgent(t *testing.T, d *listenerDeps, entry legionagents.Entry) *legionagents.Store {
	t.Helper()
	agents, err := legionagents.Open(t.Context(), d.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	putLegionAgent(t, agents, entry)
	stateAnswering(t, agents, true)
	wrapped := newLegionAgentRegistry(agents)
	d.legionAgents = wrapped
	d.handoff = &legionHandoff{client: d.client, registry: d.registry, sessions: d.sessions, agents: wrapped, logger: logging.New("known-session-handoff")}
	d.delivery.handoff = d.handoff
	return agents
}

// The case #2643 made a 200 stopped, for a Legion agent: a phase worker that stopped, whose
// session left no entry within the gone horizon and whose known-session record the store still
// holds, under a project that last said it answers. A send to it is handed to its daemon ahead of
// that record, which would store the message for a session that runs again, while the daemon
// answers it now from the agent's own conversation: 200 with the agent as recipient and no state,
// one hand-off on notifications.legion.infra.agents.<session> carrying the send, and nothing stored
// on the agent's own subject.
//
// The controls are the same send under each thing that turns the hand-off away, each then
// answered as main answers a session only the known-session store holds, and none handed off: a
// connected_only send is the 409 that stores nothing, since a daemon answering in the agent's
// place is not the agent connected; a send that carried "handoff": false, and one to a project
// that last said answering: false, are stored for the session and answered stopped.
func TestASendToAStoppedLegionAgentTheKnownStoreHoldsIsHandedToItsDaemon(t *testing.T) {
	rig := newKnownSessionRig(t)
	const agent = "ses_known_stopped_impl"
	agents := withLegionAgent(t, rig.deps, legionAgentEntry(agent, "INFRA-204", "implementer"))
	rig.register(t, agent, nil)
	rig.stop(t, agent)
	rig.record(t, agent)
	subject := contracts.LegacyAgentSubject(agent)
	requireHandoffs := func(want uint64, after string) {
		t.Helper()
		if count, data := handoffs(t, rig.harness.client, agent); count != want {
			t.Fatalf("after %s the stream holds %d hand-offs for %s, want %d; the last: %s", after, count, agent, want, data)
		}
	}
	requireStored := func(want uint64, after string) {
		t.Helper()
		if held := rig.storedOn(t, subject); held != want {
			t.Fatalf("after %s the stream holds %d messages on %s, want %d", after, held, subject, want)
		}
	}

	handed := rig.send(t, agent, nil)
	if handed.status != http.StatusOK || handed.Recipient != agent || handed.State != "" {
		t.Fatalf("a send to the stopped Legion agent %s the known-session store holds: status %d, body %s; want 200 handed off to its daemon, recipient %s and no state",
			agent, handed.status, handed.raw, agent)
	}
	requireHandoffs(1, "the send")
	_, data := handoffs(t, rig.harness.client, agent)
	if wire := decodeHandoff(t, data); wire.TargetSession != agent || wire.OriginalTopic != subject || wire.Project != "infra" {
		t.Fatalf("the stored hand-off is %+v, want the send's topic handed to %s under the project infra", wire, agent)
	}
	requireStored(0, "the send handed off")

	refused := rig.send(t, agent, map[string]any{"connected_only": true})
	if refused.status != http.StatusConflict || refused.raw != `{"error":"the session is stopped"}`+"\n" {
		t.Fatalf("a connected_only send to the stopped Legion agent %s: status %d, body %s; want 409 the session is stopped",
			agent, refused.status, refused.raw)
	}
	requireHandoffs(1, "the connected_only send")
	requireStored(0, "the connected_only send")

	optedOut := rig.send(t, agent, map[string]any{"handoff": false})
	optedOut.requireAnswer(t, "a send to the stopped Legion agent that carried \"handoff\": false", sessionStateStopped, session.Gone)
	requireHandoffs(1, "the send that opted out")
	requireStored(1, "the send that opted out")

	stateAnswering(t, agents, false)
	notAnswering := rig.send(t, agent, nil)
	notAnswering.requireAnswer(t, "a send to the stopped Legion agent of a project that said answering: false", sessionStateStopped, session.Gone)
	requireHandoffs(1, "the send to a project that does not answer")
	requireStored(2, "the send to a project that does not answer")
}

// markedSendAnswer is one marked send's answer as the test below reads it: the status and raw
// body, and the fields of a hand-off's 200 and of a keep's 202.
type markedSendAnswer struct {
	status int
	raw    string

	EventID   string `json:"event_id"`
	Recipient string `json:"recipient"`
	State     string `json:"state"`
	Kept      bool   `json:"kept"`
	KeptUntil int64  `json:"kept_until"`
}

// sendMarkedByDispatch sends one message to target marked with role through the /v1 routes mux
// holds, as Dispatch's send for a route to role arrives behind apiAuth: from principal, which the
// routes' deps name as Dispatch's (useDispatchPrincipal) and the request's context carries as
// apiAuth verified it (withVerifiedPrincipal), with Dispatch's own source and no source session.
func sendMarkedByDispatch(t *testing.T, mux *http.ServeMux, principal, target, role, message string) markedSendAnswer {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"target_session": target, "role": role, "source": "dispatch", "message": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := postJSON("/v1/messages/send", string(encoded))
	request = request.WithContext(withVerifiedPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	answer := markedSendAnswer{status: recorder.Code, raw: recorder.Body.String()}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the marked send's answer %s: %v", answer.raw, err)
	}
	return answer
}

// Dispatch marks a role's message with the role when it sends it to the session it pinned for the
// role, and its recheck of that session before the send names no registry redirect once the
// listener holds the session's known-session record, as it does for 30 days after the session
// stopped. So a mention or a message for a Legion role with no live holder reaches the send route
// marked, aimed at the role's stopped newest session, with no claim of the role standing. The send
// route offers it to the role's daemon on the role token (handOffOrKeepRoleSend): 200 with that
// session as recipient and no state, one hand-off carrying the role token, nothing kept on the
// role's kept lane, and nothing stored on the session's own subject.
//
// The control is the same send once the project says it does not answer: no daemon answers for the
// role's agent, so the message is kept on the role's kept lane for the role's next holder, 202
// kept, as #4387 keeps a marked send whose role's claim does not name its target, and nothing more
// is handed off.
func TestADispatchSendMarkedWithAStoppedRoleIsHandedToItsDaemon(t *testing.T) {
	rig := newKnownSessionRig(t)
	const principal = "system:serviceaccount:dispatch:example-dispatch"
	useDispatchPrincipal(rig.deps, principal)
	const agent = "ses_finished_role_marked"
	stopped := legionAgentEntry(agent, "INFRA-206", "reviewer")
	role := stopped.RoleToken
	agents := withLegionAgent(t, rig.deps, stopped)
	rig.register(t, agent, nil)
	rig.stop(t, agent)
	rig.record(t, agent)
	client := rig.harness.client
	subject := contracts.LegacyAgentSubject(agent)

	handed := sendMarkedByDispatch(t, rig.mux, principal, agent, role, "what did the review find?")
	if handed.status != http.StatusOK || handed.Recipient != agent || handed.State != "" || handed.Kept {
		t.Fatalf("a send marked with the role %s to its stopped newest session: status %d, body %s; want 200 handed off to %s with no state and nothing kept",
			role, handed.status, handed.raw, agent)
	}
	stored, data := handoffs(t, client, agent)
	if stored != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s, want 1", stored, agent)
	}
	if wire := decodeHandoff(t, data); wire.TargetSession != agent || wire.RoleToken != role || wire.OriginalTopic != subject || wire.Project != "infra" {
		t.Fatalf("the stored hand-off is %+v, want the send to %s handed off on the role token %s under the project infra", wire, agent, role)
	}
	assertNothingKept(t, client, role)
	if held := rig.storedOn(t, subject); held != 0 {
		t.Fatalf("the stream holds %d messages on %s, want none: the send was handed off", held, subject)
	}

	stateAnswering(t, agents, false)
	kept := sendMarkedByDispatch(t, rig.mux, principal, agent, role, "are you back?")
	if kept.status != http.StatusAccepted || !kept.Kept || kept.KeptUntil == 0 || kept.Recipient != agent {
		t.Fatalf("control: the same send once the project does not answer: status %d, body %s; want 202 kept for the role's next holder",
			kept.status, kept.raw)
	}
	waitForKeptRoleMessage(t, client, role, kept.EventID)
	if count, _ := handoffs(t, client, agent); count != 1 {
		t.Fatalf("control: the stream holds %d hand-offs for %s, want only the send's while its project answered", count, agent)
	}
}
