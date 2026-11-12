package supervise

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ProcessRead is what one read of the process a claim records found, at the moment of the read.
// It is the answer a caller that must know whether a process stands behind a claim gets, and the
// one such answer the machine gives: a claim stored working is in a turn, or it is one no
// process stands behind at all (Claim.ProcessGone, which states the windows that leave one), and
// what the daemon last heard of that process is memory a restart forgets. A caller reads the
// process itself instead (ReadProcess), and this is what it reads.
type ProcessRead string

const (
	// ProcessRuns is the runtime answering that the process the claim records is running: alive,
	// or alive at an address a process launched now is not handed (runtime.StaleAddress), which
	// is a live process with a turn of its own (repoint).
	ProcessRuns ProcessRead = "runs"
	// ProcessGone is the runtime answering that the process the claim records is not running:
	// nothing is there (runtime.Gone), or what is there is not the process the locator recorded
	// (runtime.NotRecordedProcess). Whatever the claim's session was doing when that process
	// went, no turn of it is running now.
	ProcessGone ProcessRead = "gone"
	// ProcessUnanswered is the runtime unable to say either — a verdict of runtime.Uncertain, or
	// a runtime that could not be asked at all. It is no statement about the process, so a
	// caller acts on it as it acts on knowing nothing: the claim is left exactly as it stands.
	ProcessUnanswered ProcessRead = "unanswered"
	// ProcessNotRecorded is no read having been made because the claim records no process: it
	// has none for the runtime to be asked about. It is not an answer about any process, and the
	// record is all the caller has of the claim it is given for (ReadProcess, api's
	// refuseWorkingClaim).
	ProcessNotRecorded ProcessRead = "not recorded"
	// ProcessReplacedByLaunch is no read having been made because a launch the wake pacing parks
	// waits over the process the claim records: that launch's turn lets the process go and
	// starts the next, so what the process is doing now decides nothing (wakeNow, launchNow). It
	// is not an answer about the process either; it says the claim is between states, with that
	// launch in flight, as the machine stood under the lock the read was asked under.
	ProcessReplacedByLaunch ProcessRead = "replaced by launch"
)

// processReadBound is how long the runtime has to answer one read of a claim's process
// (ReadProcess). It is a constant with no setting, as the read itself is one runtime call and
// not a deployment's business: five seconds is many times what a runtime that is answering takes
// to say what one process is doing, and it is the whole of what a runtime that has stopped
// answering may hold for. The read is made under the machine's lock, where every other event of
// the claim queues behind it — the sweep's reports, the agent's stream, the operator's own
// requests — so a runtime left to its own patience here would stop the claim rather than report
// on it. A read the bound cuts short answers ProcessUnanswered, which says exactly what has
// happened: the runtime did not answer.
const processReadBound = 5 * time.Second

// ReadProcess asks the runtime about the process the claim records, now, and answers what it
// found. It is no event and no row, as ReleaseUncertainLaunch and PodUpdateTold are neither: it
// takes the machine's lock, reads, and answers. The lock is what makes the answer one the caller
// can act on — the claim cannot move under the read, and the machine's fence drops any report of
// another incarnation — and it is held through the publish, because the read can change what the
// claim publishes (current, Claim.ProcessGone).
//
// Two claims are asked nothing, and each answers why with its own answer. A claim that records no
// process has none to read (ProcessNotRecorded), and a claim whose paced launch waits over the
// process it records has one whose fate is already decided: its turn lets that process go and
// starts the next (ProcessReplacedByLaunch, wakeNow, launchNow). probe leaves both alone for
// those same reasons. The reason is told apart here, under the lock, because this is the one
// moment it is true of the claim the caller asked about: a view read after the lock is released
// can find the parked launch already run and the claim launching. The route reads the parked
// launch before it calls this (api's relaunchWaits), and a park raised between its view and this
// lock is what ProcessReplacedByLaunch answers; a claim recording no process is no claim anyone
// asks this about.
//
// What the read finds is recorded in memory exactly as the sweep's own report of the same
// process is recorded (reportedProcess), and nothing of it is written: no store write is made,
// so a caller that refuses on the answer has changed nothing of the stored claim (api's
// refuseWorkingClaim, dispatch://INFRA-3161 R3). The one record that parts from the store is the
// death charge a running verdict clears (Claim.DeathCharged): judge writes that clear where it
// is made, and the read leaves it in memory, for the next write the claim makes to carry. A
// restart before that write restores the charge, which is the record the store held before the
// read and no worse than a read never made. Nothing else is judged: no death, no displacement,
// no repoint, no relaunch and no timer armed. What the answer means is the caller's — the
// recovery it is read for suspends the claim and starts it again — and a read that relaunched
// would start a process the caller is about to replace, so the read reports and the caller
// decides.
func (m *Machine) ReadProcess(ctx context.Context) ProcessRead {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.Locator == nil {
		m.log.Info("supervise: the claim records no process to read; the runtime is asked nothing")
		return ProcessNotRecorded
	}
	if m.launchWaits() {
		m.log.Info("supervise: the process a waiting launch replaces is not read; its turn replaces that process",
			"incarnation", m.claim.Locator.Incarnation, "due", m.wake.at)
		return ProcessReplacedByLaunch
	}
	reading, cancel := context.WithTimeout(ctx, processReadBound)
	defer cancel()
	observation, err := m.deps.Runtime.Probe(reading, *m.claim.Locator)
	if err != nil {
		// The runtime itself could not be asked, which is never a statement about the process
		// (runtime.Runtime.Probe). Nothing is recorded: the last report of this process, from
		// whenever it came, still stands.
		m.log.Warn("supervise: the runtime could not read the claim's process",
			"incarnation", m.claim.Locator.Incarnation, "error", err)
		return ProcessUnanswered
	}
	// A running verdict clears the death charge in memory and not in the store: the read writes
	// nothing, and the next write of the claim carries the clear (Claim.DeathCharged).
	m.reportedProcess(observation)
	m.log.Info("supervise: the claim's process was read at the caller's request",
		"incarnation", m.claim.Locator.Incarnation, "observed", string(observation.Kind), "detail", observation.Detail)
	read := processReadOf(observation.Kind)
	if read == ProcessUnanswered && observation.Kind != runtime.Uncertain {
		// A verdict this daemon has no word for is read as no answer rather than as a process
		// gone: the caller suspends a claim on gone, and a kind nobody here understands must not
		// stop a worker. The runtime and this package move together, so it says so in the log.
		m.log.Error("supervise: the runtime answered the read with a verdict this daemon does not know; reading it as no answer",
			"incarnation", m.claim.Locator.Incarnation, "observed", string(observation.Kind))
	}
	return read
}

// processReadOf is what a verdict of the runtime says of the process it is about, and the one
// grouping of the kinds there is: the read answers it (ReadProcess), and the machine records it
// as what the last report of the process found (reportedProcess), so the two cannot part on a
// kind. Nothing there and something that is not the process the locator recorded are both a
// process gone; a live process and one alive at a stale address both run; and a verdict of
// Uncertain, or a kind this daemon has no word for, says nothing of the process either way.
func processReadOf(kind runtime.ObservationKind) ProcessRead {
	switch kind {
	case runtime.Gone, runtime.NotRecordedProcess:
		return ProcessGone
	case runtime.Alive, runtime.StaleAddress:
		return ProcessRuns
	default:
		return ProcessUnanswered
	}
}
