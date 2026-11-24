package api

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A later version that this release cannot name an agent of is a plan the route answers as a
// refused plan, 409 PLAN_REFUSED with its reason, through the engine itself: the workflow refuses
// it before it writes anything, so the route never answers the write's error, 500
// FACT_APPLY_FAILED. Version 1 names key a implementer; version 2 gives the role implementer to a
// fresh step 1 beside key a, whose name that would take, so the reason names key a and the name.
func TestARegisteredVersionThatWouldRenameAKeyIsAnswered409(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{
		issues:    map[string]dispatch.Issue{"LEGION-208": {Key: "LEGION-208", LastSeq: 7}},
		documents: map[string]string{planDoc: "LEGION-208"},
		approvals: map[string]dispatch.Approval{planDoc: {State: "none", LatestVersion: 9}},
		plans:     map[string]dispatch.DocumentVersion{},
	}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	seedIssueAt(t, h, "LEGION-208", phase.Admitted)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	statuses.givePlan(planDoc, 1, taskBlock("a", "implementer:a tester:a reviewer"))
	statuses.givePlan(planDoc, 2, taskBlock("a", "implementer tester:a reviewer"))

	if recorder := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 1); recorder.Code != http.StatusOK {
		t.Fatalf("register version 1 = %d %s, want 200", recorder.Code, recorder.Body)
	}
	assertPlanRefused(t, registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 2), PlanReason{Task: "a",
		Reason: intake.AgentUnnamed("a", 1, "implementer", "a").Reason})
}
