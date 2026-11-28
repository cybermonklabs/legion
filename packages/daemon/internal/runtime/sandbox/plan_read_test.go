package sandbox

import (
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// errPlanUnread is a store that cannot read an issue's plan, as during a short database outage.
var errPlanUnread = errors.New("the database is not answering")

// A launch whose issue's plan cannot be read builds its pod as the stored claims' failed read
// builds its gh bundles: the slots of the runtime's roles, and the slot of its own claim where
// that is a name no role has. It fails nothing, so no outage of the store is charged to the
// claim's launch budget. The pod then lacks a slot the plan names, which its update round adds
// once the plan reads again (slotsDrift). The fault: the launch failed for the plan's read alone.
func TestALaunchWhosePlanCannotBeReadBuildsTheSlotsOfItsRolesAndItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token claim.Token
		want  []claim.Role
	}{
		{name: "a claim named for its role", token: deploymentImplementer, want: claim.Roles},
		{name: "an agent whose name is no role", token: secondImplementer, want: append(slices.Clone(claim.Roles), "implementer-2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{
				"implementer": claim.RoleImplementer, "reviewer": claim.RoleReviewer, "implementer-2": claim.RoleImplementer, "tester-2": claim.RoleTester,
			}))
			g.store.failPlan(errPlanUnread)
			loc := g.spawn(testSpec(t, tc.token, claim.RoleImplementer, testTree))
			pod := g.pod(loc.Sandbox.Name)
			if pod == nil {
				t.Fatalf("the launch built no pod %s", loc.Sandbox.Name)
			}
			if got := builtRoles(pod.Spec); !slices.Equal(got, tc.want) {
				t.Errorf("the pod's slots with the plan unread = %v, want %v", got, tc.want)
			}
		})
	}
}

// The drift of a claim's pod reads nothing of the issue's plan: the sweep asks it of every claim of
// the deployment at every tick, and the slots the plan names are one question of the pod
// (PodSlotsDrift), which the sweep asks once for each issue. So a store that cannot read the plan
// leaves every other reason of the pod readable and the pod's absence still known. The fault: each
// claim's PodDrift read the plan, and failed with it, so the sweep read the pod as unread and held
// every round of the deployment for the outage.
func TestAPodsDriftReadsNothingOfItsPlan(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer}))
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	g.store.failPlan(errPlanUnread)
	before := g.store.planReadCount()
	if drifted := driftOf(t, g, g.r, loc); len(drifted) > 0 {
		t.Errorf("PodDrift of a pod built from the configuration now, the plan unread = %q, want nothing", drifted)
	}
	if reads := g.store.planReadCount() - before; reads != 0 {
		t.Errorf("PodDrift read the issue's plan %d times, want none", reads)
	}
}

// The slots of a pod's plan are one read of that plan for each question (PodSlotsDrift), and a
// plan that cannot be read is the error it answers: the sweep that asks it once for the issue's pod
// then decides on the pod's other reasons.
func TestAPodsPlanSlotsAreOneReadOfItsPlan(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer}))
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	before := g.store.planReadCount()
	if drifted := slotsDriftOf(t, g.ctx, g.r, loc); len(drifted) > 0 {
		t.Errorf("PodSlotsDrift of a pod built with the slots of its plan = %q, want nothing", drifted)
	}
	if reads := g.store.planReadCount() - before; reads != 1 {
		t.Errorf("PodSlotsDrift read the issue's plan %d times, want once", reads)
	}
	g.store.failPlan(errPlanUnread)
	if drifted, err := g.r.PodSlotsDrift(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}); !errors.Is(err, errPlanUnread) || len(drifted) > 0 {
		t.Errorf("PodSlotsDrift with the plan unread = %q, %v; want the read's error", drifted, err)
	}
}
