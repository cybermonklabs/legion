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
// each holding a task, handed back newest-owed first, whose launches one pacer paces over one
// limiter at the shipped pace, with no jitter, on a clock the test moves. Each of restored is
// restored beside them, after them in claims.
type bootRig struct {
	t      *testing.T
	sup    *supervisor
	rt     *fake.Runtime
	pacer  *wakepace.Pacer
	clock  *waveClock
	start  time.Time
	claims []supervise.Claim
	s      *supervision
}

func newBootRig(t *testing.T, n int, store supervise.Store, restored ...supervise.Claim) *bootRig {
	t.Helper()
	return newBootRigEdited(t, n, store, nil, restored...)
}

// newBootRigEdited is newBootRig with edit, when not nil, changing its n unfinished launches before
// the boot restores them.
func newBootRigEdited(t *testing.T, n int, store supervise.Store, edit func([]supervise.Claim), restored ...supervise.Claim) *bootRig {
	t.Helper()
	project, _ := claim.ProjectToken("legion")
	clock := newWaveClock()
	r := &bootRig{t: t, rt: fake.NewRuntime(), clock: clock, start: clock.Now()}
	for i := range n {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		r.claims = append(r.claims, supervise.Claim{Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion(),
			Pending: &supervise.Delivery{ID: fmt.Sprintf("row-%d", i+1), Task: "test the change", QueuedAt: r.start.Add(-time.Duration(i+1) * time.Minute)}})
	}
	if edit != nil {
		edit(r.claims)
	}
	r.claims = append(r.claims, restored...)
	r.sup = newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	r.sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:           supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:         supervise.Timeouts{Boot: time.Hour, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:   api.DaemonAPIVersion(),
		TaskTakeContract: api.TaskTakeContract(),
		RefreshGate:      supervise.NewRefreshGate(0),
		WakePace:         wakePaceOf(wakepace.New(testWakeRate, testWakeBurst, nil), clock),
	}
	r.pacer = r.sup.deps.WakePace.Pacer
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

// wantSpawned is the runtime having been asked to launch each of claims[i] for i in want once, and
// no other claim at all.
func (r *bootRig) wantSpawned(want ...int) {
	r.t.Helper()
	if got := len(r.rt.CallsOf("Spawn")) + len(r.rt.CallsOf("Resume")); got != len(want) {
		r.t.Fatalf("the boot made %d launches, want %d, of claims %v", got, len(want), want)
	}
	for _, i := range want {
		if got := len(launchesOf(r.rt, r.claims[i].Token)); got != 1 {
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

// wantNextTurn is the next place anyone joins in the second queue now waiting for the turn k
// intervals after the rig's start: each relaunch holds one place, and none took a second at its
// gate. A waiting place draws no token, so this reads the estimate of a probe place, which it
// gives up again, where it once read the limiter's next reservation.
func (r *bootRig) wantNextTurn(k int) {
	r.t.Helper()
	probe := r.pacer.Join(wakepace.Second)
	defer probe.Leave()
	if next, want := probe.Estimate(), r.start.Add(time.Duration(k)*testWakeInterval); !next.Equal(want) {
		r.t.Fatalf("the next place is due at %s, want %s", next, want)
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

// A restart relaunches the deaths the previous daemon recorded ahead of the launches it left
// unfinished, whatever their claims' work was owed: the relaunch of a role whose own process died
// waits in the pacing's first queue (supervise.WakeQueue, dispatch://INFRA-5682 R2), and the
// boot's pass places the first queue before the second, so the death takes the first turn the
// pass hands out and the unfinished launches follow in the order their work was owed. Before R2
// the boot placed both in one owed order, and a death owed after an unfinished launch waited
// behind it; this test held that order, and holds the new one. Another launch of the restarted
// daemon has taken the fresh bucket's one turn already, so every relaunch of the pass waits at
// its place, and the order reads off those places, as the budgets the replay leaves read off a
// relaunch that has not run.
func TestABootRelaunchesRecordedDeathsAheadOfTheUnfinishedLaunches(t *testing.T) {
	t.Parallel()
	const n = 4
	// Owed two and a half minutes before the rig starts, which is microseconds after now: after
	// the unfinished launches owed four and three minutes before it, ahead of those owed two and
	// one. The first queue puts it ahead of all four.
	r := newBootRig(t, n, &outboxClaimStore{}, recordedDeathOwed(t, time.Now().UTC().Add(-150*time.Second)))
	dead := n
	if _, out := r.pacer.Join(wakepace.Second).Let(); !out {
		t.Fatal("the fresh pacer's first place waits, want it let out at its join")
	}

	if refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims[:n])), r.claims[dead].Token); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	r.wantSpawned()
	r.wantWaitingAt(dead, 1)
	for i := n - 1; i >= 0; i-- {
		r.wantWaitingAt(i, n+1-i)
	}
	r.wantNextTurn(n + 2)
	m, _ := r.sup.Machine(r.claims[dead].Token)
	if got := m.View().Claim.Budgets; got.Deaths != 1 || got.LaunchFailures != 1 {
		t.Fatalf("the recorded death's claim has budgets %+v, want the one death the previous daemon charged and its one launch failure", got)
	}
}

// A recorded death whose claim holds a suspension is placed in no queue. The death's own report
// ends that hold as a suspension, and the replay leaves it to that report (supervise's
// relaunchRecordedDeath), so a place taken for it would be given up again, and one that went
// unbound until its turn would let nothing out and put every wake behind it an interval late.
func TestABootPlacesNoRelaunchForARecordedDeathWithAHeldSuspension(t *testing.T) {
	t.Parallel()
	const n = 4
	r := newBootRig(t, n, &outboxClaimStore{}, recordedDeathOwed(t, time.Now().UTC().Add(-150*time.Second)))
	dead := r.claims[n].Token
	m, _ := r.sup.Machine(dead)
	if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: dead, By: supervise.SuspendedByOperator}); err != nil && !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatal(err)
	}
	if !m.View().Claim.SuspensionHeld {
		t.Fatalf("the operator's suspension of the restored working claim is not held, state %s", m.View().Claim.State)
	}

	if refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims[:n])), dead); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	r.wantSpawned(n - 1)
	for i := n - 2; i >= 0; i-- {
		r.wantWaitingAt(i, n-1-i)
	}
	r.wantNextTurn(n)
	if held := m.View().WakeWaiting; held != nil {
		t.Fatalf("the suspended claim waits on wake %+v, want none: its death's report ends the hold", *held)
	}
}

// recordedDeathOwed is a claim the previous daemon left working over a process it found dead, the
// death charged and its relaunch lost with that daemon's memory, holding the task it was working,
// queued to it at owed.
func recordedDeathOwed(t *testing.T, owed time.Time) supervise.Claim {
	t.Helper()
	project, _ := claim.ProjectToken("legion")
	token, err := claim.NewToken(project, "LEGION-90", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	c := liveClaim(90, token, api.DaemonAPIVersion())
	c.Token, c.Project, c.Tree, c.Issue, c.Role = token, project, "LEGION-90", "LEGION-90", claim.RoleImplementer
	c.DeathCharged, c.Budgets.Deaths, c.LastDeathCause = true, 1, "gone: pane %90 is gone"
	c.Pending = &supervise.Delivery{ID: "row-90", Task: "implement the change", QueuedAt: owed, ConfirmedAt: owed, Interrupted: true}
	return c
}

// A claim that holds only the daemon's own notice of its relaunch (queueRelaunch) owes no work, so
// it goes behind every claim owed a task, however long ago its notice was queued: the notice is
// the daemon's sentence of what the last process ended, not work a person waits on.
func TestABootRelaunchOfAClaimHoldingOnlyItsRelaunchNoticeGoesBehindOwedWork(t *testing.T) {
	t.Parallel()
	const n = 4
	r := newBootRigEdited(t, n, &outboxClaimStore{}, func(claims []supervise.Claim) {
		claims[n-1].Pending.ID = "relaunch:the-last-process-ended"
	})

	if refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims))); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	r.wantSpawned(n - 2)
	r.wantWaitingAt(n-3, 1)
	r.wantWaitingAt(n-4, 2)
	r.wantWaitingAt(n-1, 3)
}

// A claim whose release the store refuses at the boot keeps the place the boot took for it in the
// order of the work it is owed, and waits at that place when the sweep that retries it releases
// it before the place is let out: a transient refusal does not put the claim behind every claim
// released after it in owed order.
func TestABootRelaunchWhoseReleaseIsRefusedKeepsItsTurnForTheRetry(t *testing.T) {
	t.Parallel()
	const n = 4
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, n, store)
	third := r.claims[1].Token
	store.refuse = map[claim.Token]bool{third: true}

	refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))

	if place, kept := refused[third]; len(refused) != 1 || !kept || !place.Estimate().Equal(r.start.Add(2*testWakeInterval)) {
		t.Fatalf("the relaunch refused %v, want only %s, keeping its place, due two intervals on", refused, third)
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

// A refusal that outlasts the places it kept leaves no burst behind it: each kept place the pacer
// let out by the retry, unbound, is joined afresh, in owed order, so the claims a store outage held
// launch at the pace the operator set rather than all at once.
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
	// Ten turns on, the two released claims have run at their turns, and the pacer has let both
	// kept places out, with nothing bound to them. The clock moves a turn at a time, as a clock
	// that moves on its own does: the pacer reads the clock at each turn, and a turn fired ten
	// intervals late would find the bucket refilled and let out only the turns the refill holds.
	for range 10 {
		r.clock.advance(testWakeInterval)
	}
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

// The claims owed longest keep the earliest places however a refusal leaves the places they kept:
// the store refuses all four releases at the boot, and by the retry, an interval and a half on,
// the pacer has let the two earliest kept places out while the two later still wait. The retry
// pools the two still waiting with two places joined afresh and hands them out in owed order, so
// the claim owed longest still launches first, and no more places are joined than were let out.
func TestABootRelaunchRefusedAcrossPassingTurnsKeepsTheOwedOrder(t *testing.T) {
	t.Parallel()
	const n = 4
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, n, store)
	store.refuse = map[claim.Token]bool{}
	for _, c := range r.claims {
		store.refuse[c.Token] = true
	}

	refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))
	if len(refused) != n {
		t.Fatalf("the relaunch refused %v, want all %d claims", refused, n)
	}
	r.clock.advance(testWakeInterval + testWakeInterval/2)

	if again := r.s.launchUnfinished(context.Background(), refused); len(again) != 0 {
		t.Fatalf("the retry refused %v", again)
	}

	r.wantSpawned()
	for i := n - 1; i >= 0; i-- {
		r.wantWaitingAt(i, n+1-i)
	}
	r.wantNextTurn(n + 2)
}

// A refusal that never clears takes no turn of the pacing at every sweep: the claim's one place is
// let out, and the sweeps after it join it nothing more until its release comes. Where this test
// once counted the limiter's reservations, it holds the sweeps to handing back the one place the
// boot took: a place joined at each sweep would take a turn at each.
func TestABootRelaunchRefusedAtEverySweepTakesOneTurn(t *testing.T) {
	t.Parallel()
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, 1, store)
	token := r.claims[0].Token
	refuse := func() {
		store.mu.Lock()
		store.refuse = map[claim.Token]bool{token: true}
		store.mu.Unlock()
	}
	refuse()
	waiting := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))
	kept := waiting[token]
	if len(waiting) != 1 || kept == nil {
		t.Fatalf("the boot refused %v, want %s alone, holding the place the boot took for it", waiting, token)
	}
	if _, out := kept.Let(); !out {
		t.Fatalf("the place the boot took for %s waits, want it let out at its join by the fresh pacer", token)
	}
	for range 3 {
		r.clock.advance(time.Minute)
		refuse()
		if waiting = r.s.launchUnfinished(context.Background(), waiting); len(waiting) != 1 || waiting[token] != kept {
			t.Fatalf("the sweep's retry refused %v, want %s again, holding the one place the boot took for it", waiting, token)
		}
	}

	r.wantSpawned()
}

// unfinishedOf is the tokens of claims, in the order the store handed them back.
func unfinishedOf(claims []supervise.Claim) []claim.Token {
	tokens := make([]claim.Token, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, c.Token)
	}
	return tokens
}
