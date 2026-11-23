package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// A restart puts every launch the previous daemon left waiting in the wake pacing back in it, and
// the queue it rebuilds keeps the order the previous daemon was draining: the claim whose work
// has waited longest is let out first, and each after it in the order its work was owed, whatever
// order the store hands the claims back in. Reserved in whatever order the boot's relaunches
// happened to reach the gate, a claim that had waited most of an hour went back to a random place
// in a queue of hundreds at each restart, twice on 2026-11-23 (dispatch://INFRA-5482).
func TestABootRelaunchesUnfinishedLaunchesInTheOrderTheirWorkWasOwed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	project, _ := claim.ProjectToken("legion")
	owed := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	const n = 6
	claims := make([]supervise.Claim, 0, n)
	for i := range n {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		// The store hands the claims back with the newest owed work first.
		claims = append(claims, supervise.Claim{Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion,
			Pending: &supervise.Delivery{ID: fmt.Sprintf("row-%d", i+1), Task: "test the change", QueuedAt: owed.Add(time.Duration(n-i) * time.Minute)}})
	}
	store := &outboxClaimStore{}
	limiter := wakepace.New(8, 1, nil)
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	rt := fake.NewRuntime()
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		RefreshGate:    supervise.NewRefreshGate(0),
		WakePace:       supervise.WakePace{Reserve: limiter.Reserve, Rate: 8, Burst: 1},
	}
	t.Cleanup(sup.stop)
	unfinished, err := sup.restore(ctx, claims)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(unfinished) != n {
		t.Fatalf("the unfinished launches = %v, want all %d claims", unfinished, n)
	}

	s := &supervision{log: quietLogger(), supervisor: sup, refreshGate: supervise.NewRefreshGate(0)}
	if refused := s.launchUnfinished(ctx, unfinished); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	// The longest-owed claim takes the one wake the pacing lets out at once; every other waits
	// a turn later than the claim owed before it.
	oldest := claims[n-1].Token
	spawned := rt.CallsOf("Spawn")
	if len(spawned) != 1 || spawned[0].Spec.Claim != oldest {
		t.Fatalf("the boot launched %+v at once, want only %s, whose work was owed longest", spawned, oldest)
	}
	var previous time.Time
	for i := n - 2; i >= 0; i-- {
		m, _ := sup.Machine(claims[i].Token)
		held := m.View().WakeWaiting
		if held == nil {
			t.Fatalf("%s holds no wake, want one waiting its turn", claims[i].Token)
		}
		if !held.At.After(previous) {
			t.Fatalf("%s, owed since %s, waits for %s, which is not after %s, the turn of the claim owed before it",
				claims[i].Token, claims[i].Pending.QueuedAt.Format(time.TimeOnly), held.At, previous)
		}
		previous = held.At
	}
	// Each turn was charged once: the next wake anyone reserves comes one interval after the last
	// claim's, not after a second token each relaunch took at its gate.
	if next := limiter.Reserve(time.Now()); !next.Equal(previous.Add(time.Minute / 8)) {
		t.Fatalf("the next wake is due at %s, want %s, one interval after the last relaunch's turn", next, previous.Add(time.Minute/8))
	}
}
