// The settle loop: what a fenced filter write could not settle in the attempt its caller made
// under the session's lock is attempted again here, off that caller, until the session's consumer
// filters on what the bucket's newest record and row ask for or the session is gone
// (settleAgain, settleLoop); and the write whose answer did not come back, which is judged again
// once it may have landed, and for as long as the server can still apply it (unansweredWrite).

package main

import (
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// filterSettlePause is how long the settle loop waits before an attempt that follows one that
// did not settle (settleLoop). It is a bound derived from what that attempt waits on, the bus
// answering, and not a number chosen to make anything pass: the attempt before it met a bus that
// did not answer within the fence's budget, or that answered a row the attempt could not keep up
// with. A connection that lost its server tries it again every bus.ReconnectWait, so an attempt
// made sooner meets the same lost connection, and an attempt's first call waits for that
// reconnect itself within its own budget (bus.Client's ensureConnWithContext); against a server
// that answers slowly, the pause is what keeps the loop from adding a read and a write to it every
// round trip. Its cost is that a filter the bus refused stays wrong for up to one pause and one
// attempt after the bus answers again. An attempt that follows a write whose answer came back
// late makes no pause: the bus has just answered (unansweredWrite).
const filterSettlePause = bus.ReconnectWait

// settleRequest is what the settle loop running for one session has been asked to do since its
// last attempt began (settleAgain), under sessionDelivery.settleMu: after, ackFloor and why are
// the latest request's label, the highest floor any asked a create at, and why it was asked;
// pause is set when any request followed an attempt that did not settle; again is set when an
// attempt is still owed, and watch when the attempt owed is the watch's alone (lateWatch), which
// logs no line of its own; late is the consumer writes whose answers did not come back that the
// loop judges.
type settleRequest struct {
	after    string
	ackFloor uint64
	why      string
	pause    bool
	again    bool
	watch    bool
	late     []lateWatch
}

// lateWatch is a consumer write whose answer did not come back (bus.LateWrite) that the settle
// loop judges, and until when the server may still apply it (unansweredWrite): for a write whose
// late answer came, the moment it came; for one whose answer was lost with its connection, the
// server's stale limit after that loss (staleLimit) and lateWatchMargin, or that long after the
// loop last saw such a write land (lateLanded), whichever is later (lateUntil).
type lateWatch struct {
	write bus.LateWrite
	until time.Time
}

// lateWatchMargin is what a watch adds to the server's stale limit: the time between a lost
// write landing and an attempt of the loop reading it, which is up to one pause, a wait for the
// session's lock that a relay restart holds for up to two budgets (sessionRelays.restart), and
// the attempt's own budget. A landing is read within it, so the watch of every write lost with
// the same connection is still running when it is read, and moves on from it (lateLanded).
const lateWatchMargin = filterSettlePause + 3*sessionConsumerCallTimeout

// lateUntil is when w can no longer land, under settleMu: its own until, or staleLimit and
// lateWatchMargin after the last landing the loop saw of a write lost with a connection, whichever
// is later, since the server reads that connection until its stale limit after its last read.
func (m *sessionDelivery) lateUntil(w lateWatch) time.Time {
	if w.write.Landed || m.lateLanded.IsZero() {
		return w.until
	}
	return later(w.until, m.lateLanded.Add(m.staleLimit+lateWatchMargin))
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// settleAgain asks for another attempt at sessionID's consumer filter, after the attempt of a
// fenced write that did not settle it or after a write whose answer came late (fence,
// unansweredWrite): why says which, pause whether the attempt waits filterSettlePause first, and
// ackFloor is the floor a consumer the attempt creates again starts after, as the repair's is
// (repairRegistered). One loop runs per session at a time, on a goroutine of its own the shutdown
// waits for (runAfterAnswer), and a request that comes while it runs is folded into its next
// attempt, so a session whose writes fail many times over has one loop and not one per failure.
// The caller returns at once, holding whatever lock it holds: the loop takes the session's lock
// for each attempt alone.
func (m *sessionDelivery) settleAgain(sessionID, after string, ackFloor uint64, why string, pause bool) {
	m.askSettle(sessionID, after, ackFloor, why, pause, nil)
}

// askSettle is settleAgain with late, when it is not nil, a write whose answer did not come back
// for the loop to judge from its next attempt on (unansweredWrite).
func (m *sessionDelivery) askSettle(sessionID, after string, ackFloor uint64, why string, pause bool, late *lateWatch) {
	m.settleMu.Lock()
	if run, ok := m.settling[sessionID]; ok {
		run.after, run.why = after, why
		run.ackFloor = max(run.ackFloor, ackFloor)
		run.pause = run.pause || pause
		run.again, run.watch = true, false
		if late != nil {
			run.late = append(run.late, *late)
		}
		m.settleMu.Unlock()
		return
	}
	if m.settling == nil {
		m.settling = map[string]*settleRequest{}
	}
	run := &settleRequest{after: after, ackFloor: ackFloor, why: why, pause: pause, again: true}
	if late != nil {
		run.late = []lateWatch{*late}
	}
	m.settling[sessionID] = run
	m.settleMu.Unlock()
	if !m.runAfterAnswer(func() { m.settleLoop(sessionID) }) {
		m.settleMu.Lock()
		delete(m.settling, sessionID)
		m.settleMu.Unlock()
		m.logger.Warn(settleStoppingLine, slog.String("session_id", sessionID), slog.String("after", after), slog.String("why", why))
	}
}

// settleLoop makes the attempts settleAgain asked for sessionID's consumer filter until one
// settles it with no request left over and no late write left to watch, the session is gone, or
// the listener stops.
//
// Each attempt is the repair of a session's consumer (repairPass): it reads the bucket's newest
// record and interest row and makes the consumer what they ask for, under the fence
// (fenceFilterWrite) and on the fence's own budget, under the session's lock, which it releases
// before the pause after it. The repair and not the step the first attempt took for an unsubscribe,
// a settle or a restart (storedFilterStep) because it is the one write that reads the session's
// record: a record the bucket no longer holds is a session that is gone, where the loop stops
// and says so, and a record that asks for consumer delivery of a stream that holds no consumer is
// a session answered 200 that the repair gives its consumer back, at ackFloor's next sequence,
// as the delete's repair does.
//
// Each attempt also judges the late writes the loop holds (lateWatch): its repair takes the
// consumer back to the first message one of them may have made it skip (bus.LateWrite), and a
// write that it read landed, or judged for the last time, is dropped. While a write is left, an
// attempt that settled is followed by another one pause later: that is the watch, which ends once
// the server can apply none of them.
//
// It logs a WARN line each time it starts an attempt someone asked for, naming the session, the
// attempt, why it is made and the pause before it, and none for an attempt of the watch alone; an
// INFO line once an attempt settles the filter with nothing left to watch; and a WARN line when
// it stops for the session being gone or the listener stopping. A listener that stops
// mid-attempt ends that attempt (afterAnswerContext): the session's next registration sets the
// filter of the row it stores.
func (m *sessionDelivery) settleLoop(sessionID string) {
	stop, err := newFenceStop(m.afterAnswerContext())
	if err != nil {
		m.endSettle(sessionID)
		m.logger.Error(settleStoppingLine, slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
	for attempt := 1; ; attempt++ {
		m.settleMu.Lock()
		run := m.settling[sessionID]
		if !run.again {
			delete(m.settling, sessionID)
			m.settleMu.Unlock()
			m.logger.Info(settleSettledLine, slog.String("session_id", sessionID), slog.String("after", run.after), slog.Int("attempts", attempt-1))
			return
		}
		after, ackFloor, why, watch := run.after, run.ackFloor, run.why, run.watch
		pause := time.Duration(0)
		if run.pause {
			pause = filterSettlePause
		}
		run.again, run.pause, run.watch = false, false, false
		m.settleMu.Unlock()

		if !watch {
			m.logger.Warn(settleAgainLine, slog.String("session_id", sessionID), slog.String("after", after),
				slog.Int("attempt", attempt), slog.String("why", why), slog.String("pause", pause.String()))
		}
		timer := time.NewTimer(pause)
		select {
		case <-timer.C:
		case <-stop.ctx.Done():
			timer.Stop()
			m.endSettle(sessionID)
			m.logger.Warn(settleStoppingLine, slog.String("session_id", sessionID), slog.String("after", after), slog.String("why", why))
			return
		}
		late := m.lateJudgment(run)
		unlock := m.locks.lock(sessionID)
		end := fenceFilterWrite(stop, m.fenceAttemptBudget(), m.logger, m.registry, sessionID, after, m.repairPass(m.sessions, m.registry, sessionID, ackFloor, after, late))
		unlock()
		switch end {
		case fenceSettled:
			m.judged(sessionID, run, late)
		case fenceSessionGone:
			m.endSettle(sessionID)
			m.logger.Warn(settleGoneLine, slog.String("session_id", sessionID), slog.String("after", after), slog.Int("attempts", attempt))
			return
		case fenceCancelled:
			m.endSettle(sessionID)
			m.logger.Warn(settleStoppingLine, slog.String("session_id", sessionID), slog.String("after", after), slog.String("why", why))
			return
		default:
			m.settleMu.Lock()
			run.why, run.pause, run.again, run.watch = end.String(), true, true, false
			m.settleMu.Unlock()
		}
	}
}

// lateJudgment is the late writes run holds as the attempt about to start judges them: each
// write's last judgment where the server can apply it no more (lateUntil, bus.LateWrite.Last).
func (m *sessionDelivery) lateJudgment(run *settleRequest) *lateJudgment {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	late := &lateJudgment{writes: make([]bus.LateWrite, len(run.late)), seen: make([]bool, len(run.late))}
	now := time.Now()
	for i, w := range run.late {
		late.writes[i] = w.write
		late.writes[i].Last = !now.Before(m.lateUntil(w))
	}
	return late
}

// judged drops from run the late writes an attempt that settled has judged for good: a write its
// answer said landed, and a write judged for the last time. A write the attempt read standing on
// the consumer may still be watched, since nothing tells its landing from a writer of the same
// filters, but it moves every watch on (lateLanded). Where the attempt took the consumer back
// (lateJudgment.rewoundAt), every write it judged and keeps counts its Before from there: the
// consumer holds every message after it again (bus.LateWrite). While a write is left, the loop owes
// the watch its next attempt, one pause on; once none is left, the watch's end is logged.
func (m *sessionDelivery) judged(sessionID string, run *settleRequest, late *lateJudgment) {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	kept, ended := run.late[:0], false
	for i, w := range run.late {
		if i < len(late.writes) {
			if late.seen[i] && !late.writes[i].Landed {
				m.lateLanded = time.Now()
			}
			if late.writes[i].Landed || late.writes[i].Last {
				ended = ended || !late.writes[i].Landed
				continue
			}
			if late.rewoundAt > 0 {
				w.write.Before = max(w.write.Before, late.rewoundAt-1)
			}
		}
		kept = append(kept, w)
	}
	clear(run.late[len(kept):])
	run.late = kept
	switch {
	case len(kept) > 0 && !run.again:
		run.again, run.pause, run.watch = true, true, true
	case len(kept) == 0 && ended:
		m.logger.Info(lateWatchEndLine, slog.String("session_id", sessionID))
	}
}

// endSettle forgets the settle loop of sessionID, which is returning, so the next request starts
// another (settleAgain).
func (m *sessionDelivery) endSettle(sessionID string) {
	m.settleMu.Lock()
	delete(m.settling, sessionID)
	m.settleMu.Unlock()
}

// The settle loop's lines (settleAgain, settleLoop, unansweredWrite).
const (
	settleAgainLine    = "listener settles the filter of a session's consumer again: the attempt before did not leave it on the row the bucket holds"
	settleSettledLine  = "listener settled the filter of a session's consumer on the row the bucket holds"
	settleGoneLine     = "listener stopped settling the filter of a session's consumer: the bucket holds no record of the session, so no consumer of it is read"
	settleStoppingLine = "listener stopped settling the filter of a session's consumer: the listener is stopping; the session's next registration sets it"
	lateWatchLine      = "listener watches the consumer of a session at each pause: a consumer write whose answer was lost with its connection can land until the server closes that connection as stale"
	lateWatchEndLine   = "listener ended its watch of the consumer of a session: no consumer write lost with its connection can land any more"
)

// unansweredWrite is the listener's handler for a create or an update of a session consumer whose
// caller stopped waiting for its answer (bus.SessionConsumers.OnUnansweredWrite): the server may
// still apply it, and the filter it carries would then stand over whatever this listener set
// since, with nothing to read the consumer again (bus.UnansweredWrite says why the order of the
// two is the server's). It waits for that answer off the caller's goroutine, as work the shutdown
// waits for (runAfterAnswer), and hands the write to the settle loop with no pause before its
// next attempt (askSettle), which reads the consumer after the write: a filter of a plan nothing
// stored, or of a row the bucket has replaced, is taken back to the row the bucket holds, and a
// consumer the write made skip a message of that row's topics is taken back to that message
// (bus.LateWrite).
//
// A write whose answer says it landed is judged once. A write whose answer was lost with its
// connection may still land: the server goes on reading that connection until it closes it as
// stale (bus.StaleConnectionLimit), so the loop judges the consumer at each pause for that long
// and lateWatchMargin more (lateWatch), logging the watch's start and end. A refused write
// changed nothing, and the wait ends there; so does a write the server dropped, whose wait ends
// only at the connection's next reconnect or at the stop (bus.UnansweredWrite.Wait). The wait is
// counted from the moment the caller gave up, before that caller returns its error, so a reader
// waiting for the work behind an answer (awaitAfterAnswer) waits for this one and for the
// attempts it asks for.
func (m *sessionDelivery) unansweredWrite(w *bus.UnansweredWrite) {
	started := m.runAfterAnswer(func() {
		switch end := w.Wait(m.afterAnswerContext().Done()); end {
		case bus.WriteApplied:
			late := w.Late
			late.Landed = true
			m.askSettle(w.SessionID, filterAfterUnansweredWrite, 0, "a consumer write whose answer came late landed", false, &lateWatch{write: late, until: time.Now()})
		case bus.WriteAnswerLost:
			until := time.Now().Add(m.staleLimit + lateWatchMargin)
			m.logger.Info(lateWatchLine, slog.String("session_id", w.SessionID),
				slog.String("until", until.UTC().Format(time.RFC3339Nano)), slog.String("stale_limit", m.staleLimit.String()))
			m.askSettle(w.SessionID, filterAfterUnansweredWrite, 0, "a consumer write whose answer was lost with its connection may have landed", false, &lateWatch{write: w.Late, until: until})
		}
	})
	if !started {
		w.Release()
	}
}
