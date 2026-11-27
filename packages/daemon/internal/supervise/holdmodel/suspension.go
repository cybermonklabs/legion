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
	switch e.A {
	case RqGate:
		pause = d.W.Pause
	case RqPauseRow:
		pause = d.W.Pause
	}
	due := requestDue(e.A, d.Now)
	replace := h.By == 0 || by == SrcOperator || h.By == SrcReleased && h.Pause == 0 && pause != 0
	if r.Phase == PhLaunch && h.By != SrcOperator {
		replace = by != SrcReleased || h.By == 0 || h.By == SrcReleased && (pause != 0 || h.Pause == 0)
	}
	if replace {
		h.By, h.Pause = by, pause
	}
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
	if d.intentDropsPause {
		i.Pause = 0
	}
	if r.unresolved() {
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
		// A refused stop of a process this claim still owns did not land, so
		// its pause owes no launch yet: the retry's own stop records that.
		landed := !d.W.Refuses || i.Displaced
		var causes uint8
		if landed && i.Pause != 0 {
			causes = launchCauses(&r)
		}
		d.put(Effect{Action: RuntimeStop, Pause: i.Pause, Open: d.W.Pause, Causes: causes})
		if !landed {
			d.StopRetry = true
			d.crash = d.fault(CrashAfterRuntime)
			return
		}
		if !d.W.Refuses {
			d.W.Proc = false
		}
		d.W.Attached = false
		d.W.Agent, d.W.Dedupe = 0, 0
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

// dropEnded is D1's second step and E's rule: a hold, or a pre-stop intent, of
// a pause that has ended is dropped before anything can stop the claim for it.
// It runs at the start of every decision, a repair's and a drain's included,
// and at a restart (endedPause). A runtime stop that already landed (StopOwed)
// is recorded as it was asked. It reports whether it wrote.
func (d *decision) dropEnded() bool {
	r := d.M
	if !d.endedPause(&r) {
		return false
	}
	d.write(ClaimWrite, r, Event{K: KPauseEnds})
	return true
}
func (d *decision) endedPause(r *Row) bool {
	// Hold provenance is independent of an intent's mutable holder tag. A
	// takeover cannot make the pause that originally held the process vanish.
	p := r.Hold.Pause
	if p == 0 && r.Intent.By != 0 {
		p = r.Intent.Pause
	}
	if d.keepEnded || d.StopOwed || !r.live() || p == 0 || p == d.W.Pause {
		return false
	}
	displaced := r.Intent.By != 0 && r.Intent.Displaced
	r.Hold, r.Intent = Hold{}, Intent{}
	if displaced {
		// The process is gone or must be replaced, and no hold stops the claim
		// any more: it is relaunched as a let-go with no hold is (leave).
		d.relaunch(r)
	}
	return true
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
	d.relaunch(&r)
	d.write(ClaimWrite, r, e)
}

// relaunch is a let-go with no hold: the task goes back unread and a new
// process replaces the old one.
func (d *decision) relaunch(r *Row) {
	if r.unresolved() {
		take(r)
	}
	invalidate(r)
	r.Phase, r.Work = PhLaunch, WkNone
	r.Relaunch, r.Wake = false, false
	d.Prepared, d.Sending = 0, 0
	d.W.Agent, d.W.Dedupe = 0, 0
	d.W.Inc++
	d.W.Proc = true
	d.add(RuntimeReplace, 0, 0)
}

// carriesRequest is whether an accepted event names the request it answers.
func carriesRequest(e Event) bool {
	return e.K == KResult || e.K == KRefusal || e.K == KOutcome
}

// forgetMemory projects the stored row into a new daemon's memory. Every mark
// the product keeps in memory only goes: holds and their timers
// (supervise/suspend.go), the parked wake (machine.go's Machine.wake), the
// stale-plugin relaunch (Machine.staleRelaunch) and relaunchLater's retry
// (Machine.relaunch). A caller, or the boot's own pass, must ask again; that
// ask is its own event (KParkWake, KStale), not a field that survives.
func forgetMemory(r *Row) {
	r.Hold = Hold{}
	r.TurnBy = 0
	for k := range r.Att {
		r.Att[k].TurnBy = 0
	}
	r.Wake, r.Relaunch = false, false
}
func (d *decision) restart(dead bool) {
	defer func() {
		if !d.StopOwed && !d.StopRetry {
			d.finish()
		}
	}()
	if c := d.P.Cause; d.S != d.P.Want && d.S.Task.L != 0 && (d.P.Kind == EvidenceWrite || d.P.Kind == RetirementWrite) && carriesRequest(c) {
		d.lose(d.W.Requests[c.R].Root, Unapplied)
	}
	d.W.Attached = false
	d.Prepared, d.Sending = 0, 0
	d.Resend = false
	d.Restored = false
	d.P = Pending{}
	d.Deferred = Event{}
	d.StopRetry, d.StopOwed = false, false
	// The old daemon's send goroutines died with it: none of their callbacks
	// reaches the new daemon, or the shim's record of the request (M8).
	for k := uint8(0); k < d.W.Alloc; k++ {
		d.W.Requests[k].Returned = true
	}
	// A restart rebuilds the machine from the persisted delivery and the
	// world. The hold, its timers, wakes and relaunches do not survive.
	forgetMemory(&d.S)
	d.M = d.S
	if d.load != nil {
		d.M = d.load(d.M)
	}
	d.Booted = true
	if d.S.Phase == PhFailed {
		d.W.Fatal = Event{}
		d.W.FatalStuck = false
		d.FatalSeen = false
	}
	if dead {
		d.W.Proc = false
		d.W.Agent, d.W.Dedupe, d.W.N = 0, 0, 0
		d.W.Backlog = [3]Event{}
	}
	// The rebuilt row's own doubt (D10), reported for the driver's ledger.
	var doubted uint8
	if s, l := d.S, d.S.Task.L; l != 0 && d.W.Proc && s.live() && (s.Task.Conf != 0 || s.outstanding() || s.Task.Unknown) {
		doubted = l
	}
	d.put(Effect{Action: Rebuild, Task: doubted})
	if d.W.Fatal.K != 0 {
		// Redeliver on the first decision (including KDrain). This also
		// permits store-down and a second restart before the retry. The
		// rebuilt daemon receives the item again, with no pending write.
		if d.FatalSeen {
			d.judgeReceipt()
		}
		return
	}
	r := d.M
	// M7: the intent is committed, and whether its runtime stop landed is the
	// runtime's to say. With the process gone it landed, or none was needed,
	// so only the stop's write is owed, whatever has happened to the pause.
	if r.Intent.By != 0 && !d.W.Proc {
		d.StopOwed = true
		return
	}
	// Holds are gone until a source makes a new request. A committed intent is
	// distinct: it is already a durable stop fact and is reconciled below.
	if r.Intent.By != 0 {
		// M7: a process that is still there has not stopped, displaced or
		// not, so its stop is asked again.
		d.StopRetry = true
		return
	}
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
	doubt := r.Task.L != 0 && (r.Task.Conf != 0 || r.outstanding() || r.Task.Unknown)
	switch {
	case !d.W.Proc:
		// RT3: no process is left to answer, so the task goes back unread, and
		// the claim, which has no process, launches.
		if doubt {
			invalidate(&r)
			take(&r)
		}
		r.Phase, r.Work = PhLaunch, WkNone
	case doubt:
		d.Restored = true
		r.TurnBy = 0
		if r.RecoverBy == 0 {
			r.RecoverBy = after(d.Now, 10)
		}
		if r.Hold.By != 0 && r.Hold.Answer == 0 {
			r.Hold.Answer = r.RecoverBy
		}
	}
	if r != d.M {
		d.write(ClaimWrite, r, Event{K: KRestart})
	}
}
