package holdmodel

var alphabet = Alphabet()
var shapes = func() map[Event]bool {
	m := map[Event]bool{}
	for _, e := range alphabet {
		m[e] = true
	}
	return m
}()

func plain(e Event) Event { e.At, e.Fault = 0, 0; return e }
func (s *State) canPrepare() bool {
	r := s.M
	if s.W.Fatal.K != 0 || s.StopRetry || s.StopOwed {
		return false
	}
	if s.Prepared != 0 || s.Sending != 0 || s.W.Alloc == NA || !s.W.Attached || !s.W.Proc || s.W.N != 0 || s.W.Pause != 0 || s.P.Kind != 0 || r.Phase != PhIdle || r.Hold.By != 0 || r.Task.L == 0 || r.RecoverBy != 0 || r.Task.Conf != 0 || r.Task.Unknown {
		return false
	}
	for _, a := range r.Att {
		if a.open() && a.Under == r.Task.ID && a.Stage != AtLost {
			return false
		}
	}
	return true
}
func (s *State) base(e Event) bool {
	r, w := &s.M, &s.W
	k := e.R
	switch e.K {
	case KRestart:
		return e.A == 0 || w.Proc
	case KStoreDown:
		return !w.Down
	case KStoreUp:
		return w.Down
	case KLoseReply:
		return !w.LoseReply
	case KRuntimeRefuses:
		return !w.Refuses
	case KRuntimeAccepts:
		return w.Refuses
	case KTimerWrite:
		return s.P.Kind != 0 && !s.StopOwed
	case KTimerStopWrite:
		return s.StopOwed
	case KTimerStopRetry:
		return s.StopRetry
	case KWrongToken:
		return true
	case KDetach:
		return w.Attached
	case KReconnect:
		return !w.Attached && w.Proc
	case KReplay:
		return w.Attached && w.N != 0 && s.Deferred.K == 0
	case KOverflow:
		return w.N != 0
	case KDrain:
		return s.Deferred.K != 0 || w.Fatal.K != 0
	case KFailEffects:
		return r.Notify
	case KPauseEnds:
		return w.Pause != 0 // the store's own event, whatever the daemon is doing
	case KTurnStart, KTurnEnd, KNamedStart, KRefusal, KResult:
		// The agent's frames wait at the shim while the daemon's deferred
		// slot is taken (M8): backpressure never refuses them at the source.
		return s.frame(e)
	case KReport:
		if e.A == RpFatal {
			return r.live() || r.Phase == PhFailed // fatal evidence waits at its source (M6)
		}
	}
	if s.Deferred.K != 0 && !(e.K == KStartedBy && s.StopOwed) {
		return false
	}
	switch e.K {
	case KPrepare:
		return s.canPrepare()
	case KRegister:
		return s.Prepared != 0 && s.P.Kind == 0 && r.Task.L != 0 && r.Phase == PhIdle && r.Hold.By == 0 && r.RecoverBy == 0 && w.Pause == 0 && w.Attached && w.Proc
	case KWire:
		return s.P.Kind == NoWrite && !s.Restored && w.Proc && w.Attached && s.Sending == k+1 && !w.Requests[k].Written && r.Att[k].open() && r.Att[k].Under == r.Task.ID
	case KOutcome:
		if k >= w.Alloc {
			return false
		}
		q := w.Requests[k]
		if q.Returned {
			return false
		}
		if e.A == OcNoFrame {
			return !q.Written && s.Sending == k+1
		}
		if !q.Written {
			return false
		}
		if (e.A == OcRefused || e.A == OcBusy) && (q.Answered || q.Ack || q.Root != k) {
			return false
		}
		return !q.Ack || e.A == OcAck
	case KCrashFrame:
		return w.Attached && w.N == 0 && s.P.Kind == 0 && s.frame(e)
	case KDeliver, KSupersede:
		return r.live() && (e.K == KDeliver || r.Task.L == 1)
	case KSuspend:
		return (r.live() || r.Phase == PhSusp) && (e.A != RqPauseRow && e.A != RqGate || w.Pause != 0)
	case KPauseBegins:
		return w.Pause == 0 && w.LastPause < 2
	case KResume:
		return r.Phase == PhSusp && !s.StopOwed && w.Pause == 0
	// A timer fires only at the earliest armed time (armed), so none passes
	// an earlier deadline.
	case KTimerHold:
		due := r.Hold.due()
		return r.Hold.By != 0 && !s.StopRetry && due != 0 && due <= s.armed()
	case KTimerRecov:
		return r.RecoverBy != 0 && r.RecoverBy <= s.armed()
	case KTimerTurn:
		return r.TurnBy != 0 && r.RecoverBy == 0 && r.TurnBy <= s.armed()
	case KRegDeadline:
		return r.Phase == PhLaunch && r.Hold.By == SrcOperator
	case KRelaunchAgain:
		return r.Phase == PhLaunch && r.Hold.By != 0
	case KHello:
		return w.Attached && r.live()
	case KProbeIdle:
		return r.live() && (r.Work == WkBusy || s.Restored)
	case KCompact:
		return r.live() && !r.compact()
	case KCompactEnd:
		return r.compact()
	case KParkWake:
		return r.live() && !r.Wake
	case KWake:
		return r.Wake
	case KDeath:
		return r.live() && w.Proc
	case KExit, KStale, KReady:
		return r.live()
	case KStartedBy:
		return r.live() || s.StopOwed
	case KReport:
		return r.live() || r.Phase == PhFailed
	}
	return false
}

// frame is whether the agent can send one of its frames now, wherever the
// shim then holds it.
func (s *State) frame(e Event) bool {
	r, w := &s.M, &s.W
	k := e.R
	switch e.K {
	case KTurnStart:
		return w.Proc && w.Agent == 0 && (k == Foreign || w.Requests[k].Written && w.Requests[k].Root == k && !w.Requests[k].Answered && w.Requests[k].Inc == w.Inc)
	case KTurnEnd:
		return w.Agent != 0
	case KNamedStart:
		q := w.Requests[k]
		return w.Proc && q.Written && q.Inc == w.Inc && q.Root != k && w.Requests[q.Root].Started
	}
	q := w.Requests[k]
	if !q.Written {
		return false
	}
	if e.K == KRefusal && q.Root != k && w.Requests[q.Root].Answer != ARefLate {
		return false // only a root's negative response fans out
	}
	if e.K != KRefusal && q.Root != k {
		return false
	}
	if q.Answered {
		return e.K == KRefusal && q.Answer == ARefLate || e.K != KRefusal && q.Answer == e.A && !r.Att[k].open()
	}
	return true
}
