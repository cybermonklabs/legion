package supervise

import "testing"

// wantReportOwed holds that the claim the machine publishes (View) and the one it reports
// (Claim) say the report of the take's turn is owed exactly as want says, and that the store
// holds no such mark: it is the daemon's in-memory view (Claim.ReportOwed, StoredClaim).
func (h *harness) wantReportOwed(want bool, when string) {
	h.t.Helper()
	if got := h.m.View().ReportOwed; got != want {
		h.t.Fatalf("%s the published view says the report is owed %t, want %t (the claim is %s holding %+v)", when, got, want, h.state(), h.claim().Pending)
	}
	if got := h.claim().ReportOwed; got != want {
		h.t.Fatalf("%s the claim the machine reports says the report is owed %t, want %t", when, got, want)
	}
	if stored := h.store.load(h.token); stored.ReportOwed {
		h.t.Fatalf("%s the store holds the claim with its report owed, want the mark in memory alone", when)
	}
}

// Row 39k of dispatch://INFRA-2811's plan: the claim a machine publishes says whether the report of
// its take's turn is owed (Claim.ReportOwed), which a pod update round reads to hold its early end
// for that report (internal/daemon's reportingClaims). It says so in waitReport alone, the take's
// turn over and its report not come, and for every harness: Oh My Pi, and Claude Code, whose
// launch installs no plugin (Deps.NoPluginInstall). It says nothing in waitNone (no task, a task no
// take holds, a task its report finished), in waitTake (the take awaiting its turn: no turn of
// its text ran), in waitTurn (the take's turn running: the claim is stored working already), or in
// waitFinish (the report came and the store refused its finish, which the end-frame bound makes).
//
// The negative control is the head this test was written at, where no published claim says the
// report is owed.
func TestTakeWhoseTurnEndedIsPublishedAsOwingItsReportInWaitReportAlone(t *testing.T) {
	for _, harness := range []struct {
		name string
		set  func(*Deps)
	}{
		{name: "Oh My Pi", set: func(*Deps) {}},
		{name: "Claude Code", set: func(d *Deps) { d.NoPluginInstall = func(Claim) bool { return true } }},
	} {
		t.Run(harness.name, func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin, withWaits, harness.set)
			h.manualTakes()
			h.reach(StateReady)
			h.wantReportOwed(false, "with no task (waitNone),")

			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			h.wantReportOwed(false, "with a task no take holds (waitNone),")

			h.mustTake()
			h.wantReportOwed(false, "with the take awaiting its turn (waitTake),")

			h.must(StreamTurnStart{Claim: h.token})
			h.wantReportOwed(false, "in the take's turn (waitTurn),")

			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)
			h.wantReportOwed(true, "with the take's turn over and its report not come (waitReport),")

			h.mustReport(nil)
			h.wantFinishedOnce(finishedByReport)
			h.wantReportOwed(false, "once the report finished the task (waitNone),")
		})
	}
	t.Run("a report whose finish the store refused", func(t *testing.T) {
		h, _ := owedFinishOverATurnThatRan(t, true)
		h.wantReportOwed(false, "with the report come and its finish owed (waitFinish),")
	})
}
