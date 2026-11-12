package daemon

// R6 of dispatch://INFRA-2883 as this package wires it: the two readings supervision takes from
// the API already serving this process's listener (openSupervision's LaunchEnded and
// LaunchWaived, api.Answering). The row below is the one of them no refusal can drive — a launch
// whose end the daemon observes before it has served its first route, which is read from the
// daemon's own record of its start rather than from the exit, since an exit carries no time of
// its own. The refusals themselves are internal/api's rows, and the decision over the outcomes
// internal/supervise's.
//
// It is driven over a bare supervisor rather than a whole daemon boot (the same construction
// plugin_test.go's boot rows use): what it pins is which ending the daemon reads and what the
// launch budget then costs, and no store round trip or HTTP route stands between those two.

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newStartingSupervisor is a bare supervisor whose launch budget reads the API this process is
// serving, as the daemon's own boot wires it: the API has its listener and has not served a route
// yet, which is where a daemon spends its whole start.
func newStartingSupervisor(t *testing.T) (*supervisor, *fake.Runtime, *api.Answering) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	answering := api.NewAnswering(listener, quietLogger())
	t.Cleanup(answering.Stop)
	rt := fake.NewRuntime()
	sup := newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 3, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		LaunchEnded:    answering.LaunchEnded,
		LaunchWaived:   answering.LaunchWaived,
	}
	t.Cleanup(sup.stop)
	return sup, rt, answering
}

// lose has the claim's process found gone, and waits out the decision that report starts.
func lose(t *testing.T, m *supervise.Machine) {
	t.Helper()
	locator := m.Claim().Locator
	if locator == nil {
		t.Fatalf("%s records no process to lose", m.Claim().Token)
	}
	if err := m.Handle(context.Background(), supervise.RuntimeObservation{
		Observation: runtime.Observation{Locator: *locator, Kind: runtime.Gone},
	}); err != nil {
		t.Fatalf("lose the process: %v", err)
	}
	m.Wait()
}

// A launch whose end this daemon observed before it had served a route is relaunched at no
// charge: there was no answer for that launch to get, whatever it asked, so the daemon's own
// start is what ended it. Once the routes are serving, the same ending is charged again — the
// waiver is read from the daemon's record of its start, not from the claim or from the exit.
func TestALaunchEndedBeforeTheFirstRouteWasServedChargesNothing(t *testing.T) {
	sup, rt, answering := newStartingSupervisor(t)
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, "LEGION-2883", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-2883", Issue: "LEGION-2883", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion,
		BootTokenHash: supervise.HashBootToken("the-launch-that-asked-while-the-daemon-started"),
		Locator:       pluginTestLocator(token),
	}
	if _, err := sup.restore(context.Background(), []supervise.Claim{c}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	m, supervised := sup.Machine(token)
	if !supervised {
		t.Fatal("the claim is not supervised")
	}

	lose(t, m)

	if budgets := m.Claim().Budgets; budgets.LaunchFailures != 0 {
		t.Fatalf("launch failures = %d, want nothing charged for a launch that ended before this daemon served a route",
			budgets.LaunchFailures)
	}
	if generation := m.Claim().Generation; generation != 2 {
		t.Fatalf("the claim is at generation %d, want it relaunched to 2", generation)
	}
	if calls := rt.CallsOf("Spawn"); len(calls) != 1 {
		t.Fatalf("spawned %d times, want the waived relaunch made", len(calls))
	}

	// The daemon is up: its routes answer on the same listener from here, so the next launch to
	// end had every chance of an answer and is charged.
	answering.Routes(http.NotFoundHandler())

	lose(t, m)

	if budgets := m.Claim().Budgets; budgets.LaunchFailures != 1 {
		t.Fatalf("launch failures = %d, want the launch that ended under serving routes charged", budgets.LaunchFailures)
	}
}
