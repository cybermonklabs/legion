package main

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// A reply on a stopped Legion agent's ask reaches it as a message on its own agent topic, and
// INFRA-204 exists to have its daemon answer that message from the agent's conversation instead
// of dropping it. An agent that ran leaves an interest row naming the listener it registered
// through, and that row outlives its session entry, so this is the shape every stopped phase
// worker is in: interest here, no session record.
//
// On that shape session.HandleAgentMessage takes its interest path
// (internal/session/handler.go:47-51), where Resolve's ErrNoSessionRecord comes back as Err with
// ShouldNAK, and agentSubjectDelivery answers it with the delivery_failed exception
// (cmd/listener/delivery.go:778-790) and never reaches the hand-off, which sits behind
// `result.Err == nil` at :809-811. The no-owner path swallows the same error on purpose
// (handler.go:66-72) and does reach it. So the message is refused for as long as the interest row
// stands, and the agent answers nobody.
//
// Measured on the live proof at 0df41561 (scripts/e2e/answer-loop.sh, A1): the parked architect's
// ask reply logged `listener agent delivery failed ... no session record for session
// 01a1bc71-e988-7523-8558-5cdf2bc62cf9` twice, the daemon logged no question at all, and A1 failed
// with "no answer in ask 9d743249-1da3-45d6-b18d-405b214830eb's thread within four minutes".
func TestAMessageToAStoppedAgentWhoseInterestNamesThisListenerIsHandedToItsDaemon(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const sessionID = "ses-stopped-architect"

	agents, err := legionagents.Open(harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	if _, err := agents.Put(ctx, legionagents.Entry{
		SessionID: sessionID, LegionProject: "infra", Issue: "INFRA-204", Role: "architect",
		RoleToken: "legion-infra-infra-204-architect", Title: "Legion architect · INFRA-204",
		Harness: "omp", StartedAt: "2026-11-08T18:35:44Z",
	}); err != nil {
		t.Fatalf("record the stopped architect: %v", err)
	}
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}

	cfg := harness.config
	wrapped := newLegionAgentRegistry(agents)
	cfg.handoff = &legionHandoff{
		client: cfg.client, registry: cfg.registry, sessions: cfg.sessions,
		agents: wrapped, logger: logging.NewWithWriter("test", harness.logs),
	}
	handler := durableTestHandler(t, cfg)

	// The row the stopped agent left behind: it registered through this listener, and nothing
	// removes the row when its process stops.
	if _, err := harness.registry.Upsert(store.Interest{
		SessionID: sessionID, MachineID: cfg.machineID, Dir: "/legion/workspaces/infra-204",
	}, nil); err != nil {
		t.Fatalf("record the interest row the stopped agent left: %v", err)
	}
	// No session entry is written: the agent's process is gone.

	item := envelopeFor(contracts.AgentTopicPrefix+sessionID, "dispatch-16", "dispatch")
	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	count, _ := handoffs(t, harness.client, sessionID)
	if count != 1 {
		t.Fatalf("the stream holds %d hand-offs for the stopped agent %s, want one: a reply on its ask must reach its daemon, not be refused. The listener said:\n%s",
			count, sessionID, harness.logs.String())
	}
}
