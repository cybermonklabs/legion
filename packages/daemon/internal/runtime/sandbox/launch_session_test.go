package sandbox

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// exchangeKeyed is a rig option for a deployment that names
// runtime.kubernetes.agent_secrets.exchange_audience: the only one whose daemon registers a
// role's session, and so the only one whose runtime names a Claude Code launch's session.
func exchangeKeyed() rigOption {
	return withOptions(func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
	})
}

// launchSessions is the runtime's record of named launch sessions, by claim, as it stands now.
func launchSessions(r *Runtime) map[claim.Token]launchedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.sessions)
}

// TestAClaudeLaunchNamesItsSessionToTheDaemon: under a deployment with exchange_audience set, the
// runtime chose a Claude Code launch's session before the role started (claudeInputs), and it
// answers that session for the incarnation the launch started as — the very id the role's start
// command names — so the daemon can register it for the pod at the shim's hello
// (runtime.LaunchSessionNamer). It answers nothing for any incarnation but the launch's own. An
// Oh My Pi launch beside it, whose pod trades, is answered the id its own start command names
// (ompInputs, TestAnOhMyPiRoleStartsUnderTheIdItsDaemonChose), never the Claude launch's.
func TestAClaudeLaunchNamesItsSessionToTheDaemon(t *testing.T) {
	g := newRig(t, nil, exchangeKeyed())
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
	ompStart := lastStart(t, g, otherToken)
	if session, named := g.r.LaunchSession(omp); !named || session == started || !slices.Equal(sessionWords(ompStart.Argv), []string{"--session-id", session}) {
		t.Errorf("LaunchSession of an Oh My Pi launch whose pod trades = %q, %t; want the id its start's %q names", session, named, sessionWords(ompStart.Argv))
	}
}

// TestTheLaunchSessionRecordFollowsTheKeyAndTheProcess: the runtime keeps a Claude Code launch's
// session only for a deployment whose daemon registers it — one with exchange_audience set — and
// only while that launch's process is the claim's: a deployment without the key records nothing
// however many Claude launches it makes, and with the key the record goes when the process is let
// go, by a suspension or by the claim's release, as the watch's record of the process goes.
func TestTheLaunchSessionRecordFollowsTheKeyAndTheProcess(t *testing.T) {
	t.Run("without the key", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(claudeWorkerSpec(t))
		if named := launchSessions(g.r); len(named) != 0 {
			t.Errorf("with exchange_audience unset the runtime recorded %+v after a Claude launch, want nothing", named)
		}
		if session, named := g.r.LaunchSession(loc); named {
			t.Errorf("with exchange_audience unset LaunchSession(%s) = %q, want none", loc.Incarnation, session)
		}
	})
	for name, letGo := range map[string]func(g *rig, loc runtime.Locator) error{
		"suspended": func(g *rig, loc runtime.Locator) error { return g.r.Suspend(g.ctx, loc) },
		"released": func(g *rig, loc runtime.Locator) error {
			return g.r.Release(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc})
		},
	} {
		t.Run("with the key, "+name, func(t *testing.T) {
			g := newRig(t, nil, exchangeKeyed())
			loc := g.spawn(claudeWorkerSpec(t))
			if named, ok := launchSessions(g.r)[workerToken]; !ok || named.incarnation != loc.Incarnation {
				t.Fatalf("with exchange_audience set the runtime recorded %+v for %s after its Claude launch, want its incarnation %s", named, workerToken, loc.Incarnation)
			}
			if err := letGo(g, loc); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if named, ok := launchSessions(g.r)[workerToken]; ok {
				t.Errorf("after the process was %s the runtime still records %+v for %s, want no entry", name, named, workerToken)
			}
		})
	}
}
