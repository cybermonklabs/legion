package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A finding the agent of a retro after a close publishes to the role topic of its tree's root
// architect, while no live session holds that role, reaches the daemon as the Envoy listener's
// role_message wake (P6). The tree's close suspended that architect's claim as the daemon's own
// (suspended_by daemon), which a wake of a released pod never takes; while a retro after a close
// of the tree is open the daemon wakes that claim all the same: one start of the root's architect,
// marked as that wake (record.SuperviseRequest.RetroWake), of the wake's kind, naming no phase and
// no run, whose task names the topic the message was published to. Once no retro of the tree is
// open the same message wakes nothing, as before (s19: TestAWakeDoesNothingOnceTheDaemonTookTheClaimOver
// and TestAWakeDoesNothingOnceTheIssueHasLeftTheWorkflow).
func TestAMessageToTheRootArchitectOfAClosedTreeWakesItWhileARetroIsOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// started runs the child into implementing before its close, so the close owes its retro;
		// a child closed before its first phase owes none, and its tree has no retro open.
		started bool
	}{
		{name: "a retro after a close of the tree is open", started: true},
		{name: "no retro of the tree is open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			r.engine.cfg.Pods = wakePods(t, r.pool)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			if tc.started {
				r.runTo(t, "LEGION-209", phase.Implementing)
			}
			r.person(t, "LEGION-209", "done")
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			seedRunClaim(t, r.pool, "LEGION-208", claim.RoleArchitect, 1, "suspended", "daemon", "session-architect")

			mark := r.mark(t)
			token := treeRoleToken(t, "LEGION-208", claim.RoleArchitect)
			r.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: r.clock.now, Report: "report-1"})
			var starts []record.SuperviseRequest
			for _, request := range supervisesSince(t, r.pool, "LEGION-208", mark) {
				if request.Op == "start" {
					starts = append(starts, request)
				}
			}
			if !tc.started {
				if len(starts) != 0 {
					t.Fatalf("starts of the root after the message with no retro of its tree open = %+v, want none", starts)
				}
				return
			}
			if len(starts) != 1 {
				t.Fatalf("starts of the root after the message to %s = %+v, want the one wake of its architect for the open retro", token, starts)
			}
			wake := starts[0]
			if wake.Role != claim.RoleArchitect || !wake.RetroWake || wake.WakeKind != pods.WakeRoleMessage || wake.Phase != "" || wake.RetroRun != 0 ||
				wake.Generation != 1 || !strings.Contains(wake.Task, token) {
				t.Fatalf("the wake of the root architect = %+v, want its architect's start marked as a wake for an open retro, of kind %s, no phase and no run, its task naming %s",
					wake, pods.WakeRoleMessage, token)
			}
		})
	}
}
