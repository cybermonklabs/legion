package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// repoint is the claim's process found alive holding an address a process launched now is not
// handed (runtime.StaleAddress): one of the addresses the runtime hands every new process from the
// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch, the
// secrets broker) moved since this one launched, as each does when the daemon restarts with its key
// changed; the worker stream's keys are `advertise_host`, `bind` when no `advertise_host` is set,
// and `worker_stream_port`. The process keeps what it holds for as long as it runs, and what it
// holds may never reach the daemon or the service again, so there is nothing to wait out. A task
// whose turn the process was running goes back to waiting first (interrupted), the same recovery a
// death in a turn gets: that turn used the addresses the process holds, and the relaunch ends it. A
// held suspension ends here as it does for a death (endHeld): the claim is suspended, not
// relaunched. Otherwise the claim is relaunched at once through the launch path a death uses
// (launch): a Resume of its recorded session, or a Spawn over the claim's existing Sandbox when it
// has not registered yet, onto a process handed the current addresses. The stale observation is
// never charged, unlike a death (chargeDeath, relaunchAfterFailure): the process did nothing wrong.
// A relaunch the runtime refuses is charged as any launch failure is.
func (m *Machine) repoint(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: the process is alive at a stale address; replacing it with one at the current address",
		"incarnation", m.claim.Locator.Incarnation, "detail", observation.Detail)
	_, err := m.replaceProcess(ctx, "the process at a stale address is replaced mid-turn", ordinaryEnd)
	return err
}

// displaced is the claim's process found gone, or found to be some other process, through no
// fault of its own (runtime.Observation.Displaced): the daemon itself replaced its pod while it ran,
// to launch another role there, or it was another runtime's process — a Sandbox role process a
// daemon running the project under kubernetes recorded — that this daemon takes over
// (`runtime.tmux.take_over`) and cannot see. The observation's detail names which. The process did
// nothing wrong, so it is recovered as repoint recovers a stale one: nothing is charged, neither a
// launch failure nor a death with work outstanding, and the claim's session is relaunched at once
// (replaceProcess). A turn the process was in is lost, and its task goes back to waiting, sent
// again once the session resumes; a held suspension ends in a suspension. It is the one recovery
// that carries an out-of-memory end's note (runtime.Observation.OutOfMemory), since a pod the
// daemon replaced can also have run out of memory before the replacement landed, and the role
// whose turn that end cut off is told so wherever the end is recovered.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", m.claim.Locator.Incarnation, "process", m.claim.Locator.Label(), "observed", string(observation.Kind), "detail", observation.Detail)
	_, err := m.replaceProcess(ctx, "the process was displaced mid-turn, its pod replaced by the daemon or it another runtime's process this daemon takes over", observation.OutOfMemory)
	return err
}

// ProcessEnd is what a replacement did to the claim's process (replaceProcess): the end the
// machine reached, which is not always the one it set out to make. It is read from the claim the
// replacement left rather than from its error, because the two answer different questions — the
// error says whether everything that end needed was written, and this says what became of the
// worker — and a caller that tells somebody about it needs the second (backstopStalledTurn,
// Deps.StallBackstopped). A process ended whose record the store then refused is ended all the
// same, and a replacement that stopped before it touched the process ended nothing at all.
//
// It is the daemon's whole classification of what became of a worker, so a state a replacement
// can leave a worker in is a value here and never a hole between two values: the owner's notice
// is written from this type alone, and three things are total over it rather than over the paths
// anyone remembered — the telling (TellsOwner), the sentence the notice carries (the daemon's
// stallBackstopEnds) and the case that drives each end (the walk in stallbackstop_test.go).
// Every value is listed in ProcessEnds, which those walks read.
type ProcessEnd string

const (
	// EndNotMade is the claim still holding the process the replacement found, and its turn with
	// it, with no launch queued: the take-back that replacement begins with was refused
	// (interrupted), or the take-back landed and the launch's own lifecycle recheck refused
	// (relaunchRefused). No process was let go and none is coming, so the next sweep posts over
	// the claim again, which is why it is the one end told to nobody (TellsOwner).
	EndNotMade ProcessEnd = "not made"
	// EndRelaunched is the process let go and stopped and the claim's session brought back on a
	// new one, which the claim records (launchNow).
	EndRelaunched ProcessEnd = "relaunched"
	// EndRelaunchParked is the turn's task taken back (interrupted) and the launch that replaces
	// the process parked by the wake pacing until its turn (reserveWake, parkWake). The claim
	// still records the process and still runs it until that turn, which is where the launch
	// stops it and brings the session back (wakeNow, launchNow) — unless a suspension held
	// meanwhile ends the claim there instead (endHeld), the lifecycle recheck that turn makes
	// first refuses the launch and leaves the process as it is (runWake, relaunchRefused), or the
	// tree's close has dropped the wake with the process (letGo). It is an end of its own because
	// something has happened to this worker — the task it was serving is owed to it
	// again — while nothing has been relaunched yet, and the two sentences are not the same news.
	EndRelaunchParked ProcessEnd = "relaunch parked"
	// EndRelaunchPending is the process let go and stopped with nothing in its place: the
	// launch's own write was refused and the same launch waits on the probe interval
	// (relaunchLater), or the daemon's stop cut it short and the next boot makes it. The claim
	// records no process either way.
	EndRelaunchPending ProcessEnd = "relaunch pending"
	// EndSuspended is a suspension held for the turn ending the process rather than replacing it
	// (endHeld): nothing runs for the claim until it is resumed.
	EndSuspended ProcessEnd = "suspended"
	// EndFailed is every try at the replacement failing until the claim's launch budget is gone
	// (launchNow, fail): nothing runs for the claim and nothing relaunches it.
	EndFailed ProcessEnd = "failed"
)

// ProcessEnds is every value of ProcessEnd, in the order they are declared. A value added to the
// type is added here: TestProcessEndsListsEveryEndDeclared reads this package's own source and
// fails until it is, and the walks that keep the owner's notice total over the type read this
// list (TestEveryProcessEndTellsItsOwnerExactlyOnce, the daemon's
// TestTheNoticeHasWordsForEveryEndThatTellsItsOwner). A fresh slice is returned so that no
// caller can edit the one the next reads.
func ProcessEnds() []ProcessEnd {
	return []ProcessEnd{EndNotMade, EndRelaunched, EndRelaunchParked, EndRelaunchPending, EndSuspended, EndFailed}
}

// TellsOwner is whether this end is news for the claim's owner, which is the one thing the
// machine decides about the telling: the call that tells is a single one (backstopStalledTurn),
// so an end that tells is told once per decision that reaches it. A stall can be decided twice:
// a wake lives only in memory, so a daemon restart while a relaunch is parked forgets it, and
// the next sweep decides the claim again and tells again.
//
// Every end but EndNotMade is something that happened to the worker — a relaunch committed, its
// process stopped, its session brought back, its claim failed — and EndNotMade is the claim
// still holding its process with nothing queued to replace it, which telling of would send an
// architect looking for a recovery nobody made.
//
// It is written as "everything but" rather than as a list of the ends that tell, because the
// failure this rule exists against is silence: a value added to the type tells its owner before
// anyone remembers to enumerate it, and the worst it can then say is that the daemon has no
// sentence for what it did (the daemon's stallBackstopEnd), which is a notice somebody reads
// rather than a worker that stopped with nobody told.
func (e ProcessEnd) TellsOwner() bool { return e != EndNotMade }

// replaceProcess is the recovery a process ended or found stale through no fault of its own
// (repoint's stale address, refreshPlugin's stale plugin contract, displaced's replaced pod)
// shares once logged: a task whose turn the process was running goes back to waiting first
// (interrupted), the same recovery a death in a turn gets; a held suspension ends here as it does
// for a death (endHeld), suspended rather than relaunched; otherwise the claim is relaunched at
// once through the launch path a death uses (launch), a Resume of its recorded session or a Spawn
// over its existing Sandbox when it has not registered yet. interruption is the sentence the
// turn's recovery is logged with, and outOfMemory says whether the end that cut that turn off was
// the pod running out of memory: only a displacement can carry one, since the other callers
// replace a process that is still running.
//
// It answers the end it reached beside its error, for the caller that has somebody to tell
// (ProcessEnd): every one of these recoveries can end the process and still fail — the launch's
// write is refused, its tries run the launch budget out, the held suspension's write is refused
// — and the end is what happened to the worker, which is decided here, once, rather than at each
// call site.
func (m *Machine) replaceProcess(ctx context.Context, interruption string, outOfMemory bool) (ProcessEnd, error) {
	found := ""
	if loc := m.claim.Locator; loc != nil {
		found = loc.Incarnation
	}
	parked := m.wake
	// Whether the runtime left a process running is this replacement's own fact, so it starts
	// unset here: a refusal an earlier replacement of this claim met is not what the end reached
	// now says of the process it let go (stopUnconfirmed, backstopStalledTurn).
	m.stopUnconfirmed = false
	err := m.replaceNow(ctx, interruption, outOfMemory)
	return m.endReached(found, parked), err
}

// replaceNow is the replacement itself, whose end replaceProcess reads off the claim it leaves.
func (m *Machine) replaceNow(ctx context.Context, interruption string, outOfMemory bool) error {
	if err := m.interrupted(ctx, interruption, outOfMemory); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx)
	}
	return m.launch(ctx)
}

// endReached is the end a replacement made, read from the claim it left beside found, the
// incarnation that claim recorded when the replacement began, and parked, the wake that claim
// held before it. The claim says it whole: a claim whose launch the pacing has just parked waits
// for that wake's turn, one still recording that incarnation was never let go of, one recording
// another is relaunched, and a claim recording none was let go and is where the let-go left it —
// suspended, failed, or launching with the launch still to make, since every caller of letGo
// writes the claim's state in the same step.
//
// The park is read first and by identity, because it is the one end the claim alone cannot show:
// a parked launch leaves the claim exactly as the replacement found it, process and state and
// all (parkWake), and the wake the machine holds after is this replacement's only where it is
// not the one it began with. The caller holds mu.
func (m *Machine) endReached(found string, parked *wake) ProcessEnd {
	if m.wake != nil && m.wake != parked {
		return EndRelaunchParked
	}
	if loc := m.claim.Locator; loc != nil {
		if loc.Incarnation == found {
			return EndNotMade
		}
		return EndRelaunched
	}
	switch m.claim.State {
	case StateSuspended:
		return EndSuspended
	case StateFailed:
		return EndFailed
	}
	return EndRelaunchPending
}
