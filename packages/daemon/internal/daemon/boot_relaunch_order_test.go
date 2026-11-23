package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// bootWakeInterval is what one wake costs at the rate these tests pace at, eight a minute.
const bootWakeInterval = time.Minute / 8

// frozenClock is stillClock with a Now that never moves, so every turn the pacing reserves is an
// exact number of intervals after the first.
type frozenClock struct {
	stillClock
	now time.Time
}

func (c frozenClock) Now() time.Time { return c.now }

// releaseRefusingStore refuses, once, the write that releases one claim's uncertain launch (the
// claim written queued), as a store that fails at the boot's release of that claim and answers the
// sweep that retries it.
type releaseRefusingStore struct {
	*outboxClaimStore
	token   claim.Token
	refused atomic.Bool
}

func (s *releaseRefusingStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token && c.State == supervise.StateQueued && s.refused.CompareAndSwap(false, true) {
		return errors.New("the store did not answer")
	}
	return s.outboxClaimStore.PutClaim(ctx, c)
}

// bootRig is a boot's supervisor over n claims a previous daemon left launching with no process,
// each holding a task, handed back newest-owed first, whose launches one limiter paces at eight a
// minute on a clock that does not move.
type bootRig struct {
	t       *testing.T
	sup     *supervisor
	rt      *fake.Runtime
	limiter *wakepace.Limiter
	now     time.Time
	claims  []supervise.Claim
	s       *supervision
}

func newBootRig(t *testing.T, n int, store supervise.Store) *bootRig {
	t.Helper()
	project, _ := claim.ProjectToken("legion")
	owed := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	r := &bootRig{t: t, rt: fake.NewRuntime(), limiter: wakepace.New(8, 1, nil), now: owed.Add(time.Hour)}
	for i := range n {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		r.claims = append(r.claims, supervise.Claim{Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion,
			Pending: &supervise.Delivery{ID: fmt.Sprintf("row-%d", i+1), Task: "test the change", QueuedAt: owed.Add(time.Duration(n-i) * time.Minute)}})
	}
	r.sup = newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	r.sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: frozenClock{now: r.now}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		RefreshGate:    supervise.NewRefreshGate(0),
		WakePace:       supervise.WakePace{Reserve: r.limiter.Reserve, Rate: 8, Burst: 1},
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

// wantSpawned is the runtime having been asked to launch exactly the claims given, in any order.
func (r *bootRig) wantSpawned(want ...claim.Token) {
	r.t.Helper()
	spawned := r.rt.CallsOf("Spawn")
	got := map[claim.Token]bool{}
	for _, call := range spawned {
		got[call.Spec.Claim] = true
	}
	if len(spawned) != len(want) {
		r.t.Fatalf("the boot launched %d claims, want %v", len(spawned), want)
	}
	for _, token := range want {
		if !got[token] {
			r.t.Fatalf("the boot launched %v, want %v", got, want)
		}
	}
}

// wantWaitingAt is claims[i]'s wake waiting for the turn k intervals after the rig's now.
func (r *bootRig) wantWaitingAt(i, k int) {
	r.t.Helper()
	m, _ := r.sup.Machine(r.claims[i].Token)
	held := m.View().WakeWaiting
	want := r.now.Add(time.Duration(k) * bootWakeInterval)
	if held == nil || !held.At.Equal(want) {
		r.t.Fatalf("%s, owed since %s, holds wake %+v, want one due at %s, turn %d of the queue",
			r.claims[i].Token, r.claims[i].Pending.QueuedAt.Format(time.TimeOnly), held, want, k)
	}
}

// wantNextTurn is the next wake anyone reserves coming k intervals after the rig's now: each
// relaunch was charged one turn, and none took a second at its gate.
func (r *bootRig) wantNextTurn(k int) {
	r.t.Helper()
	if next, want := r.limiter.Reserve(r.now), r.now.Add(time.Duration(k)*bootWakeInterval); !next.Equal(want) {
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

	if refused := r.s.launchUnfinished(context.Background(), unfinishedOf(r.claims), map[claim.Token]time.Time{}); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	// The claim owed longest, the last the store handed back, takes the one wake the pacing lets
	// out at once, and each other waits one turn after the claim owed before it.
	r.wantSpawned(r.claims[n-1].Token)
	for i := n - 2; i >= 0; i-- {
		r.wantWaitingAt(i, n-1-i)
	}
	r.wantNextTurn(n)
}

// A claim whose release the store refuses at the boot keeps the turn the boot reserved it in the
// order of the work it is owed, and launches at that turn when the sweep that retries it releases
// it: a transient refusal does not put the claim that has waited longest behind every claim
// released before it.
func TestABootRelaunchWhoseReleaseIsRefusedKeepsItsTurnForTheRetry(t *testing.T) {
	t.Parallel()
	const n = 4
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, n, store)
	oldest := r.claims[n-1].Token
	store.token = oldest
	turns := map[claim.Token]time.Time{}

	refused := r.s.launchUnfinished(context.Background(), unfinishedOf(r.claims), turns)

	if len(refused) != 1 || refused[0] != oldest {
		t.Fatalf("the relaunch refused %v, want only %s", refused, oldest)
	}
	if turn := turns[oldest]; !turn.Equal(r.now) || len(turns) != 1 {
		t.Fatalf("the turns kept for the retry are %v, want only %s's, due at %s", turns, oldest, r.now)
	}
	r.wantSpawned()
	for i := n - 2; i >= 0; i-- {
		r.wantWaitingAt(i, n-1-i)
	}

	if refused := r.s.launchUnfinished(context.Background(), refused, turns); len(refused) != 0 {
		t.Fatalf("the retry refused %v", refused)
	}

	r.wantSpawned(oldest)
	if len(turns) != 0 {
		t.Fatalf("the turns kept after the retry are %v, want none", turns)
	}
	r.wantNextTurn(n)
}

// unfinishedOf is the tokens of claims, in the order the store handed them back.
func unfinishedOf(claims []supervise.Claim) []claim.Token {
	tokens := make([]claim.Token, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, c.Token)
	}
	return tokens
}
