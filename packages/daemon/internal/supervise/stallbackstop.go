package supervise

import (
	"context"
	"time"
)

// LastStep is the moment the claim's agent was last known to be moving: the last step the daemon
// received, or the turn's own start where the agent has sent no step since that turn began. It is
// the zero time for a claim whose record holds neither, which is a claim nothing may be judged
// from — the daemon reads this record and guesses nothing (INFRA-1274's backstop).
//
// The two are compared rather than the call's own times read, because the record is never
// cleared: after a recovery the call it names is the one that was open when the agent stopped, so
// a turn begun since is younger than the step before it, and the later of the two is what says
// when this claim last moved.
func (s StepRecord) LastStep() time.Time {
	if s.At.Before(s.TurnStartedAt) {
		return s.TurnStartedAt
	}
	return s.At
}

// StallBackstopStop is what the daemon's stall backstop did to one claim, told to the daemon
// once the machine has done it (Deps.StallBackstopped, the daemon's stallBackstopped): the
// record the request carried, the moment the machine judged from, the bound it passed, the
// end the machine reached — relaunched, the relaunch parked on the wake pacing, the relaunch
// still to make, suspended, or the claim failed (ProcessEnd) — the moment a parked launch's
// turn comes, whether the process that end let go was left possibly running, and what the
// replacement charged the claim on its way to that end. The owner's notice is written from
// this and not from what the sweep asked for, because the sweep asks over claims it read
// before the decision and the machine can answer by leaving the turn exactly as it stands.
type StallBackstopStop struct {
	Step     StepRecord
	LastStep time.Time
	Bound    time.Duration
	End      ProcessEnd
	// WakeDue is when the launch the pacing parked is due to run (wake.at, reserveWake), carried
	// so the owner's notice names that moment rather than guessing at the wait (the daemon's
	// stallBackstopEnds). It is the zero time for every other end, which queues no wake.
	WakeDue time.Time
	// StopUnconfirmed is whether the runtime left the process this end let go possibly running:
	// it refused the stop and the machine went on anyway, or the daemon's own stop cut the launch
	// short before the stop was made (Machine.stopUnconfirmed). It is carried because the claim
	// shows nothing of it — a claim whose process was stopped and one whose stop was refused both
	// record no process — and a pod nothing stopped goes on holding what it runs on, which its
	// owner acts on. It is false for an end that let no process go.
	StopUnconfirmed bool
	// LaunchFailures is how many launch failures the replacement this end came out of charged
	// the claim: one for every start of it the runtime refused (launchNow,
	// Machine.failuresCharged). It is carried because the stall itself charges nothing while
	// the replacement made for it can — one refused start and then a good one ends relaunched
	// with a failure charged, and one refused start and then a refused write leaves the
	// relaunch pending with the same — and the claim's own budget counts every failure of the
	// claim's life rather than this replacement's (the daemon's stallChargeClause). It is 0 for
	// an end no start was refused in.
	LaunchFailures int
}

// backstopStalledTurn is the daemon's stall backstop reaching a claim whose recorded last step is
// older than its role's bound (StallBackstop, the daemon's backstopStalledTurns).
//
// It re-reads the claim's own record first and acts only while that record is still older than
// the deadline the request carries, because the sweep read the claims before this decision was
// taken: a frame that arrived in between is the agent moving, and its turn is left alone. It
// reads two things and takes the later: the record the claim carries, which is the store's as
// this machine was built, and the steps this machine has read since (steppedAt, putStep), since
// a frame the stream delivered after the sweep's read is already in this machine's inbox, ahead
// of this event. That re-reading is the fence refreshPlugin makes for the same reason, and the
// one thing that decides it is the record, never a guess of the daemon's.
//
// A claim that already holds a wake is decided nothing at all, exactly as refreshPlugin decides
// nothing over one: that wake is a launch or a task of this same claim waiting its turn in the
// pacing, and a replacement raised over it would take a second token and replace the wake with
// a launch that does what the parked one was already going to do. A relaunch parked on the
// pacing leaves the claim working, so without this guard every sweep of a wave would raise
// another. Nothing is lost by returning, and nothing is owed to this request: the wake standing
// over the claim is what decides the turn — its own turn replaces the stalled process, unless a
// suspension already waiting or the tree's close ends the claim there instead (wakeNow, letGo) —
// and the claim a plugin task's round leaves working is swept again at the next tick,
// since this pass posts over every claim still in a turn past its bound.
//
// What it does when it does act is exactly what a daemon-made replacement does (replaceProcess):
// the turn in flight is interrupted, so the task it was serving goes back to waiting and the
// relaunched agent is sent it again; a held suspension ends in a suspension (endHeld); and
// otherwise the recorded session is resumed on a new process. The stop itself charges nothing —
// not a launch failure, and not a death with work outstanding (chargeDeath) — which is the whole
// reason this path is replaceProcess and not the death path: the worker's own ladder already
// ends its process twice before the daemon is reached (the plugin's stall watch), so a stall
// that repeats would spend the claim's death budget and fail the claim at launch_failure_limit,
// where the requirement is that the claim come back with its task still owed. What the
// replacement itself charges is another thing: a start the runtime refuses inside it is a launch
// failure like any other (launchNow), so the end can carry charges the stop did not make, and
// the report carries how many of them this replacement charged for the owner's notice to say
// (StallBackstopStop.LaunchFailures).
//
// Every end the machine can reach is told but one (ProcessEnd.TellsOwner, Deps.StallBackstopped),
// and it is told which end it was: the owner's notice says what happened to the worker, and the
// answers above — the turn left alone, the turn left to a wake already standing over the claim,
// the turn's task taken back with the relaunch parked on the pacing, and the turn ended — are
// not the same news. What is told is the end the machine reached and never whether the writes
// that end needed all landed (replaceProcess, ProcessEnd): a launch whose write the store
// refused, a launch that ran the claim's budget out, and a held suspension whose write failed
// have each ended the worker's turn, and the one thing the owner must not be left with is a
// worker that stopped and nobody told. The one end told to nobody is the replacement that
// changed nothing — the take-back refused or the lifecycle recheck refused (EndNotMade) — which
// leaves the record as it found it, so the next sweep posts over it again.
//
// It tells once per end, because this is the only call that tells and the end is decided once,
// where the machine makes it (endReached). A relaunch the pacing parked is told here and not
// again at the wake's turn, and that turn tells nobody either, whatever it does: so the notice
// of a parked relaunch names what the park itself settles — the launch queued, and no stop made
// of the stalled process — and names what can change that at the launch's turn as an open list,
// rather than promising the launch (wakeNow, runWake, the daemon's stallBackstopEnds).
//
// The record in that report is the request's, which is the store's own as the sweep read it: the
// claim this machine holds carries the record as it stood when the machine was built, since a
// step is written through a narrow call of its own and never read back into a machine's claim
// (putStep). The fence above is unaffected — every step written since this machine was built
// went through it, so steppedAt covers exactly what the stored record holds beyond the claim's
// own copy.
//
// It never waits for the turn to end. A suspension that arrives mid-turn is held for that turn
// and bounded by a deadline (holdSuspension, whose deadline only moves earlier); this request
// carries its deadline already passed — the recorded last step plus the role's bound — because
// the turn it exists for is one that never ends, and a request held for it would wait for ever.
func (m *Machine) backstopStalledTurn(ctx context.Context, ev StallBackstop) error {
	last := m.claim.Step.LastStep()
	if m.steppedAt.After(last) {
		last = m.steppedAt
	}
	if last.IsZero() || !last.Add(ev.Bound).Before(m.deps.Clock.Now()) {
		m.log.Info("supervise: the stall backstop found the claim's record inside its bound; the turn is left alone",
			"bound", ev.Bound, "deadline", ev.Deadline, "lastStep", last, "tool", ev.Step.Tool, "callId", ev.Step.CallID)
		return nil
	}
	if m.wake != nil {
		m.log.Info("supervise: the stall backstop found this claim already queued for a wake; its turn is left to that wake",
			"kind", m.wake.kind, "due", m.wake.at, "bound", ev.Bound, "deadline", ev.Deadline, "lastStep", last)
		return nil
	}
	suspended := m.held != nil
	// The claim's process is read through a guard, and the two reads this recovery's own other
	// entries make are too (repoint, displaced, noProcessRecorded). No live path reaches any of
	// the three with no process recorded: the sweep posts this event only over a claim in
	// StateWorking that is in no update round (the daemon's backstopStalledTurns), the table
	// runs this row from StateWorking alone, a claim reaches working only from Ready or Idle
	// after it has registered (the table's turnStarted rows), every caller of letGo writes a
	// state that is not working in the same decision, and judge drops an observation while the
	// Locator is nil, which is what would reach the other two.
	//
	// The guard is here all the same, and not because one of those is in doubt: this is a
	// backstop, which runs when a machine is already in a state nobody designed, and a nil
	// dereference inside the supervisor takes the daemon's process down with every tree of the
	// deployment it supervises. A line that says the claim records no process is the whole cost
	// of being wrong about the list above.
	incarnation := noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation = loc.Incarnation
	}
	m.log.Warn("supervise: the daemon's stall backstop ends a turn whose agent stopped stepping; the task it was serving stays owed",
		"incarnation", incarnation, "bound", ev.Bound, "deadline", ev.Deadline,
		"lastStep", last, "turnStartedAt", ev.Step.TurnStartedAt, "tool", ev.Step.Tool, "callId", ev.Step.CallID,
		"callStartedAt", ev.Step.StartedAt, "heldSuspension", suspended)
	end, err := m.replaceProcess(ctx, "the daemon's stall backstop ended a turn whose agent stopped stepping", ordinaryEnd)
	if end.TellsOwner() && m.deps.StallBackstopped != nil {
		stop := StallBackstopStop{Step: ev.Step, LastStep: last, Bound: ev.Bound, End: end,
			StopUnconfirmed: m.stopUnconfirmed, LaunchFailures: m.failuresCharged}
		if end == EndRelaunchParked {
			// The wake the machine holds now is this replacement's own: the guard above returned
			// over a claim that already held one, and the end itself is read from a wake that is
			// not the one the replacement began with (endReached).
			stop.WakeDue = m.wake.at
		}
		m.deps.StallBackstopped(m.current(), stop)
	}
	m.log.Info("supervise: the stall backstop's end of the stalled turn", "end", end)
	return err
}

// noProcessRecorded is what a line of this recovery prints in place of the claim's process
// where the claim records none (backstopStalledTurn's guard, repoint, displaced). It is the
// honest reading of a field that is not there, and the alternative to the panic an unguarded
// read of it makes.
const noProcessRecorded = "no process the claim records"

// backstopStall is the StallBackstop event's row, for the one state a claim is in a turn in.
func backstopStall(m *Machine, ctx context.Context, ev Event) error {
	return m.backstopStalledTurn(ctx, ev.(StallBackstop))
}
