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
// A credit is taken before a refresh is posted (Admit), given back at the end of the decision
// that holds it when no refresh wake of this machine's own is waiting (Release, Machine.unlock),
// and the whole count is put right at each sweep from a re-read of the wakes the lane actually
// has waiting (Correct).
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
// The correction is there for what no decision of a machine reaches: a post to a claim this
// daemon no longer supervises, which no machine of this daemon decides at all (internal/daemon's
// supervisor.post).
//
// Two windows of the lane lean the other way, and the same correction answers both: each is a
// wake of this lane that enters the pacing with no admission behind it and waits there until
// the next correction counts it. The first is the retry of a refused launch: a launch of this
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
// bound for each such retry waiting, and each of them costs it only until the next sweep's
// correction. Release is the fast path and Correct the backstop — without the correction a lane
// that lost K credits would admit nothing ever again, and without Release a credit freed inside a
// minute would sit out the rest of that minute for nothing.
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

// RefreshAdmitter is the gate as everything that holds one reaches it: these three methods and
// nothing else, so a machine's Deps and the daemon's pass can be handed a fake that records what
// was asked of it instead of a gate with a rate (Deps.RefreshGate). RefreshGate below is the one
// implementation a daemon builds, shared by every machine of that daemon, which is why every
// method of it is safe for use from several goroutines.
type RefreshAdmitter interface {
	// Admit grants a credit while fewer than K are held and answers it; nil once K are held.
	// The credit's Publish and Release are idempotent: calling either twice is harmless, and
	// releasing an unpublished credit takes it off the floor itself.
	Admit() *RefreshCredit
	// Correct sets the held count from a re-read of the wakes the lane actually has waiting,
	// plus the admissions not yet published (the floor). It is the sweep's own call, made
	// before the sweep asks for anything.
	Correct(waiting int)
}

// RefreshCredit is one admission of the refresh gate. Its Publish and Release are idempotent and
// goroutine-safe. A nil *RefreshCredit is safe: its methods do nothing, so a gate with the pacing
// off returns one whose callbacks are nil.
type RefreshCredit struct {
	mu        sync.Mutex
	published bool
	released  bool
	doPublish func()               // called once, on the first Publish before any Release
	doRelease func(published bool) // called once, on the first Release
}

// Publish marks this credit's wake as visible. Idempotent; harmless after Release.
func (c *RefreshCredit) Publish() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.published || c.released {
		return
	}
	c.published = true
	if c.doPublish != nil {
		c.doPublish()
	}
}

// Release gives this credit back. Releasing an unpublished credit also takes it off the floor.
// Idempotent; a second call does nothing.
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
		c.doRelease(c.published)
	}
}

// RefreshGate is the gate itself: K credits, one held by each stale-plugin refresh the lane has
// in the daemon's wake pacing. The admissions the sweep's re-read may not yet see are counted
// separately (unpublished): a refresh posted a moment before the sweep counted is a wake the
// count misses. Correct sets held to waiting + unpublished, and only a credit's Publish or
// Release takes it off the floor. The old design zeroed the floor at each correction, which lost
// it when two corrections ran in one window (a ticker that buffered a tick while the sweep
// stalled).
//
// A rate of zero or less is the pacing off: the gate grants every request and corrects nothing.
type RefreshGate struct {
	credits     int
	mu          sync.Mutex
	held        int
	unpublished int
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

// Admit grants a credit while fewer than K are held. A gate with the pacing off grants every
// request. The returned credit's Publish and Release are idempotent.
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
	g.unpublished++
	return &RefreshCredit{
		doPublish: func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.unpublished > 0 {
				g.unpublished--
			}
		},
		doRelease: func(published bool) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.held > 0 {
				g.held--
			}
			if !published && g.unpublished > 0 {
				g.unpublished--
			}
		},
	}
}

func (g *RefreshGate) Correct(waiting int) {
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = waiting + g.unpublished
}

func (m *Machine) admitRefresh() *RefreshCredit {
	return m.deps.RefreshGate.Admit()
}

// releaseRefresh gives back this machine's credit. The credit's Release is idempotent, so calling
// this when the machine holds no credit (nil) is harmless.
func (m *Machine) releaseRefresh() {
	if m.refreshCredit == nil {
		return
	}
	m.refreshCredit.Release()
	m.refreshCredit = nil
}

// publishRefresh marks this machine's credit as published: its wake is about to be visible.
// The credit's Publish is idempotent.
func (m *Machine) publishRefresh() {
	if m.refreshCredit == nil {
		return
	}
	m.refreshCredit.Publish()
}

// endRefreshLane holds the stale-plugin lane to its two invariants, at the one line every
// decision of a machine ends on (Machine.unlock). The credit goes back unless a refresh wake
// of this machine's own is waiting in the pacing; when kept, the wake is published so the
// next correction sees it in its reading rather than the floor.
func (m *Machine) endRefreshLane() {
	if m.wake == nil || m.wake.kind != WakePluginRefresh || m.wake.pause != 0 {
		m.releaseRefresh()
	} else {
		m.publishRefresh()
	}
	m.pluginRelaunch = nil
}
