package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

const (
	// handoffAgent is the stopped implementer the hand-off tests address, under the Legion project
	// token infra (legionEntry).
	handoffAgent     = "ses-impl"
	handoffRoleToken = "legion-infra-infra-204-architect"
	handoffArchitect = "ses-arch"
)

// handoffRig is one listener's hand-off over its own connection, with the /v1 routes that write
// the registry and the project's answering record beside it. liveWindow is how long a
// registration counts as live, so a test can put a session that is unresponsive but not gone.
type handoffRig struct {
	*legionListener
	client   *bus.Client
	sessions *session.SessionRegistry
	agents   *legionagents.Store
	handoff  *legionHandoff
}

func newHandoffRig(t *testing.T, liveWindow time.Duration) *handoffRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	options := []session.SessionRegistryOption{session.WithSessionReplicas(1)}
	if liveWindow > 0 {
		options = append(options, session.WithSessionLiveWindow(liveWindow), session.WithSessionGoneHorizon(time.Hour))
	}
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, options...)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the session cache: %v", err)
	}
	agents, err := legionagents.Open(t.Context(), client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	logger := logging.New("handoff-test")
	wrapped := newLegionAgentRegistry(agents)
	handoff := &legionHandoff{client: client, registry: registry, sessions: sessions, agents: wrapped, logger: logger}
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions, legionAgents: wrapped, handoff: handoff}
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, "handoff-test", logger)
	return &handoffRig{
		legionListener: &legionListener{deps: deps, mux: mux},
		client:         client, sessions: sessions, agents: agents, handoff: handoff,
	}
}

// register records a Legion agent of the project infra through the registry's route.
func (r *handoffRig) register(t *testing.T, sessionID, issue, role, started string) {
	t.Helper()
	r.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/"+sessionID, legionEntry(sessionID, issue, role, started))
}

// registerFinished records a Legion agent whose issue has left the workflow, so no next holder of
// its role is coming: the one shape a role's message is handed off for.
func (r *handoffRig) registerFinished(t *testing.T, sessionID, issue, role, started string) {
	t.Helper()
	r.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/"+sessionID, finishedLegionEntry(sessionID, issue, role, started))
}

// beat writes the project infra's answering record through its route, as the daemon writes it.
func (r *handoffRig) beat(t *testing.T, answering bool, at time.Time) {
	t.Helper()
	r.want(t, http.StatusOK, http.MethodPut, "/v1/legion/projects/infra",
		fmt.Sprintf(`{"answering":%t,"updated_at":%q}`, answering, at.UTC().Format("2006-01-02T15:04:05.000Z07:00")))
}

// envelopeFor is the envelope a send or a publish would have carried to target.
func envelopeFor(topic, dedupeKey, source string) contracts.Envelope {
	return contracts.Envelope{
		EventID: dedupeKey, Source: source, SourceEventID: dedupeKey, Topic: topic,
		DedupeKey: dedupeKey, IssuedAt: time.Now().UnixMilli(),
		PayloadSummary: "can you answer this?", TraceID: dedupeKey,
	}
}

// handoffs is how many hand-offs the stream holds for sessionID, and the last one's bytes.
func handoffs(t *testing.T, client *bus.Client, sessionID string) (uint64, []byte) {
	t.Helper()
	subject := listenerwire.LegionHandoffSubject("infra", sessionID)
	info, err := client.JS().StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	count := info.State.Subjects[subject]
	if count == 0 {
		return 0, nil
	}
	last, err := client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the last hand-off on %s: %v", subject, err)
	}
	return count, last.Data
}

// wireHandoff is a hand-off as LegionAgentHandoffSchema and intake.Handoff read it: exactly these
// five fields.
type wireHandoff struct {
	OriginalTopic string          `json:"original_topic"`
	TargetSession string          `json:"target_session"`
	RoleToken     string          `json:"role_token"`
	Project       string          `json:"project"`
	Envelope      json.RawMessage `json:"envelope"`
}

// decodeHandoff reads data strictly as a hand-off whose envelope is a valid wire envelope.
func decodeHandoff(t *testing.T, data []byte) wireHandoff {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var handoff wireHandoff
	if err := decoder.Decode(&handoff); err != nil {
		t.Fatalf("decode hand-off %s: %v", data, err)
	}
	if err := contracts.ValidateWire(handoff.Envelope); err != nil {
		t.Fatalf("the hand-off's envelope is not a wire envelope: %v\n%s", err, handoff.Envelope)
	}
	return handoff
}

// The project route stores the answering record and answers it; a path that is not a Legion
// project token, a body without answering or with an updated_at that is not RFC 3339 UTC, a body
// that is not JSON and a method other than PUT are refused and write nothing.
func TestLegionProjectRouteStoresTheAnsweringRecord(t *testing.T) {
	rig := newHandoffRig(t, 0)
	body := rig.want(t, http.StatusOK, http.MethodPut, "/v1/legion/projects/infra",
		`{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`)
	if strings.TrimSpace(body) != `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}` {
		t.Fatalf("the route answered %s, want the record as stored", body)
	}
	record, found, err := rig.agents.Project(context.Background(), "infra")
	if err != nil || !found || !record.Answering || record.UpdatedAt != "2026-10-26T04:00:00Z" {
		t.Fatalf("the bucket holds %#v, %t, %v; want the record the route stored", record, found, err)
	}
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
	}{
		{"an upper-case project", http.MethodPut, "/v1/legion/projects/Infra", `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`, http.StatusBadRequest},
		{"no project", http.MethodPut, "/v1/legion/projects/", `{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`, http.StatusBadRequest},
		{"no answering", http.MethodPut, "/v1/legion/projects/infra", `{"updated_at":"2026-10-26T04:00:00Z"}`, http.StatusBadRequest},
		{"a local time", http.MethodPut, "/v1/legion/projects/infra", `{"answering":true,"updated_at":"2026-10-26T04:00:00+01:00"}`, http.StatusBadRequest},
		{"no json", http.MethodPut, "/v1/legion/projects/infra", `nope`, http.StatusBadRequest},
		{"a GET", http.MethodGet, "/v1/legion/projects/infra", ``, http.StatusMethodNotAllowed},
	} {
		rig.want(t, tc.status, tc.method, tc.target, tc.body)
	}
	after, _, err := rig.agents.Project(context.Background(), "infra")
	if err != nil || after != record {
		t.Fatalf("the refusals left %#v, %v; want the first record untouched", after, err)
	}
}

// A send to a stopped Legion agent whose project last said it answers, and a role publish whose
// role is finished, are handed to that project's daemon on the retained subject, carrying the
// envelope whole. Two hand-offs of one envelope - the copies two listeners' durables make, and a
// send repeated under one idempotency key - collapse on their MsgId, so the stream stores one, and
// the second is answered duplicate.
func TestHandOffToLegionHandsAStoppedAgentToItsDaemon(t *testing.T) {
	rig := newHandoffRig(t, 0)
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")
	rig.registerFinished(t, handoffArchitect, "INFRA-204", "architect", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())

	item := envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+".k1", "agent")
	handed, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item, session: handoffAgent})
	if err != nil || handed == nil {
		t.Fatalf("a send to a stopped registered agent = %v, %v; want it handed off", handed, err)
	}
	if handed.TargetSession != handoffAgent || handed.Project != "infra" || handed.RoleToken != "legion-infra-infra-204-implementer" {
		t.Fatalf("handed off %#v, want the agent's own session, project and role token", *handed)
	}
	count, data := handoffs(t, rig.client, handoffAgent)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s, want 1", count, handoffAgent)
	}
	wire := decodeHandoff(t, data)
	if wire.OriginalTopic != item.Topic || wire.TargetSession != handoffAgent || wire.Project != "infra" {
		t.Fatalf("the stored hand-off is %#v, want the message's topic and the agent's session", wire)
	}
	if handed.duplicate {
		t.Fatal("the first hand-off of an envelope answered duplicate; the stream held nothing")
	}
	// The same envelope handed off again - another listener's durable, a repeated send - is one
	// stored message, because every listener derives the MsgId from the envelope's dedupe key, and
	// the repeat answers that the stream already held it.
	again, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item, session: handoffAgent})
	if err != nil || again == nil {
		t.Fatalf("the second hand-off of one envelope = %v, %v; want it handed off", again, err)
	}
	if !again.duplicate {
		t.Fatalf("the second hand-off of one envelope answered %+v, want duplicate: the stream already held it", *again)
	}
	if count, _ := handoffs(t, rig.client, handoffAgent); count != 1 {
		t.Fatalf("the stream holds %d copies of one hand-off, want 1", count)
	}

	// A role publish with no live holder, whose role's issue has left the workflow, resolves to
	// the role token's newest session.
	role := envelopeFor(contracts.RoleTopicPrefix+handoffRoleToken, "publish.k2", "agent")
	handedRole, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: role, roleToken: handoffRoleToken})
	if err != nil || handedRole == nil || handedRole.TargetSession != handoffArchitect || handedRole.duplicate {
		t.Fatalf("a role publish with no live holder = %v, %v; want it handed to %s, not a duplicate", handedRole, err, handoffArchitect)
	}
	if count, _ := handoffs(t, rig.client, handoffArchitect); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for the architect, want 1", count)
	}
}

// A send repeated under one idempotency key to a stopped Legion agent, and a role publish repeated
// under one key to a finished Legion role, are each one hand-off on the stream, and the repeat is
// answered as a live session's repeat is (TestSendHandler_ReportsAJetStreamDuplicate): the same
// 200 with duplicate set, naming the envelope the repeat minted. The controls are the first of
// each and a send under another key, which are hand-offs of their own and answer no duplicate.
func TestARepeatedHandOffIsAnsweredDuplicate(t *testing.T) {
	rig := newHandoffRig(t, 0)
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())
	send := func(key string) listenerwire.SendResponse {
		t.Helper()
		recorder := rig.do(t, http.MethodPost, "/v1/messages/send",
			fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"can you answer this?","idempotency_key":%q}`, handoffAgent, key))
		var answer listenerwire.SendResponse
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil || answer.Recipient != handoffAgent {
			t.Fatalf("a send to the stopped agent %s under %s = %d %s, want 200 handed off to it", handoffAgent, key, recorder.Code, recorder.Body.String())
		}
		return answer
	}
	if first := send("message-1:steer"); first.Duplicate {
		t.Fatalf("the first send under its key answered %+v, want no duplicate: the stream held nothing", first)
	}
	repeat := send("message-1:steer")
	if !repeat.Duplicate || repeat.EventID == "" {
		t.Fatalf("the repeat under one idempotency key answered %+v, want duplicate, naming the envelope it minted", repeat)
	}
	if count, _ := handoffs(t, rig.client, handoffAgent); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s after a send and its repeat, want 1", count, handoffAgent)
	}
	if other := send("message-2:steer"); other.Duplicate {
		t.Fatalf("a send under another key answered %+v, want no duplicate", other)
	}

	d, client := keptRoleDeps(t)
	withFinishedHandoff(t, d)
	publish := func() listenerwire.PublishResponse {
		t.Helper()
		recorder, _ := publishThrough(d, `{"topic":"`+contracts.RoleTopicPrefix+finishedLegionRole+
			`","source":"agent","source_session":"ses-architect","message":"what did the review find?","idempotency_key":"ask-1"}`)
		var answer listenerwire.PublishResponse
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil || answer.Recipient != finishedRoleSession {
			t.Fatalf("a publish to the finished role %s = %d %s, want 200 handed off to %s",
				finishedLegionRole, recorder.Code, recorder.Body.String(), finishedRoleSession)
		}
		return answer
	}
	if first := publish(); first.Duplicate {
		t.Fatalf("the first publish under its key answered %+v, want no duplicate: the stream held nothing", first)
	}
	if again := publish(); !again.Duplicate || again.EventID == "" {
		t.Fatalf("the publish repeated under one key answered %+v, want duplicate, naming the envelope it minted", again)
	}
	if count, _ := handoffs(t, client, finishedRoleSession); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s after a publish and its repeat, want 1", count, finishedRoleSession)
	}
}

// Spec v21, section 2.12: the hand-off keys on the project's last stated answering flag, never on
// its freshness. A daemon that stated answering: true and then stopped - its record minutes or
// hours old, and one dated ahead of this listener's clock - still has its agents' messages handed
// to it. A project that stated answering: false, and one that stated nothing at all, keep the
// refusal they had before hand-off.
func TestHandOffToLegionKeysOnTheLastStatedAnsweringFlag(t *testing.T) {
	rig := newHandoffRig(t, 0)
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")

	hand := func(key string) *legionHandedOff {
		t.Helper()
		item := envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+"."+key, "agent")
		handed, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item, session: handoffAgent})
		if err != nil {
			t.Fatalf("hand off %s: %v", key, err)
		}
		return handed
	}

	// No record at all: today's refusal.
	if handed := hand("none"); handed != nil {
		t.Fatalf("a project that stated nothing handed off %#v, want the refusal it had before", *handed)
	}
	// answering: false: today's refusal, however fresh the record.
	rig.beat(t, false, time.Now())
	if handed := hand("false"); handed != nil {
		t.Fatalf("a project that stated answering: false handed off %#v, want today's 404", *handed)
	}
	// answering: true, stated a day ago by a daemon that has since stopped: handed off.
	rig.beat(t, true, time.Now().Add(-24*time.Hour))
	if handed := hand("stale"); handed == nil {
		t.Fatal("a stopped daemon that last stated answering: true did not get the hand-off; 2.12 drops the freshness rule")
	}
	// A record dated ahead of this listener's clock is the same statement.
	rig.beat(t, true, time.Now().Add(time.Hour))
	if handed := hand("ahead"); handed == nil {
		t.Fatal("a record dated ahead did not get the hand-off; only the flag counts")
	}
	// And back to false: the refusal returns at once.
	rig.beat(t, false, time.Now())
	if handed := hand("falseagain"); handed != nil {
		t.Fatalf("a project that stated answering: false again handed off %#v", *handed)
	}
}

// What the hand-off never takes, with the project answering and the agent registered throughout:
// a session the registry does not name, a role token that is no Legion workflow role's, a role
// token whose newest entry is not finished - its issue is still in the workflow, so the role's
// next worker is coming and the message is kept for it (INFRA-514) - a source of envoy (the
// broker's wake sends and the listener's own exceptions), a body that carried "handoff": false
// (every publish the daemon makes), a session this listener holds live, and a session past the
// live window that is not gone - it keeps its claims and its stored messages and may answer in a
// moment, so the message stays the agent's.
func TestHandOffToLegionLeavesTheseAsTheyWere(t *testing.T) {
	rig := newHandoffRig(t, time.Nanosecond)
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())
	item := func(key string) contracts.Envelope {
		return envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+"."+key, "agent")
	}

	for _, tc := range []struct {
		name   string
		target legionHandoffTarget
	}{
		{"a session the registry does not name", legionHandoffTarget{envelope: item("a"), session: "ses-unknown"}},
		{"a session id the registry would not hold", legionHandoffTarget{envelope: item("b"), session: "ses bad/id"}},
		{"a role token that is no Legion role's", legionHandoffTarget{envelope: item("c"), roleToken: "reviewer"}},
		{"a role token no session holds", legionHandoffTarget{envelope: item("d"), roleToken: "legion-infra-infra-999-merger"}},
		{"a role token whose issue is still in the workflow", legionHandoffTarget{envelope: item("d2"), roleToken: "legion-infra-infra-204-implementer"}},
		{"a source of envoy", legionHandoffTarget{envelope: envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+".e", "envoy"), session: handoffAgent}},
		{"a body that opted out", legionHandoffTarget{envelope: item("f"), session: handoffAgent, optedOut: true}},
	} {
		handed, err := rig.handoff.handOffToLegion(context.Background(), tc.target)
		if err != nil || handed != nil {
			t.Errorf("%s = %v, %v; want it answered as before, with nothing handed off", tc.name, handed, err)
		}
	}

	// A session this listener holds a registration for, past the live window but inside the gone
	// horizon: unresponsive, not stopped. The registry's own `live` is false for it, and the
	// hand-off still refuses, because the agent keeps its claims and its stored messages.
	if err := rig.sessions.Put(handoffAgent, session.SessionEntry{
		SelfSubscribed: true, MachineID: "handoff-test", Title: "Legion implementer · INFRA-204",
	}); err != nil {
		t.Fatalf("register %s: %v", handoffAgent, err)
	}
	if _, state := rig.sessions.Lookup(handoffAgent); state != session.Unresponsive {
		t.Fatalf("the session reads %s, want unresponsive: the case below would prove nothing", state)
	}
	handed, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item("g"), session: handoffAgent})
	if err != nil || handed != nil {
		t.Fatalf("an unresponsive session that is not gone = %v, %v; want it answered as before", handed, err)
	}

	if count, data := handoffs(t, rig.client, handoffAgent); count != 0 {
		t.Fatalf("the stream holds %d hand-offs, want none; the last: %s", count, data)
	}
}

// The counterpart of TestAMessageToAStoppedAgentWhoseInterestNamesThisListenerIsHandedToItsDaemon
// (stopped_agent_thread_reply_test.go): the same interest-path miss - a stored row naming this
// listener, no session entry behind it - for a session the Legion agent registry does not hold is
// answered exactly as it is with no hand-off, the give-up of a session with no record
// (unplaceableAgentDelivery): nothing is handed off, the no_holder exception goes out and the
// agent wake is reported. Every session that is not a Legion agent's is in this shape, which is
// why the hand-off is offered and not assumed: what it does not take keeps the answer it has
// without it.
func TestAnInterestPathMissForASessionTheLegionRegistryDoesNotHoldIsGivenUpAsWithoutTheHandoff(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const sessionID = "ses-not-a-legion-agent"

	agents, err := legionagents.Open(t.Context(), harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	// The project answers, so the registry naming no session is the one condition the hand-off
	// fails on: what this test pins is that failing it answers as before.
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}

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

	if _, err := harness.registry.Upsert(store.Interest{
		SessionID: sessionID, MachineID: cfg.machineID, Dir: "/legion/workspaces/infra-204",
	}, nil); err != nil {
		t.Fatalf("record the interest row the stopped session left: %v", err)
	}
	// No session entry is written: the delivery finds the session gone, as it does for the
	// stopped Legion agent. The message is a core one, which nothing redelivers, so it is given up
	// on this first delivery.

	item := envelopeFor(contracts.AgentTopicPrefix+sessionID, "dispatch-17", "dispatch")
	probe, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + item.Topic)
	if err != nil {
		t.Fatalf("subscribe the exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })

	handler(&nats.Msg{Data: marshalListenerEnvelope(t, item)})

	assertDeliveryException(t, probe, item, "no_holder")
	if records := deliveryRecords(t, harness.logs.String(), "listener agent delivery given up"); len(records) != 1 || records[0].SessionID != sessionID {
		t.Fatalf("%d lines report the delivery given up, want one naming %s:\n%s", len(records), sessionID, harness.logs.String())
	}
	if failures := deliveryRecords(t, harness.logs.String(), "listener agent delivery failed"); len(failures) != 0 {
		t.Fatalf("%d lines fail the delivery as delivery_failed, want none: a session with no record is unplaceable whatever its interest row names:\n%s", len(failures), harness.logs.String())
	}
	if woken := agentWakesFor(t, wakes, sessionID, 5*time.Second, 500*time.Millisecond); woken != 1 {
		t.Fatalf("%d agent wakes were reported for %s, want one beside the give-up:\n%s", woken, sessionID, harness.logs.String())
	}
	if count, data := handoffs(t, harness.client, sessionID); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: the registry names no such agent; the last: %s", count, sessionID, data)
	}
}

// releasedLegionEntry is legionEntry's entry for a session whose claim the daemon suspended by idle
// release, keeping the session to resume on the next message's wake (legionagents.Entry.Released).
func releasedLegionEntry(id, issue, role, started string) string {
	return strings.TrimSuffix(legionEntry(id, issue, role, started), "}") + `,"released":true}`
}

// subscribeWakes subscribes to the wake topic the Legion daemons read, before anything that
// raises a wake runs.
func subscribeWakes(t *testing.T, conn *nats.Conn) *nats.Subscription {
	t.Helper()
	wakes, err := conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", bus.LegionWakeTopic, err)
	}
	t.Cleanup(func() { _ = wakes.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the wake subscription: %v", err)
	}
	return wakes
}

// A send to a session gone from the session bucket whose registry entry is released - the daemon
// released its idle issue pod and keeps the claim to resume (INFRA-945) - is the wake it was
// before the hand-off existed: answered 404, the wake reported for that session, and nothing
// handed to the daemon, whose answer loop would otherwise answer a worker that is about to come
// back. The same entry not released is handed off and answered 200, which is the control: the
// rig reaches the hand-off, and only the flag turns it away.
func TestASendToAReleasedLegionAgentIsItsWakeAndNotAHandOff(t *testing.T) {
	rig := newHandoffRig(t, 0)
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	rig.deps.wakes = newWakeReporter(lifetime, rig.client)
	wakes := subscribeWakes(t, rig.client.Conn)
	const released = "ses-released"
	rig.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/"+released,
		releasedLegionEntry(released, "INFRA-204", "tester", "2026-10-26T03:00:00Z"))
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())

	send := func(sessionID string) *httptest.ResponseRecorder {
		return rig.do(t, http.MethodPost, "/v1/messages/send",
			fmt.Sprintf(`{"target_session":%q,"source":"agent","message":"can you answer this?"}`, sessionID))
	}

	if recorder := send(released); recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), released) {
		t.Fatalf("a send to the released session %s = %d %s, want the 404 naming it that a send to a stopped session got before the hand-off",
			released, recorder.Code, recorder.Body.String())
	}
	awaitWake(t, wakes, wakeKindAgent, released)
	if count, data := handoffs(t, rig.client, released); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for the released session %s, want none: its message is a wake for the worker, not a question for the daemon; the last: %s",
			count, released, data)
	}

	recorder := send(handoffAgent)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a send to the stopped session %s, not released = %d %s, want 200 handed off", handoffAgent, recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Recipient string `json:"recipient"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Recipient != handoffAgent {
		t.Fatalf("the send to %s answered %s (%v), want %s as its recipient", handoffAgent, recorder.Body.String(), err, handoffAgent)
	}
	if count, _ := handoffs(t, rig.client, handoffAgent); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s, want the one its send made", count, handoffAgent)
	}
}

// The give-up of an agent delivery nothing can place, for a session whose registry entry is
// released, is the give-up it was before the hand-off existed: its no_holder notice, and the wake
// reported for that session, which is what brings the released pod back. Nothing is handed off.
// A session of the same project that is not released is handed off at the same point instead,
// and so is never given up: the control that the run reaches the hand-off at all.
func TestAGiveUpForAReleasedLegionAgentReachesItsWake(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const released, stopped = "ses_released_giveup", "ses_stopped_giveup"
	agents, err := legionagents.Open(t.Context(), harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	for _, entry := range []legionagents.Entry{
		{SessionID: released, Issue: "INFRA-204", Role: "tester", Released: true},
		{SessionID: stopped, Issue: "INFRA-204", Role: "implementer"},
	} {
		entry.LegionProject, entry.Harness, entry.StartedAt = "infra", "omp", "2026-10-26T03:00:00Z"
		entry.RoleToken, entry.Title = legionagents.RoleToken("infra", entry.Issue, entry.Role), "Legion "+entry.Role+" · "+entry.Issue
		if _, err := agents.Put(ctx, entry); err != nil {
			t.Fatalf("record %s: %v", entry.SessionID, err)
		}
	}
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}

	cfg := harness.config
	cfg.unplaceableRetryWindow = 0
	cfg.handoff = &legionHandoff{
		client: cfg.client, registry: cfg.registry, sessions: cfg.sessions,
		agents: newLegionAgentRegistry(agents), logger: logging.NewWithWriter("test", harness.logs),
	}
	cfg.wakes = newWakeReporter(ctx, harness.client)
	wakes := subscribeWakes(t, harness.client.Conn)

	run := startUnplaceableDelivery(t, harness, cfg, released, 0)
	waitFor(t, 15*time.Second, "the released session "+released+"'s delivery to be given up", func() bool {
		for _, record := range deliveryRecords(t, harness.logs.String(), "listener agent delivery given up") {
			if record.SessionID == released {
				return true
			}
		}
		return false
	})
	assertDeliveryException(t, run.exceptions, run.item, "no_holder")
	awaitWake(t, wakes, wakeKindAgent, released)
	if count, data := handoffs(t, harness.client, released); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for the released session %s, want none: its give-up is its wake; the last: %s", count, released, data)
	}

	startUnplaceableDelivery(t, harness, cfg, stopped, 0)
	waitFor(t, 15*time.Second, "the stopped session "+stopped+"'s delivery to be handed off", func() bool {
		count, _ := handoffs(t, harness.client, stopped)
		return count == 1
	})
	for _, record := range deliveryRecords(t, harness.logs.String(), "listener agent delivery given up") {
		if record.SessionID == stopped {
			t.Fatalf("the stopped session %s, not released, was given up as well as handed off:\n%s", stopped, harness.logs.String())
		}
	}
}

// The role path reads `released` as the session path does: a role publish whose role is finished
// and whose newest session's claim is released - the daemon released its idle issue pod and keeps
// the claim to resume (INFRA-945) - is handed to no daemon, so the publish is kept for the role
// as before the hand-off existed and the wake it raises reaches the worker. The same role's
// shape without `released` is handed off, which is the control: the rig reaches the role path's
// hand-off, and only the flag turns it away.
func TestARolePublishWhoseNewestSessionIsReleasedIsNotHandedOff(t *testing.T) {
	rig := newHandoffRig(t, 0)
	const released, stopped = "ses-rel-reviewer", "ses-fin-reviewer"
	releasedRole, stoppedRole := "legion-infra-infra-205-reviewer", "legion-infra-infra-206-reviewer"
	rig.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/"+released,
		strings.TrimSuffix(finishedLegionEntry(released, "INFRA-205", "reviewer", "2026-10-26T03:00:00Z"), "}")+`,"released":true}`)
	rig.registerFinished(t, stopped, "INFRA-206", "reviewer", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())

	role := envelopeFor(contracts.RoleTopicPrefix+releasedRole, "publish.released", "agent")
	handed, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: role, roleToken: releasedRole})
	if err != nil || handed != nil {
		t.Fatalf("a publish to the finished role %s whose newest session %s is released = %v, %v; want nothing handed off",
			releasedRole, released, handed, err)
	}
	if count, data := handoffs(t, rig.client, released); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for the released session %s, want none; the last: %s", count, released, data)
	}

	control := envelopeFor(contracts.RoleTopicPrefix+stoppedRole, "publish.stopped", "agent")
	handed, err = rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: control, roleToken: stoppedRole})
	if err != nil || handed == nil || handed.TargetSession != stopped {
		t.Fatalf("a publish to the finished role %s, not released = %v, %v; want it handed to %s", stoppedRole, handed, err, stopped)
	}
}

// The subject a hand-off is published on is read back exactly: notifications.legion.<project>.
// agents.<session> and nothing else, so a listener's durable acknowledges a hand-off unread and
// treats no other retained message as one.
func TestIsLegionHandoffSubjectNamesTheHandOffSubjectAlone(t *testing.T) {
	for subject, want := range map[string]bool{
		"notifications.legion.infra.agents.ses-impl":             true,
		listenerwire.LegionHandoffSubject("infra", handoffAgent): true,
		"notifications.legion.infra.agents":                      false,
		"notifications.legion.infra.agents.":                     false,
		"notifications.legion.infra.agents.a.b":                  false,
		"notifications.legion..agents.ses-impl":                  false,
		"notifications.legion.wake":                              false,
		"notifications.agent.ses-impl":                           false,
		"notifications.role.legion-infra-infra-204-tester":       false,
	} {
		if got := isLegionHandoffSubject(subject); got != want {
			t.Errorf("isLegionHandoffSubject(%q) = %t, want %t", subject, got, want)
		}
	}
}
