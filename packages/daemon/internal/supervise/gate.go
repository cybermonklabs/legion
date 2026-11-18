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
// straight back to the gate, because one flag holds one credit (takeRefreshCredit). A credit that
// outlived its wake would cost the lane more than the credit: the machine goes on reading it as
// held, so its next uncredited post skips the gate and parks a wake the gate never granted,
// which is the one thing this bound promises cannot happen (Machine.refreshCredit,
// refreshPlugin).
//
// The correction keeps held true to the wakes the lane really holds, which the gate's own count
// can drift from two ways. Its count is never short: a credit is out from its admission to its
// release, so held set to the larger of that count and the views holds every credit the views
// miss — a wake in its turn, a post not yet decided (out, Correct, dispatch://INFRA-3149). The
// drift only the views catch is the other way, a wake that enters the pacing with no admission
// behind it.
//
// Because the count is never reset, a credit must reach a release or it is held for the gate's
// life, and K held for good stop the lane. Every admitted credit does reach one. A post a machine
// decides is the machine's credit from the moment that decision begins, before the claim's state,
// the fence or a settle can end it (takeRefreshCredit, Machine.Handle), and it goes back at that
// decision's end whatever the decision was — the relaunch raised, refused or overturned, the post
// ignored over a claim launching, suspended, failed or retired, the claim retired, stopped,
// replaced or found dead, the turn aborted — because every decision ends in endRefreshLane
// (Machine.unlock), which the paragraphs above hold. Taken only where the relaunch is raised, a
// credit posted over a claim its own refresh turn had already relaunched was held by nothing and
// kept by the count for good, and the lane stopped one short of K (dispatch://INFRA-3149). A post
// that reaches no machine, a claim this daemon does not supervise or one posted once supervision
// is stopping, is given back by the pass that took it, which reads whether its post was queued
// (refreshStalePlugins, supervisor.post). The one credit no path reclaims is a
// post dropped as the supervision itself stops: the gate is this daemon's memory and goes with
// that supervision, so a credit held at its stop starves no later admission and the next boot's
// gate starts empty (internal/daemon's openSupervision).
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
// The bound a reader derives from this gate counts both retries: the lane can hold one wake over
// the bound for each such retry waiting, and each of them costs it only until the next correction
// reads the views and counts the wake the retry re-parked. Release is the fast path and Correct
// the backstop — without the correction a retry's re-parked wake would hold a place in the pacing
// the gate never charged, and the lane would admit past the bound by one for each; without
// Release a credit freed inside a minute would sit out the rest of that minute for nothing.
//
// It lives in this package rather than in internal/daemon because Deps already carries the rest
// of the lane's facts (PluginContract, PluginRelaunchable, NoPluginInstall, WakePace) and because
// internal/daemon imports this package: one gate per daemon serves both of the lane's askers, the
// daemon's own pass over every stale claim (refreshStalePlugins) and the machine that holds a
// stale relaunch back until the turn in flight ends (holdStaleRelaunch, refreshPlugin). The
// machine's own lines to it are at the foot of this file (admitRefresh, takeRefreshCredit,
// releaseRefresh, endRefreshLane). The credit each machine may hold is one, because a claim that
// holds a wake is decided nothing and a second admission over one held goes straight back
// (Machine.refreshCredit, takeRefreshCredit).

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
	// after all. It never takes the count below zero, so a release the gate holds no credit for
	// any longer — one that lands after a correction has set the count from a re-read its credit
	// was already out of — grants nobody an admission that was never taken.
	Release()
	// Correct sets held from a re-read of the wakes the lane has waiting, never below the credits
	// the gate has out, which it counts as it grants and releases them. It is the sweep's own
	// call, made before the sweep asks for anything.
	Correct(waiting int)
}

// RefreshGate is the gate itself: K credits, one held by each stale-plugin refresh the lane has
// in the daemon's wake pacing, and a running count of how many of those credits are out at all,
// which is the whole of what a correction must never count fewer of (Correct,
// dispatch://INFRA-3149).
//
// held and out answer two readings. held is what the gate hands out, takes back and refuses an
// admission against, and what a correction sets. out is every credit the gate has granted and not
// yet had back. They part on the credits the views a correction re-reads cannot see
// (internal/daemon's parkedWakes): a refresh whose wake is having its turn is off its machine
// from the turn's start until the decision ends (wakeNow, Machine.unlock, endRefreshLane), and a
// post the pass has admitted but no machine has decided has parked no wake at all — so a
// correction that set held from the views alone would count fewer credits than are out and hand
// the difference out a second time. The floor at out is the answer: a correction sets held to the
// larger of the wakes the views show and the credits out, so it raises the count to catch a wake
// the gate never charged — a retry that re-parked without admitting (the file doc) — and never
// lowers it below a credit still out.
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

	// mu guards held and out, which the daemon's refresh pass, each machine's own goroutine
	// and the sweep all reach: one gate per daemon, and no caller waits on what another is doing.
	mu sync.Mutex
	// held is how many credits the gate charges the lane now: what it refuses an admission
	// against and what a correction sets. A correction never sets it below the credits out, and
	// sets it above K where the views held more wakes than the gate had granted — a lane holding
	// more of the pacing than K, refused every admission until a later correction reads fewer.
	held int
	// out is how many credits the gate has granted and not yet had back: it rises with each
	// admission and falls with each release, and a correction never sets held below it. It counts
	// the credits the views a correction re-reads miss — a wake having its turn, a post not yet
	// decided — so the correction counts every credit out and not only the wakes already parked
	// (dispatch://INFRA-3149). It is not reset at a correction and holds no window: a credit is
	// out until the decision that holds it gives it back (endRefreshLane), and a credit released
	// is one neither count holds a place for, so a lane whose whole batch has run frees its whole
	// share for the next sweep.
	out int
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

// Admit grants a credit while fewer than K are held and counts it among the credits out, and
// answers false once K are held, which is the caller's refresh waiting for a later sweep.
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
	g.out++
	return true
}

// Release gives one credit back: the refresh it was taken for has run its wake, or it raised no
// wake at all and the credit is owed back at once. Neither count goes below zero, so a release
// the gate holds no credit for any longer — one that lands after a correction has set the count
// from a re-read its credit was already out of (Correct) — grants nobody an admission that was
// never taken.
//
// It lowers the credits-out count with held: a credit given back is one neither the gate charges
// the lane nor a correction holds a place for. A lane whose whole batch has run has released
// every credit of it, so the next correction reads no credit out and frees the whole share again,
// which is the drain cadence the sweep rests on (RefreshGate, dispatch://INFRA-2712 R1).
func (g *RefreshGate) Release() {
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held > 0 {
		g.held--
	}
	if g.out > 0 {
		g.out--
	}
}

// Correct sets held to the larger of waiting — the wakes the lane has parked as the sweep has
// just re-read them from the machines' published views — and the credits the gate has out. It is
// what counts a credit the views miss, a wake having its turn or a post not yet decided, so the
// correction never counts fewer credits than are out (out, dispatch://INFRA-3149). And it is what
// counts a wake the views show that the gate never charged — a retry that re-parked without
// admitting — so the count rises to the views where they hold more.
//
// A reading above K is kept as it is rather than capped: more of the pacing is held by this lane
// than the gate would grant, which is a fact about the wakes waiting and not an error, and the
// gate admits nothing until a later correction reads a smaller number. A reading below the
// credits out cannot lower the count below them, which the floor holds.
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
// the relaunch refreshPlugin reaches with none, its own guard asking there, where the roads
// that reach it holding none are named (refreshPlugin). The daemon's own pass asks the same
// gate before it posts, and hands what it was granted to the machine on the event it posts
// (StalePlugin.Admitted, refreshStalePlugins in internal/daemon).
func (m *Machine) admitRefresh() bool {
	return m.deps.RefreshGate.Admit()
}

// takeRefreshCredit moves the credit an admitted post carries onto this machine, as the first act
// of the decision that handles the post (Machine.Handle, StalePlugin.Admitted). It is taken there
// rather than where the relaunch is raised because not every decision of an admitted post reaches
// the relaunch: the state table ignores StalePlugin over a claim queued, launching, suspended,
// failed or retired, and a decision can end at the fence or a failed settle first. The pass reads
// the claims from a snapshot and the views a moment late, so it can admit a claim whose machine is
// past idle by the time the post is decided — the claim's own refresh wake, having its turn as the
// pass read it, relaunched it into launching (dispatch://INFRA-3149). Taken at the top, the credit
// is the machine's whatever the decision does, and endRefreshLane gives it back at that
// decision's end unless a refresh wake of this claim's waits for it; taken at the relaunch, a
// credit whose decision never got there was held by nothing, and the correction, which never
// resets its count, kept it for the gate's life.
//
// A post that arrives carrying a credit over a machine that already holds one gives the new
// credit straight back to the gate, keeping the flag for the wake the held one was taken for. The
// pass reads the views the machines last published, so a wake parked a moment before it counted
// is one its reading misses (internal/daemon's parkedWakes), and the post that reading raises
// reaches a machine whose refresh is already waiting. The flag is one bit and says only that a
// credit is held, so taking the second onto it would lose it: the end of the decision would keep
// one flag for the two credits, and that wake's turn would give back one credit for two
// admissions.
func (m *Machine) takeRefreshCredit() {
	if m.refreshCredit {
		m.deps.RefreshGate.Release()
		return
	}
	m.refreshCredit = true
}

// releaseRefresh gives back the credit this machine holds, if it holds one, and leaves it holding
// none. A release over a machine holding none is nothing, so no path gives one credit back twice.
//
// Its one caller is endRefreshLane below, and no decision of this package gives its machine's
// credit back of its own accord: there is no list of release sites to keep in step with the
// code (RefreshGate, Machine.unlock). The one release a decision makes itself is of a credit the
// machine never took, so it is not this helper's to give: the second credit an admitted post
// hands a machine already holding one goes back to the gate itself (takeRefreshCredit).
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
