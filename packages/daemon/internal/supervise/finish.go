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
// armed at once over a ready or idle claim, and at the end of the turn running over a working
// one — a report that came ahead of its own turn's end frame — so it is armed wherever the report
// arrived, before a bound or after one fired (armEndFrameBound). A refusal of the report's own
// finish leaves that finish owed (Machine.finishOwed), so the bound that makes it again finishes
// by the report, whatever the claim's plugin, and each refusal of the bound's own write arms it
// again, until one lands (decision 8 of dispatch://INFRA-2811). A plugin that names its
// task's number is given no other bound than this one (endFrameFinishes).
func (m *Machine) finishTask(ctx context.Context, by string) error {
	p := m.claim.Pending
	number, id := p.Number, p.ID
	if err := m.retireTask(ctx, true); err != nil {
		if by == finishedByReport {
			m.finishOwed = true
		}
		if _, armed := m.timers[TimerFinish]; !armed && (m.claim.State == StateReady || m.claim.State == StateIdle) {
			m.arm(TimerFinish, m.deps.Timeouts.RPC, id)
		}
		return err
	}
	m.log.Info(taskFinishedLine, "task", number, "by", by)
	m.deleteValue(ctx, number, valueDeletedFinished)
	return nil
}

// finishAtTurnsEnd is the end-frame bound over: the task's turn ended, and no turn-outcome
// report named its number within the RPC timeout (Timeouts.RPC), so the end of the turn finishes
// it — which is what finishes a task a worker whose plugin reports no number took
// (endFrameFinishes). A bound that makes again a finish a report decided and the store refused
// finishes by that report, and its line says so (overTurnFinish). What the turn's end held for
// the task runs now (afterTurn), as the report's finish runs it.
func finishAtTurnsEnd(m *Machine, ctx context.Context, _ Event) error {
	by, _ := m.overTurnFinish()
	if by == "" {
		by = finishedByReport
	}
	if err := m.finishTask(ctx, by); err != nil {
		return err
	}
	if m.claim.State == StateWorking {
		return nil
	}
	return m.afterTurn(ctx)
}

// endFrameFinishes is whether the end of a take's turn finishes the task where no turn-outcome
// report named its number within the RPC timeout: only for a claim whose plugin is below
// TaskTakeContract (TakesNoTask), which names no number in any report, so its report can finish
// nothing and the end of the turn is what finishes the task a migrated old worker took. A claim
// whose plugin names its task's number has its task finished by the report that names it and by
// nothing else: the end of a run says nothing of whose run it was, since the daemon gives a take
// the first run that starts after it and the worker may run its text in the run after that
// (decision 8 of dispatch://INFRA-2811; notTheTakesTurn).
func (m *Machine) endFrameFinishes() bool { return TakesNoTask(m.claim.PluginContract) }

// overTurnFinish is how a take's turn that is over settles its task where no write of the store
// has finished it yet: by the report whose finish the store refused (finishOwed), else by its end
// frame for a plugin that names no number (endFrameFinishes); false for neither, a task of a
// plugin that names its number which no report has finished, which only that report finishes.
func (m *Machine) overTurnFinish() (by string, finishes bool) {
	switch {
	case m.finishOwed:
		return finishedByReport, true
	case m.endFrameFinishes():
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

// armFinishAtAct arms the end-frame bound a daemon start left to the worker's first act
// (finishAtAct, NewMachine), at the end of the decision that act made (unlock): the claim, one
// whose end frame finishes its task (endFrameFinishes), was restored ready or idle over a take
// whose turn had started and ended under the daemon before this one, so the report that would
// finish it is the worker's to send again to this daemon, and a bound that ran from the start
// would run over the time no daemon served the worker (dispatch://INFRA-2811, whose departure 7
// restarts R5's clocks from the same act). The act's own decision may have settled the task
// already — a report that finished it — or have started a turn, whose end arms the bound in its
// turn (turnEnded), so the bound is armed only over a claim still ready or idle holding that take
// with its turn started.
func (m *Machine) armFinishAtAct() {
	if !m.finishAtAct || m.awaitsAct {
		return
	}
	m.finishAtAct = false
	p := m.claim.Pending
	if p == nil || !p.turnStarted() || (m.claim.State != StateReady && m.claim.State != StateIdle) {
		return
	}
	if _, armed := m.timers[TimerFinish]; !armed {
		m.armEndFrameBound(p.ID)
	}
}
