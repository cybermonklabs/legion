package supervise

import (
	"context"
	"slices"
)

// A claim write the store refused makes memory the stored row again (R2 of dispatch://INFRA-4099's
// spec): the refusal stands for the rest of its decision (Machine.refusal), and the decision ends
// by reading the claim's row and building the machine again over it, as a daemon start builds one
// (settleRefusal). The decisions D1 to D4 of that issue's plan are the design this file builds.

// rereadLine is the line of a claim read again after a refused claim write, at Info
// (settleRefusal): the refused write's own line (refusedWriteLine) says what was refused, and this
// one what the machine holds now, the state of the row it was built again over and the pending
// delivery that row holds. It is pinned as a constant for the reason refusedWriteLine is: an
// operator and a production check read the pair of lines whole.
const rereadLine = "supervise: the claim was read again from the store after a refused write"

// rereadFailedLine is the line of a read after a refused claim write that answered no row, at Warn
// (settleRefusal).
const rereadFailedLine = "supervise: the claim could not be read again from the store after a refused write"

// claimRefusal is a claim write the rule refused (refusedWrite), as the decision that met it holds
// it until its end (Machine.refusal): the store call, the store's error, which every later claim
// write of the decision answers (Machine.write), the SQLSTATE and the constraint (refusalOf), and
// the state and the generation of the claim the store refused.
type claimRefusal struct {
	store                string
	err                  error
	sqlstate, constraint string
	state                ClaimState
	generation           uint64
}

// refused is the claim write the store refused in the decision now running, nil for none: the
// answer of every claim write the decision makes after it, none of which reaches the store
// (Machine.write, revive's admission, ReleaseNames' release; D2 of dispatch://INFRA-4099's plan).
func (m *Machine) refused() error {
	if m.refusal == nil {
		return nil
	}
	return m.refusal.err
}

// lostSession is a forget of the claim's session held for Deps.SessionLost (Machine.lostTold): the
// claim at the forget's launch, the forget's cause, and its detail (startFresh).
type lostSession struct {
	claim  Claim
	cause  SessionLoss
	detail string
}

// tellLost tells the daemon of the forget the decision now ending made (Machine.lostTold), once:
// that decision met no refused claim write, since a rebuild drops the forget with what it gives
// back (giveBackHeld, build). It is unlock's, after settleRefusal.
func (m *Machine) tellLost() {
	lost := m.lostTold
	if lost == nil {
		return
	}
	m.lostTold = nil
	m.deps.SessionLost(lost.claim, lost.cause, lost.detail)
}

// settleRefusal ends a decision in which the store refused a claim write (Machine.refusal), and
// returns at once in one that met none: it clears the refusal, so it runs once whichever of its two
// callers comes first — handle's own defer, which runs before every other end of a decision of an
// event, and unlock, the end of every other locked section (D2 of dispatch://INFRA-4099's plan).
//
// It gives back first what the refused decision held (giveBackHeld), then reads the claim and its
// pending delivery from the store (Store.ReadClaim), once, under the claim-write bound (bounded),
// and builds the machine again over the row it read (rebuild, D3).
//
// A read that fails, or that finds no row, builds nothing: what the decision held is given back
// all the same, and the line says so (rereadFailedLine). D5 of the plan holds the claim's decisions
// until a read lands and tries the read again at the probe interval; this decision leaves memory as
// the give-back left it.
func (m *Machine) settleRefusal(ctx context.Context) {
	refusal := m.refusal
	if refusal == nil {
		return
	}
	m.refusal = nil
	m.giveBackHeld()
	reading, cancel := m.bounded(ctx)
	row, found, err := m.deps.Store.ReadClaim(reading, m.claim.Token)
	cancel()
	if err != nil || !found {
		m.log.Warn(rereadFailedLine, "store", refusal.store, "found", found, "error", err)
		return
	}
	m.rebuild(ctx, row)
	delivery := ""
	if row.Pending != nil {
		delivery = row.Pending.ID
	}
	m.log.Info(rereadLine, "store", refusal.store, "state", row.State, "generation", row.Generation, "delivery", delivery)
}

// giveBackHeld is the first half of the rebuild (D3 of dispatch://INFRA-4099's plan), run before
// the read so that a read that fails leaves nothing held either: what the refused decision held,
// given back through each one's own owner. That is every timer (disarmAll, which gives up a paced
// wake's place through the wake's placeTimer), the wake, a relaunch left waiting, the held
// suspension, a stale-plugin relaunch's mark and the plugin intent, the send in flight and the
// prompts outstanding, whose outcomes are then fenced out as stale (forgetSend, forgetOutstanding),
// and a forget held for the daemon (Machine.lostTold).
//
// The start parked at the issue pod's gate, the refresh credit and the wake wait the daemon was
// told of stay in their fields: unlock gives them back against the claim the decision leaves, as it
// does at the end of every decision (endPodPark, endRefreshLane, tellWakeQueue).
func (m *Machine) giveBackHeld() {
	m.disarmAll()
	m.wake, m.relaunch = nil, nil
	m.held, m.heldLaunch = nil, false
	m.staleRelaunch, m.pluginRelaunch = nil, nil
	m.forgetSend()
	m.forgetOutstanding()
	m.lostTold = nil
}

// rebuild is the second half of the rebuild (D3 and D4 of dispatch://INFRA-4099's plan): the
// machine built again over row, the claim and its pending delivery as the store holds them, through
// the build NewMachine builds every machine through (build), so it holds what a daemon start
// builds over that row but for what it keeps of memory. Every field of Machine is built or kept,
// and TestARefusedWriteLeavesTheMachineAsADaemonStartBuildsIt holds the kept list beside this one.
//
// Kept as they are, each for its reason:
//
//   - mu, idle, decidingMu, deciding: the lock the decision holds, and its waiters.
//   - ctx, deps, log: the values NewMachine was given.
//   - onDurableState: the daemon installs it after NewMachine (internal/daemon's supervisor.add).
//   - view: readers hold the pointer, and build publishes the rebuilt claim into it.
//   - stopping: true only inside a runtime call of this decision.
//   - turnsEnded: a count the daemon reads across decisions (internal/daemon's supervisor.add).
//   - seq: a timer armed before the rebuild must stay fenced out by its sequence.
//   - goroutines: sends and revocations still running report to it, and Wait waits on it.
//   - startMu, cancelStart, startStopped: a start's cancel belongs to the decision that holds mu.
//   - turnOutcomeDecided: a test's hook.
//   - turnOutcomeFailed: HandleTurnOutcome reads it, and clears it, after the rebuild.
//   - storeFaultTold: the hour bound of a session store's notice, which a rebuild must not tell
//     again.
//   - sentThrough: the stream's registration sequence goes on rising in this daemon's run.
//   - answer, steppedAt, openCalls: what this daemon observed of the agent; the stall backstop's
//     fence reads steppedAt.
//   - podPark, refreshCredit, queueTold: kept to the end of the decision, and settled there by
//     unlock against the rebuilt claim (giveBackHeld).
//
// Kept in part:
//
//   - registeredPods: the row's joined with memory's, since the pods an agent registered in are
//     none of the row's (build).
//   - previous, stopUnconfirmed: a process this daemon let go still runs in this runtime, which a
//     restart has none of; but a previous the row records is the claim's process again, and is
//     cleared with the mark of its stop.
//   - registration, registrationRefused, identity, enrollmentSent, helloKey: kept where the row
//     records the process each is for, since that process's shim sends this daemon no second
//     hello; otherwise the registration is stopped and each is zeroed (keepRowsProcess).
//   - podLoad: kept where the row records the process whose load it holds, since a load has a
//     bound once started; otherwise ended (keepRowsProcess).
//
// The claim's enrollment is the row's: one memory holds and the row does not is revoked at the
// broker, as the boot revokes one its repair detaches (internal/daemon's supervisor.restore).
//
// Two things differ from a daemon start's machine on purpose (D4). A process memory records and
// the row does not — a start whose locator write was refused — is stopped, and it becomes the
// process the claim last ran (previous): no row names it, and in this daemon's run no orphan sweep
// stops it. A stop the runtime refuses is logged at Error and leaves stopUnconfirmed set. And a row
// that names a launch no process carries — a live state with no process, or a released launch
// (Claim.ReleasedLaunch), the two shapes a daemon start repairs as unfinished — is left to be
// launched again at the probe interval (relaunchLater), uncharged and paced, since nothing else
// would relaunch it; the launch's first step is its own claim write (writeTry), so a store that
// refuses every write starts no process.
func (m *Machine) rebuild(ctx context.Context, row Claim) {
	process, enrollment := m.claim.Locator, m.claim.Enrollment
	m.keepRowsProcess(row)
	if enrollment != nil && (row.Enrollment == nil || *row.Enrollment != *enrollment) {
		m.revoke(*enrollment)
	}
	m.build(row)
	if process != nil && (row.Locator == nil || row.Locator.Incarnation != process.Incarnation) {
		m.previous = process
		if err := m.stopPrevious(ctx); err != nil {
			m.log.Error("supervise: after a refused claim write, the runtime did not stop the process the claim's row does not record",
				"incarnation", process.Incarnation, "error", err)
		}
	}
	if p := m.previous; p != nil && row.Locator != nil && p.Incarnation == row.Locator.Incarnation {
		m.previous, m.stopUnconfirmed = nil, false
	}
	if (row.Locator == nil && slices.Contains(live, row.State)) || row.ReleasedLaunch() {
		m.relaunchLater(m.deps.Timeouts.Probe, m.launchFromTop(launchTry{}))
		m.publish()
	}
}

// keepRowsProcess keeps, of what memory holds about the claim's process, what is about the process
// row records, and lets the rest go through its own owner: the load of the issue pod's start
// (endLoad), the pod-session registration (stopRegistration), and the marks of that registration
// and of the hello, the identity and its enrollment's send, each bound to its incarnation.
func (m *Machine) keepRowsProcess(row Claim) {
	records := func(incarnation string) bool {
		return incarnation != "" && row.Locator != nil && row.Locator.Incarnation == incarnation
	}
	if hold := m.podLoad; hold != nil && (row.Locator == nil || row.Generation != hold.line.generation) {
		m.endLoad(podEndFor(row.State, podProcessEnded))
	}
	if r := m.registration; r != nil && !records(r.incarnation) {
		m.stopRegistration()
	}
	if !records(m.registrationRefused.incarnation) {
		m.registrationRefused = registrationKey{}
	}
	if m.identity != nil && !records(m.identity.incarnation) {
		m.identity = nil
	}
	if !records(m.helloKey.incarnation) {
		m.helloKey = helloKey{}
	}
	if m.claim.Locator == nil || !records(m.claim.Locator.Incarnation) {
		m.enrollmentSent = false
	}
}
