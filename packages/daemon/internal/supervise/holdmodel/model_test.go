package holdmodel

import (
	"flag"
	"fmt"
	"strings"
	"testing"
)

var depth = flag.Int("hold-depth", 1, "maximum continuation length after each named prefix (full run: 3)")

// allSeeds is every named trace: the historical proofs, the restart orders,
// the order table, and the counterorders of the independent model read.
func allSeeds() []seed {
	return append(append(append(append(proofSeeds(), restartSeeds()...), orderSeeds()...), counterSeeds()...), xSeeds()...)
}

// seedRoots replays every seed and returns each prefix it reached.
func seedRoots(t *testing.T) []Prefix {
	t.Helper()
	var roots []Prefix
	seen := map[string]bool{}
	for _, c := range allSeeds() {
		if seen[c.name] {
			t.Fatal("duplicate seed", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) { roots = append(roots, replaySeed(t, c)...) })
	}
	return roots
}

func TestTheHoldStateMachine(t *testing.T) {
	if *depth < 1 {
		t.Fatal("depth must be positive")
	}
	roots := seedRoots(t)
	if t.Failed() {
		return
	}
	out := Explore(roots, *depth)
	fmt.Printf("depth=%d prefixes=%d roots=%d states=%d edges=%d sequences=%d seeds=%d violations=%d\n", *depth, len(roots), out.Roots, out.States, out.Edges, out.Sequences, len(allSeeds()), len(out.Violations))
	for bit, w := range out.Violations {
		t.Errorf("%s: %s + %v (length %d)", invariantName(bit), w.Origin, w.Steps, w.Length)
	}
}

// A plant is one deliberately wrong transition, run as a test-only variant of
// a door of the decision. The plants of the invariants and of the independent
// read are caught by name at depth 1 from every named seed, which is what `go
// test` runs by default; a plant of a later oracle names the prefix and the
// depth that reach it.
func TestPlants(t *testing.T) {
	var roots []Prefix
	t.Run("seeds", func(t *testing.T) { roots = seedRoots(t) })
	if t.Failed() {
		t.Fatal("the seeds do not replay")
	}
	for _, c := range []struct {
		name    string
		bit     uint16
		pre     string // "" for every seed's prefixes
		depth   int
		variant func(*decision)
	}{
		{"I1-serve-on-turn-end", I1, "", 1, func(d *decision) {
			d.store = func(_ State, k Write, r Row) Row {
				if k == ClaimWrite && r.Phase == PhIdle && r.Task.Conf != 0 {
					retire(&r, RServed, 0)
				}
				return r
			}
		}},
		{"I2-forget-answer-bound", I2, "", 1, func(d *decision) {
			d.store = func(_ State, _ Write, r Row) Row {
				if r.Hold.By != 0 {
					r.Hold.Answer = 0
				}
				return r
			}
		}},
		{"I3-claim-only-stop", I3, "", 1, func(d *decision) {
			d.store = func(_ State, k Write, r Row) Row {
				if k == StopWrite {
					r.Task = Task{}
				}
				return r
			}
		}},
		// The independent read's boot plant: a stop over a restored
		// confirmation serves the task instead of taking it back (O2).
		{"I1-boot-stop-serves-restored", I1, "", 1, func(d *decision) {
			d.store = func(s State, k Write, r Row) Row {
				if k == IntentWrite && s.Restored && s.M.Task.Conf != 0 {
					retire(&r, RServed, 0)
				}
				return r
			}
		}},
		// The independent read's error plant: a provider error during
		// recovery serves the task (A2: an invoked turn with an error is
		// never served).
		{"I1-provider-error-serves-in-recovery", I1, "", 1, func(d *decision) {
			d.store = func(s State, k Write, r Row) Row {
				if c := s.P.Cause; k == EvidenceWrite && s.Restored && c.K == KResult && c.A == AProvErr {
					retire(&r, RServed, c.R)
				}
				return r
			}
		}},
		// High 1 of the read: a lost resend clears the confirmation that an
		// older acknowledged prompt still holds, and a newer task replaces it.
		{"I1-loss-unconfirms-older-prompt", I1, ack + "wait " + next + "start :confirmed", 2, func(d *decision) {
			d.store = func(s State, k Write, r Row) Row {
				if c := s.P.Cause; k == DeliveryWrite && c.K == KOutcome && c.A == OcLost {
					r.Task.Conf = 0
				}
				return r
			}
		}},
		// Medium 3 of the read: the recovery bound fires first although the
		// hold's bound is earlier.
		{"deadline-recovery-before-hold", Late, "", 1, func(d *decision) {
			if d.input.K == KTimerHold && d.M.RecoverBy != 0 {
				d.input.K = KTimerRecov
			}
		}},
		// Medium 5 of the read: a hold whose pause has ended still stops the claim.
		{"ended-pause-stop", Ended, waiting + ":H2", 2, func(d *decision) { d.keepEnded = true }},
		// I3's launch snapshot: after a refused displacement's runtime stop
		// and a restore, a takeover's write loses the stored intent's causes.
		{"I3-takeover-loses-launch", I3, waiting + "refuses stalePlugin!5.1 :H3r", 1, func(d *decision) {
			d.store = func(_ State, k Write, r Row) Row {
				if k == ClaimWrite && r.Intent.By != 0 {
					r.Intent.Causes = 0
				}
				return r
			}
		}},
		// Oracle 1: the reader incorrectly restores an in-memory operator hold.
		{"boot-memory-restores-hold", BootMemory, work + "end op :op", 1, func(d *decision) {
			d.load = func(r Row) Row {
				r.Hold = Hold{By: SrcOperator, Request: SrcOperator, Deadline: 4}
				return r
			}
		}},
		// Oracle 2: a parked wake replaces the process during recovery.
		{"recovery-wake-replaces", RecoveryFence, work + "end park boot ok reconnect :doubt", 1, func(d *decision) {
			d.wakeBreaksRecovery = true
		}},
		// Oracle 3: receipt is remembered but the source's redeliverable item is lost.
		{"fatal-receipt-lost", FatalReceipt, waiting + ":H2", 1, func(d *decision) {
			d.dropFatal = true
		}},
		// Oracle 4: a received fatal remains pending but the model never applies it.
		{"settle-leaves-fatal", Settled, waiting + "bound!1.1 :pending", 1, func(d *decision) {
			d.skipFatalApply = true
		}},
		// Oracle 5: an earlier prompt's confirmation erases a resend's own timer.
		{"resend-loses-own-turn-bound", AttemptBound, ack + "wait " + next + "start end error :kept", 1, func(d *decision) {
			d.store = func(_ State, k Write, r Row) Row {
				if k == DeliveryWrite && r.Att[1].Stage == AtAcked {
					r.Att[1].TurnBy = 0
				}
				return r
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			from := roots
			if c.pre != "" {
				ps := replaySeed(t, seed{c.name, ready, c.pre})
				from = ps[len(ps)-1:]
			}
			out := explore(from, c.depth, func(s State, e Event) (State, Effects) {
				d := decision{State: s, input: e}
				c.variant(&d)
				return execute(d)
			})
			w, ok := out.Violations[c.bit]
			if !ok {
				t.Fatal("missed plant", c.name)
			}
			t.Logf("caught %s: %s + %v", c.name, w.Origin, w.Steps)
		})
	}
}
func invariantName(bit uint16) string {
	return map[uint16]string{
		I1: "I1", I2: "I2", I3: "I3", Twice: "told-twice",
		Late: "deadline", Ended: "ended-pause-stop", BootMemory: "boot-memory",
		RecoveryFence: "recovery-fence", FatalReceipt: "fatal-receipt",
		Settled: "settle", AttemptBound: "attempt-bound",
	}[bit]
}

// TestCleanRestartSendPlant removes D10's fence where it lives: the restore
// reads the stored row as main does, without its attempts (D12) and without
// doubt. The send path itself is the specified one (prepare, register, wire),
// and the driver must find the second request it makes.
func TestCleanRestartSendPlant(t *testing.T) {
	forget := func(r Row) Row {
		r.Task.Conf, r.Task.Unknown, r.RecoverBy = 0, false, 0
		for k, a := range r.Att {
			if a.open() {
				r.Att[k] = Attempt{}
			}
		}
		return r
	}
	step := func(s State, e Event) (State, Effects) {
		return execute(decision{State: s, input: e, load: forget})
	}
	s := ready
	for _, w := range strings.Fields(ack + "boot reconnect") {
		e := parse(t, w)
		if !s.base(e) {
			t.Fatalf("disabled %s", w)
		}
		s, _ = step(s, e)
	}
	out := explore([]Prefix{{s, "D10-restore-without-fence", 6}}, 3, step)
	w, ok := out.Violations[Twice]
	if !ok {
		t.Fatal("missed the second request after a restore without D10's fence")
	}
	t.Logf("caught D10-restore-without-fence: %s + %v", w.Origin, w.Steps)
}
func TestDisabled(t *testing.T) {
	for _, e := range []Event{{K: 255}, {K: KResult, R: 255}, {K: KWire}, {K: KWrongToken, Fault: LostReply, At: 1}} {
		if Enabled(ready, e) {
			t.Fatal("enabled", e.K)
		}
		s, x := Step(ready, e)
		if s != ready || x.N != 0 {
			t.Fatal("disabled input changed state")
		}
	}
}
