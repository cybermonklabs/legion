package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// releaseRefusingStore refuses, once for each of its claims, the write that releases that claim's
// uncertain launch (the claim written queued), as a store that fails at the boot's release and
// answers the sweep that retries it.
type releaseRefusingStore struct {
	*outboxClaimStore
	mu     sync.Mutex
	refuse map[claim.Token]bool
}

func (s *releaseRefusingStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	s.mu.Lock()
	refuse := c.State == supervise.StateQueued && s.refuse[c.Token]
	delete(s.refuse, c.Token)
	s.mu.Unlock()
	if refuse {
		return errors.New("the store did not answer")
	}
	return s.outboxClaimStore.PutClaim(ctx, c)
}

// bootRig is a boot's supervisor over n claims a previous daemon left launching with no process,
// each holding a task, handed back newest-owed first, whose launches one limiter paces at the
// shipped pace, with no jitter, on a clock the test moves.
type bootRig struct {
	t       *testing.T
	sup     *supervisor
	rt      *fake.Runtime
	limiter *wakepace.Limiter
	clock   *waveClock
	start   time.Time
	claims  []supervise.Claim
	s       *supervision
}

func newBootRig(t *testing.T, n int, store supervise.Store) *bootRig {
	t.Helper()
	project, _ := claim.ProjectToken("legion")
	clock := newWaveClock()
	r := &bootRig{t: t, rt: fake.NewRuntime(), limiter: wakepace.New(testWakeRate, testWakeBurst, nil), clock: clock, start: clock.Now()}
	for i := range n {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		r.claims = append(r.claims, supervise.Claim{Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion,
			Pending: &supervise.Delivery{ID: fmt.Sprintf("row-%d", i+1), Task: "test the change", QueuedAt: r.start.Add(-time.Duration(i+1) * time.Minute)}})
	}
	r.sup = newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	r.sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Hour, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		RefreshGate:    supervise.NewRefreshGate(0),
		WakePace:       supervise.WakePace{Reserve: r.limiter.Reserve, Rate: testWakeRate, Burst: testWakeBurst},
	}
	t.Cleanup(r.sup.stop)
	unfinished, err := r.sup.restore(context.Background(), r.claims)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(unfinished) != n {
		t.Fatalf("the unfinished launches = %v, want all %d claims", unfinished, n)
	}
	r.s = &supervision{log: quietLogger(), supervisor: r.sup, refreshGate: supervise.NewRefreshGate(0)}
	return r
}

// spawned is how many times the runtime was asked to launch token.
func (r *bootRig) spawned(token claim.Token) int {
	launched := 0
	for _, call := range r.rt.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			launched++
		}
	}
	return launched
}

// wantSpawned is the runtime having been asked to launch each of claims[i] for i in want once, and
// no other claim at all.
func (r *bootRig) wantSpawned(want ...int) {
	r.t.Helper()
	if got := len(r.rt.CallsOf("Spawn")); got != len(want) {
		r.t.Fatalf("the boot made %d launches, want %d, of claims %v", got, len(want), want)
	}
	for _, i := range want {
		if got := r.spawned(r.claims[i].Token); got != 1 {
			r.t.Fatalf("the boot launched %s %d times, want once", r.claims[i].Token, got)
		}
	}
}

// wantWaitingAt is claims[i]'s wake waiting for the turn k intervals after the rig's start.
func (r *bootRig) wantWaitingAt(i, k int) {
	r.t.Helper()
	m, _ := r.sup.Machine(r.claims[i].Token)
	held := m.View().WakeWaiting
	want := r.start.Add(time.Duration(k) * testWakeInterval)
	if held == nil || !held.At.Equal(want) {
		r.t.Fatalf("%s, owed since %s, holds wake %+v, want one due at %s, turn %d from the start",
			r.claims[i].Token, r.claims[i].Pending.QueuedAt.Format(time.TimeOnly), held, want, k)
	}
}

// wantNextTurn is the next wake anyone reserves now coming k intervals after the rig's start: each
// relaunch was charged one turn, and none took a second at its gate.
func (r *bootRig) wantNextTurn(k int) {
	r.t.Helper()
	if next, want := r.limiter.Reserve(r.clock.Now()), r.start.Add(time.Duration(k)*testWakeInterval); !next.Equal(want) {
		r.t.Fatalf("the next wake is due at %s, want %s", next, want)
	}
}

// A restart puts every launch the previous daemon left waiting in the wake pacing back in it, and
// the queue it rebuilds keeps the order the previous daemon was draining: the claim whose work
// has waited longest is let out first, and each after it in the order its work was owed, whatever
// order the store hands the claims back in. Reserved in whatever order the boot's relaunches
// happened to reach the gate, a claim that had waited most of an hour went back to a random place
// in a queue of hundreds at each restart, twice on 2026-11-23 (dispatch://INFRA-5482).
func TestABootRelaunchesUnfinishedLaunchesInTheOrderTheirWorkWasOwed(t *testing.T) {
	t.Parallel()
	const n = 6
	r := newBootRig(t, n, &outboxClaimStore{})

	if refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims))); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	// The claim owed longest, the last the store handed back, takes the one wake the pacing lets
	// out at once, and each other waits one turn after the claim owed before it.
	r.wantSpawned(n - 1)
	for i := n - 2; i >= 0; i-- {
		r.wantWaitingAt(i, n-1-i)
	}
	r.wantNextTurn(n)
}

// A claim whose release the store refuses at the boot keeps the turn the boot reserved it in the
// order of the work it is owed, and waits for that turn when the sweep that retries it releases
// it before the turn has come: a transient refusal does not put the claim behind every claim
// released after it in owed order.
func TestABootRelaunchWhoseReleaseIsRefusedKeepsItsTurnForTheRetry(t *testing.T) {
	t.Parallel()
	const n = 4
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, n, store)
	third := r.claims[1].Token
	store.refuse = map[claim.Token]bool{third: true}

	refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))

	if turn, kept := refused[third]; len(refused) != 1 || !kept || !turn.Equal(r.start.Add(2*testWakeInterval)) {
		t.Fatalf("the relaunch refused %v, want only %s, keeping its turn two intervals on", refused, third)
	}
	r.wantSpawned(n - 1)
	r.wantWaitingAt(2, 1)
	r.wantWaitingAt(0, 3)

	if again := r.s.launchUnfinished(context.Background(), refused); len(again) != 0 {
		t.Fatalf("the retry refused %v", again)
	}

	r.wantSpawned(n - 1)
	r.wantWaitingAt(1, 2)
	r.wantNextTurn(n)
}

// A refusal that outlasts the turns it kept leaves no burst behind it: each kept turn that has
// passed by the retry is reserved again, in owed order, so the claims a store outage held launch
// at the pace the operator set rather than all at once.
func TestABootRelaunchRefusedPastItsTurnIsPacedAgainAtTheRetry(t *testing.T) {
	t.Parallel()
	const n = 4
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, n, store)
	oldest, second := r.claims[n-1].Token, r.claims[n-2].Token
	store.refuse = map[claim.Token]bool{oldest: true, second: true}

	refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))
	if len(refused) != 2 {
		t.Fatalf("the relaunch refused %v, want %s and %s", refused, oldest, second)
	}
	// Ten turns on, the two released claims have run at their turns, and both kept turns have
	// passed.
	r.clock.advance(10 * testWakeInterval)
	for _, c := range r.claims {
		m, _ := r.sup.Machine(c.Token)
		m.Wait()
	}
	r.wantSpawned(0, 1)

	if again := r.s.launchUnfinished(context.Background(), refused); len(again) != 0 {
		t.Fatalf("the retry refused %v", again)
	}

	r.wantSpawned(0, 1, n-1)
	r.wantWaitingAt(n-2, 11)
	r.wantNextTurn(12)
}

// unfinishedOf is the tokens of claims, in the order the store handed them back.
func unfinishedOf(claims []supervise.Claim) []claim.Token {
	tokens := make([]claim.Token, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, c.Token)
	}
	return tokens
}
