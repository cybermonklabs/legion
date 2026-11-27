package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// A server given no deployment judges a plan against Legion's own roles, each on Oh My Pi, the
// harness a role of a plan runs on too: so `implementer:a prober:a`, one agent for the implementer
// and the plan's own prober, both on the default model, is taken, and not refused as one agent on
// two harnesses.
func TestRegisterPlanUnderTheDefaultDeploymentTakesOneAgentOfALegionRoleAndAPlansRole(t *testing.T) {
	h, _, statuses := planHarness(t)
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	prober := dispatch.DocumentBlock{ID: "prober", Type: plan.RoleBlockType, Attributes: map[string]string{"type": "testing"}, Text: "# Prober\n\nProbe the change."}
	statuses.givePlan(planDoc, 4, prober, taskBlock("a", "implementer:a prober:a"))

	answer := registerPlan(h, architect.grant(t), "LEGION-208", planDoc, 4)

	if answer.Code != http.StatusOK {
		t.Fatalf("register implementer:a prober:a with the role block prober = %d %s, want 200", answer.Code, answer.Body)
	}
}
