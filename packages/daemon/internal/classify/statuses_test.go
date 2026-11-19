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
// since, with a later created_at, makes it newer for its statuses alone: the kept status takes the
// new state, and the pull request keeps its own generation and snapshot. The same settlement
// naming the status the pull request already keeps says nothing new and changes nothing. One
// naming a context the pull request keeps no status of is applied as the first, and the kept
// statuses take the new context beside the ones they held.
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
		if got := ClassifySettlement(pr, candidate); got != SettlementStatusesOnly {
			t.Fatalf("the recreated record's settlement with ci's newer success is %s, want statuses-only: its generation is below the kept one", got)
		}
		got, changed := ApplySettlement(pr, candidate)
		got = stored(t, got)
		want := []record.CommitStatus{keptStatus("ci", record.CommitStatusSuccess, 2, "h1")}
		if !changed || !reflect.DeepEqual(got.Statuses, want) || got.Generation != 7 || got.Snapshot != "h1-7" {
			t.Fatalf("applied %t, statuses %+v, generation %d, snapshot %q; want it applied, %+v kept and the pull request's own 7, h1-7",
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

// settledAtH1 is a pull request whose head h1 is settled at ci's run 10, generation 5 and snapshot
// h1-5, with deploy's failure created at minute 2 the one status it keeps: with h1's push applied
// and no head run open, or, with underRun, with h1 arrived before its push, so that the
// settlement is a step of the head run that opened then. Each fact goes through the store's JSON
// (stored).
func settledAtH1(t *testing.T, underRun bool) record.PullRequest {
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
	kept := []record.CommitStatus{keptStatus("deploy", record.CommitStatusFailure, 2, "h1")}
	if !changed || (pr.Run != nil) != underRun || !reflect.DeepEqual(pr.Statuses, kept) {
		t.Fatalf("h1's settlement (a run open: %t): changed %t, run %+v, statuses %+v; want %+v", underRun, changed, pr.Run, pr.Statuses, kept)
	}
	return pr
}

// green is deploy's success created at minute 1, before the failure of deploy settledAtH1 keeps
// at minute 2, so it is no news of deploy against that pull request.
var green = []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 1)}

// withAndUnderRun runs check as the subtest name with no head run open, and again as name under
// an open head run, as settledAtH1 sets each up.
func withAndUnderRun(t *testing.T, name string, check func(t *testing.T, underRun bool)) {
	t.Helper()
	for _, underRun := range []bool{false, true} {
		subtest := name
		if underRun {
			subtest += ", under an open head run"
		}
		t.Run(subtest, func(t *testing.T) { check(t, underRun) })
	}
}

// A settlement applied whole, newer by its generation or by its check runs and so applied at its
// own generation and snapshot, merges the commit statuses it names into the kept ones by
// created_at: a status created before the kept one of its context leaves the kept one as it is,
// and one created in the same second takes its state, since the settlement is then the newer
// view. Each is judged with no head run open and with one open, where the settlement is a step of
// the run.
func TestASettlementAppliedWholeMergesItsStatusesByCreatedAt(t *testing.T) {
	// failedKept is the status settledAtH1 keeps: deploy's failure created at minute 2.
	failedKept := []record.CommitStatus{keptStatus("deploy", record.CommitStatusFailure, 2, "h1")}
	for _, tc := range []struct {
		name      string
		candidate SettlementCandidate
		// statuses is what the pull request keeps once the settlement is applied.
		statuses []record.CommitStatus
	}{
		{"a newer generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 6,
			Snapshot: "h1-6", Failing: []string{}, Statuses: green}, failedKept},
		{"a newer run", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 11}}, Generation: 6,
			Snapshot: "h1-6", Failing: []string{}, Statuses: green}, failedKept},
		{"a newer generation with a status created in the same second as the kept one", SettlementCandidate{Head: "h1",
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 6, Snapshot: "h1-6", Failing: []string{},
			Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2)}},
			[]record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 2, "h1")}},
	} {
		withAndUnderRun(t, tc.name, func(t *testing.T, underRun bool) {
			got, changed := ApplyCheckSettlement(settledAtH1(t, underRun), tc.candidate)
			got = stored(t, got)
			if !reflect.DeepEqual(got.Statuses, tc.statuses) {
				t.Fatalf("statuses %+v; want %+v kept: a settlement applied whole takes each status it names that is no older than the kept one of its context",
					got.Statuses, tc.statuses)
			}
			if !changed || got.Generation != tc.candidate.Generation || got.Snapshot != tc.candidate.Snapshot {
				t.Fatalf("changed %t, generation %d, snapshot %q; want the settlement applied at %d, %q",
					changed, got.Generation, got.Snapshot, tc.candidate.Generation, tc.candidate.Snapshot)
			}
		})
	}
}

// A settlement older for every check run it reports and every status it names changes nothing at
// all - one that names the kept status itself again among them, since a status created in the
// same second is no newer - and nor does any other settlement that names no newer status and is
// not applied: one older by its generation, the same settlement delivered again, one that
// conflicts with it, and one of a head that is not the pull request's. Each is judged with no
// head run open and with one open, where the settlement is a step of the run and the replay with
// it changes the same and no more.
func TestAnOlderStatusOfAContextChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate SettlementCandidate
	}{
		{"an older run", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 8}}, Generation: 6,
			Snapshot: "h1-6", Failing: []string{}, Statuses: green}},
		{"an older run naming the kept status again", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 8}},
			Generation: 6, Snapshot: "h1-6", Failing: []string{}, Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusFailure, 2)}}},
		{"an older generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 4,
			Snapshot: "h1-4", Failing: []string{}, Statuses: green}},
		{"the same settlement again", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 5,
			Snapshot: "h1-5", Failing: []string{}, Statuses: green}},
		{"a conflicting settlement", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}}, Generation: 5,
			Snapshot: "h1-5-recreated", Failing: []string{}, Statuses: green}},
		{"another head's settlement", SettlementCandidate{Head: "h0", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 11}}, Generation: 7,
			Snapshot: "h0-7", Failing: []string{}, Statuses: green}},
	} {
		withAndUnderRun(t, tc.name, func(t *testing.T, underRun bool) {
			pr := settledAtH1(t, underRun)
			got, changed := ApplyCheckSettlement(pr, tc.candidate)
			got = stored(t, got)
			if !reflect.DeepEqual(got.Statuses, pr.Statuses) {
				t.Fatalf("statuses %+v; want %+v kept: no status the settlement names is newer than deploy's kept failure", got.Statuses, pr.Statuses)
			}
			if changed || got.Generation != 5 || got.Snapshot != "h1-5" || !reflect.DeepEqual(got.CheckRuns, pr.CheckRuns) {
				t.Fatalf("changed %t, generation %d, snapshot %q, check runs %+v; want nothing changed from 5, h1-5, %+v",
					changed, got.Generation, got.Snapshot, got.CheckRuns, pr.CheckRuns)
			}
		})
	}
}

// A settlement that names a commit status newer than the kept one of its context, or one of a
// context the pull request keeps none of, and that neither its check runs nor its generation make
// newer changes the kept statuses alone (SettlementStatusesOnly): one whose check runs are older
// than the kept ones, whatever its generation, and one of the same check runs at a generation no
// higher than the kept one, lint's run left out or not - a settlement the listener sent before
// lint's run appeared compares equal to the fence of the one it sent after. The check runs, the
// names failing and cancelled, the generation and the snapshot stay as the newer settlement left
// them, though the settlement names e2e failing where the newer one has it cancelled. A status it
// names that was created in the same second as the kept one of its context leaves the kept one
// standing, since the settlement is the older view. The settlement delivered again, and the newer
// one delivered again after it, change nothing. Each is judged with no head run open and with one
// open, where the settlement is a step of the run.
func TestASettlementOfNoNewerRunsChangesItsNewerStatusesAlone(t *testing.T) {
	// newer settles h1 after settledAtH1's settlement: it adds lint's run, has e2e, a check GitHub
	// never started, cancelled, and names no status, as a record that dropped its statuses at the
	// size bound or a listener of an earlier release writes it.
	newer := SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}, {Name: "lint", ID: 3}}, Generation: 6,
		Snapshot: "h1-6", Failing: []string{}, Cancelled: []string{"e2e"}}
	fence := []record.AttemptRun{{Name: "ci", ID: 10, Head: "h1", Conclusion: record.RunSucceeded},
		{Name: "lint", ID: 3, Head: "h1", Conclusion: record.RunSucceeded}}
	// deployPassed is deploy's success, created after the kept failure of deploy.
	deployPassed := []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 3)}
	passedKept := []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 3, "h1")}
	for _, tc := range []struct {
		name      string
		candidate SettlementCandidate
		// statuses is what the pull request keeps once the settlement is applied.
		statuses []record.CommitStatus
	}{
		{"an older run at a higher generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 8}},
			Generation: 7, Snapshot: "h1-7", Failing: []string{"e2e"}, Cancelled: []string{}, Statuses: deployPassed}, passedKept},
		{"the same runs without lint at a lower generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}},
			Generation: 5, Snapshot: "h1-5", Failing: []string{"e2e"}, Cancelled: []string{}, Statuses: deployPassed}, passedKept},
		{"the same runs at the same generation", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}, {Name: "lint", ID: 3}},
			Generation: 6, Snapshot: "h1-6-recreated", Failing: []string{"e2e"}, Cancelled: []string{}, Statuses: deployPassed}, passedKept},
		{"a status created in the same second as the kept one", SettlementCandidate{Head: "h1", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 10}},
			Generation: 5, Snapshot: "h1-5", Failing: []string{"e2e"}, Cancelled: []string{},
			Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2), statusOf("sched-suite", record.CommitStatusError, 3)}},
			[]record.CommitStatus{keptStatus("deploy", record.CommitStatusFailure, 2, "h1"), keptStatus("sched-suite", record.CommitStatusError, 3, "h1")}},
	} {
		withAndUnderRun(t, tc.name, func(t *testing.T, underRun bool) {
			pr, changed := ApplyCheckSettlement(settledAtH1(t, underRun), newer)
			pr = stored(t, pr)
			if !changed || !reflect.DeepEqual(pr.CheckRuns, fence) || len(pr.Failing) != 0 || !reflect.DeepEqual(pr.Cancelled, []string{"e2e"}) ||
				pr.Generation != 6 || pr.Snapshot != "h1-6" {
				t.Fatalf("the newer settlement: changed %t, pull request %+v; want %+v, e2e cancelled at generation 6, h1-6: this test proves nothing without it",
					changed, pr, fence)
			}
			if got := ClassifySettlement(pr, tc.candidate); got != SettlementStatusesOnly {
				t.Fatalf("the settlement is %s, want statuses-only: neither its check runs nor its generation are newer", got)
			}
			got, changed := ApplyCheckSettlement(pr, tc.candidate)
			got = stored(t, got)
			if !changed || !reflect.DeepEqual(got.Statuses, tc.statuses) {
				t.Fatalf("changed %t, statuses %+v; want the settlement applied and %+v kept", changed, got.Statuses, tc.statuses)
			}
			if !reflect.DeepEqual(got.CheckRuns, pr.CheckRuns) || !reflect.DeepEqual(got.Failing, pr.Failing) || !reflect.DeepEqual(got.Cancelled, pr.Cancelled) ||
				got.Generation != pr.Generation || got.Snapshot != pr.Snapshot {
				t.Fatalf("check runs %+v, failing %v, cancelled %v at generation %d, snapshot %q; want the newer settlement's %+v, %v, %v at %d, %q",
					got.CheckRuns, got.Failing, got.Cancelled, got.Generation, got.Snapshot, pr.CheckRuns, pr.Failing, pr.Cancelled, pr.Generation, pr.Snapshot)
			}
			for _, again := range []SettlementCandidate{tc.candidate, newer} {
				if after, changed := ApplyCheckSettlement(got, again); changed || !reflect.DeepEqual(stored(t, after), got) {
					t.Fatalf("the settlement at generation %d, snapshot %q delivered again: changed %t, pull request %+v; want nothing changed from %+v",
						again.Generation, again.Snapshot, changed, after, got)
				}
			}
		})
	}
}
