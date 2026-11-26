package holdmodel

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type seed struct {
	name  string
	start State
	trace string
}

var ready = Start(PhIdle, false)
var launch = Start(PhLaunch, false)

const wire = "q prepare register w "
const ack = wire + "ack "
const work = ack + "start "
const held = work + "open row "
const waiting = held + "end "
const next = "prepare register w1 "

var words = func() map[string]Event {
	m := map[string]Event{}
	for _, e := range alphabet {
		m[e.String()] = e
	}
	for name, k := range map[string]Kind{"open": KPauseBegins, "unpause": KPauseEnds, "end": KTurnEnd, "bound": KTimerHold, "rec": KTimerRecov, "retry": KTimerStopRetry, "repair": KTimerWrite, "save": KTimerStopWrite, "wait": KTimerTurn, "down": KStoreDown, "up": KStoreUp, "boot": KRestart, "st": KStartedBy, "probe": KProbeIdle, "park": KParkWake, "stale": KStale, "refuses": KRuntimeRefuses, "accepts": KRuntimeAccepts} {
		m[name] = Event{K: k}
	}
	m["end"] = Event{K: KTurnEnd, A: OOK}
	m["q"] = Event{K: KDeliver, A: 1}
	m["phase"] = Event{K: KDeliver, A: 1, B: 1}
	m["new"] = Event{K: KDeliver, A: 2}
	m["gone"] = Event{K: KRestart, A: 1}
	m["stream"] = Event{K: KProbeIdle, A: 1}
	m["foreign"] = Event{K: KTurnStart, R: Foreign}
	m["never"] = Event{K: KSuspend, A: RqOperator, B: 1}
	m["rule"] = Event{K: KStartedBy, A: 1}
	m["compactEnd"] = Event{K: KCompactEnd, A: 1}
	m["fatalReport"] = Event{K: KReport, A: RpFatal}
	m["repeatFatal"] = Event{K: KReport, A: RpFatal, B: 1}
	m["clean"] = Event{K: KReport, A: RpClean}
	for j, n := range []string{"row", "gate", "op", "daemon", "daemonDL", "release"} {
		m[n] = Event{K: KSuspend, A: uint8(j + 1)}
	}
	for k := uint8(0); k < NA; k++ {
		suffix := strconv.Itoa(int(k))
		if k == 0 {
			suffix = ""
		}
		for name, e := range map[string]Event{"w": {K: KWire}, "start": {K: KTurnStart}, "named": {K: KNamedStart}, "ack": {K: KOutcome, A: OcAck}, "lost": {K: KOutcome, A: OcLost}, "refused": {K: KOutcome, A: OcRefused}, "busy": {K: KOutcome, A: OcBusy}, "prewire": {K: KOutcome, A: OcNoFrame}, "ok": {K: KResult, A: AOK}, "error": {K: KResult, A: AProvErr}, "fatal": {K: KResult, A: AFatal}, "no": {K: KResult, A: ANoTurn}, "busyResult": {K: KResult, A: ABusyRes}, "late": {K: KRefusal}, "streamLate": {K: KRefusal, B: 2}, "taken": {K: KCrashFrame, A: AOK}} {
			e.R = k
			m[name+suffix] = e
		}
	}
	return m
}()

func parse(t *testing.T, word string) Event {
	t.Helper()
	base, ft, _ := strings.Cut(word, "!")
	e, ok := words[base]
	if !ok {
		t.Fatalf("unknown event %q", base)
	}
	if ft != "" {
		var f, a uint8
		if _, err := fmt.Sscanf(ft, "%d.%d", &f, &a); err != nil {
			t.Fatal(err)
		}
		e.Fault, e.At = f, a
	}
	return e
}
func verdict(s State, v string) bool {
	r := s.M
	switch v {
	case "kept":
		return r.Task.L == 1 && r.Ret[1] == 0
	case "unread":
		return r.Task.L == 1 && r.Task.Conf == 0
	case "served":
		return r.Task.L == 0 && r.Ret[1] == RServed
	case "failed":
		return r.Phase == PhFailed && r.Stops[1] == 0
	case "owed":
		return r.Stops[1] == 2
	case "phaseEnd":
		return r.Ret[1] == RPhaseEnd
	case "claimEnd":
		return r.Ret[1] == RClaimEnd
	case "single":
		return s.H.Requests[1] == 1 && s.H.Admissions[1] == 1
	case "again":
		return s.H.Requests[1] == 2 && s.H.Admissions[1] == 2
	case "alias":
		return s.H.Requests[1] == 2 && s.H.Admissions[1] == 1
	case "pending":
		return s.P.Kind != 0
	case "noStop":
		return s.S.Stops[1] == 0 && s.W.Proc
	case "doubt":
		return r.RecoverBy != 0 && s.Restored
	case "provisional":
		return r.Task.Conf == 2
	case "confirmed":
		return r.Task.Conf == 1
	case "op":
		return r.Hold.By == SrcOperator
	case "release":
		return r.Hold.By == SrcReleased && r.Hold.Pause == 0
	case "daemon":
		return r.Hold.By == SrcDaemon
	case "pause":
		return r.Hold.Pause == 1
	case "second":
		return r.Task.L == 2
	case "row12":
		return s.S.StartRow == 12
	case "zeroCharge":
		return r.Charges == 0
	case "rotated":
		return r.Task.ID > 1
	case "launch":
		return r.Phase == PhLaunch
	case "relaunch":
		return r.Relaunch
	case "noError":
		return r.ErrRun == 0
	case "rt1", "rt2", "rt3", "rt4", "rt5", "lostCost":
		return s.H.Twice[1] == map[string]Reason{"rt1": OldSocket, "rt2": Overflow, "rt3": NoProcess, "rt4": Unapplied, "rt5": Expired, "lostCost": LostSend}[v]
	default:
		return s.Class() == v
	}
}
func replaySeed(t *testing.T, c seed) []Prefix {
	t.Helper()
	s := c.start
	ps := []Prefix{{s, c.name + "[0]", 0}}
	count := 0
	checks := 0
	for _, w := range strings.Fields(c.trace) {
		if v, ok := strings.CutPrefix(w, ":"); ok {
			checks++
			if !verdict(s, v) {
				t.Fatalf("%s[%d] want %s, got %s %+v", c.name, count, v, s.Class(), s)
			}
			continue
		}
		e := parse(t, w)
		if !Enabled(s, e) {
			t.Fatalf("%s[%d] disabled %s: %+v", c.name, count, w, s)
		}
		s, _ = Step(s, e)
		count++
		if bad := Check(s); bad != 0 {
			t.Fatalf("%s[%d] after %s invariant mask %d: %+v", c.name, count, w, bad, s)
		}
		ps = append(ps, Prefix{s, fmt.Sprintf("%s[%d]", c.name, count), count})
	}
	if checks == 0 || count == 0 {
		t.Fatal("empty seed/verdict", c.name)
	}
	return ps
}
