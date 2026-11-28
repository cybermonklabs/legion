package supervise

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// A claim write the store refused makes memory the stored row again (R2 of dispatch://INFRA-4099's
// spec): the refusal stands for the rest of its decision (Machine.refusal), and the decision ends
// by reading the claim's row and building the machine again over it, as a daemon start builds one
// (settleRefusal). A read that lands opens a window of one probe interval in which no claim write
// reaches the store (withheld); a read that fails holds every decision until a read lands
// (awaitRead). The decisions D1 to D5 and D9 of that issue's plan are the design this file builds.

// RereadLine is the line of a claim read again after a refused claim write, at Info
// (settleRefusal, reread): the refused write's own line (RefusedWriteLine) says what was refused,
// and this one what the machine holds now, the state of the row it was built again over, the
// pending delivery that row holds and the end of the window its writes wait out
// (Machine.writesWaitUntil); a read that lands after one that failed adds the events the wait
// dropped (Machine.rereadDropped). It is pinned as a constant for the reason RefusedWriteLine
// is: an operator and a production check read the pair of lines whole. It is exported for the
// reason RefusedWriteLine is: the claim-write-refused notice names both lines whole.
const RereadLine = "supervise: the claim was read again from the store after a refused write"

// rereadFailedLine is the line of a read after a refused claim write that failed or answered no
// row, at Warn, at each try (reread): the claim decides nothing until a read lands, and the read
// is tried again at the probe interval.
const rereadFailedLine = "supervise: the claim could not be read again from the store after a refused write"

// claimRefusal is a claim write the rule refused (refusedWrite), as the decision that met it holds
// it until its end (Machine.refusal): the store call, which the lines of the read after it name
// (reread), and the store's error, which every later claim write of the decision answers
// (Machine.write).
type claimRefusal struct {
	store string
	err   error
}

// refused is the claim write the store refused in the decision now running, nil for none: the
// answer of every claim write the decision makes after it, none of which reaches the store
// (Machine.write, and withheld, which every claim write asks; D2 of dispatch://INFRA-4099's plan).
func (m *Machine) refused() error {
	if m.refusal == nil {
		return nil
	}
	return m.refusal.err
}

// withheld is the answer of a claim write the machine does not send to the store, nil for one it
// sends: the refusal the decision met (refused), and, in the window after a read of the row that
// landed (writesWait, D5 of dispatch://INFRA-4099's plan), errWritesWait. The rule owes that one
// (SortClaimWriteFailure), so the decision goes on over memory as it stands, and the first
// decision after the window makes the write (repair). Every claim write asks it first: the writes
// through writeClaim, revive's admission and ReleaseNames' release.
func (m *Machine) withheld() error {
	if err := m.refused(); err != nil {
		return err
	}
	if _, waits := m.writesWait(); waits {
		return errWritesWait
	}
	return nil
}

// writeClaim is write for a claim write (SortClaimWriteFailure names the seven): a write withheld
// answers is not made, and every other is made as write makes it.
func (m *Machine) writeClaim(ctx context.Context, call func(context.Context) error) error {
	if err := m.withheld(); err != nil {
		return err
	}
	return m.write(ctx, call)
}

// writesWait is the end of the window after a read that landed (Machine.writesWaitUntil), and
// whether the clock is still inside it. A window that has ended is cleared, so a machine past it
// reads no clock here again.
func (m *Machine) writesWait() (time.Time, bool) {
	until := m.writesWaitUntil
	if until.IsZero() {
		return until, false
	}
	if m.deps.Clock.Now().Before(until) {
		return until, true
	}
	m.writesWaitUntil = time.Time{}
	return time.Time{}, false
}

// armWindowEnd arms TimerWindowEnd at the end of the window (Machine.writesWaitUntil) where a
// claim write the window held left a write owed (Machine.owedWrite), so the window's end makes that
// write (repair) whatever else comes. A decision in the window keeps on the owed mark what it does
// outside the machine once its record is written, and the durable report it owes (recorded,
// oweDurableState), and only a write that makes the owed write acts on them. A decision that let
// the claim's process go is followed by no observation of that process, so without this timer
// those acts and that report would wait for an event that may never come.
//
// unlock calls it after settleRefusal, once the decision's rows have run, so the decision's own
// let-go (letGo, whose disarmAll disarms every timer) cannot take it off again. A timer already
// armed is the one for this window, since a refusal's rebuild disarms every timer and builds none
// of this kind (giveBackHeld, build). Once the window has ended, or with nothing owed, it arms
// nothing.
func (m *Machine) armWindowEnd() {
	if m.owedWrite == nil {
		return
	}
	until, waits := m.writesWait()
	if !waits {
		return
	}
	if _, armed := m.timers[TimerWindowEnd]; armed {
		return
	}
	m.armAt(TimerWindowEnd, until, "")
}

// windowEnded takes timer, a firing of TimerWindowEnd, off the machine where it is the arming the
// machine holds, so no timer of the window stays armed once the window has ended. The firing's
// decision is the repair at the head of the decision alone (handle).
func (m *Machine) windowEnded(timer Timer) {
	if a, armed := m.timers[TimerWindowEnd]; armed && a.seq == timer.Seq {
		delete(m.timers, TimerWindowEnd)
	}
}

// settleRefusal ends a decision in which the store refused a claim write (Machine.refusal), and
// returns at once in one that met none: it clears the refusal, so it runs once whichever of its two
// callers comes first — handle's own defer, which runs before every other end of a decision of an
// event, and unlock, the end of every other locked section (D2 of dispatch://INFRA-4099's plan).
//
// It gives back first what the refused decision held (giveBackHeld), then reads the claim and its
// pending delivery from the store and builds the machine again over the row it read (reread).
func (m *Machine) settleRefusal(ctx context.Context) {
	refusal := m.refusal
	if refusal == nil {
		return
	}
	m.refusal = nil
	m.giveBackHeld()
	m.reread(ctx, refusal.store)
}

// reread reads the claim and its pending delivery from the store (Store.ReadClaim), once, under
// the claim-write bound (bounded), after a refused claim write: settleRefusal's read, and each try
// of it again while it is owed (awaitRead). store is the refused write's store call, which each
// line names.
//
// A read that lands builds the machine again over the row it read (rebuild, D3), and opens the
// window of D5 of dispatch://INFRA-4099's plan: until a probe interval from now no claim write
// reaches the store (Machine.writesWaitUntil, withheld). So a claim whose every write is refused
// tries one write in each probe interval, and a launch in the window starts no process, since a
// launch's first step is its launching write and the launch ends where that write fails (writeTry).
// No launch is dropped for it: one whose launching write the window held is tried again at the
// probe interval, a request's whose admission it would hold is made at the window's end
// (reviveAfterWindow), and a release of an uncertain launch waits for that end
// (ReleaseUncertainLaunch), which the boot's sweep tries again. No act of a decision is dropped
// either: one whose write the window held and left owed keeps its acts and its report on the owed
// mark, and the window's end makes that write (TimerWindowEnd, armWindowEnd).
//
// A read that fails, or that finds no row, builds nothing: the read is owed (Machine.rereadOwed)
// and tried again at the probe interval (TimerReread), and the claim decides nothing until one
// lands (awaitRead). What the refused decision held is given back all the same, and that
// decision's end gives back the rest (unlock's endPodPark, endRefreshLane and tellWakeQueue); it
// publishes nothing, so every reader reads the claim as it was published before that decision.
func (m *Machine) reread(ctx context.Context, store string) {
	reading, cancel := m.bounded(ctx)
	row, found, err := m.deps.Store.ReadClaim(reading, m.claim.Token)
	cancel()
	probe := m.deps.Timeouts.Probe
	if err != nil || !found {
		m.rereadOwed = store
		m.arm(TimerReread, probe, "")
		m.log.Warn(rereadFailedLine, "store", store, "found", found, "error", err, "retry", probe)
		return
	}
	owed, dropped := m.rereadOwed != "", m.rereadDropped
	m.rereadOwed, m.rereadDropped = "", nil
	m.rebuild(ctx, row)
	m.writesWaitUntil = m.deps.Clock.Now().Add(probe)
	attrs := []any{"store", store, "state", row.State, "generation", row.Generation, "delivery", pendingID(row.Pending),
		"writesWaitUntil", m.writesWaitUntil}
	if owed {
		total := 0
		for _, n := range dropped {
			total += n
		}
		attrs = append(attrs, "dropped", total, "droppedEvents", dropped)
	}
	m.log.Info(RereadLine, attrs...)
}

// awaitRead is handle while a read of the claim's row is owed after a refused claim write
// (Machine.rereadOwed, D5 of dispatch://INFRA-4099's plan): it decides nothing over the memory
// that decision left. The read's own timer tries the read (reread). A request is answered a wait
// until that timer's next try (rereadWait), and so is the end of a compaction, whose answer is
// what Machine.Compact reads of it; the caller asks again then. Every other event is dropped,
// counted by its kind (Machine.rereadDropped), and named in the line of the read that lands.
func (m *Machine) awaitRead(ctx context.Context, ev Event) error {
	if timer, ok := ev.(Timer); ok && timer.Kind == TimerReread {
		if pass, err := m.fence(ctx, ev); !pass {
			return err
		}
		m.reread(ctx, m.rereadOwed)
		return nil
	}
	_, request := requestName(ev)
	if _, ending := ev.(CompactEnded); request || ending {
		return m.rereadWait()
	}
	if m.rereadDropped == nil {
		m.rereadDropped = map[eventKind]int{}
	}
	m.rereadDropped[kindOf(ev)]++
	return nil
}

// rereadWait is the answer of a request, and of every locked section of the machine, while a read
// of the claim's row is owed (Machine.rereadOwed): a wait until the read's next try (TimerReread),
// at which the caller asks again, nothing of its ask having happened. It is nil when no read is
// owed.
func (m *Machine) rereadWait() error {
	if m.rereadOwed == "" {
		return nil
	}
	next := m.timers[TimerReread].due
	return wait.Until(next, fmt.Sprintf("claim %s decides nothing until its row is read again after a refused claim write; the read is tried again at %s",
		m.claim.Token, next.UTC().Format(time.RFC3339)))
}

// giveBackHeld is the first half of the rebuild (D3 of dispatch://INFRA-4099's plan), run before
// the read so that a read that fails leaves nothing held either: what the refused decision held,
// given back through each one's own owner. That is every timer (disarmAll, which gives up a paced
// wake's place through the wake's placeTimer), the wake, a relaunch left waiting, the held
// suspension, a stale-plugin relaunch's mark and the plugin intent, the send in flight and the
// prompts outstanding (forgetSend, forgetOutstanding), and what the refused decision was to do
// outside the machine once its record was written (Machine.awaitingRecord, D6): the refused write
// acts on nothing. The outcomes of that send and of those prompts are then fenced as a daemon
// start fences them (fenceOutcome): an outcome of another generation or of another delivery than
// the rebuilt claim's pending one is stale, and one of that delivery at its generation is taken.
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
	m.awaitingRecord = afterRecord{}
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
//   - launchWritten: the channel of the decision HandleStart runs, set and cleared by that
//     decision's handle, so it belongs to the decision and not to the claim.
//   - turnOutcomeFailed: HandleTurnOutcome reads it, and clears it, after the rebuild.
//   - storeFaultTold: the hour bound of a session store's notice, which a rebuild must not tell
//     again.
//   - sentThrough: the stream's registration sequence goes on rising in this daemon's run.
//   - answer, steppedAt, openCalls: what this daemon observed of the agent; the stall backstop's
//     fence reads steppedAt.
//   - podPark, refreshCredit, queueTold: kept to the end of the decision, and settled there by
//     unlock against the rebuilt claim (giveBackHeld).
//   - askDelivered: a publish of an update round's ask that happened and whose mark the store
//     refused (PublishUpdateAsk, D9), which the row read again does not hold.
//
// Set by the rebuild, the state of the rule itself (D5): refusal, cleared as the rebuild begins
// (settleRefusal); rereadOwed and rereadDropped, set while no read of the row has landed and
// cleared by the read that lands; writesWaitUntil, the end of the window that read opens
// (reread). The timer of the window's end (TimerWindowEnd) is no part of a rebuilt machine: the
// rebuild disarms every timer, and only a decision in the window that leaves a write owed arms it
// (armWindowEnd).
//
// Kept in part:
//
//   - registeredPods: the row's joined with memory's, since the pods an agent registered in are
//     none of the row's (build).
//   - previous, stopUnconfirmed: a process this daemon let go still runs in this runtime, which a
//     restart has none of; but a previous the row records is the claim's process again, and is
//     cleared with the mark of its stop. That is the process a suspension of a live process or a
//     stop or a close stopped or released before its record, where the store then refused that
//     record (suspendNow, end): the rebuilt claim records it as the row does, and the next
//     observation finds it gone and recovers it as a daemon start over the same row would.
//   - registration, registrationRefused, identity, enrollmentSent, helloKey: kept where the row
//     records the process each is for, since that process's shim sends this daemon no second
//     hello; otherwise the registration is stopped and each is zeroed (keepRowsProcess).
//   - podLoad: kept where the row records the process whose load it holds, since a load has a
//     bound once started; otherwise ended (keepRowsProcess).
//
// The claim's enrollment is the row's: one memory holds and the row does not is revoked at the
// broker, as the boot revokes one its repair detaches (internal/daemon's supervisor.restore). What
// the refused decision was to do outside the machine is dropped (giveBackHeld), and so is what the
// owed mark held for its write (owedClaimWrite.after): the row records the process, the
// enrollment and the registration those acts would have ended.
//
// Two things differ from a daemon start's machine on purpose (D4). A process memory records and
// the row does not — a start whose locator write was refused — is stopped, and it becomes the
// process the claim last ran (previous): no row names it, and in this daemon's run no orphan sweep
// stops it. A stop the runtime refuses is logged at Error and leaves stopUnconfirmed set. A
// process a refused decision let go after the runtime stopped it, which the row still records
// (suspendNow, end), is no such process: memory's claim records none any more, so it is not
// stopped again. And a row that names a launch no process carries (Claim.UnfinishedLaunch) — a
// live state with no process, or a released launch, the two shapes a daemon start repairs as
// unfinished — is left to be launched again at the probe interval (relaunchLater), uncharged and
// paced, since nothing else would relaunch it; the launch's first step is its own claim write
// (writeTry), so a store that refuses every write starts no process.
func (m *Machine) rebuild(ctx context.Context, row Claim) {
	process, enrollment := m.claim.Locator, m.claim.Enrollment
	m.keepRowsProcess(row)
	if enrollment != nil && (row.Enrollment == nil || *row.Enrollment != *enrollment) {
		m.revoke(*enrollment)
	}
	if p := m.previous; p != nil && row.Locator != nil && p.Incarnation == row.Locator.Incarnation {
		m.previous, m.stopUnconfirmed = nil, false
	}
	m.build(row)
	if process != nil && (row.Locator == nil || row.Locator.Incarnation != process.Incarnation) {
		m.previous = process
		if err := m.stopPrevious(ctx); err != nil {
			m.log.Error("supervise: after a refused claim write, the runtime did not stop the process the claim's row does not record",
				"incarnation", process.Incarnation, "error", err)
		}
	}
	if row.UnfinishedLaunch() {
		m.relaunchLater(m.deps.Timeouts.Probe, WakeLaunch, m.launchFromTop(launchTry{}))
		m.publish()
	}
}

// keepRowsProcess keeps, of what memory holds about the claim's process, what is about the process
// row records, and lets the rest go through its own owner: the load of the issue pod's start
// (endLoad), the pod-session registration (stopRegistration), and the marks of that registration
// and of the hello, the identity and its enrollment's send, each bound to its incarnation. The
// process memory last recorded is the one the claim records, or the one a let-go the store refused
// moved into previous (letGo), whose marks that let-go left standing for its record.
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
	last := m.claim.Locator
	if last == nil {
		last = m.previous
	}
	if last == nil || !records(last.Incarnation) {
		m.enrollmentSent = false
	}
}
