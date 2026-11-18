package supervise

import "testing"

// A correction never grants past the rate while a credit is out that the views it re-reads cannot
// see. Two such credits are the gate's whole reason to count more than the wakes already parked
// (RefreshGate.out, dispatch://INFRA-3149): a refresh whose wake is having its turn, off the
// machine that holds it from wakeNow until the decision ends (endRefreshLane), and a post the pass
// has admitted that no machine has decided yet, which has parked no wake at all. Neither is in the
// views a sweep re-reads (internal/daemon's parkedWakes), so a correction taken while either is in
// flight reads fewer wakes than the lane holds, and only the gate's own count of the credits out
// keeps it from handing the difference out a second time.
//
// This row drives a real gate, where the other gate tests of this package drive a fake that
// records the asks (plugin_refresh_gate_test.go): what it reads is the bound the gate holds the
// machine to across a correction, which the fake cannot answer.
//
// The negative control is the gate before this fix, whose correction floored the count at the
// admissions granted since the sweep before and reset that count at each correction (gate.go's
// `g.held = max(waiting, g.granted); g.granted = 0`): the correction inside the turn then reads no
// wake in the views and no admission since, sets the count to zero over the K credits really out,
// and grants the admission this row shows refused. The report names the overlay that restores that
// gate and runs this row red.
func TestACorrectionInATurnOrOverAnUndecidedPostNeverGrantsPastTheRate(t *testing.T) {
	gate := NewRefreshGate(wakeRate)

	// One short of the rate is out over other claims of the same sweep, each a post the pass has
	// admitted that no machine of this test decides: a credit out that no view shows.
	for i := range wakeRate - 1 {
		if !gate.Admit() {
			t.Fatalf("admission %d of %d on a fresh gate refused, want the rate granted at once", i+1, wakeRate-1)
		}
	}

	// One real machine takes the last credit and parks its refresh wake, so the gate is full: the
	// wake this row drives into its turn, beside the wakeRate-1 posts no machine has decided.
	h := pacedHarness(t, wakeRate, func(d *Deps) { d.RefreshGate = gate })
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract
	if !gate.Admit() {
		t.Fatal("the pass's own admission over a gate one short of the rate refused, want the last credit granted")
	}
	h.must(StalePlugin{Claim: h.token, Trigger: "boot", Admitted: true})
	waiting := h.wantWaitingWake(WakePluginRefresh)

	// A correction with every credit of the sweep still out — this machine's parked wake the views
	// show, the wakeRate-1 undecided posts they do not — holds the gate full.
	gate.Correct(1)
	if gate.Admit() {
		t.Fatal("a correction with the rate out granted a further admission, want the lane held to the rate")
	}

	// Correct the gate while the refresh is having its turn: its wake is off the machine (wakeNow),
	// its claim is being written with no wake (putClaim), and the credit goes back only at the
	// decision's end (endRefreshLane). No view shows a refresh now — the turn took this one off,
	// and the wakeRate-1 posts parked none — so the correction reads nothing, and the gate's count
	// of the credits out is all that holds the lane to the rate.
	var corrected bool
	h.store.onPut = func(c Claim) {
		if corrected || c.State != StateLaunching {
			return
		}
		corrected = true
		gate.Correct(0)
		if gate.Admit() {
			t.Errorf("a correction inside the refresh wake's turn granted a further admission over the %d credits out, want none: the credit in flight was counted free", wakeRate)
		}
	}
	h.advance(waiting.At.Sub(h.clock.Now()))
	if !corrected {
		t.Fatal("the refresh's turn wrote no launching claim; the correction inside the turn never ran")
	}
}
