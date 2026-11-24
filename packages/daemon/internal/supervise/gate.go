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
// A credit is taken before a refresh is posted (Admit) and comes back exactly once
// (RefreshCredit.Release, which does nothing after the first call). The gate keeps every credit
// it has granted and not had back, each with the machine that holds it once a machine has taken
// it, so its count of the credits out is exact, and each sweep's correction sets what Admit
// bounds from that count and from the refresh wakes the sweep re-reads waiting (Correct).
//
// The rule of return. Whoever holds a credit gives it back, and each holder has one place it does:
//
//   - The daemon's refresh pass holds it from Admit to the post that carries it, and gives it back
//     when the supervisor drops that post (internal/daemon's refreshStalePlugins, supervisor.post).
//   - The post holds it from its delivery until the claim's machine decides it, and gives it back
//     if the daemon's stop closes the inbox it waits in first (internal/daemon's inbox.close).
//   - The machine the post reaches holds it from then on if it takes it (refreshPlugin, through
//     holdCredit, the one place a credit becomes a machine's own). A post the machine does not take
//     — a state whose row ignores it, a settle or a fence that returns before the row, a post for
//     another claim — comes back at the end of that decision (Machine.Handle, settleUntakenCredit),
//     and so does the post that reaches a machine already holding a credit, which refreshPlugin
//     releases at once because one machine holds one credit (the arrival rule).
//   - A machine holds a credit only while a WakePluginRefresh wake of its own is parked or is
//     having its turn, and every decision that ends with no such wake on the machine gives the
//     credit back as it ends (Machine.unlock, endRefreshLane). Every path that drops, spends or
//     never raises such a wake is covered by that one check without naming the lane at all — the
//     branches of refreshPlugin that raise no relaunch, the refusal of every launch there is, the
//     park of another kind over this lane's wake, the wake's own turn, and the process let go with
//     the wake parked over it.
//
// A credit that outlived its wake would cost the lane more than the credit: the machine goes on
// reading it as held, so its next uncredited post skips the gate and parks a wake the gate never
// granted, which is the one thing this bound promises cannot happen (Machine.refreshCredit,
// refreshPlugin).
//
// The correction is the backstop for a path that rule does not name, which the bound needs:
// a credit a machine took and then let go without giving it back, or kept past the wake it was
// taken for, by a path that no decision's end reaches. At each sweep the gate asks the machine
// that holds each credit granted before the previous correction whether it still holds it, and
// waits on nothing to ask (Machine.reclaimCredit). A machine that is deciding keeps the credit,
// because the decision may be the turn of the wake that credit paid for: the turn takes the wake
// off the machine's view, and the credit comes back only as the decision ends. A machine between
// decisions keeps it while it holds it with a refresh wake of its own waiting. Every other such
// credit is taken back there and then, and logged, since a credit taken back is one a path gave
// up without giving it back. A credit granted since the previous correction is kept whatever its
// machine shows, so a correction never lowers the count below the admissions granted since the
// sweep before. A lost credit is therefore kept through the first correction after its admission
// and taken back at the second, when its machine no longer holds it; if its machine is deciding
// then, it is taken back at the first correction after that which finds the machine between
// decisions. A credit still in its post has no machine to ask: the decision of that post, or the
// stop's close of the inbox it waits in, gives it back, a machine's feed decides every event it
// takes, and nothing else drops an inbox.
//
// No re-read lowers the count below the credits the correction keeps, and the order in which a
// machine stores its view and gives a credit back cannot open a window in either direction. A
// re-read is made of the views the machines publish, so it can miss a refresh posted a moment
// before it, and a refresh whose turn is running (the turn takes the wake off the view before the
// decision that runs it ends): each of those credits is still out, its machine keeps it, and the
// count holds it. A re-read can also see a wake whose credit came back a moment before its
// machine stored the view without it: the correction then counts one more than is out, which
// admits one fewer until the next sweep.
//
// Holding a credit through the turn costs the lane while a relaunch is slow. A credit is out from
// its admission to the end of the decision that runs its turn, and that decision includes the
// relaunch itself, which under Kubernetes can wait minutes for a pod: while relaunches run, the
// gate admits fewer than K new refreshes at a sweep, though fewer than K of the lane's wakes wait
// in the pacing.
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
// the retry re-enters the gate asking it nothing in just the same way (launchTries,
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
// machine's own lines to it are at the foot of this file (admitRefresh, holdCredit,
// releaseRefresh, settleUntakenCredit, reclaimCredit, endRefreshLane) but for one: the Release
// refreshPlugin makes itself for a post that arrives over a credit already held. The credit each
// machine may hold is one, because a claim that holds a wake is decided nothing and that second
// admission never becomes a second credit (Machine.refreshCredit, refreshPlugin).

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
	// Correct takes back each credit granted before the previous correction whose machine no
	// longer holds it, then sets the held count to waiting, the refresh wakes the sweep has just
	// re-read in the pacing, or to the credits still out, whichever is more. It is the sweep's
	// own call, made before the sweep asks for anything.
	Correct(waiting int)
}

// RefreshCredit is one admission of the refresh gate, held by whoever has to give it back: the
// daemon's pass until the post that carries it is delivered, the post until the claim's machine
// decides it, then the machine if it takes it. Release gives it back once; later calls do nothing,
// so every path that ends the credit's life can release it without knowing whether another
// already has. A nil *RefreshCredit is a credit never granted, and its Release does nothing.
type RefreshCredit struct {
	// mu guards released and holder.
	mu       sync.Mutex
	released bool
	// holder is the machine that took this credit as its own (Machine.holdCredit), nil until one
	// has: the machine the gate's correction asks whether it still holds it (reclaimCredit).
	holder *Machine
	// doRelease is called once, on the first Release, under mu; nil on a gate with the pacing
	// off.
	doRelease func()
}

// Release gives this credit back to the gate that granted it. Idempotent.
func (c *RefreshCredit) Release() {
	c.giveBack()
}

// giveBack gives this credit back unless it already is, and answers whether this call gave it.
func (c *RefreshCredit) giveBack() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return false
	}
	c.released = true
	if c.doRelease != nil {
		c.doRelease()
	}
	return true
}

// heldBy records m as the machine that holds this credit.
func (c *RefreshCredit) heldBy(m *Machine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holder = m
}

// holderNow is the machine that took this credit, nil while none has.
func (c *RefreshCredit) holderNow() *Machine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holder
}

// RefreshGate is the gate itself: K credits, one held by each stale-plugin refresh the lane has
// in the daemon's wake pacing, from its post to the end of the decision that runs its turn.
//
// A rate of zero or less is the pacing off: the gate grants every request and corrects nothing.
type RefreshGate struct {
	credits int

	// mu guards out, held and window, which the daemon's refresh pass, each machine's own
	// goroutine and the sweep all reach. It is taken after a credit's own lock, never before.
	mu sync.Mutex
	// out is every credit this gate has granted and not had back, with the window it was granted
	// in. It is exact: each credit leaves it when it is given back (RefreshCredit.Release), and
	// nothing else removes one.
	out map[*RefreshCredit]uint64
	// held is what Admit bounds: never below the credits out, and above them by the refresh wakes
	// the last correction read waiting beyond them — the lane's wakes with no admission behind them
	// that the package doc names.
	held int
	// window counts the corrections made: a credit granted since the last one carries its value,
	// and the next correction keeps such a credit whatever its machine shows.
	window uint64
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
	g := &RefreshGate{out: map[*RefreshCredit]uint64{}}
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
	c := &RefreshCredit{}
	c.doRelease = func() { g.release(c) }
	g.out[c] = g.window
	g.held++
	return c
}

// release is credit c back: it leaves the credits out, and held falls with it, which keeps held at
// or above them.
func (g *RefreshGate) release(c *RefreshCredit) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.out, c)
	if g.held > 0 {
		g.held--
	}
}

// Correct is the sweep's correction. It first asks the machine that holds each credit granted
// before the previous correction whether it still holds it, and takes back each one its machine
// no longer holds (Machine.reclaimCredit, the package doc): a credit granted since the previous
// correction, and one still in its post, are kept. It then sets the held count to waiting, the
// refresh wakes the sweep has just re-read in the pacing, or to the credits still out, whichever
// is more. The re-read never lowers the count below those credits: it misses the refresh posted a
// moment before it and the one whose turn is running, and the credits of both are out. A reading
// above K is kept as it is rather than capped: the lane then holds more of the pacing than the
// gate would grant, which is a fact about the wakes waiting, and the gate admits nothing until a
// later correction reads fewer.
//
// The machines are asked with the gate's lock released, since a machine gives a credit back under
// its own lock and the credit's, and the gate's lock is the last of those.
//
// A gate with the pacing off corrects nothing: it holds no count to put right.
func (g *RefreshGate) Correct(waiting int) {
	if g.credits == 0 {
		return
	}
	g.mu.Lock()
	var earlier []*RefreshCredit
	for c, window := range g.out {
		if window != g.window {
			earlier = append(earlier, c)
		}
	}
	g.window++
	g.mu.Unlock()
	for _, c := range earlier {
		if m := c.holderNow(); m != nil {
			m.reclaimCredit(c)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = max(waiting, len(g.out))
}

// admitRefresh is a machine asking the gate for a credit of its own, which one decision does:
// the relaunch refreshPlugin reaches with none.
func (m *Machine) admitRefresh() *RefreshCredit {
	return m.deps.RefreshGate.Admit()
}

// holdCredit makes credit this machine's own, the one it holds for the relaunch it is raising,
// and names this machine on the credit as the holder the gate's correction asks (reclaimCredit).
// The caller holds mu, and this machine holds no other credit.
func (m *Machine) holdCredit(credit *RefreshCredit) {
	m.refreshCredit = credit
	credit.heldBy(m)
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

// settleUntakenCredit gives back credit, the one a refresh post carried to this decision, unless
// the decision took it (refreshPlugin set it as the machine's own). Machine.Handle defers it for
// every credited StalePlugin, so a post the machine ignores, or returns before reaching, still
// gives its credit back; a credit the decision took is the machine's to give back
// (endRefreshLane), and a second Release would do nothing anyway.
func (m *Machine) settleUntakenCredit(credit *RefreshCredit) {
	if m.refreshCredit != credit {
		credit.Release()
	}
}

// laneWakeWaiting is whether a refresh wake of this machine's own waits in the pacing, which is
// the one thing a credit is held for between decisions. A wake the project pause's gate holds
// (wake.pause) waits in no pacing, so it is none. The caller holds mu.
func (m *Machine) laneWakeWaiting() bool {
	return m.wake != nil && m.wake.kind == WakePluginRefresh && m.wake.pause == 0
}

// creditTakenBackMessage is the line a correction writes for each credit it takes back from a
// machine that no longer holds it for a refresh wake (reclaimCredit), with the claim's state and
// whether the machine holds any wake. It is pinned because a test reads it whole.
const creditTakenBackMessage = "supervise: the refresh gate's correction took back a credit this claim's machine no longer held for a refresh wake; a path gave it up without giving it back"

// reclaimCredit is the gate's correction asking this machine whether it still holds credit, a
// credit it took (holdCredit) before the previous correction, and taking it back if it does not
// (RefreshGate.Correct, the package doc). It waits on no decision: a machine whose lock is held is
// deciding, and the decision may be the turn credit paid for, which gives it back as it ends, so
// the credit is kept and the next sweep asks again. A machine between decisions holds credit only
// as its own with a refresh wake of its own waiting; a credit it let go without giving it back, or
// keeps with no such wake, is given back here, and the line names the claim, since a path that
// gave a credit up without giving it back is a defect of the lane.
//
// It is a locked section that can change the credit, so it ends in unlock, as every such section
// does.
func (m *Machine) reclaimCredit(credit *RefreshCredit) {
	if !m.mu.TryLock() {
		return
	}
	defer m.unlock()
	if m.refreshCredit == credit {
		if m.laneWakeWaiting() {
			return
		}
		m.refreshCredit = nil
	}
	if credit.giveBack() {
		m.log.Warn(creditTakenBackMessage, "state", m.claim.State, "wake", m.wake != nil)
	}
}

// endRefreshLane holds the stale-plugin lane to its invariants, at the one line every decision of
// a machine ends on (Machine.unlock): the credit goes back unless a refresh wake of this machine's
// own is waiting in the pacing, which is the one thing a credit is held for, from the park that
// raises it to the end of the turn that runs it (laneWakeWaiting). The intent goes whatever the
// decision did with it: it names one launch and no other, and a retry that still owes one carries
// it in its own closure (retryHolding).
func (m *Machine) endRefreshLane() {
	if !m.laneWakeWaiting() {
		m.releaseRefresh()
	}
	m.pluginRelaunch = nil
}
