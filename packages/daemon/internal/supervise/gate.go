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
	// Admit grants a credit while fewer than K are held, counting it, and answers false once K
	// are held: the refresh the caller was about to raise is left to a later sweep rather than
	// taking a reservation out of the pacing now.
	Admit() bool
	// Release gives one credit back, for a refresh whose wake has run or which raised no wake
	// after all. It never takes the held count below zero, so a release the count holds no
	// credit for any longer — one that lands after a correction has set the count from a
	// re-read its credit was already out of — grants nobody an admission that was never taken.
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
	// granted is how many of the admissions granted since the last correction are still out: the
	// floor that correction takes, and the whole of what keeps it from handing one credit out
	// twice. It rises with each admission and falls with each release, because what it answers is
	// "how many credits of this window has the sweep's own reading not caught up with yet", and a
	// credit already given back is one whose wake demonstrably ran or was never parked. Counting
	// every admission of the window instead, releases included, would floor each correction at the
	// whole of the batch the sweep before it admitted and so admit a batch only every other sweep,
	// which halves the rate the operator set and doubles the drain this gate exists to shorten.
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
// A gate with the pacing off grants every request and counts nothing, as its own doc states.
// There is no gate-less caller to answer for: every Deps and every supervision is built with a
// gate, one of rate 0 where the pacing is off (NewRefreshGate, which states why).
func (g *RefreshGate) Admit() bool {
	if g.credits == 0 {
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
// release the count holds no credit for any longer — one that lands after a correction has set
// that count from a re-read its credit was already out of (Correct) — grants nobody an admission
// that was never taken.
//
// It lowers the admissions counted since the last correction with it, never below zero: that
// count is of the credits of this window the sweep's reading may not have caught up with, and a
// credit given back is one it need not hold a place for.
//
// Neither count is told which window the credit it is giving back was admitted in, and one
// admitted before the last correction is the case that costs: it lowers held, which is right,
// and it lowers granted with it, which takes the floor of the window it did not belong to one
// too low. The correction that reset granted had already read that credit in the wakes waiting
// and taken it into the count it set (Correct), so the next correction can then set held below
// what the lane really holds, and the lane can hold K and one for a sweep. It is bounded at
// one such credit per window and it corrects itself at the sweep after, which is the same
// bound every other reading the sweep lags by costs (RefreshGate). Carrying the window a
// credit was admitted in would cost a field on every admission to save a sweep of one credit.
func (g *RefreshGate) Release() {
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held > 0 {
		g.held--
	}
	if g.granted > 0 {
		g.granted--
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
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = max(waiting, g.granted)
	g.granted = 0
}

// admitRefresh is a machine asking the gate for a credit of its own, which one decision does:
// the relaunch refreshPlugin reaches with none, its own guard asking there, where the roads
// that reach it holding none are named (refreshPlugin). The daemon's own pass asks the same
// gate before it posts, and hands what it was granted to the machine on the event it posts
// (StalePlugin.Admitted, refreshStalePlugins in internal/daemon).
func (m *Machine) admitRefresh() bool {
	return m.deps.RefreshGate.Admit()
}

// releaseRefresh gives back the credit this machine holds, if it holds one, and leaves it holding
// none. A release over a machine holding none is nothing, so no path gives one credit back twice.
//
// Its one caller is endRefreshLane below, and no decision of this package gives its machine's
// credit back of its own accord: there is no list of release sites to keep in step with the
// code (RefreshGate, Machine.unlock). The one release a decision makes itself is of a credit the
// machine never took, so it is not this helper's to give: the second credit an admitted post
// hands a machine already holding one goes back to the gate itself (refreshPlugin).
func (m *Machine) releaseRefresh() {
	if !m.refreshCredit {
		return
	}
	m.refreshCredit = false
	m.deps.RefreshGate.Release()
}

// endRefreshLane holds the stale-plugin lane to its two invariants, at the one line every
// decision of a machine ends on (Machine.unlock, dispatch://INFRA-2712 R1). Both are of the same
// shape — nothing of a relaunch this lane raised may outlive the decision that raised it, except
// the wake that relaunch is waiting on — and both are kept here rather than at each of the paths
// that could break them, because a path added later keeps them by ending its decision.
//
// The credit goes back unless a refresh wake of this machine's own is waiting: that wake is the
// one thing a credit may be held for, from the park that raises it to the turn that runs it
// (parkWake, wakeNow), and a decision that ends with any other wake on the machine, or with
// none, has either dropped that wake, spent it, or never raised one. A credit kept past such a
// decision is worse than a credit lost, because the machine reads it as held at its next
// uncredited post, skips the gate and parks a wake the gate never granted (refreshCredit,
// refreshPlugin).
//
// The intent goes whatever the decision did with it: it names one launch and no other, and the
// launch it names is raised, parked and committed inside the decision that marked it
// (markPluginRelaunch, reserveWake, commitPluginRelaunch). One left standing here would mark
// the next launch of this machine — a resume, a retry, a death's relaunch — as a relaunch of
// this lane, which writes PluginRelaunchFor for a contract no relaunch was made for and reports
// the lane one claim wide of what it holds (dispatch://INFRA-2712 R4). A retry that still owes
// one carries it in its own closure and puts it back where it runs, so it loses nothing here
// (retryHolding).
func (m *Machine) endRefreshLane() {
	if m.wake == nil || m.wake.kind != WakePluginRefresh {
		m.releaseRefresh()
	}
	m.pluginRelaunch = nil
}
