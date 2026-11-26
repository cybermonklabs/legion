package holdmodel

func reasons(r Row) uint8 {
	var bits uint8
	if r.Wake {
		bits |= WakeLaunch
	}
	if r.Hold.Launch {
		bits |= HeldLaunch
	}
	if r.DeathCharged {
		bits |= DeathLaunch
	}
	if r.Relaunch {
		bits |= RelaunchLaunch
	}
	if r.Task.L != 0 {
		bits |= TaskLaunch
	}
	return bits
}
func (d *decision) suspend(e Event) {
	r := d.M
	by := SrcReleased
	if e.A == RqOperator {
		by = SrcOperator
	} else if e.A == RqDaemon || e.A == RqDaemonDL {
		by = SrcDaemon
	}
	if r.Phase == PhSusp {
		if r.SuspendedBy == SrcReleased && (by == SrcOperator || by == SrcDaemon) || r.SuspendedBy == SrcDaemon && by == SrcOperator {
			r.SuspendedBy = by
			if by == SrcDaemon && r.Task.PhaseTask {
				retire(&r, RPhaseEnd, 0)
			}
			d.write(ClaimWrite, r, e)
		}
		return
	}
	if !r.live() {
		return
	}
	h := r.Hold
	if e.B != 0 && d.W.Proc && (r.Phase != PhIdle || r.waiting() || h.By != 0) {
		return
	}
	var pause uint8
	var due uint16
	switch e.A {
	case RqGate:
		pause = d.W.Pause
		due = d.Now
	case RqPauseRow:
		pause = d.W.Pause
	case RqOperator:
		due = after(d.Now, 3)
	case RqDaemon, RqDaemonDL:
		if e.A == RqDaemonDL {
			due = after(d.Now, 2)
		}
	}
	replace := h.By == 0 || by == SrcOperator || h.By == SrcReleased && h.Pause == 0 && pause != 0
	if r.Phase == PhLaunch && h.By != SrcOperator {
		replace = by != SrcReleased || h.By == 0 || h.By == SrcReleased && (pause != 0 || h.Pause == 0)
	}
	if replace {
		h.By, h.Pause = by, pause
	}
	h.Request = by
	h.Launch = r.Phase == PhLaunch || h.Launch
	h.Deadline = earlier(h.Deadline, due)
	r.Hold = h
	if r.Intent.By != 0 {
		r.Intent.By, r.Intent.Pause = h.By, h.Pause
	}
	d.write(ClaimWrite, r, e)
}

// M7: even a displaced process needs durable intent before the daemon forgets
// a wake. A dead process need not be kept alive until the write succeeds.
func (d *decision) intend(r Row, e Event, displaced bool) {
	h := r.Hold
	if h.By == 0 {
		return
	}
	i := Intent{h.By, h.Pause, reasons(r), displaced}
	i.Causes |= r.Intent.Causes
	if r.waiting() || r.Task.Conf != 0 {
		take(&r)
	}
	invalidate(&r)
	d.Prepared, d.Sending = 0, 0
	r.Intent = i
	d.write(IntentWrite, r, e)
}
func (d *decision) stop() {
	r := d.M
	i := r.Intent
	if !d.StopOwed && d.W.Proc {
		d.add(RuntimeStop, 0, 0)
		if d.W.Refuses && !i.Displaced {
			d.StopRetry = true
			d.crash = d.fault(CrashAfterRuntime)
			return
		}
		if !d.W.Refuses {
			d.W.Proc = false
		}
		d.W.Attached = false
		d.W.Agent, d.W.Dedupe = 0, 0
		if i.Pause != 0 {
			d.H.Need[i.Pause] |= reasons(r)
		}
		if d.input.Fault == CrashAfterRuntime && d.fault(CrashAfterRuntime) {
			d.crash = true
			return
		}
	}
	d.StopOwed = true
	d.StopRetry = false
	r.Phase, r.Work = PhSusp, WkNone
	r.SuspendedBy = i.By
	r.LastStop = i
	r.Hold = Hold{}
	r.Intent = Intent{}
	r.Wake, r.DeathCharged, r.Relaunch = false, false, false
	if i.By == SrcDaemon && r.Task.PhaseTask {
		retire(&r, RPhaseEnd, 0)
	}
	if i.Pause != 0 {
		owed := i.Causes != 0 || r.Task.L != 0 || d.W.Pause == 0
		if r.Stops[i.Pause] < 2 {
			r.Stops[i.Pause] = 1
			if owed {
				r.Stops[i.Pause] = 2
			}
		}
	}
	d.M = r // Runtime no longer belongs to this claim, even on a write failure.
	d.write(StopWrite, r, d.input)
}
func (d *decision) finish() {
	if d.P.Kind != 0 || d.crash || d.W.Fatal.K != 0 || d.StopRetry {
		return
	}
	r := d.M
	due := r.Hold.due()
	if r.Hold.By != 0 && r.Intent.By == 0 && (!d.W.Proc || r.Phase == PhIdle && !r.waiting() || due != 0 && due <= d.Now) {
		d.intend(r, d.input, false)
	}
	if d.M.Intent.By != 0 && d.P.Kind == 0 && !d.crash {
		d.stop()
	}
}
func (d *decision) leave(e Event) {
	r := d.M
	if e.K == KDeath {
		d.W.Proc = false
		r.DeathCharged = true
	}
	if e.K == KExit {
		if !d.Root {
			invalidate(&r)
			retire(&r, RClaimEnd, 0)
			r.Phase, r.Work = PhEnded, WkNone
			r.Hold = Hold{}
			r.Intent = Intent{}
			d.W.Proc = false
			d.write(ClaimWrite, r, e)
			return
		}
		r.Hold = Hold{By: SrcExit}
	}
	if r.Hold.By != 0 {
		d.intend(r, e, e.K == KStale || e.K == KDeath)
		return
	}
	if r.waiting() || r.Task.Conf != 0 {
		take(&r)
	}
	invalidate(&r)
	r.Phase, r.Work = PhLaunch, WkNone
	r.Relaunch, r.Wake = false, false
	d.Prepared, d.Sending = 0, 0
	d.W.Agent, d.W.Dedupe = 0, 0
	d.W.Inc++
	d.W.Proc = true
	d.add(RuntimeReplace, 0, 0)
	d.write(ClaimWrite, r, e)
}
func (d *decision) restart(dead bool) {
	defer func() {
		if !d.StopOwed && !d.StopRetry {
			d.finish()
		}
	}()
	l := d.S.Task.L
	if d.S != d.P.Want && l != 0 && (d.P.Kind == EvidenceWrite || d.P.Kind == RetirementWrite) {
		d.H.Lost[l] = Unapplied
	}
	d.W.Attached = false
	d.Prepared, d.Sending = 0, 0
	d.Resend = false
	d.Restored = false
	d.P = Pending{}
	d.Deferred = Event{}
	d.StopRetry, d.StopOwed = false, false
	d.M = d.S
	if d.S.Phase == PhFailed {
		d.W.Fatal = Event{}
	}
	if dead {
		d.W.Proc = false
		d.W.Agent, d.W.Dedupe, d.W.N = 0, 0, 0
		d.W.Backlog = [3]Event{}
	}
	if d.W.Fatal.K != 0 {
		// Redeliver on the first decision (including KDrain). This also
		// permits store-down and a second restart before the retry.
		return
	}
	if d.M.Intent.By != 0 {
		if d.W.Proc && !d.M.Intent.Displaced {
			d.StopRetry = true
		} else {
			// A refused replacement and an absent process both relinquish
			// ownership. Neither permits a second runtime stop on restore.
			d.StopOwed = true
		}
		return
	}
	r := d.M
	if !r.live() {
		return
	}
	if !d.W.Proc && r.Hold.By != 0 {
		d.intend(r, Event{K: KRestart}, true)
		if d.P.Kind == 0 {
			d.StopOwed = true
		}
		return
	}
	if r.Task.L != 0 && (r.Task.Conf != 0 || r.outstanding() || r.Task.Unknown) {
		if !d.W.Proc {
			invalidate(&r)
			take(&r)
			r.Phase, r.Work = PhLaunch, WkNone
		} else {
			d.Restored = true
			d.H.Recovering |= 1 << l
			r.TurnBy = 0
			if r.RecoverBy == 0 {
				r.RecoverBy = after(d.Now, 10)
			}
			if r.Hold.By != 0 && r.Hold.Answer == 0 {
				r.Hold.Answer = r.RecoverBy
			}
		}
		d.write(ClaimWrite, r, Event{K: KRestart})
	}
}
