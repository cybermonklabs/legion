package holdmodel

import (
	"strings"
	"testing"
)

// The counterorders of the independent model read of this package
// (dispatch://INFRA-3904/comment/87e76a67-7488-413a-bec0-4363e0c5156d), each
// with the verdict the architect's decisions on that read require
// (dispatch://INFRA-3904/comment/6ecd6258-b598-4388-8b54-be4afe0c03ae).

const waiting2 = ack + "wait " + next + "ack1 start1 open row end "

func counterSeeds() []seed {
	var cs []seed
	add := func(n, s string) { cs = append(cs, seed{"read/" + n, ready, s}) }
	// High 1: a lost resend keeps the confirmation an older acknowledged
	// prompt holds, so no newer task replaces it and the older prompt's
	// success serves it.
	add("H1/lost-resend-keeps-older-confirmation", ack+"wait "+next+"start :confirmed lost1 :confirmed new :kept end ok :served")
	add("H1/control-queue-before-loss", ack+"wait "+next+"start :confirmed new :kept lost1 :confirmed")
	add("H1/hold-after-loss", ack+"wait "+next+"start :confirmed lost1 :confirmed open row end :H2 bound :H4 :kept")
	// High 2: after a clean restart a stale-plugin refresh waits for the
	// answer or the bound; it neither replaces the process nor takes the task.
	add("H2/stale-waits-for-recovery", wire+"start end boot reconnect stale :doubt :kept :relaunch rec :launch :unread")
	add("H2/replay-decides-before-refresh", wire+"start stale end boot ok reconnect replay :served :single :launch")
	add("H2/held-stale-waits-for-recovery", wire+"start end open row boot reconnect stale :doubt :H2 :relaunch bound :H4 :owed")
	// Medium 3: the operator's earlier bound fires before the recovery bound.
	add("M3/operator-bound-before-recovery", ack+"boot open row op :op :doubt bound :H4 :unread")
	// Medium 4: fatal evidence is kept from its receipt until the failure
	// commits, across a repair and a restart (O3: no pause stop).
	add("M4/fatal-report-over-pending-intent", waiting+"bound!1.1 :pending fatalReport!1.1 :pending repair :failed :kept")
	add("M4/fatal-result-across-restart", waiting2+"down no fatal1 :pending up boot drain :failed :kept")
	add("M4/fatal-result-across-restart-then-bound", waiting2+"down no fatal1 up boot reconnect :failed :kept")
	add("M4/fatal-report-crash-before-write", waiting+"bound!1.1 fatalReport!3.1 drain :failed :kept")
	// Medium 5: a pause that has ended stops nothing, across a repair and a
	// restart (the hold is forgotten at a restart; boot asks only an open pause).
	add("M5/pause-ends-before-repair", waiting+"down no unpause :pending :pauseEnded up repair :H0 :noStop :unread")
	add("M5/pause-ends-write-fails-then-restart", waiting+"unpause!1.1 :pending boot :H0 :doubt :noStop")
	add("M5/pause-ends-behind-deferred-input", waiting+"down no ready :pending unpause up repair :H0 drain :H0 :noStop")
	add("M5/open-pause-survives-restart", waiting+"boot :H2 :pause bound :H4 :owed")
	// Medium 6 (M7): a committed displacement intent over a live process is
	// a stop still to ask, not a stop that happened.
	add("M6/displaced-intent-over-live-process", waiting+"stale!4.1 :H3r :proc retry :H4 :owed")
	add("M6/displaced-intent-over-dead-process", waiting+"death!4.1 :H4o save :H4 :owed")
	// Medium 8: an acknowledgement read after its turn ended arms no wait.
	add("M8/late-ack-arms-no-turn-wait", wire+"start end open row ack :H2 :zeroCharge ok :served :H4")
	// Medium 9: the old socket took r0's answer, but the retry is of r1,
	// whose own answer never came: the bound, not the old socket.
	add("M9/old-socket-loss-of-another-forward", ack+"wait "+next+"taken reconnect :doubt rec prepare register w2 :rt5")
	// Minor: the failure limit reached with a refused stop, and the daemon's
	// request over a working pause, which retires no phase task. The other
	// minor items strengthen the seeds where they stand (F1, CUsn, F5Q2,
	// the session conflict, the death after a failed take-back).
	ch := ready
	ch.M.Charges = ChargeLimit - 1
	ch.S = ch.M
	cs = append(cs,
		seed{"read/minor/CUsn-at-limit", ch, wire + "open row refused :atLimit :owed :H4 :kept"},
		seed{"read/minor/limit-refused-stop-one-retry", ch, wire + "open row refuses refused :atLimit :H3r :kept ready :H3r accepts retry :H4 :owed"},
	)
	add("minor/S4-over-working-pause-phase", "phase prepare register w ack start open row daemon :pause end error :H4 :kept")
	add("minor/S4DL-over-working-pause-phase", "phase prepare register w ack start open row daemonDL :pause bound :H4 :kept")
	// Medium 11 (D12 condition 2): the pre-migration daemon's request is on
	// the wire, the legacy row records no attempt, and its exact success
	// reaches the daemon. It decides nothing; the bound takes the task back.
	legacy := ready
	legacy.S.Task = Task{L: 1, ID: 1, Unknown: true}
	legacy.M = legacy.S
	legacy.W.Requests[0] = WireRequest{L: 1, Under: 1, Conn: 1, Inc: 1, Written: true, Returned: true}
	legacy.W.Alloc, legacy.W.Dedupe = 1, 1
	legacy.H.Requests[1], legacy.H.Admissions[1], legacy.H.LastRoot[1] = 1, 1, 1
	cs = append(cs,
		seed{"read/M11/legacy-row-unmatched-success", legacy, "boot reconnect :doubt ok :kept :doubt rec :unread prepare register w1 :again :unmatched"},
		seed{"read/M11/legacy-row-unmatched-success-from-backlog", legacy, "boot ok reconnect replay :kept :doubt rec :unread"},
		seed{"read/M11/legacy-row-provider-error", legacy, "boot reconnect error :kept :doubt rec :unread"},
	)
	// The architect's choices against #5535: the model's rules stand.
	add("choice/provider-error-keeps-resend-open", ack+"wait "+next+"start end error ack1 :kept :H0")
	add("choice/pre-wire-send-credits-no-turn", "q prepare register foreign :unread")
	add("choice/older-id-send-credits-no-turn", wire+"compact compactEnd foreign :unread")
	return cs
}

// xSeeds are the read's combinations of two or more hazards, each from a
// prefix the depth-3 search reaches only beyond its own length.
func xSeeds() []seed {
	var cs []seed
	add := func(n, s string) { cs = append(cs, seed{"read/X/" + n, ready, s}) }
	add("N1/restart-restart-pause-ends", waiting+"boot reconnect :doubt boot reconnect :doubt unpause :H0 :doubt rec :unread :single")
	add("N2/stop-write-fails-restart-pause-ends", waiting+"bound!1.2 :H4o boot :H4o unpause :H4 :owed :unread")
	add("N3a/lost-restart-hello-live-success", wire+"lost boot reconnect hello :doubt ok :served :single")
	add("N3b/lost-restart-hello-backlog-success", wire+"lost boot ok reconnect hello replay :served :single")
	add("N4/resend-error-of-first-restart-before-ack", ack+"wait "+next+"error :kept boot reconnect :doubt ok1 :served :again")
	add("N5/fatal-report-in-H3r-then-restart", waiting+"refuses bound :H3r fatalReport :failed boot :failed accepts failEffects :failed")
	add("N6/compaction-restart-pause", ack+"compact boot reconnect open row :H1 compactEnd :H4 :unread :owed")
	add("N7/intent-commit-crash-restart-retry", waiting+"bound!4.1 :H3r boot :H3r retry :H4 :owed :unread")
	add("N8/earlier-id-result-restart-write-fails", ack+"wait "+next+"ack1 boot reconnect hello :doubt ok!1.1 :pending repair :served")
	add("N9/operator-restart-bound", waiting+"op boot reconnect :op bound :H4 :unread")
	add("N10/bound-resend-late-success-of-first", waiting+"bound unpause resume "+next+"ok :kept :again")
	add("FIFO/fourth-frame-evicted", ack+"wait "+next+"ack1 start1 detach end late ok1 foreign reconnect replay replay replay :served")
	return cs
}

// trace runs words from s, failing on a disabled event or an invariant, and
// returns the state and every effect of the run.
func trace(t *testing.T, s State, words string) (State, []Effect) {
	t.Helper()
	var all []Effect
	for _, w := range strings.Fields(words) {
		e := parse(t, w)
		if !Enabled(s, e) {
			t.Fatalf("disabled %s at %s: %+v", w, s.Class(), s.M)
		}
		var x Effects
		s, x = Step(s, e)
		all = append(all, x.List[:x.N]...)
		if bad := Check(s); bad != 0 {
			t.Fatalf("after %s: invariant mask %d", w, bad)
		}
	}
	return s, all
}
func count(fx []Effect, a Action) int {
	n := 0
	for _, f := range fx {
		if f.Action == a {
			n++
		}
	}
	return n
}

// The read's transition contracts, where a verdict word cannot say it.
func TestCounterorderContracts(t *testing.T) {
	t.Run("O1-sources", func(t *testing.T) {
		for _, c := range []struct {
			source   string
			deadline uint16
			owner    Src
			retired  bool
		}{{"op", 3, SrcOperator, false}, {"daemon", 10, SrcDaemon, true}, {"daemonDL", 2, SrcDaemon, true}, {"release", 10, SrcReleased, false}, {"row", 10, SrcReleased, false}, {"gate", 0, SrcReleased, false}} {
			s, _ := trace(t, ready, "phase prepare register w ack start end open "+c.source)
			if c.deadline != 0 {
				if s.Class() != "H2" || s.M.Hold.due() != 1+c.deadline {
					t.Fatalf("%s: want H2 due %d, got %s due %d", c.source, 1+c.deadline, s.Class(), s.M.Hold.due())
				}
				s, _ = trace(t, s, "bound")
			}
			if s.Now != 1+c.deadline || s.Class() != "H4" || s.S.SuspendedBy != c.owner || (s.S.Ret[1] == RPhaseEnd) != c.retired || !c.retired && (s.S.Task.L != 1 || s.S.Task.Conf != 0 || s.S.Task.ID == 1) {
				t.Fatalf("%s: wrong stop or take-back at %d: %+v", c.source, s.Now, s.S)
			}
		}
	})
	t.Run("M3-earlier-operator-bound", func(t *testing.T) {
		s, _ := trace(t, ready, ack+"boot open row op")
		if Enabled(s, words["rec"]) {
			t.Fatalf("the recovery bound (%d) is enabled before the operator's (%d)", s.M.RecoverBy, s.M.Hold.due())
		}
		want := s.M.Hold.due()
		s, fx := trace(t, s, "bound")
		if s.Now != want || count(fx, RuntimeStop) != 1 {
			t.Fatalf("stopped at %d with %d runtime stops, want once at %d", s.Now, count(fx, RuntimeStop), want)
		}
	})
	t.Run("O2-intent-before-stop", func(t *testing.T) {
		s, fx := trace(t, ready, waiting+"boot bound!1.1")
		if count(fx, RuntimeStop) != 0 || s.S.Task.Conf == 0 || s.S.Stops[1] != 0 || s.P.Kind != IntentWrite {
			t.Fatal("a stop passed a failed take-back")
		}
		s, fx = trace(t, s, "repair")
		want := []Action{WriteAttempt, Commit, RuntimeStop, WriteAttempt, Commit}
		if len(fx) != len(want) || fx[1].Write != IntentWrite || fx[4].Write != StopWrite || s.S.Task.Conf != 0 || s.S.Task.L != 1 || s.S.Stops[1] != 2 {
			t.Fatalf("take-back must commit before the runtime and the stop's write: %v", fx)
		}
		for i, a := range want {
			if fx[i].Action != a {
				t.Fatalf("effect %d is %d, want %d: %v", i, fx[i].Action, a, fx)
			}
		}
	})
	t.Run("O3-fatal-kept", func(t *testing.T) {
		for _, e := range []string{"fatal", "fatalReport"} {
			s, _ := trace(t, ready, waiting+e+"!3.1 drain")
			if s.S.Phase != PhFailed || s.S.Task.L != 1 || s.S.Task.Conf != 0 || s.S.Stops[1] != 0 {
				t.Fatalf("%s must fail with the task kept and no pause stop: %+v", e, s.S)
			}
		}
	})
	t.Run("H1-older-success-credited", func(t *testing.T) {
		s, _ := trace(t, ready, ack+"wait "+next+"start lost1 new end ok")
		if s.S.Ret[1] != RServed || s.H.OK[1] == 0 {
			t.Fatalf("the older acknowledged prompt's success must serve the task: ret %v ok %v", s.S.Ret, s.H.OK)
		}
	})
	t.Run("H2-replay-before-refresh", func(t *testing.T) {
		s, _ := trace(t, ready, wire+"start stale end boot ok reconnect replay ready")
		if s.S.Ret[1] != RServed || s.H.Admissions[1] != 1 || s.H.Requests[1] != 1 {
			t.Fatalf("after the replayed success: ret %v requests %v forwards %v", s.S.Ret, s.H.Requests, s.H.Admissions)
		}
	})
	t.Run("M5-first-bound-survives-failed-write", func(t *testing.T) {
		s, _ := trace(t, ready, ack+"open row wait boot!1.1")
		want := s.P.Want.RecoverBy
		if want == 0 {
			t.Fatal("the seed must leave a failed recovery write")
		}
		s, _ = trace(t, s, "boot")
		if s.M.RecoverBy != want {
			t.Fatalf("recovery due %d, first due %d", s.M.RecoverBy, want)
		}
	})
	t.Run("M5-pause-end-after-boot-owed-stop", func(t *testing.T) {
		s, _ := trace(t, ready, waiting+"bound!5.1 down unpause up save")
		if Enabled(s, words["drain"]) {
			s, _ = trace(t, s, "drain")
		}
		if s.W.Pause != 0 || !Enabled(s, words["resume"]) || s.S.Stops[1] != 2 {
			t.Fatalf("the pause's end was lost: pause %d class %s stops %v", s.W.Pause, s.Class(), s.S.Stops)
		}
	})
	t.Run("D12-legacy-cannot-match", func(t *testing.T) {
		s, _ := trace(t, ready, work+"end")
		s.S.Att = [NA]Attempt{}
		s.S.Task.Unknown = true
		s, _ = trace(t, s, "boot ok reconnect replay")
		if !s.Restored || s.M.RecoverBy == 0 || s.S.Task.L != 1 || s.S.Ret[1] != 0 {
			t.Fatal("an unknown attempt matched")
		}
		s, _ = trace(t, s, "rec")
		if s.S.Task.L != 1 || s.S.Task.Unknown || s.S.Task.Conf != 0 || s.S.Ret[1] != 0 {
			t.Fatal("the bound must keep the task unread")
		}
	})
	t.Run("M6-displaced-intent-asks-the-live-process", func(t *testing.T) {
		s, fx := trace(t, ready, waiting+"stale!4.1")
		if count(fx, RuntimeStop) != 0 || !s.StopRetry || s.StopOwed {
			t.Fatalf("the restored intent must ask the live process again: retry %v owed %v", s.StopRetry, s.StopOwed)
		}
		if Enabled(s, words["save"]) {
			t.Fatal("the stop's write is enabled before the live process was asked to stop")
		}
		s, fx = trace(t, s, "retry")
		if count(fx, RuntimeStop) != 1 || s.W.Proc || s.Class() != "H4" {
			t.Fatalf("retry: %d runtime stops, process %v, class %s", count(fx, RuntimeStop), s.W.Proc, s.Class())
		}
	})
	t.Run("M7-no-callback-after-restart", func(t *testing.T) {
		s, _ := trace(t, ready, wire+"boot")
		for _, w := range []string{"refused", "busy", "ack", "lost"} {
			if Enabled(s, words[w]) {
				t.Errorf("the old daemon's send callback %s is enabled after the restart", w)
			}
		}
	})
	t.Run("M8-late-ack-arms-no-turn-wait", func(t *testing.T) {
		s, _ := trace(t, ready, wire+"start end open row ack")
		if s.M.TurnBy != 0 || Enabled(s, words["wait"]) {
			t.Fatalf("a wait for a turn is armed after that turn ended: %d", s.M.TurnBy)
		}
	})
	t.Run("F5Q2-one-runtime-retry", func(t *testing.T) {
		_, fx := trace(t, ready, ack+"wait "+next+"open row refuses refused1 no ready accepts retry")
		if n := count(fx, RuntimeStop); n != 2 {
			t.Fatalf("%d runtime stops, want the refused one and its one retry", n)
		}
	})
	t.Run("A7-refusal-marks-only-its-own-send", func(t *testing.T) {
		if s, _ := trace(t, ready, wire+"late foreign"); s.S.Task.Conf != 0 {
			t.Fatal("a late refusal must exclude its own send from turn attribution")
		}
		if s, _ := trace(t, ready, ack+"wait "+next+"no start1"); s.S.Task.Conf == 0 {
			t.Fatal("the older send's no-turn answer must not answer the separately forwarded resend")
		}
	})
	// The architect chose the model's rules over #5535's for these three.
	t.Run("choice-provider-error-keeps-resend-open", func(t *testing.T) {
		if s, _ := trace(t, ready, ack+"wait "+next+"start end error ack1"); !s.S.Att[1].open() {
			t.Fatal("an earlier prompt's provider error must leave the separate resend open")
		}
	})
	t.Run("choice-pre-wire-send-credits-no-turn", func(t *testing.T) {
		if s, _ := trace(t, ready, "q prepare register foreign"); s.S.Task.Conf != 0 {
			t.Fatal("a send before the wire must credit no turn (D6)")
		}
	})
	t.Run("choice-older-id-send-credits-no-turn", func(t *testing.T) {
		if s, _ := trace(t, ready, wire+"compact compactEnd foreign"); s.S.Task.Conf != 0 {
			t.Fatal("a send under an older id must credit no turn (D6)")
		}
	})
}

// Medium 10: the source-side events the daemon's one deferred slot cannot refuse.
func TestReviewDomain(t *testing.T) {
	for _, c := range []struct {
		label, prefix, input string
		want                 bool
	}{
		{"first detached result during doubt", work + "boot :doubt", "ok", true},
		{"live result during doubt", work + "boot reconnect :doubt", "ok", true},
		{"hello during written send", wire + ":H0", "hello", true},
		{"first stop during repair", waiting + "down no :pending", "op", true},
		{"second stop during repair waits at its source", waiting + "down no op :pending", "daemonDL", false},
		{"pause ends during repair", waiting + "down no :pending", "unpause", true},
		{"pause ends behind deferred input", waiting + "down no ready :pending", "unpause", true},
		{"second boot during doubt", work + "boot :doubt", "boot", true},
		{"first fatal report during intent retry", waiting + "refuses bound :H3r", "fatalReport", true},
		{"fatal during intent write with deferred input", waiting + "bound!1.1 down ready :pending", "fatalReport", true},
		{"result queued at shim during recovery with deferred input", work + "boot down ready new :doubt", "ok", true},
		{"live result behind deferred input", waiting2 + "down no ready :pending", "ok1", true},
	} {
		t.Run(c.label, func(t *testing.T) {
			p := replaySeed(t, seed{c.label, ready, c.prefix})
			s := p[len(p)-1].State
			if got := Enabled(s, parse(t, c.input)); got != c.want {
				t.Fatalf("Enabled(%s) = %v, want %v", c.input, got, c.want)
			}
		})
	}
	// A frame received behind the deferred input waits in the shim's backlog
	// and reaches the daemon after the drain.
	s, _ := trace(t, ready, waiting2+"down no ready ok1")
	if s.W.N != 1 || s.Deferred.K != KReady {
		t.Fatalf("the success must wait at the shim behind the deferred ready: backlog %d deferred %s", s.W.N, s.Deferred)
	}
	s, _ = trace(t, s, "up repair drain replay")
	if s.S.Ret[1] != RServed {
		t.Fatalf("the success must serve the task after the drain: %+v", s.S)
	}
}

// Medium 12: every printed event, with every fault suffix, replays as itself,
// and a malformed suffix is refused.
func TestParse(t *testing.T) {
	seen := map[string]Event{}
	for _, e := range alphabet {
		for _, f := range []uint8{0, FailWrite, LostReply, CrashBeforeWrite, CrashAfterCommit, CrashAfterRuntime} {
			e := e
			if f != 0 {
				e.Fault, e.At = f, 2
			}
			w := e.String()
			if prev, dup := seen[w]; dup && prev != e {
				t.Fatalf("%v and %v both print %q", prev, e, w)
			}
			seen[w] = e
			got, err := parseWord(w)
			if err != nil || got != e {
				t.Fatalf("%q replays as %v (%v), want %v", w, got, err, e)
			}
		}
	}
	for _, w := range []string{"row!", "row!1.1trailing", "row!1", "row!.1", "row!9.1", "row!1.0", "row!1.1!2.2", "row!-1.1"} {
		if _, err := parseWord(w); err == nil {
			t.Errorf("%q parsed", w)
		}
	}
}

// Medium 11: an answer can reach a legacy row, and it decides nothing.
func TestLegacyAnswers(t *testing.T) {
	var legacy seed
	for _, c := range counterSeeds() {
		if c.name == "read/M11/legacy-row-unmatched-success" {
			legacy = c
		}
	}
	s, _ := trace(t, legacy.start, "boot reconnect")
	n := 0
	for _, e := range alphabet {
		if (e.K == KResult || e.K == KRefusal) && Enabled(s, e) {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no answer can reach the legacy row: D12 condition 2's negative clause is untested")
	}
}
