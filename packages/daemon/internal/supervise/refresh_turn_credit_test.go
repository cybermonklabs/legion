package supervise

import "testing"

// freeCredits is how many credits gate grants now, at most wakeRate+1, each given back again at
// once, so a test can read the gate's free share without changing it.
func freeCredits(gate *RefreshGate) int {
	var granted []*RefreshCredit
	for len(granted) <= wakeRate {
		c := gate.Admit()
		if c == nil {
			break
		}
		granted = append(granted, c)
	}
	for _, c := range granted {
		c.Release()
	}
	return len(granted)
}

// A credit whose refresh is having its turn is still out while that turn runs, and every sweep's
// correction made then counts it, though the claim's view no longer shows the wake it paid for:
// the turn took the wake off the machine, and the credit goes back only at the end of the decision
// that runs the turn (endRefreshLane). The turn is held inside the relaunch it runs (the fake
// runtime's Resume), where the sweep reads the claim's view and corrects the gate from it, as the
// daemon's sweep does (internal/daemon's parkedWakes, refreshStalePlugins), and asks for the rest
// of the gate's share; then it does so again. The first correction keeps the credit because it was
// granted since the correction before it; the second asks the machine, which is deciding, and so
// keeps it too (reclaimCredit).
//
// The negative controls: a correction that counts the credit only while its wake is in a view
// (the gate's count of credits out replaced by the wakes read) grants the whole share at the first
// correction; and a correction that takes back a credit whose machine is deciding grants it at the
// second, both while the turn still holds one of the share.
func TestACreditWhoseTurnIsRunningIsCountedByACorrectionMadeDuringTheTurn(t *testing.T) {
	gate := NewRefreshGate(wakeRate)
	h := pacedHarness(t, wakeRate, func(d *Deps) { d.RefreshGate = gate })
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Credit: gate.Admit()})
	waiting := h.wantWaitingWake(WakePluginRefresh)

	type reading struct {
		wakeShown bool
		granted   [2]int
	}
	during := make(chan reading, 1)
	h.rt.HoldResume(func() {
		h.rt.HoldResume(nil)
		var r reading
		read := 0
		if w := h.m.View().WakeWaiting; w != nil && w.Kind == WakePluginRefresh {
			r.wakeShown = true
			read = 1
		}
		for sweep := range r.granted {
			gate.Correct(read)
			r.granted[sweep] = freeCredits(gate)
		}
		during <- r
	})

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantCalls("Resume", 1)
	var r reading
	select {
	case r = <-during:
	default:
		t.Fatal("the refresh's turn reached no relaunch, want it held inside the Resume it runs")
	}
	if r.wakeShown {
		t.Fatal("the claim's view still shows the refresh wake while its turn runs the relaunch, want the wake taken off the machine by the turn")
	}
	for sweep, granted := range r.granted {
		if granted != wakeRate-1 {
			t.Fatalf("correction %d made while the refresh's turn ran left the gate granting %d, want %d: the running turn's credit is still out",
				sweep+1, granted, wakeRate-1)
		}
	}
}

// A credit its machine no longer holds for a refresh wake is kept through the first correction
// after its admission and taken back at the second: the first correction keeps it, as every
// credit granted since the correction before is kept (RefreshGate.Correct), and the second asks
// the machine, which is between decisions, finds the credit no longer held for a wake, takes it
// back and says so. A machine that is deciding at the second correction keeps the credit (the
// running-turn row above holds that) until a later correction finds it between decisions. The loss
// is planted as a path that no decision's end reaches would make it: a locked section outside
// unlock ends the claim's refresh wake, so endRefreshLane never sees the credit, and either lets
// the credit go without giving it back or keeps it with no wake left to hold it for. The case
// beside them plants nothing: a machine between decisions that holds its credit for the refresh
// wake it waits on keeps it through both corrections, and no line is written.
//
// The negative controls: a correction that asks no machine (reclaimCredit never called) leaves
// the lost credit out at the second correction; a correction that asks every machine, the credits
// granted since the previous correction included, takes it back at the first; and a probe that
// does not read the refresh wake the machine waits on takes back the credit it still holds.
func TestACreditItsMachineNoLongerHoldsIsTakenBackAtTheSecondCorrection(t *testing.T) {
	for _, tc := range []struct {
		name string
		// plant is the path planted after the credited post parked its wake, nil for none.
		plant func(m *Machine)
	}{
		{"the machine let it go without giving it back", func(m *Machine) {
			m.disarm(TimerWake)
			m.wake = nil
			m.refreshCredit = nil
		}},
		{"the machine kept it with no refresh wake of its own", func(m *Machine) {
			m.disarm(TimerWake)
			m.wake = nil
		}},
		{"the machine holds it for the refresh wake it waits on", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewRefreshGate(wakeRate)
			h := pacedHarness(t, wakeRate, func(d *Deps) { d.RefreshGate = gate })
			h.reach(StateIdle)
			h.m.deps.PluginContract = refreshContract
			h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Credit: gate.Admit()})
			h.wantWaitingWake(WakePluginRefresh)

			lost := tc.plant != nil
			read := 1
			if lost {
				h.m.mu.Lock()
				tc.plant(h.m)
				h.m.publish()
				h.m.mu.Unlock()
				if w := h.m.View().WakeWaiting; w != nil {
					t.Fatalf("the claim's view shows a %s wake after the planted path ended it, want none", w.Kind)
				}
				read = 0
			}

			gate.Correct(read)
			if got := freeCredits(gate); got != wakeRate-1 {
				t.Fatalf("the first correction after the credit was granted left the gate granting %d, want %d: a credit granted since the correction before is kept",
					got, wakeRate-1)
			}
			if got := len(h.records.of(creditTakenBackMessage)); got != 0 {
				t.Fatalf("the first correction wrote %d lines of a credit taken back, want none", got)
			}
			gate.Correct(read)
			want, lines := wakeRate-1, 0
			if lost {
				want, lines = wakeRate, 1
			}
			if got := freeCredits(gate); got != want {
				t.Fatalf("the second correction left the gate granting %d, want %d: a credit its machine no longer holds for a refresh wake is taken back, and one it holds is kept",
					got, want)
			}
			if got := len(h.records.of(creditTakenBackMessage)); got != lines {
				t.Fatalf("the second correction wrote %d lines of a credit taken back, want %d", got, lines)
			}
			h.m.mu.Lock()
			held := h.m.refreshCredit
			h.m.mu.Unlock()
			if lost && held != nil {
				t.Fatal("the machine still holds the credit the correction took back, want it holding none, so its next relaunch asks the gate")
			}
			if !lost && held == nil {
				t.Fatal("the machine lost the credit its waiting refresh wake holds, want it kept")
			}
		})
	}
}

// A credited refresh post that reaches a claim whose row ignores it gives its credit back in the
// decision that ignores it: a suspended claim and a retired claim each receive the rate's worth
// of credited posts, and the gate grants its whole share again afterwards. The daemon's pass
// reads its claims from a snapshot, so a claim the snapshot shows idle can be suspended, retired,
// launching or failed by the time its post is decided; the machine then takes nothing, and the
// credit has no other holder to give it back (RefreshGate, the rule of return).
//
// The negative control is the settle of an untaken credit removed from Machine.Handle: each
// ignored post's credit then stays out, and the gate grants nothing after the rate's worth.
func TestACreditedRefreshPostAClaimIgnoresGivesItsCreditBack(t *testing.T) {
	for _, state := range []ClaimState{StateSuspended, StateRetired} {
		t.Run(string(state), func(t *testing.T) {
			gate := NewRefreshGate(wakeRate)
			h := pacedHarness(t, wakeRate, func(d *Deps) { d.RefreshGate = gate })
			if state == StateRetired {
				h.reach(StateIdle)
				h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
			} else {
				h.reach(state)
			}
			h.wantState(state)
			h.m.deps.PluginContract = refreshContract

			for i := range wakeRate {
				credit := gate.Admit()
				if credit == nil {
					t.Fatalf("the gate refused the %dth credited post to a %s claim, want every credit of the ignored posts back", i+1, state)
				}
				if err := h.handle(StalePlugin{Claim: h.token, Trigger: "sweep", Credit: credit}); err != nil {
					t.Fatalf("the credited post to a %s claim: %v", state, err)
				}
			}

			h.wantState(state)
			if got := admissions(t, gate, wakeRate+1); got != wakeRate {
				t.Fatalf("the gate granted %d after %d credited posts a %s claim ignored, want %d: an ignored post's credit comes back",
					got, wakeRate, state, wakeRate)
			}
		})
	}
}
