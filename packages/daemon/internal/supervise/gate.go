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
// re-read of the wakes the lane actually has waiting (Correct).
//
// The release is the rule the lane holds to everywhere it can reach: a machine holds a credit
// only while the refresh wake that credit was taken for is waiting or is having its turn, and
// every path that drops such a wake gives the credit back where it drops it. releaseRefresh
// names every one of those paths. A credit that outlived its wake would cost the lane more than
// the credit: the machine goes on reading it as held, so its next uncredited post skips the
// gate and parks a wake the gate never granted, which is the one thing this bound promises
// cannot happen (Machine.refreshCredit, refreshPlugin).
//
// The correction is there for what no release can reach: a post to a claim this daemon no longer
// supervises, which no machine of this daemon decides at all (internal/daemon's supervisor.post),
// and the turn take-back the store refused, at the wake's own turn and at the relaunch's own
// decision (runWake's interrupted, replaceNow). Each of those leaves one credit out until the
// next sweep reads the wakes the lane really has waiting.
//
// One window of the lane leans the other way, and the same correction answers it: a launch of
// this lane the lifecycle recheck refuses gives its credit back at the refusal itself, wherever
// that refusal came from — the launch path's own pre-check, the recheck a wake's turn makes,
// the relaunch a failure raises — as the rule above says it must (relaunchRefused,
// releaseRefresh), and the retry that refusal may arm re-enters the pacing asking this gate
// nothing — so the wake it parks is one the lane holds with no admission behind it until the
// next correction counts it.
//
// The bound a reader derives from this gate is read against both: each of the paths above costs
// the lane one credit, the refusal's retry costs it one wake over the bound, and each costs it
// only until the next sweep's correction. Release is the fast path and Correct the backstop —
// without the correction a lane that lost K credits would admit nothing ever again, and without
// Release a credit freed inside a minute would sit out the rest of that minute for nothing.
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
// credit given back twice — which a lane whose relaunch has several exits can do — grants nobody
// an admission that was never taken.
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
// none. It is the one way a credit goes back, so no path gives one credit back twice: a relaunch
// that is abandoned at one step and re-decided at another releases once, and every further call
// over the same machine is nothing.
//
// Where it is called is requirement R1's own list (dispatch://INFRA-2712), and the whole of it,
// because the rule it keeps is that no credit of this lane outlives the wake it was taken for
// (RefreshGate). It is called at every exit of refreshPlugin that raises no gated relaunch, at
// the lifecycle refusal relaunchStale makes before it reserves, at the refusal of every launch
// there is, which is where a refused launch of this lane gives its credit back wherever the
// refusal came from and whose retry then re-enters the gate as a launch of the lane with no
// credit of its own (relaunchRefused, launchAfter, runWake, relaunchAfterFailure), at the three
// places the wake's work is about to run (reserveWake's exempt and straight-through paths,
// runWake), at the re-read that overturns a refresh at its own turn (refreshOverturned, R2),
// and at the three places a wake is dropped with nothing of its refresh left waiting: the
// machine letting the claim's process go with the wake it was parked over, which every
// suspension, failure and retirement reaches and which the suspension a wake's turn ends
// reaches too (letGo, wakeNow's endHeld, suspendedFor); the park of another lane's wake over
// the one this credit was taken for, a park of this lane's own kind keeping it (parkWake); and
// the turn of a wake that starts no process, which is spent where it is read and runs nothing
// (wakeNow).
//
// What no call of it reaches is left to the sweep's own correction (Correct,
// refreshStalePlugins), which is one rule for all of them rather than a release at each: a post
// to a claim the daemon no longer supervises, and a take-back the store refused at the wake's
// turn or at the relaunch's own decision (runWake's interrupted, replaceNow).
func (m *Machine) releaseRefresh() {
	if !m.refreshCredit {
		return
	}
	m.refreshCredit = false
	m.deps.RefreshGate.Release()
}
