package supervise

import "context"

// What finishes a taken task (dispatch://INFRA-2811, decision 8): the start of its take's turn,
// the finish itself, and the end-frame bound, which finishes a task only for a plugin that names
// no number, or makes again a finish a report decided and the store refused.

// startTakesTurn is the write of a turn's start that is the turn of the pending task's accepted
// take: the claim and the delivery together. The delivery records the turn started
// (TurnStarted) and keeps the take's text, so a take of the number sent again from here on is
// still answered it (answeredFromRecord), and a report that then says this turn was not the
// take's gives the take back its turn with the same text (notTheTakesTurn). The claim it writes
// carries the prompt budgets at zero, which is the reset every turn's start makes (turnStarted,
// R5): the take that came before it reset nothing.
func (m *Machine) startTakesTurn(ctx context.Context) error {
	p := m.claim.Pending
	p.TurnStarted = true
	return m.putClaimAndPendingOwed(ctx)
}

// finishTask finishes the pending task, whose take's turn ran: the delivery retires, the claim
// records the number finished and the session that finished it (Claim.TaskFinished,
// TaskFinishedSession) in the same write, and the value is deleted. by names what finished it,
// for the line: the turn-outcome report that named its number (finishedByReport), or the end of
// its turn with no report after it (finishedByEndFrame).
//
// A finish whose retirement the store refused leaves the task pending with its turn over, so the
// end-frame bound is armed again for it, whatever its plugin: the bound that fired, or the report
// that named it, has been spent, nothing else would finish it (settle finishes it in every state
// but ready, idle and working), and no worker sends a report again once it was answered. It is
// armed at once over a ready or idle claim, and over a working one at the end of what it works
// for: the turn running — a report that came ahead of its own turn's end frame — or a compaction
// with no turn under it (compactEnded). So it is armed wherever the report arrived, before a
// bound or after one fired (armEndFrameBoundUnlessArmed: the refusal leaves a finish that bound
// makes, the report's owed or the end frame's). A refusal of the report's own finish leaves that
// finish owed (Machine.finishOwed), so the bound that makes it again finishes by the report,
// whatever the claim's plugin, and each refusal of the bound's own write arms it again, until one
// lands (decision 8 of dispatch://INFRA-2811). A plugin that names its task's number is given no
// other bound than this one (TakesNoTask).
//
// A finish says the take's turn ran with no provider error (a report of one goes to the ladder,
// providerErrored, never here), so it makes final what the take credited of news of the approved
// design: the delivery is marked so (Delivery.Ran) and written before it retires, and that write
// folds the copy into the told rows (the claim store's settleDesignNews), so the retirement, which
// ends the row's credit with the row, ends nothing of it. A write of the mark that fails is a
// finish that failed, owed and made again as a refused retirement is; the mark stands in memory,
// so the delivery is unsaved and the next write of it folds the copy then, and the finish made
// again writes no second mark. A task that carries no copy writes nothing more than it did.
func (m *Machine) finishTask(ctx context.Context, by string) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	number, id := p.Number, p.ID
	err := m.creditRan(ctx)
	if err == nil {
		err = m.retireTask(ctx, true)
	}
	if err != nil {
		if by == finishedByReport {
			m.finishOwed = true
		}
		if m.claim.State == StateReady || m.claim.State == StateIdle {
			m.armEndFrameBoundUnlessArmed(id)
		}
		return err
	}
	m.log.Info(taskFinishedLine, "task", number, "by", by)
	m.deleteValue(ctx, number, valueDeletedFinished)
	return nil
}

// creditRan marks the pending task's credit of news of the approved design final (Delivery.Ran)
// and writes the delivery, which folds that credit into the told rows (finishTask says why): only
// for a taken task whose accepted take carried a copy (Delivery.Carried) not yet marked so.
func (m *Machine) creditRan(ctx context.Context) error {
	p := m.claim.Pending
	if !p.taken() || len(p.Carried) == 0 || p.Ran {
		return nil
	}
	p.Ran = true
	return m.putPending(ctx)
}

// makeOwedFinish makes the finish a report decided and the store refused (takePhase, waitFinish),
// and answers whether the take was in that phase, with that finish's error: every take-back of a
// take asks it first, and one in that phase is not taken back but finished by that report,
// whichever turn the take awaits, since the report decides, and taken back under its number the
// task would run again. The take-backs that ask it are a newer task's (retirePending), a let-go's
// (interrupted, takeBackUnstarted), one that keeps the process (takeBack, which the compaction
// asks through it too) and the ladder's (takeToLadder). A finish the store refuses again stays
// owed (finishTask), for the end-frame bound or the next decision (settle) to make.
func (m *Machine) makeOwedFinish(ctx context.Context) (owed bool, err error) {
	if m.takePhase() != waitFinish {
		return false, nil
	}
	return true, m.finishTask(ctx, finishedByReport)
}

// finishAtTurnsEnd is the end-frame bound over: the task's turn ended, and no turn-outcome
// report named its number within the RPC timeout (Timeouts.RPC), so the end of the turn finishes
// it — which is what finishes a task a worker whose plugin reports no number took
// (TakesNoTask). A bound that makes again a finish a report decided and the store refused
// finishes by that report, and its line says so (overTurnFinish). What the turn's end held for
// the task runs now, as the report's finish runs it: a held suspension that waited for that finish
// among it (settleThenAfterTurn, reportwait.go). A claim still working, for a turn or a
// compaction, runs it at that end instead.
func finishAtTurnsEnd(m *Machine, ctx context.Context, _ Event) error {
	by, _ := m.overTurnFinish()
	if by == "" {
		by = finishedByReport
	}
	return m.settleThenAfterTurn(ctx, func() error { return m.finishTask(ctx, by) })
}

// overTurnFinish is how a take's turn that is over settles its task where no write of the store
// has finished it yet: by the report whose finish the store refused (takePhase, waitFinish),
// whatever the claim's plugin, else by its end frame for a plugin that names no number
// (TakesNoTask); false for neither, a task of a plugin that names its number which no report has
// finished, which only that report finishes.
func (m *Machine) overTurnFinish() (by string, finishes bool) {
	switch {
	case m.takePhase() == waitFinish:
		return finishedByReport, true
	case TakesNoTask(m.claim.PluginContract, m.deps.TaskTakeContract):
		return finishedByEndFrame, true
	}
	return "", false
}

// armEndFrameBound arms the end-frame bound over the pending task whose take's turn ended
// (TimerFinish), where that end finishes the task (overTurnFinish): a plugin that names no number,
// or a finish a report decided and the store refused; and arms nothing for any other.
func (m *Machine) armEndFrameBound(id string) {
	if _, finishes := m.overTurnFinish(); finishes {
		m.arm(TimerFinish, m.deps.Timeouts.RPC, id)
	}
}

// armEndFrameBoundUnlessArmed arms the end-frame bound (armEndFrameBound) only where none stands,
// so a bound armed already keeps its time (compactEnded, armFinishAtAct).
func (m *Machine) armEndFrameBoundUnlessArmed(id string) {
	if _, armed := m.timers[TimerFinish]; !armed {
		m.armEndFrameBound(id)
	}
}

// armFinishAtAct arms the end-frame bound a daemon start left to the worker's first act
// (finishAtAct, NewMachine), at the end of the decision that act made (unlock): the claim, one
// whose end frame finishes its task (TakesNoTask), was restored ready or idle over a take
// whose turn had started and ended under the daemon before this one, so the report that would
// finish it is the worker's to send again to this daemon, and a bound that ran from the start
// would run over the time no daemon served the worker (dispatch://INFRA-2811, whose departure 7
// restarts R5's clocks from the same act). The act's own decision may have settled the task
// already — a report that finished it — or have started a turn, whose end arms the bound in its
// turn (turnEnded), so the bound is armed only over a claim still ready or idle holding that take
// with its turn run (takesTurnRan).
func (m *Machine) armFinishAtAct() {
	if !m.finishAtAct || m.awaitsAct {
		return
	}
	m.finishAtAct = false
	p := m.claim.Pending
	if p == nil || !m.takesTurnRan() || (m.claim.State != StateReady && m.claim.State != StateIdle) {
		return
	}
	m.armEndFrameBoundUnlessArmed(p.ID)
}
