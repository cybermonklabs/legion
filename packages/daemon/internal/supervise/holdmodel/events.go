package holdmodel

import "fmt"

// Kind is an event's kind. Each names the row of the design's tables it models.
type Kind uint8

const (
	// The agent's frames, delivered live while the shim is attached and holds no backlog, held in
	// the backlog while it is detached, and replayed in order at the next attach.
	KTurnStart  Kind = iota + 1 // T2: K is the root whose turn starts, or Foreign
	KTurnEnd                    // T1: A is the root's outcome (OOK, OErr, OFatal)
	KResult                     // A1, A2, A2f, A3, A3w, A6: K the root, A the answer kind
	KRefusal                    // A4, A5 (when replayed): K the request, A busy (1) or not (0)
	KNamedStart                 // the shim's replayed named start of an alias's root (D6, dedupe.go:84-93)

	// The send goroutine (startSend, Conn.Prompt).
	KWire    // the request's frame is written (the shim forwards or deduplicates it)
	KOutcome // T3, A7, A6, A8, A8′: A is OcAck, OcRefused, OcBusy, OcLost, OcNoFrame

	// The connection and the backlog.
	KDetach    // the connection breaks: frames wait in the backlog
	KReconnect // T5: a connection registers and its hello is posted
	KReplay    // the backlog's head frame reaches the daemon (Replayed)
	KOverflow  // the backlog drops its oldest frame (RT2: beyond 1000)

	// Requests and the daemon's own events.
	KSuspend       // S1-S5: A is the request (RqPauseRow, RqGate, RqOperator, RqDaemon, RqDaemonDL, RqRelease), B NeverHold
	KDeliver       // Q: A is the logical task, B a phase task
	KStartedBy     // St: a start row (its write carries an owed stop)
	KReady         // R, and HL's ready
	KProbeIdle     // P: the busy probe answers "in no prompt"
	KReport        // F, F′: A is RpFatal, RpClean, RpRefused; B a replay of the last report
	KExit          // X
	KCompact       // C1
	KCompactEnd    // C2's end: A compacted
	KStale         // SP
	KParkWake      // a refresh pass parks a wake over the live process
	KWake          // W
	KDeath         // D
	KPauseBegins   // a project pause opens
	KPauseEnds     // E
	KRegDeadline   // HL: the registration deadline
	KRelaunchAgain // HL: relaunchAgain
	KWrongToken    // an event for another claim

	// Timers.
	KTimerTurn      // T4t
	KTimerHold      // T4
	KTimerRecov     // the recovery bound (M5)
	KTimerStopRetry // D13's runtime retry
	KTimerWrite     // the write retry at the probe interval
	KTimerStopWrite // T6

	// Faults.
	KStoreDown
	KStoreUp
	KLoseReply // the next commit lands and loses its reply
	KRuntimeRefuses
	KRuntimeAccepts
	KRestart    // A: 0 clean between two decisions, 1 the process ended across it (RT3)
	KCrashFrame // RT1/RT4: the old daemon took K's result (kind A) and crashed before applying or writing it

	KPrepare
	KRegister
	KHello
	KResume
	KDrain
	KFailEffects
	KSupersede
	kindEnd
)

// Send outcomes (KOutcome's A).
const (
	OcAck uint8 = iota + 1
	OcRefused
	OcBusy
	OcLost
	OcNoFrame
)

// Suspension requests (KSuspend's A).
const (
	RqPauseRow uint8 = iota + 1 // S1: the pause's stop row, no deadline
	RqGate                      // S2: the gate's stop, deadline now
	RqOperator                  // S3: deadline Timeouts.Stop
	RqDaemon                    // S4: no deadline
	RqDaemonDL                  // S4: with a deadline
	RqRelease                   // S5: the idle release
)

// Reports (KReport's A).
const (
	RpFatal   uint8 = iota + 1 // F: a fatal unprompted report
	RpClean                    // F′: a clean newest prompted report
	RpRefused                  // F′: a refused prompted report
)

// Event is one model event. Crash cuts the decision after the runtime's stop lands and before
// the stop's transaction, and restarts the daemon there.
type Event struct {
	K         Kind
	R         uint8 // a request slot
	A         uint8
	B         uint8
	Fault, At uint8 // fail / lose reply / restart at the At-th transaction, or after the runtime call
}

var kindNames = [...]string{
	KTurnStart: "turnStart", KTurnEnd: "turnEnd", KResult: "result", KRefusal: "refusal", KNamedStart: "namedStart",
	KWire: "wire", KOutcome: "outcome", KDetach: "detach", KReconnect: "reconnect", KReplay: "replay",
	KOverflow: "overflow", KSuspend: "suspend", KDeliver: "deliver", KStartedBy: "startedBy", KReady: "ready",
	KProbeIdle: "probeIdle", KReport: "report", KExit: "exit", KCompact: "compact", KCompactEnd: "compactEnd",
	KStale: "stalePlugin", KParkWake: "parkWake", KWake: "wake", KDeath: "death", KPauseBegins: "pauseBegins",
	KPauseEnds: "pauseEnds", KRegDeadline: "regDeadline", KRelaunchAgain: "relaunchAgain", KWrongToken: "wrongToken",
	KTimerTurn: "timerTurn", KTimerHold: "timerHold", KTimerRecov: "timerRecovery", KTimerStopRetry: "timerStopRetry",
	KTimerWrite: "timerWrite", KTimerStopWrite: "timerStopWrite", KStoreDown: "storeDown", KStoreUp: "storeUp",
	KLoseReply: "loseNextReply", KRuntimeRefuses: "runtimeRefuses", KRuntimeAccepts: "runtimeAccepts",
	KRestart: "restart", KCrashFrame: "crashTakingFrame",
	KPrepare: "prepare", KRegister: "register", KHello: "hello", KResume: "resume", KDrain: "drain", KFailEffects: "failEffects", KSupersede: "supersede",
}

var answerNames = [...]string{AOK: "ok", AProvErr: "providerError", AFatal: "fatal", ANoTurn: "noTurn", ABusyRes: "busy",
	ARefLate: "late", ARefLateBusy: "lateBusy", ARefSend: "refused", ARefBusy: "refusedBusy", AFatalReport: "fatalReport"}
var outcomeNames = [...]string{OcAck: "ack", OcRefused: "refused", OcBusy: "busy", OcLost: "lost", OcNoFrame: "noFrame"}
var requestNames = [...]string{RqPauseRow: "pauseRow", RqGate: "gate", RqOperator: "operator", RqDaemon: "daemon",
	RqDaemonDL: "daemonDeadline", RqRelease: "release"}
var reportNames = [...]string{RpFatal: "fatalUnprompted", RpClean: "promptedClean", RpRefused: "promptedRefused"}
var turnOutNames = [...]string{OOK: "ok", OErr: "providerError", OFatal: "fatal"}

func req(k uint8) string {
	if k == Foreign {
		return "foreign"
	}
	return fmt.Sprintf("r%d", k)
}

func (e Event) String() string {
	name := "?"
	if int(e.K) < len(kindNames) && kindNames[e.K] != "" {
		name = kindNames[e.K]
	}
	var s string
	switch e.K {
	case KTurnStart, KNamedStart, KWire:
		s = fmt.Sprintf("%s(%s)", name, req(e.R))
	case KTurnEnd:
		s = fmt.Sprintf("%s(%s)", name, turnOutNames[e.A])
	case KResult, KCrashFrame:
		s = fmt.Sprintf("%s(%s,%s)", name, req(e.R), answerNames[e.A])
	case KRefusal:
		busy := ""
		if e.A == 1 {
			busy = ",busy"
		}
		s = fmt.Sprintf("%s(%s%s)", name, req(e.R), busy)
	case KOutcome:
		s = fmt.Sprintf("%s(%s,%s)", name, req(e.R), outcomeNames[e.A])
	case KSuspend:
		never := ""
		if e.B == 1 {
			never = ",neverHold"
		}
		s = fmt.Sprintf("%s(%s%s)", name, requestNames[e.A], never)
	case KDeliver:
		ph := ""
		if e.B == 1 {
			ph = ",phase"
		}
		s = fmt.Sprintf("%s(T%d%s)", name, e.A, ph)
	case KReport:
		rp := ""
		if e.B == 1 {
			rp = ",replay"
		}
		s = fmt.Sprintf("%s(%s%s)", name, reportNames[e.A], rp)
	case KCompactEnd:
		s = fmt.Sprintf("%s(compacted=%v)", name, e.A == 1)
	case KRestart:
		if e.A == 1 {
			s = name + "(processEnded)"
		} else {
			s = name + "(clean)"
		}
	default:
		s = name
	}
	if e.Fault != 0 {
		s += fmt.Sprintf("+fault%d@%d", e.Fault, e.At)
	}
	return s
}

// Alphabet is every event the driver tries in every state; Enabled filters it.
func Alphabet() []Event {
	var es []Event
	add := func(e Event) { es = append(es, e) }
	for k := uint8(0); k <= Foreign; k++ {
		add(Event{K: KTurnStart, R: k})
	}
	for _, o := range []uint8{OOK, OErr, OFatal} {
		add(Event{K: KTurnEnd, A: o})
	}
	for k := uint8(0); k < NA; k++ {
		for _, a := range []uint8{AOK, AProvErr, AFatal, ANoTurn, ABusyRes} {
			add(Event{K: KResult, R: k, A: a})
			add(Event{K: KCrashFrame, R: k, A: a})
		}
		add(Event{K: KRefusal, R: k})
		add(Event{K: KRefusal, R: k, A: 1})
		add(Event{K: KNamedStart, R: k})
		add(Event{K: KWire, R: k})
		for _, o := range []uint8{OcAck, OcRefused, OcBusy, OcLost, OcNoFrame} {
			add(Event{K: KOutcome, R: k, A: o})
		}
	}
	for _, k := range []Kind{KDetach, KReconnect, KReplay, KOverflow, KStartedBy, KReady, KProbeIdle, KExit, KCompact,
		KStale, KParkWake, KWake, KDeath, KPauseBegins, KPauseEnds, KRegDeadline, KRelaunchAgain, KWrongToken,
		KTimerTurn, KTimerHold, KTimerRecov, KTimerStopRetry, KTimerWrite, KTimerStopWrite,
		KStoreDown, KStoreUp, KLoseReply, KRuntimeRefuses, KRuntimeAccepts} {
		add(Event{K: k})
	}
	for _, r := range []uint8{RqPauseRow, RqGate, RqOperator, RqDaemon, RqDaemonDL, RqRelease} {
		add(Event{K: KSuspend, A: r})
	}
	add(Event{K: KSuspend, A: RqOperator, B: 1})
	for l := uint8(1); l <= 2; l++ {
		add(Event{K: KDeliver, A: l})
		add(Event{K: KDeliver, A: l, B: 1})
	}
	for _, r := range []uint8{RpFatal, RpClean, RpRefused} {
		add(Event{K: KReport, A: r})
		add(Event{K: KReport, A: r, B: 1})
	}
	add(Event{K: KCompactEnd})
	add(Event{K: KCompactEnd, A: 1})
	add(Event{K: KRestart})
	add(Event{K: KRestart, A: 1})
	for _, k := range []Kind{KPrepare, KRegister, KHello, KResume, KDrain, KFailEffects, KSupersede} {
		add(Event{K: k})
	}
	add(Event{K: KProbeIdle, A: 1}) // streaming
	add(Event{K: KStartedBy, A: 1}) // rule holds
	for k := uint8(0); k < NA; k++ {
		add(Event{K: KRefusal, R: k, B: 2})
	} // non-busy text, streaming
	return es
}
