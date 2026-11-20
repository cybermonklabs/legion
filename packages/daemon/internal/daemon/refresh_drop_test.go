package daemon

import (
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A refresh pass the daemon's stop halts gives back every credit it admitted and could not post.
// The supervisor never removes a machine (supervisor.add is the one writer of its machines), so
// the one way a post the pass makes goes nowhere is a stop that halts the supervisor while the
// pass runs (supervisor.post, supervisor.halt). The pass's own release of a dropped post's credit
// (refreshStalePlugins) is what keeps that credit off the gate's floor: without it the credit is
// admitted, never published and never released, and the gate holds it for the life of the gate.
//
// The stop lands where it can in production, between an admission and its post, through the
// pass's test seam (afterRefreshAdmit), which halts the supervisor at the pass's first admission.
// Every post of the pass then goes nowhere, and the gate must be whole again afterwards: rate
// fresh admissions granted and the next refused.
//
// The negative control is the pass's release of a dropped post's credit removed: the gate then
// holds the rate's worth of credits nothing will ever give back, and grants none.
func TestARefreshPassTheStopHaltsGivesBackEveryCreditItCouldNotPost(t *testing.T) {
	const rate = 8
	sup, _ := newPluginSupervisor(t, 24, true)
	project, _ := claim.ProjectToken("legion")
	var claims []supervise.Claim
	for i := range rate {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		claims = append(claims, supervise.Claim{
			Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateIdle, Session: "ses_" + issue, SessionFile: "/sessions/" + issue + ".jsonl",
			PluginContract: 1, Locator: pluginTestLocator(token),
		})
	}
	if _, err := sup.restore(t.Context(), claims); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gate := supervise.NewRefreshGate(rate)
	admitted := 0
	s := &supervision{log: quietLogger(), supervisor: sup, claims: claims, plan: plan{pluginRelaunchable: true},
		refreshGate: gate,
		afterRefreshAdmit: func(claim.Token, *supervise.RefreshCredit) {
			admitted++
			if admitted == 1 {
				sup.halt()
			}
		}}

	s.refreshStalePlugins(s.claims, "sweep")

	if admitted != rate {
		t.Fatalf("the pass admitted %d claims, want the rate, %d: every claim is stale and idle, so the gate grants each one",
			admitted, rate)
	}
	granted := 0
	for granted <= rate && gate.Admit() != nil {
		granted++
	}
	if granted != rate {
		t.Fatalf("the gate granted %d admissions after a pass whose every post the stop dropped, want %d: each dropped post's credit goes back to the gate",
			granted, rate)
	}
}
