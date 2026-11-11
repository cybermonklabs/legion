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
// A credit is taken before a refresh is posted (Admit), given back where the relaunch it was
// taken for runs or is abandoned (Release), and the whole count is put right at each sweep from a
// re-read of the wakes the lane actually has waiting (Correct). The correction is there because
// a credit can be lost to a path that gives none back: a machine that drops a parked wake with
// the process it was parked over (letGo), a wake a second launch replaces (reserveWake), a post
// to a claim this daemon no longer supervises (internal/daemon's supervisor.post). Release is the
// fast path and Correct the backstop — without the correction a lane that lost K credits would
// admit nothing ever again, and without Release a credit freed inside a minute would sit out the
// rest of that minute for nothing.
//
// It lives in this package rather than in internal/daemon because Deps already carries the rest
// of the lane's facts (PluginContract, PluginRelaunchable, NoPluginInstall, WakePace) and because
// internal/daemon imports this package: one gate per daemon serves both of the lane's askers, the
// daemon's own pass over every stale claim (refreshStalePlugins) and the machine that holds a
// stale relaunch back until the turn in flight ends (holdStaleRelaunch, refreshPlugin). The
// machine's own two lines to it are at the foot of this file (admitRefresh, releaseRefresh), and
// the credit each machine may hold is one, because a claim that already holds a wake is decided
// nothing (Machine.refreshCredit, refreshPlugin).

import "sync"

// RefreshAdmitter is the gate as everything that holds one reaches it: these three methods and
// nothing else, so a machine's Deps and the daemon's pass can be handed a fake that records what
// was asked of it instead of a gate with a rate (Deps.RefreshGate). RefreshGate below is the one
// implementation a daemon builds, shared by every machine of that daemon, which is why every
// method of it is safe for use from several goroutines.
type RefreshAdmitter interface {
	// Admit grants a credit while fewer than K are held, counting it, and answers false once K
	// are held: the refresh the caller was about to raise is left to a later sweep rather than
	// taking a reservation out of the pacing now.
	Admit() bool
	// Release gives one credit back, for a refresh whose wake has run or which raised no wake
	// after all. It never takes the held count below zero, so a release more than the gate
	// granted — two paths giving the same credit back, which a lane with several exits can do —
	// hands out no credit that was never taken.
	Release()
	// Correct sets the held count from a re-read of the wakes the lane actually has waiting,
	// never below the admissions granted since the correction before it, and resets that count
	// of admissions. It is the sweep's own call, made before the sweep asks for anything.
	Correct(waiting int)
}

// RefreshGate is the gate itself: K credits, one held by each stale-plugin refresh the lane has
// in the daemon's wake pacing, and a count of the admissions granted since the last correction,
// which is what keeps a correction from taking back a credit whose wake has not been parked yet.
//
// The two counts are separate because the two readings they answer are. held is what the gate
// gives out and takes back as the lane acts, instant by instant. granted is what the sweep's
// re-read may not yet see: the machines publish their waiting wakes at the end of each decision
// (Machine.publish, read by internal/daemon's waitingWakes), so a refresh posted a moment before
// the sweep counted is a wake the count misses, and a correction that believed that count would
// hand its credit out a second time. The floor at granted is the answer, and it has a price the
// sweep pays knowingly: the view lags the other way too, so a correction can also keep a credit
// whose wake has already run and been released. That credit comes back at the next correction,
// one minute later, against the hours the gate itself removes.
//
// A rate of zero or less is the pacing off, which is how the daemon reads an operator that paces
// nothing (WakePace.Reserve is nil there too): the gate then bounds nothing, grants every
// request, counts none of them and corrects nothing. The lane behaves exactly as it did before
// this gate existed, which is the only sensible reading of an operator who asked for no pacing at
// all.
type RefreshGate struct {
	// credits is K, the most refreshes the lane may hold in the pacing at once: the rate the
	// operator set (worker_wakes_per_minute), which is one minute of the pacing because the sweep
	// that refills the gate runs each minute. Zero is the pacing off, and the only value below 1
	// this field takes: NewRefreshGate turns every rate of zero or less into it.
	credits int

	// mu guards held and granted, which the daemon's refresh pass, each machine's own goroutine
	// and the sweep all reach: one gate per daemon, and no caller waits on what another is doing.
	mu sync.Mutex
	// held is how many credits are out now — never more than credits by an admission of this
	// gate's own, though a correction from a reading above K may set it higher, which is a lane
	// holding more of the pacing than K and is refused every further admission until the next
	// correction.
	held int
	// granted is how many admissions have been granted since the last correction, the floor that
	// correction takes. It is reset there and nowhere else: a release does not lower it, because
	// what it counts is what was handed out rather than what is still held.
	granted int
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

// Admit grants a credit while fewer than K are held and counts it against the next correction,
// and answers false once K are held, which is the caller's refresh waiting for a later sweep.
//
// A gate with the pacing off grants every request and counts nothing, as its own doc states. A
// nil gate does the same: Deps.RefreshGate is nil for a daemon or a harness that built none, and
// a nil gate reached through a typed field answers exactly as that nil field means — every
// request granted — rather than panicking.
func (g *RefreshGate) Admit() bool {
	if g == nil || g.credits == 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held >= g.credits {
		return false
	}
	g.held++
	g.granted++
	return true
}

// Release gives one credit back: the refresh it was taken for has run its wake, or it raised no
// wake at all and the credit is owed back at once. The held count never goes below zero, so a
// credit given back twice — which a lane whose relaunch has several exits can do — grants nobody
// an admission that was never taken.
//
// It does not lower the admissions counted since the last correction, because that count is of
// what the gate handed out rather than of what is still out: a wake granted and released inside
// one sweep is a wake the sweep's own re-read may have seen either way, and the floor at granted
// is what keeps a correction from handing its credit out twice (RefreshGate).
func (g *RefreshGate) Release() {
	if g == nil || g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held > 0 {
		g.held--
	}
}

// Correct sets the held count from waiting, the wakes the lane actually has waiting as the sweep
// has just re-read them, and never below the admissions granted since the correction before it;
// the count of those admissions starts again from here. It is what takes back a credit lost to a
// path that returns none, and what keeps a credit whose wake the re-read has not caught up with
// (RefreshGate states both halves, and what each costs).
//
// A reading above K is kept as it is rather than capped: more of the pacing is held by this lane
// than the gate would grant, which is a fact about the wakes waiting and not an error, and the
// gate admits nothing until a later correction reads a smaller number. A reading below zero
// cannot lower the count below the admissions since, which are never negative.
//
// A gate with the pacing off corrects nothing: it holds no count to put right.
func (g *RefreshGate) Correct(waiting int) {
	if g == nil || g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = max(waiting, g.granted)
	g.granted = 0
}

// admitRefresh is a machine asking the gate for a credit of its own, which one asker does: the
// relaunch a turn's end releases, held back until that turn was over (afterTurn,
// holdStaleRelaunch). The daemon's own pass asks the same gate before it posts, and hands what it
// was granted to the machine on the event it posts (StalePlugin.Admitted, refreshStalePlugins in
// internal/daemon).
//
// A machine whose Deps carries no gate is granted everything. The field is an interface, and a
// daemon or a harness that built none leaves it nil, which means exactly what a gate with the
// pacing off means: the lane is bounded by nothing. That reading is made here and in
// releaseRefresh and nowhere else, which is why no decision of this package touches
// Deps.RefreshGate itself.
func (m *Machine) admitRefresh() bool {
	return m.deps.RefreshGate == nil || m.deps.RefreshGate.Admit()
}

// releaseRefresh gives back the credit this machine holds, if it holds one, and leaves it holding
// none. It is the one way a credit goes back, so no path gives one credit back twice: a relaunch
// that is abandoned at one step and re-decided at another releases once, and every further call
// over the same machine is nothing.
//
// Where it is called is requirement R1's own list (dispatch://INFRA-2712): every exit of
// refreshPlugin that raises no gated relaunch, the lifecycle refusal relaunchStale makes before
// it reserves, and the three places the wake's work is about to run (reserveWake's exempt and
// straight-through paths, runWake). Every other way a relaunch can be lost — a wake dropped with
// the process it was parked over (letGo), a wake a second launch replaced, a suspension that
// ended it (endHeld), a post to a claim the daemon no longer supervises — gives nothing back
// here and is taken back by the sweep's own correction instead (Correct, refreshStalePlugins),
// which is one rule for all of them rather than a release at each.
func (m *Machine) releaseRefresh() {
	if !m.refreshCredit {
		return
	}
	m.refreshCredit = false
	if m.deps.RefreshGate != nil {
		m.deps.RefreshGate.Release()
	}
}
