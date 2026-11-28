package supervise

import (
	"context"
	"errors"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A decision of the daemon that ends its claim's process acts outside the machine only once its
// claim write has landed or failed owed (R3 of dispatch://INFRA-4099's spec, D6 of its plan): the
// broker's revocation of the agent's capability, the stop of the pod's session registration, the
// clean stop of the process's Envoy session, and the daemon's report of a forgotten session. Where
// the runtime's stop of the process is best effort — a failure, an exit, a suspension over a
// process found gone or replaced, a suspension with no process — that stop follows the write too,
// and a stop the runtime then refuses leaves the record standing. A suspension of a live process
// and a stop or a close (suspendNow, end) ask the runtime first, as they always have: a stop or a
// release the runtime refuses is the decision's refusal and writes nothing, so the claim, its row,
// its registration, the tree's cleanup reservation and the process stand as they were. A write the
// store refuses acts on nothing, so the capability and the registration stay as the row the machine
// is then built again over says they are (settleRefusal). A write the window after a refusal holds
// back keeps those acts on the owed mark for the write that makes it, which the window's end makes
// if no other decision does first (D5, TimerWindowEnd).

// afterRecord is what a decision does outside the machine once its claim write has recorded it:
// the acts letGo's memory changes leave to the record (letGo), the stop of the process the
// decision ended, the revocation of an enrollment the write's normalizing detached (stored), the
// report of a forget the launch made (startFresh), and the notice of a run of provider errors
// (tellOfProviderErrors). The zero value acts on nothing.
type afterRecord struct {
	// stops are the processes the decisions ended, oldest first, each with the locator saved
	// before letGo cleared it (letGo moves the claim's locator into previous, and Runtime.Release
	// of no locator stops nothing): the runtime's stop or release of each that is still to be made,
	// and the clean stop of its Envoy session.
	stops []processStop
	// stopPrevious is the stop of the process a launch let go, owed by a launching write that
	// failed owed (writeTry): the launch started nothing, so nothing waits that process out.
	stopPrevious bool
	// revoke is every enrollment the decisions took off the claim, to be revoked at the broker.
	revoke []Enrollment
	// forgetProcess is the stop of the pod-session registration the machine keeps for the process
	// let go, and the end of the marks of that process's hello, its identity and its enrollment's
	// send (Machine.registration, identity, enrollmentSent, helloKey), which stay set until the
	// record of the let-go is written.
	forgetProcess bool
	// lost is the forget of the claim's session a launch made (startFresh), reported to the
	// daemon once the claim write that records the forget is made (Deps.SessionLost); nil for
	// none.
	lost *lostSession
	// told is the notice of a run of provider-error turns that reached providerErrorRunTold,
	// handed to the daemon once the claim write that records the run's count is made
	// (tellOfProviderErrors, Deps.ProviderErrorRunReached); nil for none.
	told *providerErrorNotice
}

// lostSession is a forget of the claim's session held for Deps.SessionLost (afterRecord.lost): the
// claim at the forget's launch, the forget's cause, and its detail (startFresh).
type lostSession struct {
	claim  Claim
	cause  SessionLoss
	detail string
}

// join is a's acts followed by b's.
func (a afterRecord) join(b afterRecord) afterRecord {
	a.stops = append(a.stops, b.stops...)
	a.stopPrevious = a.stopPrevious || b.stopPrevious
	a.revoke = append(a.revoke, b.revoke...)
	a.forgetProcess = a.forgetProcess || b.forgetProcess
	if b.lost != nil {
		a.lost = b.lost
	}
	if b.told != nil {
		a.told = b.told
	}
	return a
}

// empty is whether a acts on nothing at all.
func (a afterRecord) empty() bool {
	return len(a.stops) == 0 && !a.stopPrevious && len(a.revoke) == 0 && !a.forgetProcess && a.lost == nil && a.told == nil
}

// processStop is one process a decision ended, as its record leaves it to the acts that follow
// (afterRecord.stops): a suspension's Runtime.Suspend of loc, or a retirement's Runtime.Release of
// the claim and loc, made once the record is written, best effort (stopRecorded) — or, where made
// is set, already made before the record by the decision itself (suspendNow), which leaves only
// the clean stop to follow the record. session and sessionFile name the Envoy session whose clean
// stop is sent once the runtime has made a suspension's stop and the record is written
// (sessionGone), "" for a stop that sends none: a failure's, a retirement's, and one whose decision
// has sent the clean stop already (stopSent).
type processStop struct {
	// loc is the process; nil only for a release of a claim that records none, which ends the
	// claim at the runtime all the same (Runtime.Release).
	loc                  *runtime.Locator
	release              bool
	made                 bool
	session, sessionFile string
}

// incarnation is the stopped process's incarnation, for a line, "" for none.
func (s processStop) incarnation() string {
	if s.loc == nil {
		return ""
	}
	return s.loc.Incarnation
}

// stopRefusedLine is the line of a best-effort stop or release the runtime refused after the
// record of the decision that made it was written (stopRecorded), at Error: the claim reads as its
// record does, and the process may still run. It is pinned as a constant because a search for it
// is the search for every process the daemon let go of and could not stop.
const stopRefusedLine = "supervise: the runtime refused the suspend or the release of a process whose claim's record is written; the record stands"

// awaitRecord adds after to the acts that wait for the claim's next record (Machine.awaitingRecord):
// the next claim write that writes the claim as memory holds it acts on them once it has landed or
// failed owed, and drops them where the store refuses it (recorded).
func (m *Machine) awaitRecord(after afterRecord) {
	m.awaitingRecord = m.awaitingRecord.join(after)
}

// recorded hands the acts that wait for the claim's record (Machine.awaitingRecord) what the claim
// write that records it answered, err. Every claim write that writes the claim as memory holds it,
// normalized (stored), calls it once the store has answered.
//
//   - A write that landed acts at once.
//   - A write the window after a refusal held back (errWritesWait) acts on nothing yet: the acts go
//     onto the owed mark (owedClaimWrite.after), for the write that makes that write to act on
//     once it lands (settleOwedWrite) or fails owed with a store's own failure (below). The
//     window's end makes that write if no other decision does first (armWindowEnd).
//   - A write that failed owed acts at once, and first acts on whatever the mark held, since the
//     write it held them for has now been tried and failed with the store's own answer: the
//     decision stands, its write owed, as every decision of an owed write stands (R3).
//   - A write the store refused drops them: the decision ends by building the machine again over
//     the claim's row (settleRefusal), whose process, enrollment and registration are the ones
//     these acts would have ended.
func (m *Machine) recorded(ctx context.Context, err error) {
	after := m.awaitingRecord
	m.awaitingRecord = afterRecord{}
	switch {
	case err == nil:
		m.act(ctx, after)
	case errors.Is(err, errWritesWait):
		m.holdAfterRecord(after)
	case SortClaimWriteFailure(err) == WriteOwed:
		m.actOnHeld(ctx)
		m.act(ctx, after)
	}
}

// holdAfterRecord keeps after on the owed mark (owedClaimWrite.after) while the window after a
// refused claim write holds the write back (D5 of dispatch://INFRA-4099's plan): the write that
// makes the owed write acts on them (settleOwedWrite, recorded), and a refusal of it drops them with
// the mark. An after that acts on nothing makes no mark: a write whose decision holds nothing for
// its record changes memory only once the store takes it, or keeps a mark of its own (oweWrite, or
// the delivery's unsaved), so a mark made here alone would owe a write that records nothing.
func (m *Machine) holdAfterRecord(after afterRecord) {
	if after.empty() {
		return
	}
	if m.owedWrite == nil {
		m.owedWrite = &owedClaimWrite{}
	}
	m.owedWrite.after = m.owedWrite.after.join(after)
}

// actOnHeld acts on what the owed mark held for its write (owedClaimWrite.after), now that a claim
// write has tried that write and failed owed with the store's own answer (recorded).
func (m *Machine) actOnHeld(ctx context.Context) {
	if w := m.owedWrite; w != nil {
		held := w.after
		w.after = afterRecord{}
		m.act(ctx, held)
	}
}

// act makes a decision's acts outside the machine, in the order D6 of dispatch://INFRA-4099's plan
// gives them: the runtime's stop of each process still to be stopped, with the clean stop of its
// Envoy session, the stop of a process a launch let go, the revocation of each enrollment, the stop
// of the pod-session registration with the marks of the process's hello, and the report of a
// forgotten session; and last the notice of a run of provider errors.
func (m *Machine) act(ctx context.Context, a afterRecord) {
	for _, s := range a.stops {
		m.stopRecorded(ctx, s)
	}
	if a.stopPrevious {
		if err := m.stopPrevious(ctx); err != nil {
			m.log.Warn("supervise: the runtime did not stop the process a launch let go once the launch's write was made; the launch's next try waits it out",
				"generation", m.claim.Generation, "error", err)
		}
	}
	for _, e := range a.revoke {
		m.revoke(e)
	}
	if a.forgetProcess {
		m.identity, m.enrollmentSent, m.helloKey = nil, false, helloKey{}
		m.stopRegistration()
	}
	if l := a.lost; l != nil && m.deps.SessionLost != nil {
		m.deps.SessionLost(l.claim, l.cause, l.detail)
	}
	if n := a.told; n != nil {
		m.tellProviderErrorRun(*n)
	}
}

// stopRecorded is the stop s of a process whose decision's record is written. A stop its decision
// made before the record (processStop.made: suspendNow) sends the clean stop of the process's Envoy
// session and nothing else. Any other is best effort: the runtime is asked for it now, and a stop
// the runtime refuses leaves the record standing, since the decision is made — it is logged at
// Error (stopRefusedLine) and recorded as a process the runtime may have left running
// (stopUnconfirmed), and the claim moves all the same, as a failure, an exit and a suspension over
// a process found gone always moved it.
func (m *Machine) stopRecorded(ctx context.Context, s processStop) {
	if s.made {
		m.sessionGoneOf(ctx, s.session, s.sessionFile)
		return
	}
	err := m.makeStop(ctx, s)
	if err == nil {
		return
	}
	if s.loc != nil {
		m.stopUnconfirmed = true
	}
	m.log.Error(stopRefusedLine, "incarnation", s.incarnation(), "release", s.release, "state", m.claim.State, "error", err)
}

// makeStop asks the runtime for the stop s, while View says the process is being stopped
// (ClaimView.Stopping), and sends the clean stop of the process's Envoy session once a suspension's
// stop has returned (sessionGone): both runtimes answer only when the process has ended. A stop the
// runtime refused sends nothing, since the process may still run.
func (m *Machine) makeStop(ctx context.Context, s processStop) error {
	defer m.stoppingProcess()()
	var err error
	if s.release {
		err = m.deps.Runtime.Release(ctx, runtime.Known{Claim: m.claim.Token, Locator: cloneLocator(s.loc)})
	} else {
		err = m.deps.Runtime.Suspend(ctx, *s.loc)
	}
	if err != nil {
		return err
	}
	if s.session != "" {
		m.sessionGoneOf(ctx, s.session, s.sessionFile)
	}
	return nil
}
