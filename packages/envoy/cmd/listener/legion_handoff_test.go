package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	options := []session.SessionRegistryOption{session.WithSessionReplicas(1)}
	if liveWindow > 0 {
		options = append(options, session.WithSessionLiveWindow(liveWindow), session.WithSessionGoneHorizon(time.Hour))
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, options...)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the session cache: %v", err)
	}
	agents, err := legionagents.Open(client.Conn, legionagents.WithReplicas(1))
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
	subject := legionHandoffSubject("infra", sessionID)
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
// send repeated under one idempotency key - collapse on their MsgId, so the stream stores one.
func TestHandOffToLegionHandsAStoppedAgentToItsDaemon(t *testing.T) {
	rig := newHandoffRig(t, 0)
	rig.register(t, handoffAgent, "INFRA-204", "implementer", "2026-10-26T03:00:00Z")
	rig.registerFinished(t, handoffArchitect, "INFRA-204", "architect", "2026-10-26T03:00:00Z")
	rig.beat(t, true, time.Now())

	item := envelopeFor(contracts.AgentSubject(handoffAgent), "agent."+handoffAgent+".k1", "agent")
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
	// The same envelope handed off again - another listener's durable, a repeated send - is one
	// stored message, because every listener derives the MsgId from the envelope's dedupe key.
	if _, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item, session: handoffAgent}); err != nil {
		t.Fatalf("the second hand-off of one envelope: %v", err)
	}
	if count, _ := handoffs(t, rig.client, handoffAgent); count != 1 {
		t.Fatalf("the stream holds %d copies of one hand-off, want 1", count)
	}

	// A role publish with no live holder, whose role's issue has left the workflow, resolves to
	// the role token's newest session.
	role := envelopeFor(contracts.RoleTopicPrefix+handoffRoleToken, "publish.k2", "agent")
	handedRole, err := rig.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: role, roleToken: handoffRoleToken})
	if err != nil || handedRole == nil || handedRole.TargetSession != handoffArchitect {
		t.Fatalf("a role publish with no live holder = %v, %v; want it handed to %s", handedRole, err, handoffArchitect)
	}
	if count, _ := handoffs(t, rig.client, handoffArchitect); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for the architect, want 1", count)
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

	hand := func(key string) *legionHandoffMessage {
		t.Helper()
		item := envelopeFor(contracts.AgentSubject(handoffAgent), "agent."+handoffAgent+"."+key, "agent")
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
		return envelopeFor(contracts.AgentSubject(handoffAgent), "agent."+handoffAgent+"."+key, "agent")
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
		{"a source of envoy", legionHandoffTarget{envelope: envelopeFor(contracts.AgentSubject(handoffAgent), "agent."+handoffAgent+".e", "envoy"), session: handoffAgent}},
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
// refused exactly as it was before the hand-off was offered on that path. Nothing is handed off,
// the delivery_failed exception goes out, and the message is NAKed for the stream to redeliver
// it. Every session that is not a Legion agent's is in this shape, which is why the hand-off is
// offered and not assumed: what it does not take keeps the answer it always had.
func TestAnInterestPathMissForASessionTheLegionRegistryDoesNotHoldIsRefusedAsBefore(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const sessionID = "ses-not-a-legion-agent"

	agents, err := legionagents.Open(harness.client.Conn, legionagents.WithReplicas(1))
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
	handler := durableTestHandler(t, cfg)

	if _, err := harness.registry.Upsert(store.Interest{
		SessionID: sessionID, MachineID: cfg.machineID, Dir: "/legion/workspaces/infra-204",
	}, nil); err != nil {
		t.Fatalf("record the interest row the stopped session left: %v", err)
	}
	// No session entry is written: the delivery resolves session.ErrNoSessionRecord, as it does
	// for the stopped Legion agent.

	item := envelopeFor(contracts.AgentTopicPrefix+sessionID, "dispatch-17", "dispatch")
	probe, err := harness.client.Conn.SubscribeSync("notifications.envoy.exceptions." + item.Topic)
	if err != nil {
		t.Fatalf("subscribe the exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })

	handler(&nats.Msg{Data: marshalListenerEnvelope(t, item)})

	assertDeliveryException(t, probe, item, "delivery_failed")
	if records := deliveryRecords(t, harness.logs.String(), "listener agent delivery failed"); len(records) != 1 || records[0].SessionID != sessionID {
		t.Fatalf("%d lines report the delivery failed, want one naming %s:\n%s", len(records), sessionID, harness.logs.String())
	}
	if naked := counterValue(t, metricsBody(t, harness), nakedCounter); naked != 1 {
		t.Fatalf("the NAK counter reads %d, want 1: an interest-path failure nothing took is retried, not acknowledged", naked)
	}
	if count, data := handoffs(t, harness.client, sessionID); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: the registry names no such agent; the last: %s", count, sessionID, data)
	}
}

// The subject a hand-off is published on is read back exactly: notifications.legion.<project>.
// agents.<session> and nothing else, so a listener's durable acknowledges a hand-off unread and
// treats no other retained message as one.
func TestIsLegionHandoffSubjectNamesTheHandOffSubjectAlone(t *testing.T) {
	for subject, want := range map[string]bool{
		"notifications.legion.infra.agents.ses-impl":       true,
		legionHandoffSubject("infra", handoffAgent):        true,
		"notifications.legion.infra.agents":                false,
		"notifications.legion.infra.agents.":               false,
		"notifications.legion.infra.agents.a.b":            false,
		"notifications.legion..agents.ses-impl":            false,
		"notifications.legion.wake":                        false,
		"notifications.agent.ses-impl":                     false,
		"notifications.role.legion-infra-infra-204-tester": false,
	} {
		if got := isLegionHandoffSubject(subject); got != want {
			t.Errorf("isLegionHandoffSubject(%q) = %t, want %t", subject, got, want)
		}
	}
}
