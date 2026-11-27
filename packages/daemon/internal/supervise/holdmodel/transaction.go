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
	// EligibleOK: the daemon read a success that is the exact answer of an
	// open attempt of its pending task (matches). Lose: the answer of one
	// forwarded request cannot reach this daemon, for the named reason.
	// Judged: the daemon received fatal evidence. Rebuild: a restart rebuilt
	// the machine from the store, naming the task its doubt covers.
	EligibleOK
	Lose
	Judged
	Rebuild
)

// Effect is one observable act of a decision. The driver reads these, and
// only these, to keep its ledger: a transition reports what it did, and never
// writes the ledger itself.
type Effect struct {
	Action  Action
	Write   Write
	Request uint8 // the request slot a send, an answer or a loss names
	Task    uint8 // EligibleOK, Rebuild: the logical task
	Why     Reason
	Pause   uint8 // RuntimeStop: the pause the stop's snapshot belongs to
	Open    uint8 // RuntimeStop: the pause open in the world when it ran
	Causes  uint8 // RuntimeStop: the launch causes that snapshot owes
}
type Effects struct {
	List      [28]Effect
	N, Writes uint8
	Cut       bool
}

func (x *Effects) put(e Effect) {
	if int(x.N) == len(x.List) {
		panic("holdmodel: one decision had more effects than the list holds")
	}
	x.List[x.N] = e
	x.N++
}
func (x *Effects) add(a Action, w Write, k uint8) { x.put(Effect{Action: a, Write: w, Request: k}) }

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

// Written is one committed write as the driver observes it: the store row
// before and after it, the row the decision wanted, its cause, and the world
// it landed in. commitLog is per step and is never part of a State, so the
// exploration's memory does not carry it.
type Written struct {
	Kind            Write
	Cause           Event
	Prev, Row, Want Row
	Proc            bool
	Pause           uint8
	Now             uint16
}
type commitLog struct {
	List [6]Written
	N    uint8
}

type decision struct {
	Machine
	Effects
	// log is the step's write trace, which the driver reads afterwards. It is
	// not the ledger: no History is reachable from a decision.
	log                                                                                                  commitLog
	input                                                                                                Event
	crash, keepEnded                                                                                     bool
	wakeBreaksRecovery, dropFatal, skipFatalApply, queueDropsCredit, intentDropsPause, forceFatalFailure bool
	badFatalClose                                                                                        bool
	// Test variants substitute one door; nil is the specified one. store sees
	// the deciding machine and one write; load sees the row a restart reads;
	// the booleans are the named plant defects.
	store func(Machine, Write, Row) Row
	load  func(Row) Row
}

func (d *decision) fault(f uint8) bool { return d.input.Fault == f && d.input.At == d.Writes }

// lose reports, as an effect, why the answer of one forwarded request cannot
// reach this daemon. The driver's ledger reads it; no transition does.
func (d *decision) lose(k uint8, why Reason) { d.put(Effect{Action: Lose, Request: k, Why: why}) }

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
	// A hold whose stop is under way (a committed intent) has had its bound:
	// the stop's own retries end it (D13), so no new bound is armed.
	if r.Hold.By != 0 && r.Intent.By == 0 && !r.waiting() && r.Hold.Deadline == 0 && r.Hold.Watch == 0 {
		r.Hold.Watch = after(d.Now, 10)
	}
	cause.Fault, cause.At = 0, 0
	d.P = Pending{Kind: w, Want: r, Cause: cause}
	return d.flush()
}
func fatalCause(e Event) bool {
	return e.K == KReport && e.A == RpFatal || e.K == KResult && e.A == AFatal
}

// closeFatal forgets the received item at the durable commit, not on the
// reply. The driver records the close from the same commit.
func (d *decision) closeFatal(p Pending) {
	if d.FatalSeen && p.Kind == EvidenceWrite && p.Want.Phase == PhFailed && fatalCause(p.Cause) {
		d.FatalSeen = false
	}
}

// record keeps the committed write for the driver, which reads its rows, its
// cause and the world it landed in to keep the ledger and judge the commit.
func (d *decision) record(prev Row, p Pending) {
	l := &d.log
	if int(l.N) == len(l.List) {
		panic("holdmodel: one decision committed more writes than the log holds")
	}
	l.List[l.N] = Written{Kind: p.Kind, Cause: p.Cause, Prev: prev, Row: d.S, Want: p.Want,
		Proc: d.W.Proc, Pause: d.W.Pause, Now: d.Now}
	l.N++
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
	// afterResume (daemon/pause.go): the stop's transaction reads the pause
	// when it commits, and a stop that commits after its pause ended owes the
	// launch, whatever the decision computed while the pause was open.
	if q := d.S.LastStop.Pause; p.Kind == StopWrite && q != 0 && d.W.Pause == 0 && d.S.Stops[q] == 1 {
		d.S.Stops[q] = 2
	}
	if d.store != nil {
		d.S = d.store(d.Machine, p.Kind, d.S)
	}
	d.add(Commit, p.Kind, 0)
	d.record(prev, p)
	d.closeFatal(p)
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
		// A read-back installs a failure an earlier step committed. Its close
		// was recorded at that commit; a receipt that arrived since is settled
		// by that failure and closes nothing (the read-back commits nothing).
		d.W.Fatal = Event{}
		d.W.FatalStuck = false
		d.FatalSeen = false
	}
}
