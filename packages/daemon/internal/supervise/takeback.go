package supervise

// The take-back of an accepted take (take.go): the task goes back to waiting under its number, for
// a reason that keeps the process (takeBack) or for a let-go that stops it (takeBackUnstarted), and
// what the take recorded goes back with it (giveBackTake, dropTake).

import (
	"context"
	"time"
)

// takeBack returns a taken task to waiting under the number it has, for a reason that keeps the
// process: a provider error's turn and the second wait. The take goes (ConfirmedAt,
// TakenText, TurnStarted, TakeTold, which dropTake resets for both take-backs of a take), so the
// next take of the number is decided afresh and its text composed
// again with whatever note the cause left (Delivery.message); the end of the task's turn is no
// longer awaited. The take state the claim recorded stays, which is how the end of the cause's
// hold knows the worker asked at this revision and writes the value again (offerPending), and the
// machine remembers the number (Machine.rewrite), so that write is logged as the retake it is.
//
// What the take told and, for a take whose turn had not started, the handshake's confirmation go
// back with it (giveBackTake): a provider error's turn started on the take's text, and the second
// wait's had not. Where either moved, the claim goes with the delivery in one write. A compaction
// takes no take back: it is Oh My Pi's handoff, which aborts no turn (compactStart).
//
// What the take-back establishes holds in memory at once, whether or not its write lands: a
// write that fails leaves the delivery unsaved, and settle writes it again, with the claim.
//
// A take awaiting its turn while the claim's connection does not show every frame of its turns, or
// one marked so, goes back as a cut turn with its note (dropTake, turnMayHaveRunUnseen): R5's
// second wait takes back a take marked so after a turn's end the daemon never saw begin.
// The task is then marked for the record of a let-go of the process that comes before its next
// take (Machine.cutTurnOf), so that record tells the cut turn rather than a turn the agent was not
// in.
//
// A take whose report's finish the store refused (takePhase, waitFinish) is not taken back but
// finished, by that report (makeOwedFinish): the report decides whichever turn the take awaits,
// and taken back under its number the task would run again.
func (m *Machine) takeBack(ctx context.Context, cause string) error {
	if owed, err := m.makeOwedFinish(ctx); owed {
		return err
	}
	if m.takeBackInMemory(cause) {
		return m.putClaimAndPendingOwed(ctx)
	}
	return m.putPending(ctx)
}

// takeBackInMemory is takeBack with no write: the take goes from the delivery and the claim in
// memory, for a caller that writes them in a write of its own, together with what it decides
// next — the ladder's step, which must land with the take-back or not at all (takeToLadder). It
// answers whether the claim moved with it (giveBackTake). A take that goes back as a cut turn is
// marked for the let-go's record at once, as the take-back stands in memory (Machine.cutTurnOf,
// dropTake).
func (m *Machine) takeBackInMemory(cause string) (claimMoved bool) {
	p := m.claim.Pending
	if p == nil {
		return false
	}
	m.disarm(TimerFinish)
	claimMoved, cut := m.clearTake(p, cause)
	if cut {
		m.cutTurnOf = p.ID
	}
	m.rewrite = valueRewrite{number: p.Number, cause: TaskValueRetake}
	return claimMoved
}

// giveBackTake gives back what the accepted take of p recorded, for a take-back that leaves p
// pending: the one rule every take-back of an accepted take follows (takeBack, interrupted, and
// takeBackUnstarted for a retirement and a suspension). It reads whether the take's turn had
// started from p's take (Delivery.startRecorded), so it is called before the take is cleared.
//
//   - Each of the claim's two records this take told (Delivery.TakeTold) goes back to untold, so
//     the next accepted take carries its sentence again (untell). A provider error's take-back, whose
//     turn started, untells too: the error is a model call that failed, often the turn's first, so
//     the agent never read the text, and where an earlier call of the turn did, the sentence is
//     told twice rather than lost. The second wait finds no turn of the take at all, so it ran
//     none of the text.
//   - Where the take's turn had not started, the claim's handshake gives back its confirmation of
//     the take's entry (EntryHandshake.unconfirmed), read before the take's ConfirmedAt is
//     cleared: a take is the worker's own word that it took the entry, but one whose text no turn
//     ran is no proof the worker read it, and the state would give an `entryConfirmedAt` for an
//     entry the worker never ran (dispatch://INFRA-2614 R6). A provider error's turn started on the
//     take's text, so the handshake keeps it.
//
// It answers whether the claim changed, which is whether the caller's write of the take-back must
// carry the claim with the delivery. A let-go that cuts off a turn of the take gives nothing back
// and does not call it: that turn ran the take's text, so the agent read what it carried
// (interrupted).
func (m *Machine) giveBackTake(p *Delivery) (claimMoved bool) {
	claimMoved = m.untell(p)
	if p.startRecorded() {
		return claimMoved
	}
	if handshake, moved := m.claim.Handshake.unconfirmed(*p); moved {
		m.claim.Handshake, claimMoved = handshake, true
	}
	return claimMoved
}

// untell takes back what the accepted take of p told (accept): each of the claim's two records
// that take marked told (Delivery.TakeTold), and that still names p's id (ToldBy), goes back to
// untold, so the next accepted take carries its sentence again, and the mark is cleared. It
// reports whether any record was told by that take.
//
// The give-back is keyed on the take, not on the delivery: the id stands across every take-back
// of the task, so a record an earlier take of the same delivery told — one whose turn started and
// was cut off, which keeps its telling (interrupted) — names p's id too, and a later take that
// carried nothing must not give it back, or the next take tells it a second time. The mark is the
// delivery's, written with the take, so a daemon restarted between a take and its give-back reads
// what that take told rather than every record that names the id. The one record told by no take
// is the old session's record of a let-go, which the abandonment of that session ends
// (forgetSession), and no take-back untells it.
func (m *Machine) untell(p *Delivery) bool {
	moved, told := false, p.TakeTold
	p.TakeTold = Sentences{}
	if a := &m.claim.Abandoned; told.Abandoned && a.Told && a.ToldBy == p.ID {
		a.Told, a.ToldBy, moved = false, "", true
	}
	if e := &m.claim.EndedWork; told.Ended && e.Told && e.ToldBy == p.ID {
		e.Told, e.ToldBy, moved = false, "", true
	}
	if moved {
		m.log.Info("supervise: the take that told the agent what became of its earlier work was taken back; the next accepted take tells it again",
			"task", p.Number, "delivery", p.ID)
	}
	return moved
}

// untellUnstarted is recordEnd's give-back of an accepted take that awaits its turn, which the
// let-go takes back (takeBackUnstarted, interrupted): no turn the daemon saw ran that take's text,
// so what it told goes back to untold (untell) before the let-go records its end, and the record
// of this let-go joins the record the take had marked told (joined). A take whose turn may have
// run unseen (turnMayHaveRunUnseen) is untold too: it goes back as a cut turn, and its next take
// tells again what this one told, since untold is the safe way round. A take whose turn ran keeps
// what it told, since that turn ran the text: one whose turn started, and one whose report came
// with its finish owed (takeAwaitsTurn, takesTurnRan). It writes nothing: the let-go's own write
// carries the claim.
func (m *Machine) untellUnstarted() {
	if m.takeAwaitsTurn() {
		m.untell(m.claim.Pending)
	}
}

// takeBackUnstarted takes back the accepted take of the pending task whose turn has not started,
// for a let-go that stops the claim's process and keeps the task pending: a retirement (retire)
// and a suspension (suspended), a held suspension's deadline among them. The process that took it
// is the one the claim no longer runs, so no turn of it starts from here, and where the claim's
// connection showed every frame of its turns none started before either: the worker ran nothing
// of the take's text. What the take told and the handshake's confirmation of its entry go back
// with it (giveBackTake), and the next process's take of the number is decided afresh, its text
// composed again (Delivery.message) behind the records that tell then, this let-go's own among
// them; a take answered from the record instead (answerAgain) would hand the next process the text
// the stopped one was given, which carries none of what this let-go recorded and marks nothing
// told. The task keeps its number, as every take-back does. It writes nothing and reports whether
// it took a take back, so the caller's own write carries the delivery with the claim
// (putClaimAndPendingOwed). cause is the take-back's line's (takenBackLine).
//
// A take whose turn may have run with no frame of it reaching the machine goes back as a turn cut
// off instead (Delivery.Interrupted), as the reconnect deadline takes it back: the let-go that
// came first found the claim's connection not showing every frame of its turns, and kept that
// for this take-back (turnMayHaveRunUnseen, letGo), so the worker may have run it whole. The
// take-back reads that by the one rule of every take-back of a take, and marks nothing for the
// record of the let-go, which wrote that record already (dropTake).
//
// The take it takes back is one awaiting its turn, as the one rule of where the take stands reads
// it (takeAwaitsTurn). One whose start is not recorded (Delivery.awaitsStart) but whose report's
// finish the store refused (takePhase, waitFinish) is not taken back but finished, by that report
// (makeOwedFinish), which writes the claim with the retirement; a finish the store refuses again
// answers its error, and the next decision makes it (settle).
func (m *Machine) takeBackUnstarted(ctx context.Context, cause string) (bool, error) {
	switch p := m.claim.Pending; {
	case m.takeAwaitsTurn():
		m.clearTake(p, cause)
		return true, nil
	case p != nil && p.awaitsStart():
		_, err := m.makeOwedFinish(ctx)
		return false, err
	}
	return false, nil
}

// clearTake is the take-back of the accepted take of p in memory that its two callers share, each
// with its own write: takeBack (takeBackInMemory), for a reason that keeps the process, and
// takeBackUnstarted, for the retirement or the suspension that lets the process go. What the take
// recorded goes back (giveBackTake), the take goes, so the next take of the number is decided
// afresh and its text composed again (Delivery.message), and the take-back is logged under cause
// (takenBackLine). The take goes by the one rule of both take-backs of a take (dropTake), with no
// turn of it cut off by this take-back itself: as a turn cut off where it awaits its turn and that
// turn may have run unseen, and with its copy of the news of the approved design, so the
// take-back's write ends the credit it held and the next accepted take carries that news again.
// It answers giveBackTake's answer, whether the claim changed, and dropTake's, whether the take
// went back as a cut turn, which takeBack alone marks for the let-go's record (dropTake). With no
// take given it takes nothing back.
func (m *Machine) clearTake(p *Delivery, cause string) (claimMoved, cut bool) {
	if p == nil {
		return false, false
	}
	claimMoved = m.giveBackTake(p)
	cut = m.dropTake(p, false, ordinaryEnd)
	m.log.Info(takenBackLine, "task", p.Number, "cause", cause)
	return claimMoved, cut
}

// dropTake drops the accepted take from d — the pending task's delivery, or the copy of it a
// take-back writes before it becomes the claim's — and answers whether the take goes back as a cut
// turn: the one rule of both take-backs of an accepted take, interrupted's and clearTake's. The
// take goes (ConfirmedAt, TakenText, TurnStarted, TakeTold), and its copy of the news of the
// approved design with it (Delivery.Carried, credit). It goes back as a turn cut off
// (Delivery.Interrupted, and outOfMemory as Delivery.OutOfMemory), so its next accepted take
// carries the sentence of its interruption (Delivery.message), where inTurn says the take-back
// cut a turn of it off, and where the take awaits its turn and that turn may have run with no
// frame of it reaching the machine (turnMayHaveRunUnseen: the claim's connection does not show
// every frame of its turns, or the take is marked so), for every take-back that keeps the task
// pending (dispatch://INFRA-2811, decision 8, row 26). That is read off the pending take, so d is
// that take, or a copy of it no step has cleared yet.
//
// A take that goes back as a cut turn before the let-go of the claim's process records anything
// is marked for that record (Machine.cutTurnOf), by the caller, where its take-back lands in
// memory: interrupted once its write lands, and takeBack, which keeps the process, at once
// (takeBackInMemory). That record reads the take, which is gone by then, so without the mark it
// says the agent was in no turn, and its sentence stands in place of the cut turn's note
// (Delivery.message). takeBackUnstarted marks nothing: the let-go it follows recorded first,
// reading the take's own mark (recordEnd, Machine.turnUnseen), and letGo cleared cutTurnOf, so a
// mark set there would stand into the let-go of the next process.
func (m *Machine) dropTake(d *Delivery, inTurn, outOfMemory bool) (cut bool) {
	cut = inTurn || m.turnMayHaveRunUnseen()
	d.ConfirmedAt, d.TakenText, d.TurnStarted, d.TakeTold = time.Time{}, "", false, Sentences{}
	d.credit(nil)
	if cut {
		d.Interrupted, d.OutOfMemory = true, outOfMemory
	}
	return cut
}
