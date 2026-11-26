package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A Legion agent whose issue pod the daemon released because it was idle has stopped as any other
// stopped agent has, and its daemon decides what its message does: the answer loop answers a
// question for it and wakes it for everything else. So the listener hands the agent's message to
// its daemon on notifications.legion.<project>.agents.<session>, and raises no wake report for a
// message it handed off, on every path a message reaches the hand-off by: a send to its session, a
// role publish, a send Dispatch marked with its role (a message and a picker mention of the role),
// and the give-up of a copy Dispatch put on its agent subject (a person's reply in a thread it
// wrote in). Each test's control is the same message for a target the registry does not name,
// which is kept or stored as before the hand-off existed and raises the wake.
//
// The registry's entry carries no `released` any more. Each test writes the released agent's entry
// as the daemon of 043f9c67 wrote the entry of a claim idle release suspended (putReleasedEntry):
// the listener reads past the field, while the listener of 043f9c67 read it and handed the
// agent's message to no daemon, which is what each test fails on with that code.

// putReleasedEntry writes entry to the Legion agent registry as the daemon of 043f9c67 wrote the
// entry of a session whose claim idle release suspended: the entry as the registry stores it
// (agents.Put, which writes its three keys and its recorded_at), then each of those keys again as
// raw bytes carrying "released": true, a field the registry's entry no longer declares.
func putReleasedEntry(t *testing.T, client *bus.Client, agents *legionagents.Store, entry legionagents.Entry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored, err := agents.Put(ctx, entry)
	if err != nil {
		t.Fatalf("record %s: %v", entry.SessionID, err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("encode %s's entry: %v", entry.SessionID, err)
	}
	released := append(encoded[:len(encoded)-1:len(encoded)-1], `,"released":true}`...)
	kv, err := client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the %s bucket: %v", bus.LegionAgentBucket, err)
	}
	for _, key := range []string{
		"session." + stored.SessionID,
		"role." + stored.RoleToken + "." + stored.SessionID,
		"issue." + stored.Issue + "." + stored.SessionID,
	} {
		if _, err := kv.Put(key, released); err != nil {
			t.Fatalf("write %s as the daemon of 043f9c67 wrote a released entry: %v", key, err)
		}
	}
}

// reportWakes has d's routes report their wakes, and reads them from here on.
func reportWakes(t *testing.T, d *listenerDeps) *natsgo.Subscription {
	t.Helper()
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	d.wakes = newWakeReporter(lifetime, d.client)
	return subscribeWakes(t, d.client.Conn)
}

// A send to a released agent that stopped and left its known-session record, as a released pod's
// session does: 200 with the agent as recipient and no state, one hand-off carrying the send,
// nothing stored on the agent's own subject, and no agent wake. The control, a session the
// registry does not name in the same shape, is stored for that session and answered stopped, and
// its wake is raised.
func TestReleasedAgentsSendIsHandedToItsDaemonAndRaisesNoWake(t *testing.T) {
	rig := newKnownSessionRig(t)
	const released, unnamed = "ses_released_send", "ses_unnamed_send"
	entry := legionAgentEntry(released, "INFRA-204", "tester")
	agents := withLegionAgent(t, rig.deps, entry)
	putReleasedEntry(t, rig.harness.client, agents, entry)
	for _, id := range []string{released, unnamed} {
		rig.register(t, id, nil)
		rig.stop(t, id)
		rig.record(t, id)
	}
	wakes := reportWakes(t, rig.deps)

	subject := contracts.LegacyAgentSubject(released)
	handed := rig.send(t, released, nil)
	if handed.status != http.StatusOK || handed.Recipient != released || handed.State != "" {
		t.Fatalf("a send to the released agent %s: status %d, body %s; want 200 handed off to its daemon, recipient %s and no state",
			released, handed.status, handed.raw, released)
	}
	count, data := handoffs(t, rig.harness.client, released)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs on %s, want the send's", count, "notifications.legion.infra.agents."+released)
	}
	if wire := decodeHandoff(t, data); wire.TargetSession != released || wire.OriginalTopic != subject || wire.Project != "infra" {
		t.Fatalf("the stored hand-off is %+v, want the send's topic handed to %s under the project infra", wire, released)
	}
	if held := rig.storedOn(t, subject); held != 0 {
		t.Fatalf("the stream holds %d messages on %s, want none: the send was handed off", held, subject)
	}
	requireNoWake(t, wakes, wakeKindAgent, released)

	stored := rig.send(t, unnamed, nil)
	stored.requireAnswer(t, "control: a send to a stopped session the registry does not name", sessionStateStopped, session.Gone)
	if held := rig.storedOn(t, contracts.LegacyAgentSubject(unnamed)); held != 1 {
		t.Fatalf("control: the stream holds %d messages on %s's subject, want the one the send stored", held, unnamed)
	}
	awaitWake(t, wakes, wakeKindAgent, unnamed)
	if count, _ := handoffs(t, rig.harness.client, unnamed); count != 0 {
		t.Fatalf("control: the stream holds %d hand-offs for %s, want none", count, unnamed)
	}
}

// A publish to a role no live session holds whose newest entry is a released agent: 200 with that
// agent as recipient, nothing kept on the role's kept lane, one hand-off carrying the role token,
// and no role wake. The control, a role the registry holds no entry for, is kept for the role's
// first holder, 202 kept unclaimed, and its wake is raised.
func TestReleasedRolesPublishIsHandedToItsDaemonAndRaisesNoWake(t *testing.T) {
	d, client := keptRoleDeps(t)
	const released = "ses_released_publish"
	entry := legionAgentEntry(released, "INFRA-207", "reviewer")
	role := entry.RoleToken
	agents := withLegionAgent(t, d, entry)
	putReleasedEntry(t, client, agents, entry)
	wakes := reportWakes(t, d)
	publish := func(role string) string {
		return `{"topic":"` + contracts.RoleTopicPrefix + role + `","source":"agent","source_session":"ses-architect","message":"what did the review find?"}`
	}

	recorder, _ := publishToRole(t, d, role, publish(role))
	var handed handedPublishAnswer
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &handed) != nil || handed.Kept || handed.Recipient != released {
		t.Fatalf("a publish to the role %s whose newest agent %s is released: status %d, body %s; want 200 handed off to %s with nothing kept",
			role, released, recorder.Code, recorder.Body.String(), released)
	}
	count, data := handoffs(t, client, released)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs on %s, want the publish's", count, "notifications.legion.infra.agents."+released)
	}
	if wire := decodeHandoff(t, data); wire.TargetSession != released || wire.RoleToken != role || wire.OriginalTopic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the stored hand-off is %+v, want the publish to %s handed to %s", wire, role, released)
	}
	assertNothingKept(t, client, role)
	requireNoWake(t, wakes, wakeKindRole, role)

	unheld := legionagents.RoleToken("infra", "INFRA-208", "reviewer")
	recorder, kept := publishToRole(t, d, unheld, publish(unheld))
	if recorder.Code != http.StatusAccepted || !kept.Kept || kept.KeptReason != "unclaimed" {
		t.Fatalf("control: a publish to the role %s the registry holds no entry for: status %d, body %s; want 202 kept unclaimed for its first holder",
			unheld, recorder.Code, recorder.Body.String())
	}
	waitForKeptRoleMessage(t, client, unheld, kept.EventID)
	awaitWake(t, wakes, wakeKindRole, unheld)
}

// A send Dispatch marked with a role, as its message and its picker mention of `role:<token>`
// arrive when it pinned the role's stopped newest session, whose entry is a released agent: 200
// with that agent as recipient and no state, one hand-off carrying the role token, nothing kept on
// the role's kept lane and nothing stored on the agent's subject, and no role wake. The control,
// the same send marked with a role the registry holds no entry for, is kept for the role's first
// holder, 202 kept, and its wake is raised.
func TestReleasedRolesMarkedSendIsHandedToItsDaemonAndRaisesNoWake(t *testing.T) {
	rig := newKnownSessionRig(t)
	const principal = "system:serviceaccount:dispatch:example-dispatch"
	useDispatchPrincipal(rig.deps, principal)
	const released = "ses_released_marked"
	entry := legionAgentEntry(released, "INFRA-209", "planner")
	role := entry.RoleToken
	agents := withLegionAgent(t, rig.deps, entry)
	putReleasedEntry(t, rig.harness.client, agents, entry)
	rig.register(t, released, nil)
	rig.stop(t, released)
	rig.record(t, released)
	client := rig.harness.client
	wakes := reportWakes(t, rig.deps)

	handed := sendMarkedByDispatch(t, rig.mux, principal, released, role, "what did the plan leave out?")
	if handed.status != http.StatusOK || handed.Recipient != released || handed.State != "" || handed.Kept {
		t.Fatalf("a send marked with the role %s to its released newest agent: status %d, body %s; want 200 handed off to %s with no state and nothing kept",
			role, handed.status, handed.raw, released)
	}
	count, data := handoffs(t, client, released)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs on %s, want the send's", count, "notifications.legion.infra.agents."+released)
	}
	if wire := decodeHandoff(t, data); wire.TargetSession != released || wire.RoleToken != role || wire.Project != "infra" {
		t.Fatalf("the stored hand-off is %+v, want the send handed to %s on the role token %s", wire, released, role)
	}
	assertNothingKept(t, client, role)
	if held := rig.storedOn(t, contracts.LegacyAgentSubject(released)); held != 0 {
		t.Fatalf("the stream holds %d messages on %s's subject, want none: the send was handed off", held, released)
	}
	requireNoWake(t, wakes, wakeKindRole, role)

	unheld := legionagents.RoleToken("infra", "INFRA-210", "planner")
	kept := sendMarkedByDispatch(t, rig.mux, principal, "ses_unheld_marked", unheld, "who plans this?")
	if kept.status != http.StatusAccepted || !kept.Kept || kept.KeptUntil == 0 {
		t.Fatalf("control: a send marked with the role %s the registry holds no entry for: status %d, body %s; want 202 kept for its first holder",
			unheld, kept.status, kept.raw)
	}
	waitForKeptRoleMessage(t, client, unheld, kept.EventID)
	awaitWake(t, wakes, wakeKindRole, unheld)
}

// A person's reply in a comment thread a released agent wrote in, which Dispatch puts on that
// agent's subject: the agent left an interest row naming this listener and no session entry, so
// its delivery finds the session gone and gives it up, and the give-up hands it to the agent's
// daemon in place of its no_holder notice and its wake. The control, a session the registry does
// not name in the same shape, is given up with its no_holder notice and its wake.
func TestReleasedAgentsThreadReplyIsHandedToItsDaemonAndRaisesNoWake(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const released, unnamed = "ses_released_thread", "ses_unnamed_thread"
	agents, err := legionagents.Open(t.Context(), harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	putReleasedEntry(t, harness.client, agents, legionAgentEntry(released, "INFRA-211", "architect"))
	stateAnswering(t, agents, true)

	cfg := harness.config
	cfg.handoff = &legionHandoff{
		client: cfg.client, registry: cfg.registry, sessions: cfg.sessions,
		agents: newLegionAgentRegistry(agents), logger: logging.NewWithWriter("test", harness.logs),
	}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	cfg.wakes = newWakeReporter(lifetime, harness.client)
	wakes := subscribeWakes(t, harness.client.Conn)
	handler := durableTestHandler(t, cfg)
	reply := func(sessionID string) (contracts.Envelope, *natsgo.Subscription) {
		t.Helper()
		if _, err := harness.registry.Upsert(store.Interest{
			SessionID: sessionID, MachineID: cfg.machineID, Dir: "/legion/workspaces/infra-211",
		}, nil); err != nil {
			t.Fatalf("record the interest row %s left: %v", sessionID, err)
		}
		item := envelopeFor(contracts.AgentTopicPrefix+sessionID, "dispatch-thread-reply-"+sessionID, "dispatch")
		probe, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + item.Topic)
		if err != nil {
			t.Fatalf("subscribe the exception probe: %v", err)
		}
		t.Cleanup(func() { _ = probe.Unsubscribe() })
		if err := harness.client.Conn.Flush(); err != nil {
			t.Fatalf("flush the exception probe: %v", err)
		}
		handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
		return item, probe
	}
	givenUp := func(sessionID string) int {
		count := 0
		for _, record := range deliveryRecords(t, harness.logs.String(), "listener agent delivery given up") {
			if record.SessionID == sessionID {
				count++
			}
		}
		return count
	}

	item, probe := reply(released)
	count, data := handoffs(t, harness.client, released)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs on %s, want the reply's. The listener said:\n%s",
			count, "notifications.legion.infra.agents."+released, harness.logs.String())
	}
	if wire := decodeHandoff(t, data); wire.TargetSession != released || wire.OriginalTopic != item.Topic {
		t.Fatalf("the stored hand-off is %+v, want the reply on %s handed to %s", wire, item.Topic, released)
	}
	if exception, err := probe.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the reply handed off raised the exception %s, want none", exception.Data)
	}
	if count := givenUp(released); count != 0 {
		t.Fatalf("%d lines give the reply to %s up, want none: it was handed off:\n%s", count, released, harness.logs.String())
	}
	requireNoWake(t, wakes, wakeKindAgent, released)

	item, probe = reply(unnamed)
	assertDeliveryException(t, probe, item, "no_holder")
	if count := givenUp(unnamed); count != 1 {
		t.Fatalf("control: %d lines give the reply to %s up, want one:\n%s", count, unnamed, harness.logs.String())
	}
	awaitWake(t, wakes, wakeKindAgent, unnamed)
	if count, _ := handoffs(t, harness.client, unnamed); count != 0 {
		t.Fatalf("control: the stream holds %d hand-offs for %s, want none", count, unnamed)
	}
}
