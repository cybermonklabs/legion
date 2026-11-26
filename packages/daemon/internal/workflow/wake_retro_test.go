package workflow

import (
	"strings"
	"testing"
	"time"

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

// A retro's agent publishes its finding and reports a moment later, so the listener's report of
// the message can reach the daemon after that report ended the tree's last open retro. Envoy keeps
// the message for an hour (pods.RetroWakeWindow), and for that hour after the tree's last retro
// after a close ended, the root architect the tree's close suspended is woken for it as while a
// retro is open: one start of the architect marked as a retro's wake. Here the root's own retro is
// the tree's only one. A message more than that hour after the end wakes nothing, and neither does
// one to a tree past its linger, whose last retro's end closed it again (closeTreeAfterRetros).
func TestARetroWakeStandsForTheHourAfterTheTreesLastRetroEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// linger is the root's linger, and after how long after the retro's end the message comes.
		linger, after time.Duration
		wakes         bool
	}{
		{name: "the tree's last retro ended a minute before the message", linger: 3 * time.Hour, after: time.Minute, wakes: true},
		{name: "the tree's last retro ended more than an hour before the message", linger: 3 * time.Hour, after: time.Hour + time.Minute},
		{name: "the tree is past its linger", linger: 30 * time.Minute, after: 31 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			r.engine.cfg.Pods = wakePods(t, r.pool)
			r.engine.cfg.Linger = tc.linger
			r.root(t, phase.Admitted)
			r.runTo(t, "LEGION-208", phase.Implementing)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			seedRunClaim(t, r.pool, "LEGION-208", claim.RoleArchitect, 1, "suspended", "daemon", "session-architect")
			if result := r.report(t, "LEGION-208", 1); result.Refusal != nil {
				t.Fatalf("the report of the root's retro was refused: %+v", result.Refusal)
			}
			if row, found := runRetroOf(t, r.pool, "LEGION-208", 1); !found || row.open() {
				t.Fatalf("the root's run_retros row after its report = %+v (found %t), want its retro ended: no retro of the tree is open", row, found)
			}

			r.at(tc.after)
			mark := r.mark(t)
			token := treeRoleToken(t, "LEGION-208", claim.RoleArchitect)
			r.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: r.clock.now, Report: "report-late"})
			var wakes []record.SuperviseRequest
			for _, request := range supervisesSince(t, r.pool, "LEGION-208", mark) {
				if request.Op == "start" && request.RetroWake {
					wakes = append(wakes, request)
				}
			}
			if !tc.wakes {
				if len(wakes) != 0 {
					t.Fatalf("retro wakes of the root architect %s after the end = %+v, want none", tc.after, wakes)
				}
				return
			}
			if len(wakes) != 1 || wakes[0].Role != claim.RoleArchitect || wakes[0].WakeKind != pods.WakeRoleMessage || !strings.Contains(wakes[0].Task, token) {
				t.Fatalf("retro wakes of the root architect %s after the tree's last retro ended = %+v, want its one wake for the message to %s", tc.after, wakes, token)
			}
		})
	}
}
