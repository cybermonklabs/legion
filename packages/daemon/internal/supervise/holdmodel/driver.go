package holdmodel

// The driver. It runs one decision at a time, keeps its own ledger from what
// that decision did, and explores the machine. Nothing here is reachable from
// a decision: a decision carries a Machine, and History lives only out here.

// History is the driver's own record of the run. Every field is written here,
// from the events the driver applied, the effects the decision reported, the
// writes it committed and the world it left. No transition can read or write
// any of it: the Go type of a decision does not contain it (rule 6).
type History struct {
	OK                   [3]uint8 // bit set by observation of an eligible incoming success, not retirement
	Requests, Admissions [3]uint8
	LastRoot             [3]uint8
	Twice                [3]Reason
	// Lost is why the answer of one forwarded request cannot reach the daemon,
	// per request: a loss of one forward never explains a retry of another.
	Lost [NA]Reason
	// Per forwarded request, not just the latest reason per logical task.
	// Certified records the decision that actually committed the retry.
	Certified  [NA]Reason
	Recovering uint8 // bit per logical task, from boot until an authoritative decision
	// Turned is the driver's ledger of turn credits: a bit per request whose
	// attempt a committed turn start confirmed the logical task through. The
	// replacement oracle reads it.
	Turned [3]uint8
	// Started is the driver's ledger of turn starts delivered to the daemon:
	// a bit per request whose start frame this daemon read. The shim's record
	// that the agent started (World.Requests.Started) is not it: a frame
	// still waiting in the backlog carries that too.
	Started uint8
	Need    [3]uint8 // independent pre-runtime launch snapshots
	// FatalPending is the driver's own reading of fatal evidence received and
	// not yet made durable; FatalL, FatalID and FatalOpen are its own reading
	// of the task that item named, judged when the item arrived or when a
	// restart judged it again. The close is judged against these, never
	// against the Receipt the daemon kept.
	FatalPending    bool
	FatalOpen       bool
	FatalReceived   uint8
	FatalClosed     uint8
	FatalL, FatalID uint8
	// TopID is the driver's highest committed id of each logical task: a
	// take-back moves a task to a new id, never back to one it held.
	TopID [3]uint8
	// Each deadline as the driver read it when its own event committed: a
	// suspension request's, the wait each acknowledgement owes, and the
	// recovery bound a restart opened. Late reads these, never a timer a
	// transition wrote.
	HoldDue, RecovDue uint16
	TurnDue           [NA]uint16
	Bad               uint16
}

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
	d := decision{Machine: s.Machine, input: plain(e)}
	execute(&d)
	return hasCut(&d.Effects, e)
}
func hasCut(x *Effects, e Event) bool {
	for f, at := range x.cuts {
		if e.Fault == f && e.At == at {
			return true
		}
	}
	return false
}
func Step(s State, e Event) (State, Effects) {
	if !Enabled(s, e) {
		return s, Effects{}
	}
	return run(s, e)
}

// run is the search's own step. The plant door of stepWith is deliberately
// absent: a function value there would make every decision of the search
// escape to the heap.
func run(s State, e Event) (State, Effects) {
	d := decision{Machine: s.Machine, input: e}
	execute(&d)
	return State{Machine: d.Machine, H: observe(&s, e, &d.Machine, &d.Effects, &d.log)}, d.Effects
}

// stepWith runs one decision through a test-only plant, and then lets the
// driver read its ledger from what the decision did.
func stepWith(s State, e Event, plant func(*decision)) (State, Effects) {
	d := decision{Machine: s.Machine, input: e}
	plant(&d)
	execute(&d)
	return State{Machine: d.Machine, H: observe(&s, e, &d.Machine, &d.Effects, &d.log)}, d.Effects
}
func execute(d *decision) {
	d.dispatch(plain(d.input))
	if d.crash {
		d.Cut = true
		d.input = Event{}
		d.crash = false
		d.restart(false)
	}
}

// observe derives the driver's ledger from one step: the effects the decision
// reported, in the order it reported them, the writes it committed, and the
// machine it left.
func observe(before *State, input Event, m *Machine, fx *Effects, log *commitLog) History {
	h := before.H
	committed := 0
	for _, f := range fx.List[:fx.N] {
		switch f.Action {
		case EligibleOK:
			h.OK[f.Task] |= 1 << f.Request
		case Lose:
			h.Lost[f.Request] = f.Why
		case Judged:
			h.FatalReceived++
			h.FatalPending = true
			r := receiptOf(&before.Machine)
			h.FatalL, h.FatalID, h.FatalOpen = r.L, r.ID, r.Open
		case Rebuild:
			if f.Task != 0 {
				h.Recovering |= 1 << f.Task
			}
		case RuntimeStop:
			if f.Pause != 0 {
				if f.Pause != f.Open {
					h.Bad |= Ended // the oracle: no runtime stop runs for a pause that has ended
				}
				h.Need[f.Pause] |= f.Causes
			}
		case Send:
			l := m.W.Requests[f.Request].L
			h.Requests[l]++
			if h.Recovering&(1<<l) != 0 {
				h.Bad |= Twice // D10 forbids even a deduplicated second request here
			}
		case Forward:
			h.admit(f.Request, &m.W)
		case Commit:
			c := &log.List[committed]
			committed++
			h.Bad |= checkCommit(&h, before.Root, c)
			h.certify(c, &m.W)
			if h.FatalPending && c.Kind == EvidenceWrite && c.Want.Phase == PhFailed && fatalCause(c.Cause) {
				h.FatalClosed = h.FatalReceived
				h.FatalPending = false
			}
			// RT1: the answer the old daemon's socket took is lost to this
			// daemon. Only the recovery bound decides that task, so no
			// write of the answer path may follow such a frame again. A
			// stop whose own bound fired on the same input writes an intent
			// or a stop row, and decides nothing about that answer.
			answered := c.Kind == RetirementWrite || c.Kind == EvidenceWrite || c.Kind == DeliveryWrite
			if answered && carriesRequest(c.Cause) && !fatalCause(c.Cause) && c.Cause.R < NA {
				if q := before.W.Requests[c.Cause.R]; q.Written && before.H.Lost[q.Root] == OldSocket {
					h.Bad |= RT1
				}
			}
		}
	}
	// A failure settles every receipt that is still open, whatever installed
	// it: the committed close above recorded its own.
	if m.M.Phase == PhFailed || m.S.Phase == PhFailed {
		h.FatalPending = false
	}
	// A deadline is a bound of this daemon once it has the row that carries
	// it. A write whose reply the daemon never received is not yet its own,
	// so its deadline waits rather than measuring a timer it cannot know.
	// That write is the step's last commit, the one the machine still holds
	// as uncertain; every commit before it got its reply and is recorded.
	n := log.N
	if last := n - 1; n != 0 && m.P.Uncertain && log.List[last].Kind == m.P.Kind &&
		log.List[last].Want == m.P.Want && log.List[last].Cause == m.P.Cause {
		n = last
	}
	for i := range log.List[:n] {
		h.recordDue(&log.List[i])
	}
	// At a restart that judges a redelivered item again, the driver reads the
	// rebuilt machine, not the one the item first arrived at.
	if m.Booted && h.FatalPending && m.W.Fatal.K != 0 {
		r := receiptOf(m)
		h.FatalL, h.FatalID, h.FatalOpen = r.L, r.ID, r.Open
	}
	// The deadline oracle reads each deadline the driver wrote down when its
	// own event happened, rather than a timer a transition recorded, so a
	// transition that forgets one cannot hide that a later timer passed it.
	if a := recordedDue(before); a != 0 && m.Now > before.Now && m.Now > a {
		h.Bad |= Late
	}
	switch e := appliedInput(&before.Machine, plain(input)); e.K {
	case KTurnStart, KNamedStart:
		// A start the shim held in its backlog reached no daemon. A replay
		// is the backlog handing one over, so that one did.
		if e.R < NA && (plain(input).K == KReplay ||
			before.W.Attached && before.W.N == 0 && before.Deferred.K == 0) {
			h.Started |= 1 << e.R
		}
	}
	// A bound the machine no longer carries is owed by nothing, so the next
	// one of its kind is recorded afresh.
	if m.M.Hold.By == 0 {
		h.HoldDue = 0
	}
	for k := range h.TurnDue {
		if m.M.Att[k].TurnBy == 0 {
			h.TurnDue[k] = 0
		}
	}
	if m.M.RecoverBy == 0 {
		h.RecovDue = 0
	}
	// Ids only grow, so no older prompt's answer reads as the current one's.
	// A failure keeps the id its receipt named (fatalCloseBad judges that).
	if l := m.S.Task.L; l != 0 && m.S.Phase != PhFailed {
		if m.S.Task.ID < h.TopID[l] {
			h.Bad |= I1
		}
		h.TopID[l] = max(h.TopID[l], m.S.Task.ID)
	}
	if fatalClose(before.H, h) {
		if fatalCloseBad(&before.Machine, m, &h, fx) {
			h.Bad |= FatalClose
		}
	} else if recoveryMutation(&before.Machine, m, fx) && !recoveryAuthorized(&before.Machine, plain(input), m, fx) {
		h.Bad |= RecoveryFence
	}
	return h
}

// admit records one forwarded request: the admission it is, and whether the
// evidence that authorized it allows another successful delivery.
func (h *History) admit(k uint8, w *World) {
	l := w.Requests[k].L
	if h.Admissions[l] != 0 {
		prev := h.LastRoot[l] - 1
		reason := h.Certified[prev]
		last := w.Requests[prev]
		if reason == Negative && (!last.Answered || last.Answer == AOK) || reason == Original {
			h.Bad |= Twice
		}
		h.Twice[l] = reason
	}
	h.LastRoot[l] = k + 1
	h.Admissions[l]++
}

// recordDue writes down, at the commit of the event that set it, each
// deadline the oracle later measures a timer against: the deadline a
// suspension request named (S1-S5), the wait an acknowledgement owes its own
// prompt, and the recovery bound a restart opened. A request a decision
// refused commits nothing and names no deadline.
func (h *History) recordDue(c *Written) {
	if c.Prev.Hold.By != 0 && c.Row.Hold.By == 0 {
		// The hold this deadline bounded is gone, even where the same
		// decision asks for another one.
		h.HoldDue = 0
	}
	switch {
	case c.Cause.K == KSuspend && c.Row.Hold.By != 0:
		due := requestDue(c.Cause.A, c.Now)
		if c.Prev.Hold.By == 0 {
			// A request that starts a hold where the store held none
			// starts its own deadline. The earlier hold's bound ended with
			// that hold, even where the write that ended it lost its reply
			// and a later read-back installed it, so that no commit the
			// driver read showed the hold go.
			h.HoldDue = due
		} else {
			h.HoldDue = earlier(h.HoldDue, due)
		}
	case c.Cause.K == KOutcome && c.Cause.A == OcAck && c.Cause.R < NA:
		h.TurnDue[c.Cause.R] = earlier(h.TurnDue[c.Cause.R], after(c.Now, 1))
	}
	if c.Row.RecoverBy != 0 && c.Prev.RecoverBy == 0 {
		h.RecovDue = after(c.Now, 10)
	}
}

// certify observes the durable event authorizing a new admission,
// independently of the send decision. A negative answer cannot license a
// retry of another root.
func (h *History) certify(c *Written, w *World) {
	r := c.Row
	// The ledger records a credit only once the turn-start write commits. A
	// failed transition cannot manufacture a credit for its oracle.
	if c.Kind == ClaimWrite && (c.Cause.K == KTurnStart || c.Cause.K == KNamedStart) &&
		c.Cause.R < NA && r.Task.L != 0 && r.Task.Conf != 0 {
		h.Turned[r.Task.L] |= 1 << c.Cause.R
	}
	prev := c.Prev
	l := prev.Task.L
	if l == 0 {
		return
	}
	if r.Task.L != l || !r.live() {
		h.Recovering &^= 1 << l
	}
	if prev.Task.ID == r.Task.ID || r.Task.L != l {
		return
	}
	last := h.LastRoot[l]
	if last == 0 {
		// A legacy row or a registered-but-never-written request has no
		// admission to classify, and nothing to tell twice: the take-back
		// that leaves no answer to come ends the doubt, whatever caused it.
		if !r.waiting() {
			h.Recovering &^= 1 << l
		}
		return
	}
	k := last - 1
	q := w.Requests[k]
	var why Reason
	switch {
	case !c.Proc || c.Cause.K == KDeath || c.Cause.K == KRestart && r.Phase == PhLaunch:
		why = NoProcess
	case c.Cause.K == KTimerRecov || c.Kind == IntentWrite && prev.Hold.due() != 0 && prev.Hold.due() <= c.Now:
		// The last forward's own answer decides why it never came: a loss of
		// another forward's answer never certifies this one.
		why = Expired
		if h.Lost[k] != Original {
			why = h.Lost[k]
		} else if q.Written && prev.Att[k].Stage == AtLost {
			why = LostSend
		}
	case q.Answered && q.Answer != AOK && (c.Kind == EvidenceWrite || c.Kind == RetirementWrite):
		why = Negative
	case c.Cause.K == KTimerTurn || c.Cause.K == KCompact || c.Cause.K == KStale || c.Cause.K == KWake || c.Cause.K == KExit || c.Cause.K == KRegDeadline || c.Cause.K == KRelaunchAgain:
		// A written prompt was cut off before its outcome was known.
		why = LostSend
	}
	if why != Original {
		h.Certified[k] = why
		h.Recovering &^= 1 << l
	}
}

// Prefix is a start or a prefix that the seed runner actually reached. Origin
// names the seed and the prefix length. Length includes the seed prefix, so a
// shortest witness is not just a short suffix of an arbitrarily long trace.
type Prefix struct {
	State  State
	Origin string
	Length int
}
type Witness struct {
	Origin string
	Steps  []Event
	Length int
}
type Stats struct {
	States, Edges, Sequences uint64
	Roots                    int
	Violations               map[uint16]Witness
}
type path struct {
	before *path
	ev     Event
	root   int
}
type reach struct {
	ways   uint64
	p      *path
	length int
}
type transition func(State, Event) (State, Effects)

// Explore expands exact comparable states at each remaining depth. It counts
// all paths, including paths that converge or stutter, via their multiplicity.
// Terminal states are checked on the incoming edge and need no expansion.
func Explore(prefixes []Prefix, depth int) Stats { return explore(prefixes, depth, run) }
func explore(roots []Prefix, depth int, step transition) Stats {
	out := Stats{Violations: map[uint16]Witness{}}
	front := map[State]reach{}
	for i, r := range roots {
		n, ok := front[r.State]
		if !ok || n.length > r.Length {
			front[r.State] = reach{1, &path{root: i}, r.Length}
		}
	}
	out.Roots = len(front)
	for level := 0; level < depth; level++ {
		next := map[State]reach{}
		for s, n := range front {
			out.States++
			visit := func(e Event, t State) {
				out.Edges++
				out.Sequences += n.ways
				bad := Check(t)
				if bad != 0 {
					for bit := uint16(1); bit != 0 && bit <= lastInvariant; bit <<= 1 {
						if bad&bit == 0 {
							continue
						}
						old, found := out.Violations[bit]
						if found && old.Length <= n.length+1 {
							continue
						}
						p := n.p
						steps := make([]Event, level+1)
						steps[level] = e
						for j := level - 1; j >= 0; j-- {
							steps[j] = p.ev
							p = p.before
						}
						out.Violations[bit] = Witness{roots[p.root].Origin, steps, n.length + 1}
					}
				}
				if level+1 == depth {
					return
				}
				v, ok := next[t]
				v.ways += n.ways
				if !ok || v.length > n.length+1 {
					v.length = n.length + 1
					v.p = &path{before: n.p, ev: e}
				}
				next[t] = v
			}
			for _, e := range alphabet {
				if !s.base(e) {
					continue
				}
				t, x := step(s, e)
				visit(e, t)
				for fault, at := range x.cuts {
					e.Fault, e.At = fault, at
					t, _ = step(s, e)
					visit(e, t)
				}
			}
		}
		front = next
	}
	return out
}
