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
	d.dispatch(plain(d.input))
	if d.crash {
		d.Cut = true
		d.input = Event{}
		d.crash = false
		d.restart(false)
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
		d.H.Lost[d.W.Requests[e.R].L] = Overflow
	}
}
func (d *decision) dispatch(e Event) {
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
		q := &w.Requests[e.R]
		q.Answered, q.Answer = true, e.A
		d.H.Lost[q.L] = OldSocket
		d.restart(false)
		return
	case KReplay:
		e = d.pop()
		if e.K == KRefusal {
			e.B = 1
		}
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
		if !w.Attached || w.N != 0 {
			if w.N == uint8(len(w.Backlog)) {
				d.evict()
			}
			w.Backlog[w.N] = e
			w.N++
			return
		}
	}
	// A runtime stop is irreversible. At this point the old delivery and
	// its captured obligation own the next write; new rows cannot replace them.
	if d.StopOwed && d.P.Kind == 0 && e.K != KTimerStopWrite {
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
		if e.K != KTimerWrite && e.K != KTimerStopWrite && e.K != KDrain && d.Deferred.K == 0 {
			d.Deferred = e
		}
		return
	}
	if d.W.Fatal.K != 0 && d.W.Fatal != e {
		if d.Deferred.K == 0 && e.K != KTimerWrite {
			d.Deferred = e
		}
		e = d.W.Fatal
	}
	d.row(e)
	d.finish()
}
func (d *decision) row(e Event) {
	r := d.M
	if !r.live() {
		switch e.K {
		case KResume, KSuspend, KStartedBy, KPauseBegins, KPauseEnds, KFailEffects, KTimerStopWrite:
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
		d.W.Requests[e.R].Returned = true
		if e.A == OcRefused || e.A == OcBusy {
			q := &d.W.Requests[e.R]
			q.Answered, q.Answer = true, ARefLate
			if d.W.Dedupe == q.Root+1 {
				d.W.Dedupe = 0
			}
		}
		d.outcome(e)
	case KResult, KRefusal:
		a := r.Att[e.R]
		q := d.W.Requests[e.R]
		if e.K == KResult && a.open() && a.L == r.Task.L && q.L == r.Task.L && q.Under == a.Under && q.Written && q.Root == e.R {
			if e.A == AOK {
				d.H.OK[q.L] |= 1 << e.R
			}
			if e.A == AFatal {
				d.W.Fatal = e
			}
		}
		d.answer(e)
	case KReport:
		if e.A == RpFatal && r.live() && (e.B == 0 || r.ReportSeq == 0) {
			d.W.Fatal = e
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
	case KTimerHold:
		if due := r.Hold.due(); due != 0 && !d.StopRetry {
			d.Now = max(d.Now, due)
		}
	case KTimerRecov:
		if r.RecoverBy != 0 {
			d.Now = max(d.Now, r.RecoverBy)
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
		if r.TurnBy != 0 {
			d.Now = max(d.Now, r.TurnBy)
			r.TurnBy = 0
			take(&r)
			if r.Charges < ChargeLimit {
				r.Charges++
			}
			d.write(DeliveryWrite, r, e)
		}
	case KPauseBegins:
		d.W.LastPause++
		d.W.Pause = d.W.LastPause
	case KPauseEnds:
		d.W.Pause = 0
		if r.Hold.Pause != 0 && !d.StopOwed {
			r.Hold = Hold{}
			r.Intent = Intent{}
			d.write(ClaimWrite, r, e)
		}
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
		if r.Phase == PhWork || r.Hold.By == 0 && (d.Sending != 0 || hasAck(r)) {
			r.Relaunch = true
			d.write(ClaimWrite, r, e)
		} else {
			d.leave(e)
		}
	case KDeath, KWake, KExit:
		d.leave(e)
	}
	if d.P.Kind == 0 && !d.crash && d.M.Relaunch && d.M.Phase == PhIdle && d.M.Hold.By == 0 && d.Sending == 0 && !hasAck(d.M) {
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
