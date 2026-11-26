package holdmodel

import (
	"flag"
	"fmt"
	"testing"
)

var depth = flag.Int("hold-depth", 1, "maximum continuation length after each named prefix (full run: 3)")

func TestTheHoldStateMachine(t *testing.T) {
	if *depth < 1 {
		t.Fatal("depth must be positive")
	}
	var roots []Prefix
	seen := map[string]bool{}
	seeds := append(append(proofSeeds(), restartSeeds()...), orderSeeds()...)
	for _, c := range seeds {
		if seen[c.name] {
			t.Fatal("duplicate seed", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) { roots = append(roots, replaySeed(t, c)...) })
	}
	if t.Failed() {
		return
	}
	out := Explore(roots, *depth)
	fmt.Printf("depth=%d prefixes=%d roots=%d states=%d edges=%d sequences=%d seeds=%d violations=%d\n", *depth, len(roots), out.Roots, out.States, out.Edges, out.Sequences, len(seen), len(out.Violations))
	for bit, w := range out.Violations {
		t.Errorf("%s: %s + %v (length %d)", invariantName(bit), w.Origin, w.Steps, w.Length)
	}
}
func TestPlants(t *testing.T) {
	for _, c := range []struct {
		name string
		bit  uint8
		pre  string
		mut  func(Write, Row) Row
	}{
		{"I1-serve-on-turn-end", I1, held + ":H1", func(k Write, r Row) Row {
			if k == ClaimWrite && r.Phase == PhIdle && r.Task.Conf != 0 {
				retire(&r, RServed, 0)
			}
			return r
		}},
		{"I2-forget-answer-bound", I2, ack + "open :H0", func(k Write, r Row) Row {
			if r.Hold.By != 0 {
				r.Hold.Answer = 0
			}
			return r
		}},
		{"I3-claim-only-stop", I3, waiting + ":H2", func(k Write, r Row) Row {
			if k == StopWrite {
				r.Task = Task{}
			}
			return r
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ps := replaySeed(t, seed{c.name, ready, c.pre})
			roots := ps[len(ps)-1:]
			out := explore(roots, 1, func(s State, e Event) (State, Effects) {
				return execute(decision{State: s, input: e, store: c.mut})
			})
			w, ok := out.Violations[c.bit]
			if !ok {
				t.Fatal("missed plant", c.name)
			}
			t.Logf("caught %s: %s + %v", c.name, w.Origin, w.Steps)
		})
	}
}
func invariantName(bit uint8) string {
	return map[uint8]string{I1: "I1", I2: "I2", I3: "I3", Twice: "told-twice"}[bit]
}

func TestCleanRestartSendPlant(t *testing.T) {
	ps := replaySeed(t, seed{"D10-send-from-hello", ready, ack + "boot reconnect :doubt"})
	out := explore(ps[len(ps)-1:], 1, func(s State, e Event) (State, Effects) {
		if e.K == KHello {
			// The old request reached the wire. A premature resend here is
			// wrong even if the shim keeps a second copy out of the agent.
			d := decision{State: s}
			d.wire(0)
			return d.State, d.Effects
		}
		return run(s, e)
	})
	w, ok := out.Violations[Twice]
	if !ok {
		t.Fatal("missed premature resend")
	}
	t.Logf("caught D10-send-from-hello: %s + %v", w.Origin, w.Steps)
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
