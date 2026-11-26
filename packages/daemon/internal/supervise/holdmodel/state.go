// Package holdmodel specifies a held suspension of one claim. State is a value:
// copying it forks an execution without sharing timers, requests or storage.
package holdmodel

const NA = 4 // fresh request slots, never reused, even across restart
const Foreign = NA
const ChargeLimit = 2

type Phase uint8

const (
	PhIdle Phase = iota
	PhLaunch
	PhWork
	PhSusp
	PhFailed
	PhEnded
)

type Work uint8

const (
	WkNone Work = iota
	WkTurn
	WkBusy
	WkCompact
	WkCompactTurn
)

type Src uint8

const (
	SrcNone Src = iota
	SrcReleased
	SrcOperator
	SrcDaemon
	SrcExit
)

type Stage uint8

const (
	AtNone Stage = iota
	AtReg
	AtAcked
	AtLost
	AtClosed
	AtInvalid
)
const (
	RNone uint8 = iota
	RServed
	RPhaseEnd
	RClaimEnd
	RReplaced
)
const (
	AOK uint8 = iota + 1
	AProvErr
	AFatal
	ANoTurn
	ABusyRes
	ARefLate
	ARefLateBusy
	ARefSend
	ARefBusy
	AFatalReport
)
const (
	OOK uint8 = iota + 1
	OErr
	OFatal
)

// Reason is the evidence that authorized another wire admission. A negative
// answer is a retry of rejected work, not another successful delivery.
type Reason uint8

const (
	Original Reason = iota
	Negative
	OldSocket
	Overflow
	NoProcess
	Unapplied
	Expired
	LostSend
)

type Task struct {
	L, ID, Conf                     uint8
	PhaseTask, Interrupted, Unknown bool
}
type Attempt struct {
	Stage          Stage
	L, Under, News uint8
}

func (a Attempt) open() bool { return a.Stage >= AtReg && a.Stage <= AtLost }

type Hold struct {
	By               Src
	Request          Src
	Pause            uint8
	Deadline, Answer uint16
	// Watch bounds a stalled turn/registration with neither a source deadline
	// nor wire evidence. Unlike Answer it cannot be extended by another turn.
	Watch  uint16
	Launch bool
}

func (h Hold) due() uint16 { return earlier(earlier(h.Deadline, h.Answer), h.Watch) }

// Intent survives let-go. A refusal on a displaced process is not evidence that it stopped.
type Intent struct {
	By            Src
	Pause, Causes uint8
	Displaced     bool
}

const (
	WakeLaunch uint8 = 1 << iota
	HeldLaunch
	DeathLaunch
	RelaunchLaunch
	TaskLaunch
	ResumeLaunch
)

// Row is one locked claim+delivery image. Retirements keep the request that
// justified service. Meta stands for the claim metadata written with queue.
type Row struct {
	Phase                                            Phase
	Work                                             Work
	Task                                             Task
	Att                                              [NA]Attempt
	Hold                                             Hold
	Intent                                           Intent
	Ret                                              [3]uint8
	ServedBy                                         [3]uint8
	Stops                                            [3]uint8
	LastStop                                         Intent
	Meta, StartRow, Charges, ErrRun, News, ReportSeq uint8
	SuspendedBy                                      Src // current owner; LastStop is immutable provenance
	Wake, DeathCharged, Relaunch, Notify             bool
	TurnBy, RecoverBy                                uint16
}

func (r Row) outstanding() bool {
	for _, a := range r.Att {
		if a.open() {
			return true
		}
	}
	return false
}
func (r Row) live() bool    { return r.Phase <= PhWork }
func (r Row) waiting() bool { return r.outstanding() || r.RecoverBy != 0 }
func (r Row) compact() bool { return r.Work == WkCompact || r.Work == WkCompactTurn }

type WireRequest struct {
	L, Under, Conn, Inc, Root uint8
	Written, Ack, Answered    bool
	Answer                    uint8
	Started                   bool
	Returned                  bool // send callback already posted; a later frame is not another callback
}

// The FIFO belongs to the shim, not to a machine. A result names a root;
// negative responses are fanned out as exact-request refusals, not results.
type World struct {
	Proc, Attached, Down, Refuses, LoseReply          bool
	Inc, Conn, Pause, LastPause, Alloc, Dedupe, Agent uint8
	Requests                                          [NA]WireRequest
	Backlog                                           [3]Event
	N                                                 uint8
	Fatal                                             Event // redelivered until its durable acknowledgement (M6)
}
type Write uint8

const (
	NoWrite Write = iota
	DeliveryWrite
	ClaimWrite
	RetirementWrite
	IntentWrite
	StopWrite
	EvidenceWrite
	RegisterWrite
)

// Want is immutable between attempts at the same transaction. Uncertain means
// the DB committed Want but the caller did not receive the returned image.
type Pending struct {
	Kind      Write
	Want      Row
	Uncertain bool
	Cause     Event
}
type History struct {
	OK                   [3]uint8 // bit set by observation of an eligible incoming success, not retirement
	Requests, Admissions [3]uint8
	LastRoot             [3]uint8
	Twice                [3]Reason
	Lost                 [3]Reason
	// Per forwarded request, not just the latest reason per logical task.
	// Certified records the decision that actually committed the retry.
	Certified  [NA]Reason
	Ledger     [NA]Reason
	Recovering uint8    // bit per logical task, from boot until an authoritative decision
	Need       [3]uint8 // independent pre-runtime launch snapshots
	Bad        uint8
}
type State struct {
	M, S                                        Row
	W                                           World
	H                                           History
	P                                           Pending
	Now                                         uint16
	Root, Restored, StopRetry, StopOwed, Resend bool
	Prepared, Sending                           uint8 // slot+1; zero means none
	Deferred                                    Event // M8: one unconsumed input, then backpressure
}

// Start is a reachable ready, launching or working claim with a live process.
func Start(p Phase, root bool) State {
	r := Row{Phase: p}
	if p == PhWork {
		r.Work = WkTurn
	}
	return State{M: r, S: r, Root: root, Now: 1, W: World{Proc: true, Attached: true, Inc: 1, Conn: 1}}
}

// Positive times use ordinal units: now, probe/turn, stop, answer. Only their
// order matters. Saturation keeps the transition total for arbitrarily deep runs.
func after(now, d uint16) uint16 {
	if now > 65535-d {
		return 65535
	}
	return now + d
}
func earlier(x, y uint16) uint16 {
	if x == 0 || y != 0 && y < x {
		return y
	}
	return x
}
func (s State) Class() string {
	switch {
	case s.StopOwed:
		return "H4o"
	case s.M.Phase == PhSusp:
		return "H4"
	case s.M.Hold.By == 0:
		return "H0"
	case s.M.Phase == PhLaunch:
		return "HL"
	case s.M.Phase == PhWork:
		if s.StopRetry {
			return "H1r"
		}
		return "H1"
	case s.StopRetry:
		return "H3r"
	case s.M.waiting():
		return "H2"
	default:
		return "H3"
	}
}
