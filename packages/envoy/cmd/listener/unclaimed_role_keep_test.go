package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
)

// auditLegionRole is a Legion workflow role token the agent registry takes.
const auditLegionRole = "legion-infra-infra-204-tester"

// withAuditHandoff puts a Legion agent registry on d, records one stopped agent of
// auditLegionRole and states that the project answers for it, exactly as the daemon does.
func withAuditHandoff(t *testing.T, d *listenerDeps) {
	t.Helper()
	agents, err := legionagents.Open(d.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := agents.Put(ctx, legionagents.Entry{
		SessionID: "ses-old-tester", LegionProject: "infra", Issue: "INFRA-204", Role: "tester",
		RoleToken: auditLegionRole, Title: "Legion tester · INFRA-204", Harness: "omp",
		StartedAt: "2026-10-26T03:00:00Z",
	}); err != nil {
		t.Fatalf("record the stopped tester: %v", err)
	}
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}
	wrapped := newLegionAgentRegistry(agents)
	d.legionAgents = wrapped
	d.handoff = &legionHandoff{client: d.client, registry: d.registry, sessions: d.sessions, agents: wrapped, logger: logging.New("audit")}
	d.delivery.handoff = d.handoff
}

// A publish to a Legion role nobody holds must still be kept for the role's next holder
// (INFRA-514's 202), because the role is claimed again by the next worker of that phase. The
// hand-off replaces the 404 writeRoleHolderError gives, not the keep (plan 2.2).
func TestAuditAPublishToAnUnclaimedLegionRoleIsStillKeptForItsNextHolder(t *testing.T) {
	d, client := keptRoleDeps(t)
	withAuditHandoff(t, d)

	recorder, answer := publishToRole(t, d, auditLegionRole,
		`{"topic":"`+contracts.RoleTopicPrefix+auditLegionRole+`","source":"agent","source_session":"ses-architect","message":"the tester's next round starts at abc123"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept || answer.KeptReason != "unclaimed" {
		t.Fatalf("publish to an unclaimed Legion role: status %d, kept %t, reason %q, body %s; want 202 kept unclaimed so the role's next holder is handed it",
			recorder.Code, answer.Kept, answer.KeptReason, recorder.Body.String())
	}
	waitForKeptRoleMessage(t, client, auditLegionRole, answer.EventID)
}
