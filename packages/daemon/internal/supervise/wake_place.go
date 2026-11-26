package supervise

// A wake's place in the pacing's queues: the queue a paced wake of a claim waits in (WakeQueue),
// the place it takes there (placeFor), the moves a message mark and a place the boot took earlier
// make it (markMessage, takeEarlierPlace), and the binding of the machine's TimerWake to that place
// (bindWake, placeTimer). Whether a launch waits for its place, runs at once or is held by the
// pause is the wake gate's decision, in wake.go (reserveWake, wakeNow).

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// NewPacer is the wake pacer a daemon builds over limiter on clock, the clock its machines keep
// (internal/daemon's run): the pacer reads that clock's now and arms its turns on it (Clock.At),
// so a paced wake comes due on the clock that times everything else of a machine.
func NewPacer(limiter *wakepace.Limiter, clock Clock) *wakepace.Pacer {
	return wakepace.NewPacer(limiter, clock.Now, func(at time.Time, f func()) { clock.At(at, f) })
}

// letOut reports how the pacing let the wake's place out, and false for a wake whose place has not
// been let out or that holds none (wake.place).
func (w *wake) letOut() (wakepace.Let, bool) {
	if w.place == nil {
		return wakepace.Let{}, false
	}
	return w.place.Let()
}

// due is the moment the pacer estimates it lets the wake's place out, read now
// (wakepace.Place.Estimate), and the zero time for a wake that holds no place (wake.place).
func (w *wake) due() time.Time {
	if w.place == nil {
		return time.Time{}
	}
	return w.place.Estimate()
}

// leave gives up the wake's place in the pacing, if it holds one: the turn the place would have
// taken goes to the wake behind it (wakepace.Place.Leave). A place let out already is left as it
// is.
func (w *wake) leave() {
	if w.place != nil {
		w.place.Leave()
	}
}

// WakeQueue is the queue a paced wake of c waits in, and the one statement of the rule that sorts
// them (dispatch://INFRA-5682 R1, R2): the first queue (wakepace.First) for a claim whose message
// mark stands (Claim.MessageMarked) — a wake for a role with an Envoy message waiting, which Envoy
// drops once it has kept it an hour — and for one that holds a charged death (Claim.chargedDeath),
// the relaunch of a role whose own process died; the second (wakepace.Second) for every other. A
// displaced relaunch charges no death, and neither does the relaunch of an end from outside the
// pod, though its decision writes Claim.DeathCharged (decidedFromOutside), so each waits in the
// second queue with the rest of a restart's backlog, which the pacer still lets out one turn in
// four. The gate reads it for the place it joins (placeFor), a mark that comes while the wake
// waits moves the wake by it (markMessage), the release of a launch a pause held joins it
// (letOutHeldLaunch), and the boot reads it for the places it takes (internal/daemon's
// placeRelaunches), so none of them sorts a claim apart.
func WakeQueue(c *Claim) wakepace.Queue {
	if c.MessageMarked() || c.chargedDeath() {
		return wakepace.First
	}
	return wakepace.Second
}

// placeFor is the place a paced launch of this claim waits in: carried, the place its caller took
// for it (RequestSpawn.Place, RecordedDeath.Place), by the rule RequestSpawn.Place states, and
// otherwise a place joined now at the back of the queue the claim's class names (WakeQueue). A
// carried place that waits in the second queue while the claim's class is the first moves there
// (wakepace.Place.Mark), as a mark moves a waiting wake (markMessage). A carried place the pacer
// let out an interval or more before now is not run at once: the launch joins its queue afresh,
// and the gate says so.
func (m *Machine) placeFor(carried *wakepace.Place, now time.Time) *wakepace.Place {
	queue := WakeQueue(&m.claim)
	if carried == nil {
		return m.deps.WakePace.Pacer.Join(queue)
	}
	let, out := carried.Let()
	if !out {
		if queue == wakepace.First {
			carried.Mark()
		}
		return carried
	}
	if now.Sub(let.At) < m.deps.WakePace.Interval {
		return carried
	}
	place := m.deps.WakePace.Pacer.Join(queue)
	m.log.Warn("supervise: this launch reached the wake gate an interval or more after the pacing let out the place its caller took; it joins its queue afresh",
		"letOut", let.At, "late", now.Sub(let.At), "queue", queue.String(), "due", place.Estimate())
	return place
}

// bindWake hands place the release that fires this machine's TimerWake when the pacer lets the
// place out, through the pipeline every other timer of the machine takes: the Timer event is taken
// first, under the next sequence number (nextTimer), and recorded as TimerWake with the place
// itself as its cancel (placeTimer), so that whatever disarms the timer gives the place up, and a
// release that comes after the timer was replaced or dropped is ignored as a stale timer is
// (Machine.fence). A place the pacer has let out already reports its Let and true, and nothing is
// recorded: the caller runs the wake itself (reserveWake, letOutHeldLaunch), or arms TimerWake at
// the let-out moment so the wake runs at once through its own timer (rebindWake).
func (m *Machine) bindWake(place *wakepace.Place) (wakepace.Let, bool) {
	ev := m.nextTimer(TimerWake, "")
	if let, out := place.Bind(func(wakepace.Let) { m.fire(ev) }); out {
		return let, true
	}
	m.timers[TimerWake] = armed{seq: ev.Seq, cancel: placeTimer{place}}
	return wakepace.Let{}, false
}

// rebindWake binds TimerWake to place for a wake taken up at a place it did not join in this
// decision (reparkWake, takeEarlierPlace). A place that still waits fires the timer at its let-out
// (bindWake). One the pacer has let out meanwhile arms the timer at that let-out's moment, which
// has come, so the wake runs at once through its own timer, as its let-out would have run it.
func (m *Machine) rebindWake(place *wakepace.Place) {
	if let, out := m.bindWake(place); out {
		m.armAt(TimerWake, let.At, "")
	}
}

// placeTimer is TimerWake's cancel while its wake waits in the pacing's queue (bindWake): the
// release the place holds is the timer, so stopping it gives the place up (wakepace.Place.Leave),
// and the turn the place would have taken goes to the wake behind it, since a waiting place has
// drawn nothing of the limiter. A place the pacer has let out already is left as it is, and Stop
// reports false, as a timer's Stop does once it has fired.
type placeTimer struct{ place *wakepace.Place }

func (t placeTimer) Stop() bool {
	t.place.Leave()
	_, out := t.place.Let()
	return !out
}

// detachWake takes TimerWake off the wake it holds without giving up that wake's place
// (dropWakeFor): a place's release is replaced by none (wakepace.Place.Bind), so the place waits
// on in its queue and a let-out finds nothing to fire, and the timer is forgotten rather than
// stopped, since stopping it would leave the place (placeTimer). A release already on its way is
// a Timer this machine no longer records, and is ignored as a stale timer is (Machine.fence).
// Any other TimerWake — the clock's, armed at the let-out of a place taken up again
// (reparkWake) — is disarmed as any timer is.
func (m *Machine) detachWake() {
	if a, ok := m.timers[TimerWake]; ok {
		if waiting, binds := a.cancel.(placeTimer); binds {
			waiting.place.Bind(nil)
			delete(m.timers, TimerWake)
			return
		}
	}
	m.disarm(TimerWake)
}

// takeEarlierPlace is whether the wake the pacing holds over this claim moves to carried, a place
// the boot took earlier in the pacing's order for the same launch (RecordedDeath.Place), and it
// moves the wake there when it does. The boot's replay of a recorded death and the runtime's first
// report of that death raise one relaunch, and a report that reaches the machine after the boot's
// pass placed that relaunch, and before the replay, parks it at a place of its own behind every
// place the pass took in its queue; moved, the relaunch waits at the place the boot gave it, and
// its own is given up (placeTimer), the turn it would have taken going to the wake behind it.
//
// The wake moves to a carried place the pacer would let out before its own (wakepace.Place.Before)
// and to one the pacer let out less than an interval ago, which runs the wake at once, through its
// timer. A carried place let out an interval or more ago is not taken, for the reason the gate
// joins such a place afresh (placeFor): run now, a place that late would let out at once every
// launch the pass placed with it. A carried place the wake does not take is given up as the
// decision ends (Machine.handle). Only a wake of the pacing's own moves: a launch the pause's gate
// holds or one waiting for its pod's start has no place of the pacing's to move.
//
// Nothing is told the daemon here: the wake still waits in a queue, which it was told at the end
// of the decision that parked it (tellWakeQueue), and its let-out is told when it comes (wakeNow).
func (m *Machine) takeEarlierPlace(carried *wakepace.Place) bool {
	w := m.pacedWake()
	if carried == nil || w == nil || w.place == nil || !m.placeBefore(carried, w.place) {
		return false
	}
	m.log.Info("supervise: the relaunch a report of the recorded death parked takes the place the boot took for it",
		"due", carried.Estimate(), "queue", carried.Queue().String())
	w.place = carried
	m.rebindWake(carried)
	return true
}

// placeBefore is whether the pacer lets carried out before own, a place that still waits: carried
// waits ahead of own under the share rule (wakepace.Place.Before), or the pacer let carried out
// less than an interval ago (takeEarlierPlace).
func (m *Machine) placeBefore(carried, own *wakepace.Place) bool {
	let, out := carried.Let()
	if !out {
		return carried.Before(own)
	}
	_, ownOut := own.Let()
	return !ownOut && m.deps.Clock.Now().Sub(let.At) < m.deps.WakePace.Interval
}

// carriedPlace is the place in the wake pacing ev carries for the launch it raises, the one the
// boot took (RequestSpawn.Place, RecordedDeath.Place), and nil for none.
func carriedPlace(ev Event) *wakepace.Place {
	switch ev := ev.(type) {
	case RequestSpawn:
		return ev.Place
	case RecordedDeath:
		return ev.Place
	}
	return nil
}

// leaveUntaken gives carried up at the end of the decision of the event that carried it, unless
// the wake the machine holds now waits at it (Machine.handle): a refusal, an ignore, a launch the
// lifecycle refuses or the pause's gate holds, a launch that joined afresh, and a replay that
// moved no wake each leave it untaken, and the turn it would have taken goes to the wake behind
// it. A place the pacer has let out already is left as it is.
func (m *Machine) leaveUntaken(carried *wakepace.Place) {
	if m.wake == nil || m.wake.place != carried {
		carried.Leave()
	}
}

// messageMarked is the MessageMarked row: the claim is marked for the message the listener's
// report carried (markMessage), and nothing else of it changes.
func messageMarked(m *Machine, _ context.Context, ev Event) error {
	m.markMessage(ev.(MessageMarked).At)
	return nil
}

// markMessage marks the claim for an Envoy message waiting since at (Claim.MessageMarkAt): the
// mark becomes at where none stands, and of two standing marks the earlier is kept, since it is
// the message that has waited longest, and the first Envoy drops. A wake of the claim that waits
// in the pacing's second queue moves to the first (wakepace.Place.Mark), its turns still counted
// from its join, so the class every paced wake of the claim waits in stays the one WakeQueue
// names. It writes nothing to the store, as MessageMarked states.
func (m *Machine) markMessage(at time.Time) {
	c := &m.claim
	if !c.MessageMarked() || at.Before(c.MessageMarkAt) && at.After(c.LastTurnStart) {
		c.MessageMarkAt = at
	}
	if w := m.wake; c.MessageMarked() && w != nil && w.place != nil && w.place.Mark() {
		m.log.Info("supervise: a waiting Envoy message marks this claim; its paced wake moves to the message queue",
			"kind", w.kind, "markedAt", c.MessageMarkAt, "due", w.place.Estimate())
	}
}
