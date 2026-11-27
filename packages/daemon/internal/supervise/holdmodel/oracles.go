package holdmodel

// The oracles. Each reads the model's committed state, its world and the
// driver's ledger (History), and never decides anything the model then uses:
// nothing in this file is reachable from a decision, which carries a Machine.

const (
	I1 uint16 = 1 << iota
	I2
	I3
	Twice
	Late          // a timer fired past an earlier recorded deadline (O1, D2)
	Ended         // a runtime stop ran for a pause that had ended (E, D1)
	BootMemory    // restart restored a memory-only field without a new request
	RecoveryFence // recovery replaced, let go, or invalidated before an allowed cause
	FatalReceipt  // fatal evidence taken from the shim was lost before failure committed
	Settled       // drain, repair or an armed timer left work no own step can make
	AttemptBound  // an open attempt waits without its own turn or answer bound
	FatalClose    // committed fatal failure did not preserve its required outcome
	RT1           // an answer the old daemon's socket took reached this daemon again
	lastInvariant = RT1
)

// turnedOpen is whether an attempt the driver's ledger saw a turn credited
// through still waits for its answer. A lost attempt's own credit went with
// its loss (D3); another prompt's loss leaves it standing.
func turnedOpen(r *Row, turned uint8) bool {
	for k, a := range r.Att {
		if a.open() && a.Stage != AtLost && turned&(1<<k) != 0 {
			return true
		}
	}
	return false
}

// creditPromptOpen is the driver's turn-credit ledger applied to the shim's
// prompt roots. A loss of the alias that reported the turn does not erase that
// credit while another alias of the forwarded prompt is still open.
func creditPromptOpen(s *State, l uint8) bool {
	for k := uint8(0); k < NA; k++ {
		if s.H.Turned[l]&(1<<k) == 0 {
			continue
		}
		root := s.W.Requests[k].Root
		for j, a := range s.M.Att {
			if j != int(k) && a.open() && a.L == l && s.W.Requests[j].Written && s.W.Requests[j].Root == root {
				return true
			}
		}
	}
	return false
}

// bootMemoryBad is the restart projection rule. A boot starts from the
// committed delivery and the world: the whole memory row must be the stored
// row with every memory-only field forgotten, and no field is listed here, so
// none can slip past. An intent is persisted separately, so its stop progress
// is derived from the world.
func bootMemoryBad(s *State) bool {
	if !s.Booted {
		return false
	}
	want := s.M
	forgetMemory(&want)
	if s.M != want {
		return true // a memory-only field the boot should have forgotten
	}
	if s.P.Kind == 0 {
		// With no write pending, memory is exactly the stored row rebuilt.
		stored := s.S
		forgetMemory(&stored)
		if s.M != stored {
			return true
		}
	}
	if s.Prepared != 0 || s.Sending != 0 || s.Resend || s.Deferred != (Event{}) {
		return true
	}
	if s.W.Fatal.K != 0 {
		return false // fatal evidence decides before a stored intent is reconciled
	}
	return s.StopRetry != (s.S.Intent.By != 0 && s.W.Proc) ||
		s.StopOwed != (s.S.Intent.By != 0 && !s.W.Proc)
}

func recoveryMutation(before, after *Machine, fx *Effects) bool {
	// Recovery is open in the committed row. Memory lags it only behind a
	// lost reply, and the step that committed that write was judged then.
	if before.S.RecoverBy == 0 || !before.W.Proc || before.S.Task.L == 0 {
		return false
	}
	for _, f := range fx.List[:fx.N] {
		if f.Action == RuntimeReplace {
			return true
		}
	}
	if !after.W.Proc {
		return true
	}
	for k, a := range before.S.Att {
		// The fence judges the commit that invalidates an open attempt: not a
		// failed write's Want, and not a later read-back that installs a
		// commit an earlier step made (that step was judged when it committed).
		if a.open() && after.S.Att[k].Stage == AtInvalid {
			return true
		}
	}
	return false
}

// appliedInput is the event a step decides on: the driver's input, or, for a
// drain or a backlog replay, the item the driver delivered earlier and the
// step now consumes.
func appliedInput(before *Machine, e Event) Event {
	switch e.K {
	case KDrain:
		if before.W.Fatal.K != 0 {
			return before.W.Fatal
		}
		return before.Deferred
	case KReplay:
		if before.W.N != 0 {
			return before.W.Backlog[0]
		}
	}
	return e
}

// recoveryAuthorized names D10's causes: its deciding answer, its answer
// bound, no live process, or a stop that cannot wait (which records an intent
// or calls the runtime rather than replacing it). A drain or a replay is
// judged by the item it applies, never by its own kind.
func recoveryAuthorized(before *Machine, input Event, after *Machine, fx *Effects) bool {
	if !before.W.Proc || after.M.Phase == PhEnded || after.S.Phase == PhEnded {
		return true // D10: no live process remains
	}
	e := appliedInput(before, input)
	switch e.K {
	case KResult, KRefusal:
		return true // D10: the restored prompt's deciding answer
	case KTimerRecov:
		return true // D10: the answer bound
	case KDeath, KExit:
		return true // D10: the process ended
	case KRestart:
		return e.A != 0 // D10: process-ended restart
	}
	if before.P.Cause.K == KResult || before.P.Cause.K == KRefusal || before.P.Cause.K == KTimerRecov || before.P.Cause.K == KDeath || before.P.Cause.K == KExit {
		return true // a repair is installing one of those three D10 causes
	}
	if after.M.Intent.By != 0 || after.P.Kind == IntentWrite && after.P.Want.Intent.By != 0 {
		return true // D10: a stop that cannot wait is durably recorded
	}
	for _, f := range fx.List[:fx.N] {
		if f.Action == RuntimeStop {
			return true // D10: that non-waitable stop reached the runtime
		}
		if f.Action == Commit && f.Write == IntentWrite {
			// D10: this step committed a stop that cannot wait, which took
			// the restored task back, even if a later decision in the same
			// step (a StartedBy release) then cleared the intent.
			return true
		}
	}
	return false
}

// attemptBoundBad checks the deadline an acknowledgement arms for its own
// prompt. A task may keep an earlier prompt's confirmation while a separate
// resend waits for a turn, so this is intentionally per request. The bound is
// owed unless the driver's ledger of starts delivered to the daemon
// (History.Turned) records this request's own turn: the shim's record that
// the agent started is not one the daemon read. An open attempt under a hold
// shares that hold's answer bound. The row's turn bound is the earliest bound
// of an open attempt: one left armed after its attempts closed or were
// invalidated takes the task back for a prompt nobody awaits.
func attemptBoundBad(s *State) bool {
	r := s.M
	if s.P.Kind != 0 {
		r = s.P.Want // a failed repair already computed and armed its bounds
	}
	var bound uint16
	for _, a := range r.Att {
		if a.open() {
			bound = earlier(bound, a.TurnBy)
		}
	}
	if r.TurnBy != bound {
		return true
	}
	if s.H.FatalPending || s.W.Fatal.K != 0 {
		return false // fatal evidence decides before an attempt can wait again
	}
	for k, a := range r.Att {
		if !a.open() || a.L != r.Task.L {
			continue
		}
		if a.Stage == AtAcked && a.Under == r.Task.ID && r.RecoverBy == 0 &&
			s.W.Requests[k].Root == uint8(k) && s.H.Started&(1<<k) == 0 {
			if a.TurnBy == 0 || r.TurnBy == 0 || r.TurnBy > a.TurnBy {
				return true
			}
		}
		if r.Hold.By != 0 && r.waiting() && r.Hold.Answer == 0 {
			return true
		}
	}
	return false
}

// settleOne is the model's own immediate maintenance loop: repair first, then
// a fatal redelivery, an owed stop write, a refused runtime retry, and a timer
// already due at Now. It deliberately does not invent an external frame.
func settleOne(s State) (State, bool) {
	var e Event
	switch {
	case s.W.Fatal.K != 0 || s.Deferred.K != 0:
		e.K = KDrain
	case s.StopOwed:
		e.K = KTimerStopWrite
	case s.P.Kind != 0:
		e.K = KTimerWrite
	case s.StopRetry:
		e.K = KTimerStopRetry
	case s.M.Hold.By != 0 && !s.StopRetry && s.M.Hold.due() != 0 && s.M.Hold.due() <= s.Now:
		e.K = KTimerHold
	case s.M.RecoverBy != 0 && s.M.RecoverBy <= s.Now:
		e.K = KTimerRecov
	case s.M.TurnBy != 0 && s.M.TurnBy <= s.Now:
		e.K = KTimerTurn
	default:
		return s, false
	}
	if !Enabled(s, e) {
		return s, false
	}
	t, _ := Step(s, e)
	return t, t != s
}

// settleBad reaches the fixed point of the model's own maintenance steps.
// A no-progress store or runtime is explicit in World; otherwise a received
// fatal, an owed write or a due hold has no excuse to remain.
func settleBad(s *State) bool {
	t := *s
	for range 16 {
		next, changed := settleOne(t)
		if !changed {
			break
		}
		t = next
	}
	if t.W.Down {
		return false
	}
	if t.H.FatalPending || t.W.Fatal.K != 0 {
		return true
	}
	if !t.W.Down && (t.P.Kind != 0 || t.StopOwed) {
		return true
	}
	if !t.W.Refuses && t.StopRetry {
		return true
	}
	return !t.W.Down && t.M.Hold.By != 0 && !t.StopRetry && t.M.Hold.due() != 0 && t.M.Hold.due() <= t.Now
}

// receiptOf is the driver's own reading of the task a received fatal item
// names, and of whether it names an open attempt of that task: the deciding
// machine when the item arrived, or the rebuilt machine at the restart that
// judges it again. The close is judged against this, never against the
// Receipt the daemon itself kept.
func receiptOf(m *Machine) Receipt {
	r := m.M
	retiring := m.P.Kind != 0 && r.Task.L != 0 && m.P.Want.Ret[r.Task.L] != 0
	return Receipt{L: r.Task.L, ID: r.Task.ID,
		Open: r.Task.L != 0 && r.outstanding() && m.S.Ret[r.Task.L] == 0 && !retiring}
}

// fatalClose is the receipt-ledger classifier. The normal close and a bad
// close both decide recovery before the recovery fence observes its delta.
func fatalClose(before, after History) bool {
	return after.FatalClosed > before.FatalClosed && after.FatalOpen && after.FatalL != 0
}

// fatalCloseBad checks a classified close's result. Its task and pause-stop
// checks read the close step's delta: an older committed pause stop is not a
// violation of this close.
func fatalCloseBad(before, after *Machine, h *History, fx *Effects) bool {
	l, id := h.FatalL, h.FatalID
	if after.S.Phase != PhFailed || after.S.RecoverBy != 0 {
		return true
	}
	if after.S.Task.L != l || after.S.Task.ID != id || after.S.Task.Conf != 0 || after.S.Ret[l] != 0 {
		return true
	}
	for pause, stops := range after.S.Stops {
		if stops != before.S.Stops[pause] {
			return true
		}
	}
	for _, f := range fx.List[:fx.N] {
		if f.Action == RuntimeReplace || f.Action == Send || f.Action == Forward {
			return true
		}
	}
	return false
}

func checkServed(r *Row, h *History) uint16 {
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

// launchCauses is the oracles' own reading of a row's launch causes (the
// snapshot a pause's runtime stop takes, and the causes an intent must carry),
// kept apart from the transitions' reasons so a defect there cannot hide.
func launchCauses(r *Row) uint8 {
	var causes uint8
	if r.Wake {
		causes |= WakeLaunch
	}
	if r.Hold.Launch {
		causes |= HeldLaunch
	}
	if r.DeathCharged {
		causes |= DeathLaunch
	}
	if r.Relaunch {
		causes |= RelaunchLaunch
	}
	if r.Task.L != 0 {
		causes |= TaskLaunch
	}
	return causes
}

// checkCommit judges one committed write against the ledger as it stood
// before that commit.
func checkCommit(h *History, root bool, c *Written) uint16 {
	r, prev := &c.Row, &c.Prev
	bad := checkServed(r, h)
	if r.Hold.By != 0 && r.Intent.By == 0 && (r.Hold.due() == 0 || r.waiting() && r.Hold.Answer == 0) {
		bad |= I2
	}
	// The answer bound lasts only while an answer is awaited: a stale one
	// stops a claim whose wait has ended at a bound it no longer has.
	if !r.waiting() && r.Hold.Answer != 0 {
		bad |= I2
	}
	if l := prev.Task.L; l != 0 && r.Task.L != l {
		switch r.Ret[l] {
		case RServed: // checked against independent result history above
		case RReplaced:
			// D5 against the oracle's own history: neither recovery doubt nor
			// a confirmed task with an answer to come is replaced.
			if prev.RecoverBy != 0 || h.Recovering&(1<<l) != 0 || prev.Task.Conf != 0 && prev.waiting() || turnedOpen(prev, h.Turned[l]) {
				bad |= I1
			}
		case RPhaseEnd:
			if r.SuspendedBy != SrcDaemon || !prev.Task.PhaseTask {
				bad |= I1
			}
		case RClaimEnd:
			if root || r.Phase != PhEnded {
				bad |= I1
			}
		default:
			bad |= I1 // deleted without any authorized retirement
		}
	}
	if c.Kind == IntentWrite && r.Intent.Pause != 0 {
		want := prev.Intent.Causes | launchCauses(prev)
		if c.Cause.K == KDeath {
			want |= DeathLaunch
		}
		if r.Intent.Causes != want {
			bad |= I3
		}
	}
	if r.Phase == PhSusp && prev.Phase != PhSusp && c.Kind != StopWrite {
		bad |= I3
	}
	if c.Kind == StopWrite {
		i := prev.Intent
		if i.By == 0 || r.LastStop.By != i.By || r.LastStop.Pause != i.Pause || r.Phase != PhSusp || r.Task != c.Want.Task || r.Att != c.Want.Att || r.Meta != c.Want.Meta || r.StartRow != c.Want.StartRow || r.Task.Conf != 0 {
			bad |= I3
		}
		if prev.Task != r.Task && !(prev.Task.PhaseTask && i.By == SrcDaemon && r.Ret[prev.Task.L] == RPhaseEnd) || prev.Att != r.Att || prev.Meta != r.Meta {
			bad |= I3
		}
		owed := i.Causes != 0 || r.Task.L != 0 || c.Pause == 0 || prev.Stops[i.Pause] == 2
		if i.Pause != 0 && (r.Stops[i.Pause] == 0 || (r.Stops[i.Pause] == 2) != owed || r.LastStop.Causes != i.Causes) {
			bad |= I3
		}
	}
	return bad
}

// recordedDue is the earliest deadline the driver wrote down when its own
// event happened — a suspension request's, an acknowledgement's wait for a
// turn, a restart's recovery bound — for a bound the machine still carries.
// It reads no deadline a transition wrote, so a transition that forgets one
// cannot hide that a later timer passed it.
func recordedDue(s *State) uint16 {
	var t uint16
	if s.M.RecoverBy != 0 {
		t = earlier(t, s.H.RecovDue)
	}
	for k, a := range s.M.Att {
		if a.TurnBy != 0 {
			t = earlier(t, s.H.TurnDue[k])
		}
	}
	if s.M.Hold.By != 0 && !s.StopRetry {
		t = earlier(t, s.H.HoldDue)
	}
	return t
}

// Check compares control state with the timers that can actually run. Transient
// due states inside a decision are checked at its return, not mid-transaction.
func Check(s State) uint16 {
	bad := s.H.Bad | checkServed(&s.M, &s.H) | checkServed(&s.S, &s.H)
	if l := s.M.Task.L; l != 0 && s.M.Task.Conf == 0 &&
		s.S.Task.L == l && s.S.Task.Conf == 0 && creditPromptOpen(&s, l) {
		bad |= I1
	}
	if bootMemoryBad(&s) {
		bad |= BootMemory
	}
	if s.S.Intent.By != 0 && s.S.Hold.Pause != 0 && s.S.Intent.Pause != s.S.Hold.Pause {
		bad |= Ended
	}
	if s.H.FatalPending && s.W.Fatal.K == 0 && s.M.Phase != PhFailed && s.S.Phase != PhFailed {
		bad |= FatalReceipt
	}
	if attemptBoundBad(&s) {
		bad |= AttemptBound
	}
	if settleBad(&s) {
		bad |= Settled
	}
	r := s.M
	repair := s.P.Kind != 0 || s.StopRetry || s.StopOwed || s.W.Fatal.K != 0
	if r.Hold.By != 0 && !repair {
		if r.Phase == PhIdle && !r.waiting() || r.Hold.due() <= s.Now || r.waiting() && r.Hold.Answer == 0 || !r.waiting() && r.Hold.Answer != 0 || !r.live() {
			bad |= I2
		}
	}
	if s.Restored && r.Task.L != 0 && (r.waiting() || r.Task.Unknown || r.Task.Conf != 0) && r.RecoverBy == 0 && !repair {
		bad |= I2
	}
	if s.StopOwed && s.S.Intent.By == 0 && s.S.Phase != PhSusp || s.StopRetry && s.M.Intent.By == 0 {
		bad |= I3
	}
	// The launch a pause's runtime stop snapshot owes survives until it is
	// recorded: as that pause's owed stop, in a stored intent that carries the
	// snapshot's causes, or, after a holder's takeover (D13), in the stop the
	// new holder recorded with those causes. A claim that has ended or failed
	// is never relaunched, and a live claim that no intent is stopping and that
	// has a process or is launching one (a pause that ended before the stop,
	// then a relaunch) has had its launch.
	for id, n := range s.H.Need {
		r := s.S
		carried := n == 0 || r.Stops[id] == 2 ||
			r.Intent.By != 0 && r.Intent.Causes&n == n ||
			r.Phase == PhSusp && r.LastStop.Pause != uint8(id) && r.LastStop.Causes&n == n ||
			r.Phase == PhEnded || r.Phase == PhFailed || r.Intent.By == 0 && r.live() && (s.W.Proc || r.Phase == PhLaunch)
		if !carried {
			bad |= I3
		}
	}
	return bad
}
