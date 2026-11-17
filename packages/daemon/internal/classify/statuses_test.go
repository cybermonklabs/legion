package classify

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A settlement applied while a head run is open keeps the commit statuses it named through every
// replay of the run: the run's anchor holds the statuses kept before it opened, its step holds the
// settlement's own list, and both go through the store's JSON (stored) between every fact, as the
// daemon reads the record back. The statuses are each stamped with the head they were settled at,
// a redelivery of the settlement takes no step, and the push that closes the run leaves them as
// the replay found them. A move of the head leaves them stamped with the head they are of; the
// settlement of the new head starts its fence afresh and clears them with its check runs, through
// a replay too, and a reopen clears them again.
func TestStatusesSurviveAReplayUnderAnOpenHeadRun(t *testing.T) {
	ofH2 := []record.CommitStatus{{Context: "deploy", State: record.CommitStatusFailure, Head: "h2"}}
	pr := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{}, Generation: 1, Snapshot: "h2-1",
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1, Head: "h2", Conclusion: record.RunSucceeded}}, Statuses: ofH2}

	// h3 arrives before its push: the run opens, anchored at h2's settlement and its statuses.
	pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "h3"))
	if pr.Run == nil || !reflect.DeepEqual(pr.Run.Anchor.Statuses, ofH2) || !reflect.DeepEqual(pr.Statuses, ofH2) {
		t.Fatalf("h3 before its push: run %+v, statuses %+v; want the run open, anchored with h2's %+v", pr.Run, pr.Statuses, ofH2)
	}

	ofH3 := []record.CommitStatus{{Context: "deploy", State: record.CommitStatusSuccess, Head: "h3"},
		{Context: "lint", State: record.CommitStatusError, Head: "h3"}}
	h3 := SettlementCandidate{Head: "h3", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 1, Snapshot: "h3-1",
		Failing: []string{}, Statuses: []record.CommitStatus{{Context: "deploy", State: record.CommitStatusSuccess},
			{Context: "lint", State: record.CommitStatusError}}}
	settled, changed := ApplyCheckSettlement(pr, h3)
	pr = stored(t, settled)
	if !changed || pr.Run == nil || pr.CheckedHead != "h3" || !reflect.DeepEqual(pr.Statuses, ofH3) {
		t.Fatalf("h3's settlement under the run: changed %t, run %+v, checked %q, statuses %+v; want the run open and %+v",
			changed, pr.Run, pr.CheckedHead, pr.Statuses, ofH3)
	}
	if again, changed := ApplyCheckSettlement(pr, h3); changed || len(again.Run.Steps) != len(pr.Run.Steps) {
		t.Fatalf("h3's settlement delivered again: changed %t, %d steps; want no step beside the %d", changed, len(again.Run.Steps), len(pr.Run.Steps))
	}

	// The push of h3 replays the run from its anchor through the stored step, and closes it.
	pr = stored(t, ApplyPush(pr, record.ClassifiedPush{SHA: "h3", Before: "h2"}))
	if pr.Run != nil || pr.CheckedHead != "h3" || !reflect.DeepEqual(pr.Statuses, ofH3) {
		t.Fatalf("after h3's push: run %+v, checked %q, statuses %+v; want the run closed and %+v", pr.Run, pr.CheckedHead, pr.Statuses, ofH3)
	}

	// h4 arrives before its push, and its settlement names no status: the head's reset clears h3's.
	pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "h4"))
	if pr.Run == nil || !reflect.DeepEqual(pr.Statuses, ofH3) {
		t.Fatalf("h4 before its push: run %+v, statuses %+v; want the run open and h3's statuses kept, stamped h3", pr.Run, pr.Statuses)
	}
	settled, changed = ApplyCheckSettlement(pr, SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
		Generation: 1, Snapshot: "h4-1", Failing: []string{}})
	pr = stored(t, settled)
	if !changed || pr.CheckedHead != "h4" || len(pr.Statuses) != 0 {
		t.Fatalf("h4's settlement under the run: changed %t, checked %q, statuses %+v; want h4 checked and no status", changed, pr.CheckedHead, pr.Statuses)
	}
	pr = stored(t, ApplyPush(pr, record.ClassifiedPush{SHA: "h4", Before: "h3"}))
	if pr.Run != nil || pr.CheckedHead != "h4" || len(pr.Statuses) != 0 {
		t.Fatalf("after h4's push: run %+v, checked %q, statuses %+v; want the run closed and no status", pr.Run, pr.CheckedHead, pr.Statuses)
	}

	// A later settlement of h4 names a status, and a reopen starts the settlement afresh.
	settled, changed = ApplyCheckSettlement(pr, SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
		Generation: 2, Snapshot: "h4-2", Failing: []string{}, Statuses: []record.CommitStatus{{Context: "ci", State: record.CommitStatusFailure}}})
	if pr = stored(t, settled); !changed || !reflect.DeepEqual(pr.Statuses, []record.CommitStatus{{Context: "ci", State: record.CommitStatusFailure, Head: "h4"}}) {
		t.Fatalf("h4's second settlement: changed %t, statuses %+v; want ci failed at h4", changed, pr.Statuses)
	}
	if pr = stored(t, Reopen(pr, "h4")); pr.CheckedHead != "" || len(pr.Statuses) != 0 {
		t.Fatalf("after a reopen: checked %q, statuses %+v; want the settlement started afresh, no status kept", pr.CheckedHead, pr.Statuses)
	}
}

// A settlement the daemon does not apply leaves the commit statuses the pull request keeps as they
// are, whatever list it names: one older than the recorded one by its generation or by its runs,
// the same settlement delivered again, one that conflicts with it, and one of a head that is not
// the pull request's. Each is judged with no head run open and with one open, where the
// settlement is a step of the run and the replay with it changes nothing.
func TestAStaleSettlementLeavesTheStatusesAsTheyAre(t *testing.T) {
	kept := []record.CommitStatus{{Context: "deploy", State: record.CommitStatusFailure, Head: "h1"}}
	green := []record.CommitStatus{{Context: "deploy", State: record.CommitStatusSuccess}}
	settledAtH1 := func(t *testing.T, underRun bool) record.PullRequest {
		t.Helper()
		pr := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{}, Generation: 1, Snapshot: "h0-1",
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 9, Head: "h0", Conclusion: record.RunSucceeded}}}
		if !underRun {
			pr = ApplyPush(pr, record.ClassifiedPush{SHA: "h1", Before: "h0"})
		}
		pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "h1"))
		settled, changed := ApplyCheckSettlement(pr, SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}},
			Generation: 5, Snapshot: "h1-5", Failing: []string{}, Statuses: []record.CommitStatus{{Context: "deploy", State: record.CommitStatusFailure}}})
		pr = stored(t, settled)
		if !changed || (pr.Run != nil) != underRun || !reflect.DeepEqual(pr.Statuses, kept) {
			t.Fatalf("h1's settlement (a run open: %t): changed %t, run %+v, statuses %+v; want %+v", underRun, changed, pr.Run, pr.Statuses, kept)
		}
		return pr
	}
	for _, tc := range []struct {
		name      string
		candidate SettlementCandidate
	}{
		{"an older generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 4,
			Snapshot: "h1-4", Failing: []string{}, Statuses: green}},
		{"an older run", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 8}}, Generation: 6,
			Snapshot: "h1-6", Failing: []string{}, Statuses: green}},
		{"the same settlement again", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 5,
			Snapshot: "h1-5", Failing: []string{}, Statuses: green}},
		{"a conflicting settlement", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 5,
			Snapshot: "h1-5-recreated", Failing: []string{}, Statuses: green}},
		{"another head's settlement", SettlementCandidate{Head: "h0", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 11}}, Generation: 7,
			Snapshot: "h0-7", Failing: []string{}, Statuses: green}},
	} {
		for _, underRun := range []bool{false, true} {
			name := tc.name
			if underRun {
				name += ", under an open head run"
			}
			t.Run(name, func(t *testing.T) {
				pr := settledAtH1(t, underRun)
				got, changed := ApplyCheckSettlement(pr, tc.candidate)
				got = stored(t, got)
				if changed || got.Generation != 5 || got.Snapshot != "h1-5" || !reflect.DeepEqual(got.Statuses, kept) {
					t.Fatalf("changed %t, generation %d, snapshot %q, statuses %+v; want nothing changed and %+v kept",
						changed, got.Generation, got.Snapshot, got.Statuses, kept)
				}
			})
		}
	}
}
