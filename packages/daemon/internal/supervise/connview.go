package supervise

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The bounds and the connection (dispatch://INFRA-2811, decision 8). Three
// bounds judge the worker by the frames its connection carries: R5's second wait (TimerTurnStart),
// the bound of the wait for a take's report (TimerReport), with nothing held as with work held,
// and R5's first wait over a value the worker reported seeing (TimerTake). Each runs only while
// the connection shows the machine every frame of the worker's turns (viewWhole): a close disarms
// all three (closeView), none fires while no connection stands, and each is armed again whole,
// never from what was left of it, once the view is whole again. A remainder carried across a cut
// could pass before the report of a turn that ran unseen lands, and take back a task whose turn
// ran.
//
// The view is whole again at the agent's answer, on the new connection, to whether it is in a turn
// (get_state), which reaches the machine behind every frame the shim replayed (StreamTurnAnswered,
// answerView), or at a turn start or end frame on that connection, whichever comes first
// (seeView). The daemon asks after every reconnect hello of a claim that holds a pending task,
// whatever the hello says of the shim's backlog (askView). A hello saying the backlog is whole
// does not stand in for the answer: the shim keeps a frame only where its write failed, so a frame
// that sat in the socket's send buffer when the connection broke is lost and never replayed, and
// if it was the start of the take's turn, a bound armed at the hello would pass during that turn.
// The backlog's word decides only what it decided before: the restored anchor (bindRestoredAnchor)
// and the count of the process's turns (readBacklog).
//
// "In a turn" is read as a turn start, over a take awaiting its turn as the take's own turn
// (turnStarted): no bound runs over a turn, and its end arms them whole. "Not in a turn" over a
// claim read in a turn is read as that turn's end (turnEnded), so a lost end frame leaves no claim
// working for ever, and the bounds are armed whole from the answer. Every answer the daemon reads
// makes the view whole in one place (answerView), which ends the reconnect deadline below, and is
// applied to the claim there, by one rule: this ask's, and a daemon start's own.
//
// The ask is made once on each reconnect hello over a pending task, and never again at a sweep
// while that connection stands (helloAsked, observe). Its one bound is the reconnect deadline
// (TimerRegistration, reconnectBound), armed whole at the hello and disarmed by the answer or a
// turn frame: at it the process is relaunched, charged nothing, and the task taken back as a cut
// turn (reconnectDeadline). The hello marks its view asked and arms that deadline before any step
// of its decision that can fail (helloView): reading the hello (readHello), whose put-back writes
// the delivery, and the enrollment (ensureEnrolled), which writes the claim. A decision the store
// fails there leaves the view bounded, and its deadline is that of a hello that asked, an
// uncharged relaunch, never the path of a shim that never said hello. Nor does the step before
// the row fail it on the store: a provider error's take-back and ladder step that a daemon start
// owes, made there at the claim's first decision (settle, Machine.ladderOwed), stand in memory
// when the store refuses their write, which is owed, and the hello's row runs. A later hello is a
// new connection, which asks once on it and arms the deadline again. A daemon start's own ask — of
// a claim restored working (askFirst, askTurnOver) or holding a take on a stale plugin (askTurn,
// askRestoredTurn) — serves only the first hello since that start, the one that makes it
// (startAskServes), since the stream reads an answer only on the connection that asked: that hello
// asks no second time and arms the same deadline whole, once, and every later hello over a pending
// task asks its own, so no connection is asked twice. The stale one's ask is dated (askedAt) at
// that same point, before any step that can fail, though it is made after them: a decision that
// fails between leaves the deadline that ask's bound, which a turn frame does not end (seeView),
// and at it the stale-plugin lane decides the relaunch, uncharged. An answer to any of these asks
// answers the daemon start's own too, which ends with it (askFirst, askTurn). A hello over no
// pending task asks nothing of its own: a daemon start's own ask is made at it and again at each
// sweep until it is answered, as before, the stall backstop bounding a working claim and the
// deadline askRestoredTurn arms a stale one.
//
// A close of a live process whose agent has said it is ready and whose claim holds a pending task
// sets the reconnect deadline to the later of the one standing and the close plus the reconnect
// bound, never earlier (reconnectFromClose): a hello ends it, and at it the process is probed, and
// a live one retired and relaunched with CauseNeverReconnected, the task taken back as a cut turn.
// So a shim that comes back within a whole bound of its close is not retired, whatever an earlier
// deadline — of a hello's ask still unanswered, or of a daemon start — said. A close of a ready
// claim holding no pending task arms nothing and disarms the deadline standing, so a claim with no
// task is not retired for a shim that reconnects late, and is asked nothing: an idle worker whose
// shim reconnects late is treated as before. Before the ready the timer is the registration
// deadline, which a close leaves as it stands: pushed at every close, it would leave unretired a
// process that never says ready and keeps dropping its connection. A task that becomes pending
// over a view that is not whole, with no deadline standing, arms it whole from then
// (armReconnectForTask, queue), so every take and every wait behind the stale-plugin relaunch's
// guard (refreshPlugin) is bounded. And the mirror: a deadline armed over a view closed since the
// last hello goes when the pending task goes, where the agent said it is ready
// (dropReconnectWithTask, retireTask), since at it the no-hello path would retire and charge a
// worker that took and reported over HTTP while its shim stayed down and then held no task. Before
// the ready that timer is the registration deadline and stays; over a hello's ask still unanswered
// (viewAsked) it stays too, and at it the idle worker is relaunched uncharged, unless the
// connection closes first, whose close over no task disarms it.
//
// After a daemon start every claim reconnects with a hello, so a machine NewMachine restores with a
// process — launching, shim-connected, registered, ready, idle or working — starts with no
// connection (viewNoHello): a first act that reaches the daemon before the hello over HTTP, a take,
// a report or the agent's ready, arms none of the three bounds.
//
// The agent's ready over HTTP over a view that is not whole keeps the reconnect deadline rather
// than end it, as the ready ends the registration deadline over a whole one (readyView): nothing
// else bounds a shim that never comes back once its agent is ready, so a take it holds would wait
// for ever.
//
// Every change of the view goes through one helper (setView), which logs each one that crosses
// whole: one line where the view stops being whole (viewCutLine) and one where it becomes whole
// (viewWholeLine), each naming the claim and the task pending where one is. The cost reading reads
// those two lines alone of the connection, so it stops and starts each claim's clocks where the
// machine does (ReadTaskCost).
//
// One case is stated, not closed: a connection that closes again before each bound passes
// postpones the bounds, each time to a whole bound after its next hello. The reconnect deadline
// bounds each gap, and nothing bounds how many gaps come.
type connView uint8

const (
	// viewWhole is a connection standing whose frames the machine reads whole: the first hello of
	// the process, or a reconnect hello the agent's answer or a turn frame followed, or a reconnect
	// hello of a claim that held no pending task. The three bounds run.
	viewWhole connView = iota
	// viewNoHello is a claim a daemon start restored with a process — launching, shim-connected,
	// registered, ready, idle or working — its shim's first hello to this daemon still to come
	// (NewMachine).
	viewNoHello
	// viewClosed is a claim whose connection closed, its shim's next hello still to come (closeView).
	viewClosed
	// viewAsked is a reconnect hello over a claim holding a pending task, neither the agent's answer
	// to whether it is in a turn nor a turn frame on that connection come yet (helloView).
	viewAsked
)

// viewAskedLine is the line of the ask a reconnect hello makes (askView), at Info, with the task
// pending: the three bounds wait for the agent's answer or a turn frame.
const viewAskedLine = "supervise: the shim reconnected over a pending task; the daemon asks its agent whether it is in a turn, and the bounds on the worker wait for that answer or a turn frame"

// viewUnansweredLine is the line of a reconnect hello's ask the agent did not answer (askView), at
// Warn: the bounds wait for a turn frame, and the reconnect deadline of the hello bounds the wait.
const viewUnansweredLine = "supervise: the agent did not say whether it is in a turn after its shim reconnected; the bounds on the worker wait for a turn frame or the reconnect deadline"

// viewAnsweredLine is the line of the answer that makes the view whole again (answerView), at Info,
// with whether the agent is in a turn: the answer to a reconnect hello's ask, or to a daemon start's
// own ask of the first hello since that start.
const viewAnsweredLine = "supervise: the agent said whether it is in a turn after its shim reconnected; the bounds on the worker run again from here"

// viewUnansweredRelaunchLine is the line of the reconnect deadline of a hello no answer or turn
// frame followed (reconnectDeadline), at Warn: the process is relaunched, charged nothing, and its
// task taken back as a cut turn.
const viewUnansweredRelaunchLine = "supervise: the agent did not say whether it is in a turn within the reconnect deadline of its shim's hello; it is relaunched, uncharged, and its task taken back as a cut turn"

// viewCutLine is the line of the claim's view ceasing to be whole (setView), at Info, with the
// task pending where one is: a daemon start that restored the claim with a process (NewMachine), a
// close (closeView), or a reconnect hello over a pending task that found the view whole
// (helloView). The three bounds stand still from here. The cost reading stops the claim's clocks at
// it (ReadTaskCost).
const viewCutLine = "supervise: the daemon no longer sees every frame of the worker's turns; the bounds on the worker stand still until it does again"

// viewWholeLine is the line of the claim's view becoming whole again (setView), at Info, with the
// task pending where one is: the new process's first hello (helloed), a hello of a shim that
// reconnected before its agent said it is ready (enrolledOnly), a reconnect hello over no pending
// task (helloView), the agent's answer to a hello's ask or to a daemon start's own ask (answerView:
// turnAnswered, askTurnOver), a turn frame (seeView), and the let-go of the process, whose next one
// opens a connection of its own (letGo). The bounds the view held still are armed whole from here,
// as each one's other conditions allow. The cost reading starts the claim's clocks again at it,
// whole (ReadTaskCost).
const viewWholeLine = "supervise: the daemon sees every frame of the worker's turns again; the bounds on the worker run again, whole, from here"

// setView is the claim's view of the worker's turns becoming v (connSeen). Every change of it goes
// through here, and one that crosses whole is logged: viewCutLine where the view stops being
// whole, viewWholeLine where it becomes whole. A change between two views that are not whole — a
// hello over a closed view or a daemon start's, which asks — logs nothing, since the bounds stood
// still and still do; nor does a view set to what it was.
func (m *Machine) setView(v connView) {
	was := m.connSeen
	m.connSeen = v
	if (was == viewWhole) == (v == viewWhole) {
		return
	}
	line := viewCutLine
	if v == viewWhole {
		line = viewWholeLine
	}
	var task []any
	if p := m.claim.Pending; p != nil {
		task = []any{"task", p.Number}
	}
	m.log.Info(line, task...)
}

// reconnectBound is the reconnect deadline's bound: the registration deadline's base,
// Timeouts.Boot × RegistrationIntervals.
func (m *Machine) reconnectBound() time.Duration {
	return runtime.RegistrationDeadline(m.deps.Timeouts.Boot, m.deps.Timeouts.RegistrationIntervals, 0)
}

// closeView is the claim's connection closing (closed): the view is cut, so the three bounds go at
// the end of this decision (armWaits), and the ask of that connection's hello goes with it
// (helloAsked). A live process whose agent said it is ready is given the reconnect deadline a
// close sets (reconnectFromClose): over a pending task, the later of the one standing — of a
// hello's ask still unanswered, or of a daemon start — and the close plus the reconnect bound,
// never earlier; over none, no deadline. A claim not ready keeps the registration deadline it runs
// under as it stands, and its ready, with no hello since, is given the deadline this close sets
// (readyView).
func (m *Machine) closeView() {
	m.setView(viewClosed)
	m.closedAt = m.deps.Clock.Now()
	m.helloAsked = false
	m.cancelProtectedAsk()
	if SaidReady(m.claim.State) && m.claim.Locator != nil {
		m.reconnectFromClose()
	}
}

// reconnectFromClose sets the reconnect deadline of a live process whose agent has said it is
// ready over a view closed since its last hello (closedAt): at the close (closeView), and at a
// ready that comes over that view (readyView). Over a pending task it is the later of the deadline
// standing and the close plus the reconnect bound, never earlier, so a shim that comes back within
// a whole bound of its close is not retired and charged as never reconnected at a deadline armed
// before the close, by a hello whose ask went unanswered or by a daemon start. Over no pending task
// the deadline goes: its no-hello path would retire and charge a worker that holds no task, for a
// shim that reconnects late.
func (m *Machine) reconnectFromClose() {
	if m.claim.Pending == nil {
		m.disarm(TimerRegistration)
		return
	}
	due := m.laterOfHandoff(m.closedAt.Add(m.reconnectBound()))
	if a, armed := m.timers[TimerRegistration]; armed && !a.due.Before(due) {
		return
	}
	m.armAt(TimerRegistration, due, "")
}

// armReconnectForTask arms the reconnect deadline whole from now over a pending task the claim's
// view does not show whole, where none stands: where a task becomes pending over such a view
// (queue), the one case a close leaves unarmed, since a close over no pending task arms nothing
// (closeView). It arms nothing for a claim whose agent has not said it is ready, which runs under
// its registration deadline, nor for one with no process.
func (m *Machine) armReconnectForTask() {
	if m.connSeen == viewWhole || !SaidReady(m.claim.State) || m.claim.Locator == nil {
		return
	}
	m.armReconnectUnlessArmed()
}

// armReconnectUnlessArmed arms the reconnect deadline whole from now (reconnectBound) only where
// none stands, so a deadline armed already keeps its time: where a task becomes pending over a view
// not whole (armReconnectForTask), and at a ready over a view no hello has reached since a daemon
// start (readyView). A close sets the deadline by its own rule (reconnectFromClose).
func (m *Machine) armReconnectUnlessArmed() {
	if _, armed := m.timers[TimerRegistration]; !armed {
		m.arm(TimerRegistration, m.reconnectBound(), "")
	}
}

// dropReconnectWithTask is the claim's pending task gone — finished, dropped or retired
// (retireTask) — over a view closed since its last hello (viewClosed): the reconnect deadline
// armed for that task goes with it, since its no-hello path reads no pending task and would retire
// and charge, as never reconnected, a worker that took and reported over HTTP while its shim
// stayed down. Only where the agent has said it is ready: before that, the timer is the
// registration deadline the claim runs under, which retires a process that never says ready. The
// deadline a daemon start gives a claim with no hello seen (viewNoHello) stays, and so does one
// over a hello's ask still unanswered (viewAsked): the view is not made whole there, since a task
// that arrives later would then arm the bounds over frames the daemon may have lost, and at it the
// idle worker is relaunched uncharged, unless its connection closes first, whose close over no
// pending task disarms it (closeView).
func (m *Machine) dropReconnectWithTask() {
	if m.connSeen == viewClosed && SaidReady(m.claim.State) {
		m.disarm(TimerRegistration)
	}
}

// readyView is the agent's ready (ready) read against the claim's connection. Over a whole view
// the ready ends the registration deadline, as it always has. Over a view that is not whole the
// deadline stays as the reconnect deadline (reconnectDeadline), since nothing else bounds a shim
// that never comes back once its agent is ready:
//
//   - a close before the ready (viewClosed) of a live process is given the deadline that close
//     gives a ready claim (reconnectFromClose): over a pending task, the later of the deadline
//     standing and the close plus the reconnect bound, so a hello inside either finds nothing
//     retired; over none, no deadline, as a claim with no process is given none;
//   - any other view not whole — a daemon start's before the shim's first hello (viewNoHello) —
//     keeps the deadline standing, or is given one whole from now where none stands, as a claim the
//     start restored ready is.
func (m *Machine) readyView() {
	switch m.connSeen {
	case viewWhole:
		m.disarm(TimerRegistration)
		return
	case viewClosed:
		if m.claim.Locator == nil {
			m.disarm(TimerRegistration)
			return
		}
		m.reconnectFromClose()
		return
	}
	m.armReconnectUnlessArmed()
}

// startAskServes is whether the hello now read is the one a daemon start's own ask serves: the
// claim was restored with that ask to make — a claim restored working (askFirst) or holding a take
// on a stale plugin (askTurn) — and no hello has come since that start (viewNoHello). The stream
// reads an answer only on the connection that asked, so that ask serves no later hello. It reads
// the view as the hello found it, so the hello reads it before it marks the view (helloView).
func (m *Machine) startAskServes() bool {
	return (m.askFirst || m.askTurn) && m.connSeen == viewNoHello
}

// helloView opens the view a reconnect hello reads (reconnected, reconnectedMidTurn), before any
// step of the hello's decision that can fail (readHello, ensureEnrolled), and answers whether a
// daemon start's own ask serves this hello (startAskServes), read first, from the view as the
// hello found it. Where that ask is the one of a claim restored holding a take on a stale plugin
// (askTurn), it is dated here too (askedAt), though it is made only once those steps are done
// (askRestoredTurn): a decision that fails between leaves it dated, so the deadline below is read
// as that ask's bound, which a turn frame does not end (seeView), and at it the stale-plugin lane
// decides the relaunch, uncharged, rather than the path of a shim that never said hello
// (reconnectDeadline); over no pending task, the next sweep makes the ask (observe). A claim
// holding no pending task asks nothing: its view is whole, and the deadline the hello ends goes.
// One holding a task waits for the agent's answer or a turn frame (viewAsked), its connection is
// one a hello asked (helloAsked), and the reconnect deadline is armed whole from the hello, here
// alone on the hello's path: a daemon start's own ask that serves the hello is made under that
// deadline and arms none of its own (askRestoredTurn, askTurnOver), and the hello's own ask arms
// nothing (askView). A decision that fails after this leaves the view bounded by that deadline, at
// which the process is relaunched uncharged, as after a hello whose ask went unanswered, rather
// than retired as one whose shim never said hello (reconnectDeadline).
func (m *Machine) helloView() (served bool) {
	m.cancelProtectedAsk()
	served = m.startAskServes()
	if served && m.askTurn {
		m.askedAt = m.deps.Clock.Now()
	}
	if m.claim.Pending == nil {
		m.helloAsked = false
		m.setView(viewWhole)
		m.disarm(TimerRegistration)
		return served
	}
	m.setView(viewAsked)
	m.helloAsked = true
	m.armNoEarlierThanHandoff(TimerRegistration, m.reconnectBound(), "")
	return served
}

// askView makes the ask of a reconnect hello over a pending task (reconnected,
// reconnectedMidTurn), over the view the hello opened (helloView), once the steps of its decision
// that can fail are done, and answers whether it asked. served is helloView's answer: the first
// hello since a daemon start whose own ask serves it is asked by its caller instead (askTurnOver,
// askRestoredTurn), and a hello over no pending task asks nothing. An ask the agent does not
// answer is not asked again, here or at a sweep (observe): the view waits for a turn frame, or for
// the reconnect deadline the hello armed.
func (m *Machine) askView(ctx context.Context, served bool) (asked bool) {
	if _, protected := m.handoffUntil(); protected {
		// A handoff may still run over the take, and Oh My Pi answers no ask behind it: the one
		// ask of this hello waits for its answer on a goroutine of its own, through the deadline
		// the hello armed (askProtected), and a daemon start's own ask is served by it too.
		if conn, ok := m.deps.Conns.Conn(m.claim.Token); ok {
			m.askProtected(conn)
		}
		return true
	}
	if served || m.claim.Pending == nil {
		return false
	}
	m.log.Info(viewAskedLine, "task", m.claim.Pending.Number)
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// The connection closed before the decision that reads its hello: its close is queued
		// behind the hello, and cuts the view again.
		return true
	}
	if _, answered := m.streaming(ctx, conn); !answered {
		m.log.Warn(viewUnansweredLine, "task", m.claim.Pending.Number)
	}
	return true
}

// seeView is a turn start or end frame (Handle): the connection it came on carries the worker's
// turns, so the view is whole, and the reconnect deadline of a hello's ask goes, unless it is the
// bound of a daemon start's own ask (askTurn), which only its answer ends.
func (m *Machine) seeView() {
	if m.connSeen == viewWhole {
		return
	}
	asked := m.connSeen == viewAsked
	m.setView(viewWhole)
	if asked && !m.askTurn {
		m.disarm(TimerRegistration)
	}
}

// turnMayHaveRunUnseen is whether the pending take, awaiting its turn (takeAwaitsTurn), may have
// had that turn run with no frame of it reaching the machine: the view is not whole (connSeen) —
// no hello since a daemon start or a close, or since the hello neither the agent's answer nor a
// turn frame — or the take is marked so (Machine.turnUnseen). Every let-go reads it, so its
// take-back takes the take back as a turn cut off and its next text carries that note, as the
// reconnect deadline takes it back: the record of a let-go reads it into the mark and the record
// alike (recordEnd: letGo, died, displaced), for a take-back made after the let-go has made the
// view whole for the next process (takeBackUnstarted for a retirement, a suspension and a held
// suspension's deadline; interrupted for a death and a displacement), and the take-back of a
// relaunch of a live process, made before its let-go records anything, reads it directly
// (interrupted, replaceNow).
func (m *Machine) turnMayHaveRunUnseen() bool {
	return (m.turnUnseen || m.connSeen != viewWhole) && m.takeAwaitsTurn()
}

// markTurnUnseen marks the pending take as one whose turn may have run with no frame of it
// reaching the machine (Machine.turnUnseen), where it awaits its turn and its finish is not owed
// (takeAwaitsTurn): a migrated worker's take awaiting its turn (waitNone) is marked as a current
// one's is, since its turn can run unseen as well, and only a take whose report came is left out,
// which every let-go finishes by that report before it reads the mark (interrupted). It is the one
// test of every site that marks: the reconnect deadline (reconnectDeadline), a stale-plugin
// relaunch decided behind a cut backlog (markRestoredTurnUnseen), and a turn's end the daemon
// never saw begin (blindTurnEnded). It is not the mark's only writer, and each writes it through
// setTurnUnseen: the record of a let-go sets the mark to what that let-go reads
// (turnMayHaveRunUnseen), and so clears it where the take no longer awaits its turn (recordEnd),
// and the next accepted take clears it (accept).
func (m *Machine) markTurnUnseen() {
	if m.takeAwaitsTurn() {
		m.setTurnUnseen(true)
	}
}

// setTurnUnseen is the one writer of the mark of a take whose turn may have run unseen
// (Machine.turnUnseen): markTurnUnseen sets it, by its one test, at every site that marks; the
// record of a let-go sets it to what the let-go reads (recordEnd, turnMayHaveRunUnseen), which
// marks it over a view not whole and clears it where the take no longer awaits its turn; and an
// accepted take clears it (accept).
func (m *Machine) setTurnUnseen(unseen bool) { m.turnUnseen = unseen }

// answerView is the agent's answer to whether it is in a turn (streaming), on every path an answer
// reaches the machine: the ask a reconnect hello made, over the view that hello left waiting for it
// (turnAnswered), and a daemon start's own ask of the first hello since that start, of a restored
// take on a stale plugin (turnAnswered, askTurn) or of a claim restored working (askTurnOver,
// askFirst). It is the one place an answer makes the view whole: the view is whole (setView, and
// viewAnsweredLine where the answer made it so), the reconnect deadline that bounded the ask goes,
// and the daemon start's own asks are answered and end with it (askFirst, askTurn), so no sweep
// asks them again. Left armed, that deadline would find the view whole and take the path of a shim
// that never said hello (retireNeverReady), and retire and charge a live worker in its turn.
//
// The answer is then applied to the claim, read behind every frame its shim sent before it, by the
// one rule of every answer the daemon reads: a turn running over a claim not busy is a turn start,
// the take's own over a take awaiting its turn (turnStarted), and no turn over a claim in one is
// that turn's end (turnEnded), so an end frame the connection lost leaves no claim working for
// ever. An answer the claim already agrees with changes nothing. The bounds are armed whole from
// here at the end of this decision, or at the end of the turn the answer started.
//
// A turn running over a claim whose session is being compacted with no turn started under the
// compaction is a turn's start too, by the rule of every start (turnStartedWhileBusy): the
// compaction took the claim from ready or idle, so the turn the answer reports began under it, and
// the first such turn after an accepted take is the take's (departure 10 of dispatch://INFRA-2811's
// plan). A claim a daemon start restored working over a take a handoff may still run over
// (Machine.restoredHandoff) is not read so: the stored working state cannot tell a take whose text
// Oh My Pi parked from a foreign turn the take was put back behind (notTheTakesTurn), so the answer
// gives no task the turn there, and only an explicit start frame (turnStartedWhileBusy) or the
// take's own report (reportedTask, heldUnrun) does.
func (m *Machine) answerView(ctx context.Context, streaming bool) error {
	cut := m.connSeen != viewWhole
	m.setView(viewWhole)
	m.disarm(TimerRegistration)
	m.askFirst, m.askTurn, m.unansweredAsks = false, false, 0
	if cut {
		m.log.Info(viewAnsweredLine, "streaming", streaming)
	}
	switch {
	case streaming && !m.busy():
		return turnStarted(m, ctx, nil)
	case streaming && m.compacting != nil && !m.compacting.inTurn:
		// The turn began under the compaction (above), and its start frame, replayed behind the
		// answer, counts nothing again (Machine.admittedByAnswer).
		if err := turnStartedWhileBusy(m, ctx, nil); err != nil {
			return err
		}
		m.admittedByAnswer = true
		return nil
	case !streaming && m.inTurn():
		return turnEnded(m, ctx, nil)
	}
	return nil
}

// reconnectDeadline is the reconnect deadline: of a claim a daemon start restored with its agent
// ready, idle or working (NewMachine), or whose connection closed while it held a pending task, no
// earlier than a whole bound from that close (closeView, reconnectFromClose), or whose agent said
// it is ready over a connection whose view was not whole (readyView), or which was given a pending
// task over a connection closed since its last hello (armReconnectForTask), its shim not having
// said hello since; or of a reconnect hello of a claim holding a pending task whose agent has not
// answered whether it is in a turn, nor sent a turn frame, since (helloView), a hello whose
// decision failed after it opened that view included.
//
// With no hello, it probes and retires as the registration deadline does (registrationDeadline,
// retireNeverReady). A process found gone takes the death path (judge, died). A process found
// alive is retired and relaunched with a charged launch failure, CauseNeverReconnected, since a
// live process whose shim never comes back is held by nothing else: no frame of it reaches the
// daemon, so none of R5's waits can be judged of it. The task its turn was running is taken back
// first, as a replacement takes it (interrupted), so the relaunched worker takes it again under
// its number; a take awaiting its turn goes back as a cut turn (markTurnUnseen,
// Machine.turnUnseen), since that turn may have run while no frame of the claim reached the
// machine.
//
// A suspension held for the claim's turn ends here instead: no turn's end will reach it from a
// shim that never came back. A relaunch the tree lifecycle refuses, a take-back the store would
// not write and a stop the runtime refuses each keep the process, and the deadline is tried again
// at the probe interval, as the registration deadline's are.
//
// After a hello no answer followed, the process is relaunched, charged nothing, and its task taken
// back as a cut turn (replaceProcess, endUnanswered): the process came back, but none of the three
// bounds can be judged of it until its agent answers or a turn frame comes, so a hung agent's turn
// is cut, and a take awaiting its turn may have had that turn run unseen. A held suspension ends
// the process there instead, as every replacement ends it (replaceNow, endHeld). A take-back the
// store refuses leaves the process as it was, and the deadline is tried again at the probe
// interval.
//
// A claim whose shim did say hello, and whose agent has not said since whether it is in a turn
// for the stale-plugin relaunch that waits for the answer (askTurn, askRestoredTurn), reaches the
// deadline its hello armed again for that ask, a hello whose decision failed before it made the
// ask included, since the hello dated the ask first (askedAt, helloView): a hung agent's turn
// costs nothing to cut, and the daemon relaunches onto the current plugin a worker it can (R10).
// That relaunch is the stale-plugin lane's own (refreshPlugin), charged nothing, as the lane's
// relaunch is: the process did come back, and the relaunch is for its plugin, not for a failure
// of the claim. A gate the lane finds full leaves it to the next sweep's post, which no answer
// holds back any more. The deadline then bounds that ask, not a suspension held over the claim,
// whose shim came back and whose turn's end and take's report reach the machine: a hold over a
// claim working, or one that waits for the report of its take's turn (heldWaits, reportwait.go),
// is left to that turn and that wait, as reready leaves it, and the relaunch waits with it
// (refreshPlugin's own holds). Run here, its stop would cut off the take's own turn, or come
// before that turn's report, and settle would take back as a cut turn a task whose turn ran
// whole. A hold neither waits for runs here, as before the hello. And where a hello of the
// process's generation did not say its shim's backlog is whole, a take still awaiting its turn
// may have had that turn run unseen, its frames dropped from the replay, so the relaunch takes it
// back as a cut turn rather than unstarted, as at the agent's answer of no turn
// (markRestoredTurnUnseen, turnAnswered).
//
// Such a claim read in a turn whose end its connection cannot show is no case of the lane's: a
// replayed start made it working, its connection broke with that turn's end frame in it, and
// neither an answer nor a turn frame came on the next hello's connection, or no hello came at all
// (connSeen not viewWhole). The lane would hold the relaunch for a turn's end that may never come
// (refreshPlugin, holdStaleRelaunch), and leave the claim working until the stall backstop. So it
// is decided below, as any claim whose view is not whole at this deadline: relaunched, uncharged,
// after a hello no answer followed, or retired as a shim that never reconnected with no hello since
// its close, a held suspension stopping the process in either place, and the task taken back as a
// cut turn (interrupted).
//
// Over a view not whole this deadline is also the bound of work held for the report of the take's
// turn, since the report wait's own bound judges only a whole view: the operator's stop
// (stopAtDeadline), read before every path below, a held suspension's among them, whose let-go
// would drop the stop the route answered 202 for; and a held replacement (replacementAtDeadline),
// read after a suspension the deadline runs above, which ends the process instead, and before the
// hello paths, which would charge a stale observation.
func reconnectDeadline(m *Machine, ctx context.Context, _ Event) error {
	m.cancelProtectedAsk()
	askedTurn := m.askTurn && !m.askedAt.IsZero()
	if askedTurn && m.inTurn() && m.connSeen != viewWhole {
		m.askTurn, m.unansweredAsks, askedTurn = false, 0, false
	}
	if m.heldStop {
		m.markTurnUnseen()
		return m.stopAtDeadline(ctx)
	}
	helloed := askedTurn || m.connSeen == viewAsked
	if (m.held != nil && !helloed) || m.heldRunsNow() {
		return m.suspendHeld(ctx)
	}
	if askedTurn {
		m.askTurn, m.unansweredAsks = false, 0
		m.log.Warn("supervise: the agent of a claim the daemon's start restored did not say whether it is in a turn within the reconnect deadline of its hello; the stale-plugin lane relaunches it, uncharged",
			"askedAt", m.askedAt)
		m.markRestoredTurnUnseen()
		// This deadline is the bound of the wait for a whole view, so its relaunch does not wait
		// for one (refreshPlugin), and row 26 takes its take back as a cut turn (interrupted).
		return m.refreshPlugin(ctx, m.askTrigger, nil, atReconnectDeadline)
	}
	m.markTurnUnseen()
	if m.heldReplacement != nil {
		return m.replacementAtDeadline(ctx)
	}
	if m.connSeen == viewAsked {
		m.log.Warn(viewUnansweredRelaunchLine, "task", pendingNumber(m.claim.Pending))
		end, err := m.replaceProcess(ctx, "the agent did not say whether it is in a turn by the reconnect deadline of its shim's hello", ordinaryEnd, endUnanswered, heldAtItsTurn)
		if end == EndNotMade {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
		}
		return err
	}
	never := neverReady{
		whose: "shim never reconnected",
		never: "never said hello to the daemon that started",
		line:  "supervise: the shim never reconnected after the daemon started; retired its process",
		cause: CauseNeverReconnected,
	}
	why := "the shim never reconnected after the daemon started"
	if m.connSeen == viewClosed {
		never.never = "never said hello again after its connection closed"
		never.line = "supervise: the shim never reconnected after its connection closed; retired its process"
		why = "the shim never reconnected after its connection closed"
	}
	never.before = func(ctx context.Context) error {
		if err := m.interrupted(ctx, why, ordinaryEnd); err != nil {
			return fmt.Errorf("take the task back: %w", err)
		}
		return nil
	}
	return m.retireNeverReady(ctx, never)
}
