package holdmodel

// Enabled rejects unknown shapes and faults at nonexistent operations. Disabled
// inputs are stutters. The driver uses the same base decision's effect list to
// enumerate precisely these cuts without executing the base decision twice.
func Enabled(s State, e Event) bool {
	if !shapes[plain(e)] || !s.base(e) {
		return false
	}
	if e.Fault == 0 {
		return e.At == 0
	}
	_, fx := run(s, plain(e))
	return hasCut(fx, e)
}
func hasCut(x Effects, e Event) bool {
	for f, at := range x.cuts {
		if e.Fault == f && e.At == at {
			return true
		}
	}
	return false
}

// cuts exposes only operations that actually happened. In particular a
// persistently failing write cannot lose a *commit* reply.
func (x *Effects) cuts(yield func(uint8, uint8) bool) {
	var at uint8
	for _, f := range x.List[:x.N] {
		switch f.Action {
		case WriteAttempt:
			at++
			if !yield(FailWrite, at) || !yield(CrashBeforeWrite, at) {
				return
			}
		case Commit:
			if !yield(LostReply, at) || !yield(CrashAfterCommit, at) {
				return
			}
		case RuntimeStop:
			if !yield(CrashAfterRuntime, at) {
				return
			}
		}
	}
}
func Step(s State, e Event) (State, Effects) {
	if !Enabled(s, e) {
		return s, Effects{}
	}
	return run(s, e)
}
func run(s State, e Event) (State, Effects) {
	return execute(decision{State: s, input: e})
}
func execute(d decision) (State, Effects) {
	before := d.State
	input := plain(d.input)
	d.dispatch(input)
	if d.crash {
		d.Cut = true
		d.input = Event{}
		d.crash = false
		d.restart(false)
	}
	// The deadline oracle reads each timer the driver recorded, rather than
	// the scheduler's own selection, so a selection defect cannot hide itself.
	if a := before.recordedDeadline(); a != 0 && d.Now > before.Now && d.Now > a {
		d.H.Bad |= Late
	}
	if fatalClose(before, d.State) {
		if fatalCloseBad(before, d.State, d.Effects) {
			d.H.Bad |= FatalClose
		}
	} else if recoveryMutation(before, d.State, d.Effects) && !recoveryAuthorized(before, input, d.State, d.Effects) {
		d.H.Bad |= RecoveryFence
	}
	return d.State, d.Effects
}
func (d *decision) pop() Event {
	w := &d.W
	e := w.Backlog[0]
	copy(w.Backlog[:], w.Backlog[1:])
	w.N--
	w.Backlog[w.N] = Event{}
	return e
}
func (d *decision) evict() {
	e := d.pop()
	if e.K == KResult || e.K == KRefusal {
		d.H.Lost[d.W.Requests[e.R].Root] = Overflow
	}
}

// receiveFatal records fatal evidence when the daemon receives it, before it
// retries any pending write: its source redelivers it until the failure
// commits (M6), across a failed write's repair and across a restart alike.
func (d *decision) receiveFatal(e Event) {
	r := d.M
	if r.Phase == PhEnded || r.Phase == PhFailed {
		return
	}
	if e.K == KCrashFrame {
		e.K = KResult
	}
	switch {
	case e.K == KReport && e.A == RpFatal && (e.B == 0 || r.ReportSeq == 0):
	case e.K == KResult && e.A == AFatal && d.matches(e):
	default:
		return
	}
	if d.H.FatalPending {
		return // the source's replay is the same item until this one commits
	}
	d.H.FatalPending = true
	d.H.FatalReceived++
	d.judgeReceipt()
	if d.skipFatalApply {
		d.W.FatalStuck = true
	}
	if !d.dropFatal {
		d.W.Fatal = e
	}
}

// judgeReceipt records, in the receipt ledger, the task the received fatal
// item names and whether it names an open attempt of that task. It does not
// when the store has already retired the task, or when this daemon's pending
// write retires it (served, replaced, phase or claim end): a pending write is
// retried before the fatal item is applied, so that earlier decision closes
// the task first. A pending take-back retires nothing, so the fatal close
// still keeps the task. A restart rebuilds memory and drops the pending
// write, and the source delivers the item again, so restart judges it again.
func (d *decision) judgeReceipt() {
	r := d.M
	d.H.FatalL, d.H.FatalID = r.Task.L, r.Task.ID
	retiring := d.P.Kind != 0 && r.Task.L != 0 && d.P.Want.Ret[r.Task.L] != 0
	d.H.FatalOpen = r.Task.L != 0 && r.outstanding() && d.S.Ret[r.Task.L] == 0 && !retiring
}

// deferInput keeps the one input a failed repair left unconsumed (M8). Retries
// and a drain are the repair's own, a pause's end has already changed the
// store, and fatal evidence waits at its source.
func (d *decision) deferInput(e Event) {
	switch e.K {
	case KTimerWrite, KTimerStopWrite, KDrain, KPauseEnds:
		return
	}
	if d.Deferred.K == 0 && e != d.W.Fatal {
		d.Deferred = e
	}
}
func (d *decision) dispatch(e Event) {
	d.Booted = false
	w := &d.W
	switch e.K {
	case KWrongToken:
		return
	case KStoreDown:
		w.Down = true
		return
	case KStoreUp:
		w.Down = false
		return
	case KLoseReply:
		w.LoseReply = true
		return
	case KRuntimeRefuses:
		w.Refuses = true
		return
	case KRuntimeAccepts:
		w.Refuses = false
		return
	case KDetach:
		w.Attached = false
		return
	case KReconnect:
		w.Attached = true
		w.Conn++
		e = Event{K: KHello}
	case KRestart:
		d.restart(e.A != 0)
		return
	case KOverflow:
		d.evict()
		return
	case KWire:
		d.wire(e.R)
		return
	case KPrepare:
		d.prepare()
		return
	case KCrashFrame:
		d.receiveFatal(e)
		q := &w.Requests[e.R]
		q.Answered, q.Answer = true, e.A
		d.H.Lost[e.R] = OldSocket
		d.restart(false)
		return
	case KOutcome:
		// The send's synchronous answer is the shim's: it refused or deferred
		// the request whatever the daemon's fence then does with the callback.
		q := &w.Requests[e.R]
		q.Returned = true
		if e.A == OcRefused || e.A == OcBusy {
			q.Answered, q.Answer = true, ARefLate
			if w.Dedupe == q.Root+1 {
				w.Dedupe = 0
			}
		}
	case KPauseEnds:
		// The pause ends in the store whatever the daemon is doing. The daemon
		// drops the hold at the start of its next decision (dropEnded), so the
		// event takes no deferred slot.
		w.Pause = 0
		if d.Deferred.K != 0 {
			return
		}
	case KReport:
		d.receiveFatal(e)
		if d.Deferred.K != 0 {
			return // fatal evidence waits at its source until the drain (M6, M8)
		}
	case KReplay:
		e = d.pop()
		if e.K == KRefusal {
			e.B = 1
		}
		d.receiveFatal(e)
	// A timer fires at the time it was armed, before a pending write's
	// read-back can move its bound; the decision then reads the bound again.
	// A drained timer fired when it arrived and moves no time.
	case KTimerHold:
		if due := d.M.Hold.due(); due != 0 && !d.StopRetry {
			d.Now = max(d.Now, due)
		}
	case KTimerRecov:
		d.Now = max(d.Now, d.M.RecoverBy)
	case KTimerTurn:
		d.Now = max(d.Now, d.M.TurnBy)
	case KDrain:
		if d.P.Kind != 0 && !d.flush() {
			return
		}
		if w.Fatal.K != 0 {
			e = w.Fatal
		} else {
			e = d.Deferred
			d.Deferred = Event{}
		}
	case KResult, KTurnStart, KTurnEnd, KNamedStart, KRefusal:
		if e.K == KTurnStart {
			w.Agent = e.R + 1
			if e.R < NA {
				w.Requests[e.R].Started = true
			}
		}
		if e.K == KTurnEnd {
			w.Agent = 0
		}
		if e.K == KResult || e.K == KRefusal {
			q := &w.Requests[e.R]
			q.Answered = true
			q.Answer = e.A
			if e.K == KRefusal {
				q.Answer = ARefLate
				if w.Dedupe == q.Root+1 {
					w.Dedupe = 0
				}
			}
		}
		// The shim holds the frame while no daemon is attached, while older
		// frames wait before it, and while the daemon's deferred slot is taken.
		if !w.Attached || w.N != 0 || d.Deferred.K != 0 {
			if w.N == uint8(len(w.Backlog)) {
				d.evict()
			}
			w.Backlog[w.N] = e
			w.N++
			return
		}
		d.receiveFatal(e)
	}
	// A runtime stop is irreversible. At this point the old delivery and
	// its captured obligation own the next write; new rows cannot replace them.
	if d.StopOwed && d.W.Fatal.K == 0 && d.P.Kind == 0 && e.K != KTimerStopWrite {
		if e.K == KStartedBy && e.A == 0 {
			d.M.StartRow = 12
		}
		d.stop()
		if d.crash || d.P.Kind != 0 {
			return
		}
	}
	if e.K == KStartedBy && e.A == 0 && d.P.Kind == StopWrite && !d.P.Uncertain {
		d.P.Want.StartRow = 12
	}
	if d.P.Kind != 0 && !d.flush() {
		d.deferInput(e)
		return
	}
	// Unacknowledged fatal evidence decides first (M6, O3): its failure ends
	// the hold and any intent, so nothing below can stop the claim.
	if d.W.Fatal.K != 0 {
		// A receipt may arrive while another write read back an ended or
		// suspended row. It still takes priority until failure commits.
		if d.W.Fatal != e {
			d.deferInput(e)
			e = d.W.Fatal
		}
	}
	// D1's order holds in every other decision, a repair's and a drain's
	// included: a hold whose pause has ended is dropped before anything can
	// stop the claim for it.
	if d.W.Fatal.K == 0 && d.dropEnded() && d.P.Kind != 0 {
		d.deferInput(e)
		return
	}
	d.row(e)
	d.finish()
}
func (d *decision) row(e Event) {
	r := d.M
	if d.W.FatalStuck && d.H.FatalPending {
		return
	}
	// Receipt made this fatal eligible. It has priority over a held or already
	// stopped claim too: M6/O3 require its durable failure, not a pause stop.
	if !d.W.FatalStuck && d.W.Fatal == e && (e.K == KReport && e.A == RpFatal || e.K == KResult && e.A == AFatal) {
		// A runtime stop may have landed before its transaction did. Failure
		// owns the claim now, so that uncommitted pause stop is cancelled.
		d.StopOwed, d.StopRetry = false, false
		// Receipt preceded a repair that may have taken the task back. A fatal
		// close from an open attempt keeps the source's task, whether the
		// receipt was the prompted result or an unprompted fatal report.
		if d.H.FatalL != 0 {
			r.Task.Conf = 0
		}
		if d.H.FatalOpen && d.H.FatalL != 0 {
			r.Task.L, r.Task.ID = d.H.FatalL, d.H.FatalID
		}
		d.fail(&r)
		if d.badFatalClose {
			take(&r)
			r.Stops[1] = 1
		}
		d.write(EvidenceWrite, r, e)
		return
	}
	if !r.live() {
		switch e.K {
		case KResume, KSuspend, KStartedBy, KPauseBegins, KFailEffects, KTimerStopWrite:
		default:
			return
		}
	}
	switch e.K {
	case KRegister:
		if d.Prepared != 0 && r.Task.L != 0 {
			d.register(e)
		}
	case KDeliver:
		d.queue(e)
	case KSupersede:
		e.A = 2
		d.queue(e)
	case KTurnStart, KTurnEnd, KNamedStart, KProbeIdle:
		d.turn(e)
	case KOutcome:
		d.outcome(e)
	case KResult, KRefusal:
		if e.K == KResult && e.A == AOK && d.matches(e) {
			d.H.OK[r.Task.L] |= 1 << e.R
		}
		if e.K == KResult && r.Task.Unknown && r.Att[e.R].Stage == AtNone && d.W.Requests[e.R].L == r.Task.L {
			// D12 condition 2: a legacy row records no attempt to match this
			// answer against, so it decides nothing and cannot reach the task.
			d.H.Lost[e.R] = Unmatched
		}
		d.answer(e)
	case KReport:
		if d.forceFatalFailure {
			d.fail(&r)
			d.write(EvidenceWrite, r, e)
			return
		}
		d.report(e)
	case KFailEffects:
		if r.Notify {
			if d.W.Proc {
				d.add(RuntimeStop, 0, 0)
				if d.W.Refuses {
					d.crash = d.fault(CrashAfterRuntime)
					return
				}
				if d.fault(CrashAfterRuntime) {
					d.W.Proc = false
					d.crash = true
					return
				}
				d.W.Proc = false
			}
			d.add(Notify, 0, 0)
			r.Notify = false
			d.write(ClaimWrite, r, e)
		}
	case KSuspend:
		d.suspend(e)
	case KTimerStopRetry:
		d.StopRetry = false
	case KTimerStopWrite:
		if d.StopOwed && r.Intent.By != 0 {
			d.stop()
		}
	case KTimerWrite: // flush above; finish below
	case KTimerHold: // time moved above; finish stops a hold that is due
	case KTimerRecov:
		if r.RecoverBy != 0 && r.RecoverBy <= d.Now {
			if r.Hold.By != 0 {
				d.intend(r, e, false)
			} else {
				invalidate(&r)
				take(&r)
				d.Restored = false
				d.write(DeliveryWrite, r, e)
			}
		}
	case KTimerTurn:
		if r.TurnBy != 0 && r.TurnBy <= d.Now {
			for k, a := range r.Att {
				if a.open() && a.TurnBy != 0 && a.TurnBy <= d.Now {
					r.Att[k].TurnBy = 0
				}
			}
			refreshTurnBy(&r)
			take(&r)
			if r.Charges < ChargeLimit {
				r.Charges++
			}
			d.write(DeliveryWrite, r, e)
		}
	case KPauseBegins:
		d.W.LastPause++
		d.W.Pause = d.W.LastPause
	case KReady:
		if r.Phase == PhLaunch {
			r.Phase = PhIdle
			d.W.Proc = true
		}
		d.write(ClaimWrite, r, e)
	case KRegDeadline, KRelaunchAgain:
		if r.Hold.By != 0 {
			d.intend(r, e, false)
		}
	case KResume:
		r.Phase, r.Work = PhIdle, WkNone
		r.SuspendedBy = SrcNone
		d.W.Proc, d.W.Attached = true, true
		d.W.Inc++
		d.W.Dedupe, d.W.Agent = 0, 0
		d.write(ClaimWrite, r, e)
	case KStartedBy:
		if e.A == 1 || r.StartRow == 12 {
			return
		}
		r.StartRow = 12
		if r.Hold.By == SrcDaemon {
			r.Hold = Hold{}
			r.Intent = Intent{}
		}
		d.write(ClaimWrite, r, e)
	case KHello:
		if d.Sending != 0 {
			d.Resend = true
		}
		if d.Restored && r.Phase == PhWork && d.W.Agent == 0 {
			d.turn(Event{K: KProbeIdle})
		}
	case KCompact:
		if r.Hold.By == 0 && r.RecoverBy == 0 {
			if r.waiting() {
				invalidate(&r)
				take(&r)
			}
			r.Phase, r.Work = PhWork, WkCompact
			d.write(DeliveryWrite, r, e)
		}
	case KCompactEnd:
		if r.Work == WkCompactTurn {
			r.Work = WkTurn
		} else {
			r.Phase, r.Work = PhIdle, WkNone
		}
		d.write(ClaimWrite, r, e)
	case KParkWake:
		r.Wake = true
		d.write(ClaimWrite, r, e)
	case KStale:
		// D10's fence: recovery doubt holds the replacement back, as it holds
		// back a newer task and a compaction's take-back. The relaunch waits
		// for the answer or the bound, and a hold's stop then owes it.
		if r.Phase == PhWork || r.RecoverBy != 0 || r.Hold.By == 0 && (d.Sending != 0 || hasAck(r)) {
			r.Relaunch = true
			d.write(ClaimWrite, r, e)
		} else {
			d.leave(e)
		}
	case KDeath, KExit:
		d.leave(e)
	case KWake:
		// D10: a parked wake is not a no-process, forced-stop or expiry
		// cause. It remains parked until the recovery answer decides.
		if r.RecoverBy != 0 && d.W.Proc && !d.wakeBreaksRecovery {
			return
		}
		d.leave(e)
	}
	if d.P.Kind == 0 && !d.crash && d.M.Wake && d.M.Phase == PhIdle && d.M.Hold.By == 0 && d.M.RecoverBy == 0 &&
		(e.K == KResult || e.K == KRefusal || e.K == KTimerRecov) {
		// A wake parked across recovery runs only after its deciding answer
		// or the recovery bound. An ordinary park still waits for KWake.
		d.leave(Event{K: KWake})
		return
	}
	if d.P.Kind == 0 && !d.crash && d.M.Relaunch && d.M.Phase == PhIdle && d.M.Hold.By == 0 && d.M.RecoverBy == 0 && d.Sending == 0 && !hasAck(d.M) {
		d.leave(Event{K: KStale})
	}
}
func hasAck(r Row) bool {
	for _, a := range r.Att {
		if a.Stage == AtAcked {
			return true
		}
	}
	return false
}
