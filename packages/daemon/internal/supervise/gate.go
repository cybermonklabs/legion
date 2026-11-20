package supervise

// The stale-plugin refresh gate: the bound on how much of the daemon's wake pacing the
// stale-plugin refresh lane may hold at once (dispatch://INFRA-2712). Every claim that lane
// refreshes takes a reservation out of the one limiter the daemon paces every wake from
// (WakePace, internal/wakepace), and a boot on a new plugin contract makes the lane as long as
// the fleet: each live session of the release before this one is one more refresh in the queue,
// and the first launch of a tree admitted after that boot waits behind all of them. Measured on
// the INFRA daemon, 62 newly admitted trees held their slots two to three hours with no architect
// running, behind hundreds of refresh wakes the pacing had already given turns to.
//
// The gate is what keeps that queue short. The lane may hold K wakes in the pacing at a time, K
// the rate the operator set, which is one minute of it: the claims the gate refuses are not
// posted at all, and the sweep that runs every minute (internal/daemon's orphanSweepInterval)
// refills the gate and posts the next K. Nothing of the refresh is lost by a refusal — the lane
// re-reads every stale claim at each sweep (refreshStalePlugins) — and what the daemon gains is
// that every other lane's wakes, a newly admitted tree's first architect launch among them, queue
// behind one minute of refreshes rather than behind the whole fleet's worth.
//
// A credit is taken before a refresh is posted (Admit) and given back once, at the end of the
// decision that holds it when no refresh wake of this machine's own is waiting (RefreshCredit.
// Release, Machine.unlock). The gate counts the credits it has granted and not had back, exactly,
// and each sweep raises that count to the refresh wakes it re-reads waiting when the re-read
// finds more (Correct).
//
// The release is one rule enforced in one place: a machine holds a credit only while a
// WakePluginRefresh wake of its own is parked or is having its turn, and every decision that ends
// with no such wake on the machine gives the credit back as it ends (Machine.unlock,
// endRefreshLane). Every path that drops, spends or never raises such a wake is covered by that
// one check without naming the lane at all — the branches of refreshPlugin that raise no
// relaunch, the refusal of every launch there is, the park of another kind over this lane's wake,
// the wake's own turn, and the process let go with the wake parked over it — which is why just
// one decision of this package releases a credit of its own accord. That one is the arrival
// rule: a post that reaches a machine already holding a credit gives the credit it carries
// straight back to the gate, because one flag holds one credit (refreshPlugin). A credit that
// outlived its wake would cost the lane more than the credit: the machine goes on reading it as
// held, so its next uncredited post skips the gate and parks a wake the gate never granted,
// which is the one thing this bound promises cannot happen (Machine.refreshCredit,
// refreshPlugin).
//
// Every credit comes back, because every place a credit's life can end gives it back: the end of
// every decision of the machine that holds it, the arrival rule, and the daemon's own pass when
// the post it made goes nowhere (internal/daemon's refreshStalePlugins, supervisor.post). Release
// is idempotent, so no path can give one credit back twice either. The count of credits out is
// therefore the count Admit bounds, and no re-read lowers it: a re-read is made of views the
// machines publish at the ends of their decisions, so it misses the refresh posted a moment
// before it and the refresh whose turn is running, and each of those credits is still out.
//
// Two windows of the lane lean the other way, and the correction answers both: each is a wake of
// this lane that enters the pacing with no admission behind it and waits there until the next
// correction counts it. The first is the retry of a refused launch: a launch of this
// lane the lifecycle recheck refuses gives its credit back at the end of the decision that met
// the refusal, wherever that refusal came from — the launch path's own pre-check, the recheck a
// wake's turn makes, the relaunch a failure raises — and the retry that refusal may arm
// re-enters the pacing asking this gate nothing (relaunchRefused, retryHolding). The second is
// the retry of a launch whose write of StateLaunching failed: the credit went back at the end
// of the decision that launch ran in, whether it waited a turn or ran where it was raised, and
// the retry re-enters the gate asking it nothing in just the same way (launchNow,
// relaunchLater).
//
// The bound a reader derives from this gate counts both: the lane can hold one wake over the
// bound for each such retry waiting, and each of them costs it only until a sweep's correction
// reads more refresh wakes waiting than credits out.
//
// It lives in this package rather than in internal/daemon because Deps already carries the rest
// of the lane's facts (PluginContract, PluginRelaunchable, NoPluginInstall, WakePace) and because
// internal/daemon imports this package: one gate per daemon serves both of the lane's askers, the
// daemon's own pass over every stale claim (refreshStalePlugins) and the machine that holds a
// stale relaunch back until the turn in flight ends (holdStaleRelaunch, refreshPlugin). The
// machine's own lines to it are at the foot of this file (admitRefresh, releaseRefresh,
// endRefreshLane) but for one: the Release refreshPlugin makes itself for a post that arrives
// over a credit already held. The credit each machine may hold is one, because a claim that
// holds a wake is decided nothing and that second admission never becomes a second credit
// (Machine.refreshCredit, refreshPlugin).

import "sync"

// RefreshAdmitter is the gate as everything that holds one reaches it: these two methods and
// nothing else, so a machine's Deps and the daemon's pass can be handed a fake that records what
// was asked of it instead of a gate with a rate (Deps.RefreshGate). RefreshGate below is the one
// implementation a daemon builds, shared by every machine of that daemon, which is why every
// method of it is safe for use from several goroutines.
type RefreshAdmitter interface {
	// Admit grants a credit while fewer than K are held and answers it; nil once K are held.
	// The credit comes back to the gate when its Release is called, once however often it is.
	Admit() *RefreshCredit
	// Correct raises the held count to waiting, the refresh wakes the sweep has just re-read in
	// the pacing, when that is more than the credits out, and otherwise sets it to the credits
	// out. It is the sweep's own call, made before the sweep asks for anything.
	Correct(waiting int)
}

// RefreshCredit is one admission of the refresh gate, held by whoever has to give it back: the
// daemon's pass until the post that carries it is delivered, then the machine it was posted to.
// Release gives it back once; later calls do nothing, so every path that ends the credit's life
// can release it without knowing whether another already has. A nil *RefreshCredit is a credit
// never granted, and its Release does nothing.
type RefreshCredit struct {
	mu        sync.Mutex
	released  bool
	doRelease func() // called once, on the first Release; nil on a gate with the pacing off
}

// Release gives this credit back to the gate that granted it. Idempotent.
func (c *RefreshCredit) Release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	c.released = true
	if c.doRelease != nil {
		c.doRelease()
	}
}

// RefreshGate is the gate itself: K credits, one held by each stale-plugin refresh the lane has
// in the daemon's wake pacing, from its post to the end of the decision that runs its turn.
//
// A rate of zero or less is the pacing off: the gate grants every request and corrects nothing.
type RefreshGate struct {
	credits int

	// mu guards out and held, which the daemon's refresh pass, each machine's own goroutine and
	// the sweep all reach.
	mu sync.Mutex
	// out is how many credits this gate has granted and not had back. It is exact: each credit
	// is given back once (RefreshCredit.Release), and nothing else moves it.
	out int
	// held is what Admit bounds: never below out, and above it by the refresh wakes the last
	// correction read waiting beyond the credits out — the lane's wakes with no admission behind
	// them that the package doc names.
	held int
}

// NewRefreshGate builds the gate of a daemon whose pacing is rate wakes a minute: K is the rate,
// so the lane holds one minute of the pacing and the sweep a minute later admits the next
// minute's worth. The daemon builds one, beside the limiter itself and from the same setting
// (internal/daemon's run, wakepace.New).
//
// A rate of zero or less is the pacing off: the gate grants every request, counts nothing and
// corrects nothing. One is still built at that rate rather than left nil, so that every caller
// has a gate to ask and the reading of "the pacing is off" is made in one place — here — rather
// than at each asker.
func NewRefreshGate(rate int) *RefreshGate {
	g := &RefreshGate{}
	if rate > 0 {
		g.credits = rate
	}
	return g
}

// var _ states the one implementation the daemon builds against the surface its callers hold, so
// a method that changes shape fails here rather than at the Deps literal.
var _ RefreshAdmitter = (*RefreshGate)(nil)

// Admit grants a credit while fewer than K are held, and answers nil once K are held, which is
// the caller's refresh waiting for a later sweep. A gate with the pacing off grants every request
// and counts nothing.
func (g *RefreshGate) Admit() *RefreshCredit {
	if g.credits == 0 {
		return &RefreshCredit{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held >= g.credits {
		return nil
	}
	g.held++
	g.out++
	return &RefreshCredit{doRelease: g.release}
}

// release is one credit back: out falls by one, and held with it, which keeps held at or above
// out.
func (g *RefreshGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.out--
	if g.held > 0 {
		g.held--
	}
}

// Correct sets the held count to waiting, the refresh wakes the sweep has just re-read in the
// pacing, or to the credits out, whichever is more. The re-read never lowers the count below the
// credits out: it misses the refresh posted a moment before it and the one whose turn is running,
// and the credits of both are out (the package doc). A reading above K is kept as it is rather
// than capped: the lane then holds more of the pacing than the gate would grant, which is a fact
// about the wakes waiting, and the gate admits nothing until a later correction reads fewer.
//
// A gate with the pacing off corrects nothing: it holds no count to put right.
func (g *RefreshGate) Correct(waiting int) {
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = max(waiting, g.out)
}

// admitRefresh is a machine asking the gate for a credit of its own, which one decision does:
// the relaunch refreshPlugin reaches with none.
func (m *Machine) admitRefresh() *RefreshCredit {
	return m.deps.RefreshGate.Admit()
}

// releaseRefresh gives back the credit this machine holds, if it holds one, and leaves it holding
// none.
func (m *Machine) releaseRefresh() {
	if m.refreshCredit == nil {
		return
	}
	m.refreshCredit.Release()
	m.refreshCredit = nil
}

// endRefreshLane holds the stale-plugin lane to its invariants, at the one line every decision of
// a machine ends on (Machine.unlock): the credit goes back unless a refresh wake of this machine's
// own is waiting in the pacing, which is the one thing a credit is held for, from the park that
// raises it to the end of the turn that runs it.
func (m *Machine) endRefreshLane() {
	if m.wake == nil || m.wake.kind != WakePluginRefresh || m.wake.pause != 0 {
		m.releaseRefresh()
	}
	m.pluginRelaunch = nil
}
