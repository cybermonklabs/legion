package daemon

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// TestRigDaemon is the label's own case of the skill-scenario rig's daemon
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh build): the label's whole daemon on
// the rig of an issue that closes when its checks pass (done_by_checks_rig_test.go), with a real
// PostgreSQL, a real NATS, a Dispatch of its own and a GitHub stand-in of its own, over the fake
// runtime. The issue's task runs the steps `implementer reviewer acceptance:owner`, and fake
// claimants work each step. It reads no request. Each case runs alone by its name:
//
//	-run '^TestRigDaemon$/^the_route$'
//	-run '^TestRigDaemon$/^the_transfer$'
func TestRigDaemon(t *testing.T) {
	t.Parallel()

	// The route: in its retro, the implementer sends one report_defect through the daemon's HTTP
	// API with its claim's grant. The daemon records the defect, the issue stays in retro, and no
	// retro failed.
	t.Run("the route", func(t *testing.T) {
		t.Parallel()
		r := newRigDaemon(t, rigDaemonSteps)
		r.passSteps(len(rigDaemonSteps))
		implementer := r.worker(claim.RoleImplementer)
		implementer.take()
		const text = "rig route case: src/config.ts loadConfig returns DEFAULTS on a parse error"
		if status, body := r.reportDefect(implementer, text); status != http.StatusOK {
			t.Fatalf("POST %s as the implementer in retro = %d %s, want 200", rigDefectRoute, status, body)
		}

		got := r.rigRecord()
		if len(got.Defects) != 1 || got.Defects[0].Role != string(claim.RoleImplementer) || got.Defects[0].Text != text {
			t.Fatalf("the record's defects are %+v, want one of role implementer with the text sent", got.Defects)
		}
		if got.Phase != string(phase.Retro) {
			t.Errorf("the record's phase is %q, want retro", got.Phase)
		}
		if failed := slices.IndexFunc(got.Steps, func(s rigDaemonStep) bool { return s.Kind == "retro" && s.Result == "failed" }); failed >= 0 {
			t.Errorf("the record's steps hold a failed retro: %+v", got.Steps[failed])
		}
		if !slices.ContainsFunc(got.Calls, func(c rigDaemonCall) bool {
			return c.Method == http.MethodPost && c.Path == rigDefectRoute && c.Status == http.StatusOK
		}) {
			t.Errorf("the record's calls hold no POST %s with status 200", rigDefectRoute)
		}
		t.Logf("the route case: defect %s of %s recorded, %s stands in %s", got.Defects[0].ID, got.Issue, got.Issue, got.Phase)
	})

	// The transfer: a claim keeps the session that registered first, so a second session's
	// registration on the implementer's claim is refused (claim.SameAgentRefusal). The
	// controller's fresh session stops the claim and launches it again in a new session, which
	// then registers and reports ready, and the claim names it.
	t.Run("the transfer", func(t *testing.T) {
		t.Parallel()
		r := newRigDaemon(t, rigDaemonSteps)
		r.passSteps(1)
		token := r.token(claim.RoleImplementer)
		first := r.worker(claim.RoleImplementer)
		launch := lastLaunch(t, r.rt, token)

		second := "ses_second_" + randomSuffix(t)[:6]
		status, body := r.registerAs(launch.BootToken, second)
		if status != http.StatusConflict || refusalOf(body) != claim.SameAgentRefusal.Message {
			t.Fatalf("a second session's registration on %s = %d %s, want 409 naming SameAgentRefusal (%q)",
				token, status, body, claim.SameAgentRefusal.Message)
		}
		t.Logf("before the fresh session, %s's registration on %s answered %d %s", second, token, status, strings.TrimSpace(body))

		before := len(r.launchesOf(token))
		r.registerController()
		status, body = r.freshSession(claim.RoleImplementer)
		if status != http.StatusOK || strings.Contains(body, `"phaseNotRestarted"`) {
			t.Fatalf("the controller's fresh session of %s = %d %s, want 200 with no phaseNotRestarted", token, status, body)
		}
		first.dropConnection()
		spawned := r.awaitNewLaunch(token, before, launch.BootToken)

		delete(r.claims, token)
		taken := r.worker(claim.RoleImplementer)
		if held := r.d.claim(token); held.Session != taken.session {
			t.Fatalf("after the fresh session %s names session %q, want the new session %s", token, held.Session, taken.session)
		}
		t.Logf("the transfer case: %s moved from %s to %s at generation %d, boot token %s, registered (200) and ready (204)",
			token, first.session, taken.session, spawned.Spec.Generation, spawned.Spec.BootToken)
	})
}
