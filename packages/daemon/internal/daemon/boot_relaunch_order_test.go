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
// shipped pace, with no jitter, on a clock the test moves. Each of restored is restored beside
// them, after them in claims.
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
	if edit != nil {
		edit(r.claims)
	}
	r.claims = append(r.claims, restored...)
	r.sup = newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	r.sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Hour, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		RefreshGate:    supervise.NewRefreshGate(0),
		WakePace:       wakePaceOf(r.limiter),
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

// A restart reserves the relaunches of the deaths the previous daemon recorded in the same order
// as the launches it left unfinished, the order their claims' work was owed: both were waiting in
// that daemon's pacing when it stopped, and a wake is memory only. A death whose work was owed
// after an unfinished launch's waits behind that launch, and one owed before it goes ahead,
// whichever the boot raises first. Raised apart, each recorded death took the first turns of the
// restart's queue, ahead of claims whose work had waited longer (dispatch://INFRA-5472).
func TestABootRelaunchesRecordedDeathsInOneOrderWithTheUnfinishedLaunches(t *testing.T) {
	t.Parallel()
	const n = 4
	// Owed two and a half minutes before the rig starts, which is microseconds after now: after
	// the unfinished launches owed four and three minutes before it, ahead of those owed two and
	// one.
	r := newBootRig(t, n, &outboxClaimStore{}, recordedDeathOwed(t, time.Now().UTC().Add(-150*time.Second)))
	dead := n

	if refused := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims[:n])), r.claims[dead].Token); len(refused) != 0 {
		t.Fatalf("the relaunch refused %v", refused)
	}

	r.wantSpawned(n - 1)
	r.wantWaitingAt(n-2, 1)
	r.wantWaitingAt(dead, 2)
	r.wantWaitingAt(1, 3)
	r.wantWaitingAt(0, 4)
	r.wantNextTurn(5)
	m, _ := r.sup.Machine(r.claims[dead].Token)
	if got := m.View().Claim.Budgets; got.Deaths != 1 || got.LaunchFailures != 1 {
		t.Fatalf("the recorded death's claim has budgets %+v, want the one death the previous daemon charged and its one launch failure", got)
	}
}

// A recorded death whose claim holds a suspension is reserved no turn. The death's own report
// ends that hold as a suspension, and the replay leaves it to that report (supervise's
// relaunchRecordedDeath), so a turn handed to it would be left spent and every wake behind it
// would run an interval late.
func TestABootReservesNoTurnForARecordedDeathWithAHeldSuspension(t *testing.T) {
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
	c := liveClaim(90, token, api.DaemonAPIVersion)
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

// The claims owed longest keep the earliest turns however a refusal leaves their kept turns: the
// store refuses all four releases at the boot, and by the retry, an interval and a half on, the
// two earliest kept turns have passed while the two later have not. The retry pools the two still
// to come with two fresh ones and hands them out in owed order, so the claim owed longest still
// launches first, and no more turns are reserved than were passed.
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

// A refusal that never clears takes no turn of the pacing at every sweep: the claim's one turn
// passes, and the sweeps after it reserve it nothing more until its release comes.
func TestABootRelaunchRefusedAtEverySweepTakesOneTurn(t *testing.T) {
	t.Parallel()
	store := &releaseRefusingStore{outboxClaimStore: &outboxClaimStore{}}
	r := newBootRig(t, 1, store)
	token := r.claims[0].Token
	reserved := 0
	r.sup.deps.WakePace.Reserve = func(now time.Time) time.Time {
		reserved++
		return r.limiter.Reserve(now)
	}
	refuse := func() {
		store.mu.Lock()
		store.refuse = map[claim.Token]bool{token: true}
		store.mu.Unlock()
	}
	refuse()
	waiting := r.s.launchUnfinished(context.Background(), waitingLaunches(unfinishedOf(r.claims)))
	for range 3 {
		r.clock.advance(time.Minute)
		refuse()
		if waiting = r.s.launchUnfinished(context.Background(), waiting); len(waiting) != 1 {
			t.Fatalf("the sweep's retry refused %v, want %s again", waiting, token)
		}
	}

	r.wantSpawned()
	if reserved != 1 {
		t.Fatalf("the boot and three sweeps reserved %d turns for a release the store never took, want the boot's one", reserved)
	}
}

// unfinishedOf is the tokens of claims, in the order the store handed them back.
func unfinishedOf(claims []supervise.Claim) []claim.Token {
	tokens := make([]claim.Token, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, c.Token)
	}
	return tokens
}
