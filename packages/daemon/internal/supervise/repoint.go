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
type ProcessEnd string

const (
	// EndNotMade is the claim still holding the process the replacement found, and its turn with
	// it: the take-back that replacement begins with was refused (interrupted), the launch's own
	// lifecycle recheck was (relaunchRefused), or the pacing parked that launch and the wake's
	// own turn is what replaces the process later (reserveWake, wakeNow). Nothing has happened
	// to this worker yet.
	EndNotMade ProcessEnd = "not made"
	// EndRelaunched is the process let go and stopped and the claim's session brought back on a
	// new one, which the claim records (launchNow).
	EndRelaunched ProcessEnd = "relaunched"
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
	err := m.replaceNow(ctx, interruption, outOfMemory)
	return m.endReached(found), err
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
// incarnation that claim recorded when the replacement began. The claim says it whole: a claim
// still recording that incarnation was never let go of, one recording another is relaunched, and
// a claim recording none was let go and is where the let-go left it — suspended, failed, or
// launching with the launch still to make, since every caller of letGo writes the claim's state
// in the same step. The caller holds mu.
func (m *Machine) endReached(found string) ProcessEnd {
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
