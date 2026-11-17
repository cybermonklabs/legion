package classify

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// statusAt is the created_at of a commit status these tests name: minute minutes past a fixed
// hour, in UTC as the decoder keeps it, so a later minute is a newer status of its context.
func statusAt(minute int) time.Time { return time.Date(2026, 11, 17, 5, minute, 0, 0, time.UTC) }

// statusOf is the commit status context in state created at minute, as a settlement names it:
// with no stamp.
func statusOf(context string, state record.CommitStatusState, minute int) record.CommitStatus {
	return record.CommitStatus{Context: context, State: state, CreatedAt: statusAt(minute)}
}

// keptStatus is that status as the pull request keeps it, stamped with the head it was settled at.
func keptStatus(context string, state record.CommitStatusState, minute int, head string) record.CommitStatus {
	status := statusOf(context, state, minute)
	status.Head = head
	return status
}

// A settlement applied while a head run is open keeps the commit statuses it named through every
// replay of the run: the run's anchor holds the statuses kept before it opened, its step holds the
// settlement's own list, and both go through the store's JSON (stored) between every fact, as the
// daemon reads the record back. The statuses are each stamped with the head they were settled at,
// a redelivery of the settlement takes no step, and the push that closes the run leaves them as
// the replay found them. A move of the head leaves them stamped with the head they are of; the
// settlement of the new head starts its fence afresh and clears them with its check runs, through
// a replay too. Each later settlement of that head is merged into the kept statuses per context
// by created_at: a newer status of a context replaces its state, an older one does not put the
// old state back, whatever generation it comes at, and a settlement that names no status leaves
// the kept ones as they are. A reopen clears them again.
func TestStatusesSurviveAReplayUnderAnOpenHeadRun(t *testing.T) {
	ofH2 := []record.CommitStatus{keptStatus("deploy", record.CommitStatusFailure, 1, "h2")}
	pr := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{}, Generation: 1, Snapshot: "h2-1",
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1, Head: "h2", Conclusion: record.RunSucceeded}}, Statuses: ofH2}

	// h3 arrives before its push: the run opens, anchored at h2's settlement and its statuses.
	pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "h3"))
	if pr.Run == nil || !reflect.DeepEqual(pr.Run.Anchor.Statuses, ofH2) || !reflect.DeepEqual(pr.Statuses, ofH2) {
		t.Fatalf("h3 before its push: run %+v, statuses %+v; want the run open, anchored with h2's %+v", pr.Run, pr.Statuses, ofH2)
	}

	ofH3 := []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 2, "h3"),
		keptStatus("lint", record.CommitStatusError, 2, "h3")}
	h3 := SettlementCandidate{Head: "h3", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 1, Snapshot: "h3-1",
		Failing: []string{}, Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2),
			statusOf("lint", record.CommitStatusError, 2)}}
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

	// A later settlement of h4 names a status.
	settled, changed = ApplyCheckSettlement(pr, SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
		Generation: 2, Snapshot: "h4-2", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("ci", record.CommitStatusFailure, 3)}})
	ciFailed := []record.CommitStatus{keptStatus("ci", record.CommitStatusFailure, 3, "h4")}
	if pr = stored(t, settled); !changed || !reflect.DeepEqual(pr.Statuses, ciFailed) {
		t.Fatalf("h4's second settlement: changed %t, statuses %+v; want %+v", changed, pr.Statuses, ciFailed)
	}

	// Each later settlement of h4 is merged into the kept statuses per context, by created_at.
	ciPassed := []record.CommitStatus{keptStatus("ci", record.CommitStatusSuccess, 4, "h4")}
	for _, step := range []struct {
		what      string
		candidate SettlementCandidate
		changed   bool
	}{
		{"a newer status of ci that passed replaces its failure", SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
			Generation: 3, Snapshot: "h4-3", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("ci", record.CommitStatusSuccess, 4)}}, true},
		{"ci's older failure at a later generation does not put it back", SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
			Generation: 4, Snapshot: "h4-4", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("ci", record.CommitStatusFailure, 3)}}, true},
		{"ci's older failure with an older run does not put it back", SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}},
			Generation: 5, Snapshot: "h4-5", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("ci", record.CommitStatusFailure, 3)}}, false},
		{"a settlement that names no status leaves the kept ones", SettlementCandidate{Head: "h4", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
			Generation: 6, Snapshot: "h4-6", Failing: []string{}}, true},
	} {
		settled, changed = ApplyCheckSettlement(pr, step.candidate)
		pr = stored(t, settled)
		if changed != step.changed || !reflect.DeepEqual(pr.Statuses, ciPassed) {
			t.Fatalf("%s: changed %t, statuses %+v; want changed %t and %+v", step.what, changed, pr.Statuses, step.changed, ciPassed)
		}
	}
	if pr.Generation != 6 || pr.Snapshot != "h4-6" {
		t.Fatalf("after h4's later settlements the pull request is at generation %d, snapshot %q; want the last applied one's 6, h4-6",
			pr.Generation, pr.Snapshot)
	}

	// A reopen starts the settlement afresh.
	if pr = stored(t, Reopen(pr, "h4")); pr.CheckedHead != "" || len(pr.Statuses) != 0 {
		t.Fatalf("after a reopen: checked %q, statuses %+v; want the settlement started afresh, no status kept", pr.CheckedHead, pr.Statuses)
	}
}

// The listener numbers the settlements of a record the bucket's TTL removed and the reconcile
// wrote again afresh, so after such a recreation a settlement of the same head and the same check
// runs comes at a generation below the one the pull request keeps. A status that changed state
// since, with a later created_at, makes it newer all the same: it is applied and the kept status
// takes the new state. The same settlement naming the status the pull request already keeps says
// nothing new and changes nothing. One naming a context the pull request keeps no status of is
// applied, and the kept statuses take the new context beside the ones they held.
func TestAStatusChangeAfterTheRecordWasRecreatedIsApplied(t *testing.T) {
	pr := record.PullRequest{HeadSHA: "h1", CheckedHead: "h1", Failing: []string{}, Cancelled: []string{}, Generation: 7, Snapshot: "h1-7",
		CheckRuns: []record.AttemptRun{{Name: "build", ID: 5, Head: "h1", Conclusion: record.RunSucceeded}},
		Statuses:  []record.CommitStatus{keptStatus("ci", record.CommitStatusFailure, 1, "h1")}}
	recreated := func(statuses ...record.CommitStatus) SettlementCandidate {
		return SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "build", ID: 5}}, Generation: 0, Snapshot: "h1-recreated",
			Failing: []string{}, Cancelled: []string{}, Statuses: statuses}
	}

	t.Run("a status that changed state", func(t *testing.T) {
		candidate := recreated(statusOf("ci", record.CommitStatusSuccess, 2))
		if got := ClassifySettlement(pr, candidate); got != SettlementNewer {
			t.Fatalf("the recreated record's settlement with ci's newer success is %s, want newer whatever its generation", got)
		}
		got, changed := ApplySettlement(pr, candidate)
		got = stored(t, got)
		want := []record.CommitStatus{keptStatus("ci", record.CommitStatusSuccess, 2, "h1")}
		if !changed || !reflect.DeepEqual(got.Statuses, want) || got.Generation != 0 || got.Snapshot != "h1-recreated" {
			t.Fatalf("applied %t, statuses %+v, generation %d, snapshot %q; want it applied, %+v kept and the candidate's numbering",
				changed, got.Statuses, got.Generation, got.Snapshot, want)
		}
	})

	t.Run("the status the pull request keeps", func(t *testing.T) {
		candidate := recreated(statusOf("ci", record.CommitStatusFailure, 1))
		if got := ClassifySettlement(pr, candidate); got != SettlementStale && got != SettlementConflict {
			t.Fatalf("the recreated record's settlement naming the kept status is %s, want stale or conflict: it says nothing new", got)
		}
		if got, changed := ApplySettlement(pr, candidate); changed || !reflect.DeepEqual(got, pr) {
			t.Fatalf("applied %t, pull request %+v; want nothing changed from %+v", changed, got, pr)
		}
	})

	t.Run("a new context", func(t *testing.T) {
		candidate := recreated(statusOf("ci", record.CommitStatusFailure, 1), statusOf("deploy", record.CommitStatusSuccess, 1))
		got, changed := ApplySettlement(pr, candidate)
		got = stored(t, got)
		want := []record.CommitStatus{keptStatus("ci", record.CommitStatusFailure, 1, "h1"), keptStatus("deploy", record.CommitStatusSuccess, 1, "h1")}
		if !changed || !reflect.DeepEqual(got.Statuses, want) {
			t.Fatalf("applied %t, statuses %+v; want it applied and %+v kept", changed, got.Statuses, want)
		}
	})
}

// A settlement the daemon does not apply leaves the commit statuses the pull request keeps as they
// are, whatever list it names so long as no status in it is newer than the kept one of its
// context: one older than the recorded one by its generation or by its runs, the same settlement
// delivered again, one that conflicts with it, and one of a head that is not the pull request's.
// Each is judged with no head run open and with one open, where the settlement is a step of the
// run and the replay with it changes nothing.
func TestAStaleSettlementLeavesTheStatusesAsTheyAre(t *testing.T) {
	kept := []record.CommitStatus{keptStatus("deploy", record.CommitStatusFailure, 2, "h1")}
	// green is deploy's success created before the kept failure, so it is no news of deploy.
	green := []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 1)}
	settledAtH1 := func(t *testing.T, underRun bool) record.PullRequest {
		t.Helper()
		pr := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{}, Generation: 1, Snapshot: "h0-1",
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 9, Head: "h0", Conclusion: record.RunSucceeded}}}
		if !underRun {
			pr = ApplyPush(pr, record.ClassifiedPush{SHA: "h1", Before: "h0"})
		}
		pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "h1"))
		settled, changed := ApplyCheckSettlement(pr, SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}},
			Generation: 5, Snapshot: "h1-5", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusFailure, 2)}})
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
