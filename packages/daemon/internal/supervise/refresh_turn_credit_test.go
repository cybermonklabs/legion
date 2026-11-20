package supervise

import "testing"

// A credit whose refresh is having its turn is still out while that turn runs, and a sweep's
// correction made then counts it, though the claim's view no longer shows the wake it paid for:
// the turn took the wake off the machine, and the credit goes back only at the end of the decision
// that runs the turn (endRefreshLane). The turn is held inside the relaunch it runs (the fake
// runtime's Resume), where the sweep reads the claim's view and corrects the gate from it, as the
// daemon's sweep does (internal/daemon's parkedWakes, refreshStalePlugins), and asks for the rest
// of the gate's share.
//
// The negative control is a correction that counts the credit only while its wake is in a view
// (the gate's count of credits out replaced by the credits whose wake is not yet published): the
// running turn's credit is then in neither, and the gate grants its whole share while the turn
// still holds one of it.
func TestACreditWhoseTurnIsRunningIsCountedByACorrectionMadeDuringTheTurn(t *testing.T) {
	gate := NewRefreshGate(wakeRate)
	h := pacedHarness(t, wakeRate, func(d *Deps) { d.RefreshGate = gate })
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Credit: gate.Admit()})
	waiting := h.wantWaitingWake(WakePluginRefresh)

	type reading struct {
		wakeShown bool
		granted   int
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
		gate.Correct(read)
		var granted []*RefreshCredit
		for c := gate.Admit(); c != nil && len(granted) <= wakeRate; c = gate.Admit() {
			granted = append(granted, c)
		}
		r.granted = len(granted)
		for _, c := range granted {
			c.Release()
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
	if r.granted != wakeRate-1 {
		t.Fatalf("a correction made while the refresh's turn ran left the gate granting %d, want %d: the running turn's credit is still out",
			r.granted, wakeRate-1)
	}
}
