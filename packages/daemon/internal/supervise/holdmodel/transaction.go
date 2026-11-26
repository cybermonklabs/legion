package holdmodel

const (
	FailWrite uint8 = iota + 1
	LostReply
	CrashBeforeWrite
	CrashAfterCommit
	CrashAfterRuntime
)

type Action uint8

const (
	WriteAttempt Action = iota + 1
	Commit
	ReadBack
	RuntimeStop
	RuntimeReplace
	Send
	Forward
	Deduplicate
	Notify
)

type Effect struct {
	Action  Action
	Write   Write
	Request uint8
}
type Effects struct {
	List      [16]Effect
	N, Writes uint8
	Cut       bool
}

func (x *Effects) add(a Action, w Write, k uint8) { x.List[x.N] = Effect{a, w, k}; x.N++ }

type decision struct {
	State
	Effects
	input Event
	crash bool
	store func(Write, Row) Row // nil for the specified store; test variants substitute one write
}

func (d *decision) fault(f uint8) bool { return d.input.Fault == f && d.input.At == d.Writes }

// write is the only store door. Commit is observable even when the caller
// crashes or receives an error. The reply carries the full locked row (M9).
func (d *decision) write(w Write, r Row, cause Event) bool {
	if r.Intent.By != 0 {
		// A wake/task can arrive during a refused runtime stop. Do not
		// discard the pre-refusal causes when incorporating the new work.
		r.Intent.Causes |= reasons(r)
	}
	if r.Hold.By != 0 && r.waiting() && r.Hold.Answer == 0 {
		r.Hold.Answer = after(d.Now, 10)
	}
	if !r.waiting() {
		r.Hold.Answer = 0
	}
	if r.Hold.By != 0 && !r.waiting() && r.Hold.Deadline == 0 && r.Hold.Watch == 0 {
		r.Hold.Watch = after(d.Now, 10)
	}
	cause.Fault, cause.At = 0, 0
	d.P = Pending{Kind: w, Want: r, Cause: cause}
	return d.flush()
}
func (d *decision) flush() bool {
	p := d.P
	if p.Uncertain {
		d.add(ReadBack, p.Kind, 0)
		if d.W.Down {
			return false
		}
		d.install()
		return true
	}
	d.Writes++
	d.add(WriteAttempt, p.Kind, 0)
	if d.fault(CrashBeforeWrite) {
		d.crash = true
		return false
	}
	if d.W.Down || d.fault(FailWrite) {
		return false
	}
	prev := d.S
	d.S = p.Want
	if d.store != nil {
		d.S = d.store(p.Kind, d.S)
	}
	d.add(Commit, p.Kind, 0)
	d.observeCommit(prev, p)
	d.H.Bad |= checkCommit(d.State, prev, p)
	if d.fault(CrashAfterCommit) {
		d.crash = true
		return false
	}
	if d.W.LoseReply || d.fault(LostReply) {
		d.W.LoseReply = false
		d.P.Uncertain = true
		return false
	}
	d.install()
	return true
}
func (d *decision) install() {
	p := d.P
	d.M = d.S
	d.P = Pending{}
	if d.M.RecoverBy == 0 {
		d.Restored = false
	}
	if p.Kind == RegisterWrite && d.Prepared != 0 {
		d.Sending = d.Prepared
		d.Prepared = 0
	}
	if p.Kind == StopWrite {
		d.StopOwed = false
		d.StopRetry = false
	}
	if !d.M.live() {
		d.Prepared, d.Sending = 0, 0
		d.Restored = false
	}
	if d.M.Intent.By == 0 {
		d.StopRetry = false
	}
	if d.M.Phase == PhFailed {
		d.W.Fatal = Event{}
	}
}

// Observe the durable event authorizing a new admission, independently of
// the send decision. A negative answer cannot license a retry of another root.
func (d *decision) observeCommit(prev Row, p Pending) {
	r := d.S
	l := prev.Task.L
	if l == 0 {
		return
	}
	if r.Task.L != l || !r.live() {
		d.H.Recovering &^= 1 << l
	}
	if prev.Task.ID == r.Task.ID || r.Task.L != l {
		return
	}
	last := d.H.LastRoot[l]
	if last == 0 {
		// A legacy row or a registered-but-never-written request has no
		// admission to classify. The recovery deadline still releases it.
		if p.Cause.K == KTimerRecov || !d.W.Proc || p.Kind == IntentWrite {
			d.H.Recovering &^= 1 << l
		}
		return
	}
	k := last - 1
	q := d.W.Requests[k]
	var why Reason
	switch {
	case !d.W.Proc || p.Cause.K == KDeath || p.Cause.K == KRestart && r.Phase == PhLaunch:
		why = NoProcess
	case p.Cause.K == KTimerRecov || p.Kind == IntentWrite && prev.Hold.due() != 0 && prev.Hold.due() <= d.Now:
		why = Expired
		if d.H.Lost[l] != Original {
			why = d.H.Lost[l]
		} else if q.Written && prev.Att[k].Stage == AtLost {
			why = LostSend
		}
	case q.Answered && q.Answer != AOK && (p.Kind == EvidenceWrite || p.Kind == RetirementWrite):
		why = Negative
	case p.Cause.K == KTimerTurn || p.Cause.K == KCompact || p.Cause.K == KStale || p.Cause.K == KWake || p.Cause.K == KExit || p.Cause.K == KRegDeadline || p.Cause.K == KRelaunchAgain:
		// A written prompt was cut off before its outcome was known.
		why = LostSend
	}
	if why != Original {
		d.H.Certified[k] = why
		d.H.Recovering &^= 1 << l
	}
}

const (
	I1 uint8 = 1 << iota
	I2
	I3
	Twice
)

func checkServed(r Row, h History) uint8 {
	for l, reason := range r.Ret {
		if reason == RServed {
			k := r.ServedBy[l]
			if k == 0 || k > NA || h.OK[l]&(1<<(k-1)) == 0 {
				return I1
			}
		}
	}
	return 0
}
func checkCommit(s State, prev Row, p Pending) uint8 {
	bad := checkServed(s.S, s.H)
	if r := s.S; r.Hold.By != 0 && (r.Hold.due() == 0 || r.waiting() && r.Hold.Answer == 0) {
		bad |= I2
	}
	if l := prev.Task.L; l != 0 && s.S.Task.L != l {
		switch s.S.Ret[l] {
		case RServed: // checked against independent result history above
		case RReplaced:
			if prev.RecoverBy != 0 || prev.Task.Conf != 0 && prev.waiting() {
				bad |= I1
			}
		case RPhaseEnd:
			if s.S.SuspendedBy != SrcDaemon || !prev.Task.PhaseTask {
				bad |= I1
			}
		case RClaimEnd:
			if s.Root || s.S.Phase != PhEnded {
				bad |= I1
			}
		default:
			bad |= I1 // deleted without any authorized retirement
		}
	}
	if p.Kind == IntentWrite && s.S.Intent.Pause != 0 {
		want := prev.Intent.Causes | reasons(prev)
		if p.Cause.K == KDeath {
			want |= DeathLaunch
		}
		if s.S.Intent.Causes != want {
			bad |= I3
		}
	}
	if s.S.Phase == PhSusp && prev.Phase != PhSusp && p.Kind != StopWrite {
		bad |= I3
	}
	if p.Kind == StopWrite {
		r := s.S
		i := prev.Intent
		if i.By == 0 || r.LastStop.By != i.By || r.LastStop.Pause != i.Pause || r.Phase != PhSusp || r.Task != p.Want.Task || r.Att != p.Want.Att || r.Meta != p.Want.Meta || r.StartRow != p.Want.StartRow || r.Task.Conf != 0 {
			bad |= I3
		}
		if prev.Task != r.Task && !(prev.Task.PhaseTask && i.By == SrcDaemon && r.Ret[prev.Task.L] == RPhaseEnd) || prev.Att != r.Att || prev.Meta != r.Meta {
			bad |= I3
		}
		owed := i.Causes != 0 || r.Task.L != 0 || s.W.Pause == 0 || prev.Stops[i.Pause] == 2
		if i.Pause != 0 && (r.Stops[i.Pause] == 0 || (r.Stops[i.Pause] == 2) != owed || r.LastStop.Causes != i.Causes) {
			bad |= I3
		}
	}
	return bad
}

// Check compares control state with the timers that can actually run. Transient
// due states inside a decision are checked at its return, not mid-transaction.
func Check(s State) uint8 {
	bad := s.H.Bad | checkServed(s.M, s.H) | checkServed(s.S, s.H)
	r := s.M
	repair := s.P.Kind != 0 || s.StopRetry || s.StopOwed || s.W.Fatal.K != 0 || s.Deferred.K != 0
	if r.Hold.By != 0 && !repair {
		if r.Phase == PhIdle && !r.waiting() || r.Hold.due() <= s.Now || r.waiting() && r.Hold.Answer == 0 || !r.live() {
			bad |= I2
		}
	}
	if s.Restored && r.Task.L != 0 && (r.waiting() || r.Task.Unknown || r.Task.Conf != 0) && r.RecoverBy == 0 && !repair {
		bad |= I2
	}
	if s.StopOwed && s.S.Intent.By == 0 && s.S.Phase != PhSusp || s.StopRetry && s.M.Intent.By == 0 {
		bad |= I3
	}
	for id, n := range s.H.Need {
		if n != 0 && s.S.Stops[id] != 2 && !(s.S.Intent.Pause == uint8(id) && s.S.Intent.Causes != 0) {
			bad |= I3
		}
	}
	return bad
}
