package sandbox

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// TestAClaudeLaunchNamesItsSessionToTheDaemon: the runtime chose a Claude Code launch's session
// before the role started (claudeInputs), and it answers that session for the incarnation the
// launch started as — the very id the role's start command names — so the daemon can register it
// for the pod at the shim's hello (runtime.LaunchSessionNamer). It answers nothing for an Oh My Pi
// launch, whose agent mints its own id, and nothing for any incarnation but the launch's own.
func TestAClaudeLaunchNamesItsSessionToTheDaemon(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(claudeWorkerSpec(t))
	started := startEnv(lastStart(t, g, workerToken))[claudelaunch.SessionVariable]
	if started == "" {
		t.Fatalf("the Claude launch's start command names no %s", claudelaunch.SessionVariable)
	}
	if session, named := g.r.LaunchSession(loc); !named || session != started {
		t.Errorf("LaunchSession(%s) = %q, %t; want the session the start names, %q", loc.Incarnation, session, named, started)
	}
	earlier := loc
	earlier.Incarnation = runtime.SandboxIncarnation(loc.Sandbox.PodUID, loc.Sandbox.Generation+1)
	if session, named := g.r.LaunchSession(earlier); named {
		t.Errorf("LaunchSession of an incarnation the launch did not start as = %q, want none", session)
	}

	omp := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if session, named := g.r.LaunchSession(omp); named {
		t.Errorf("LaunchSession of an Oh My Pi launch = %q, want none: its agent mints its id", session)
	}
}
