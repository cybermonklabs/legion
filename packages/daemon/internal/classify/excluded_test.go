package classify

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// exHead is the head every settlement below is of.
const exHead = "h1"

// exRun is a check run as a settlement reports it: its name and id alone.
func exRun(name string, id int64) record.AttemptRun { return record.AttemptRun{Name: name, ID: id} }

// exFenced is that run as the fence keeps it, stamped with exHead and what it concluded.
func exFenced(name string, id int64, conclusion record.RunConclusion) record.AttemptRun {
	return record.AttemptRun{Name: name, ID: id, Head: exHead, Conclusion: conclusion}
}

// exSettlement is a settlement of exHead at generation with runs, the names failing, and the runs
// excluded.
func exSettlement(generation int64, runs []record.AttemptRun, failing []string, excluded ...record.AttemptRun) SettlementCandidate {
	return SettlementCandidate{Head: exHead, CheckRuns: runs, Generation: generation, Snapshot: fmt.Sprintf("%s-%d", exHead, generation),
		Failing: append([]string{}, failing...), Cancelled: []string{}, Excluded: excluded}
}

// exSettled is a pull request at exHead with nothing settled, given each settlement in turn as
// the workflow gives it (ApplyCheckSettlement), each of which must apply.
func exSettled(t *testing.T, settlements ...SettlementCandidate) record.PullRequest {
	t.Helper()
	pr := record.PullRequest{HeadSHA: exHead, Failing: []string{}, Cancelled: []string{}}
	for i, settlement := range settlements {
		var applied bool
		if pr, applied = ApplyCheckSettlement(pr, settlement); !applied {
			t.Fatalf("settlement %d (%+v) was not applied to %+v", i, settlement, pr)
		}
	}
	return stored(t, pr)
}

// The listener counted ci-ok's run 20, then learned that GitHub does not count its workflow run
// for the pull request (a workflow_dispatch run of the branch) and settles again with the lower
// run 10 that stands in its place, naming 20 excluded. The daemon takes the correction whichever
// way it turns the head: a green run 20 excluded for a red run 10 leaves the head red, and a red
// run 20 excluded for a green run 10 leaves it green. Before, the lower id read as an older view
// and the settlement was stale, so the head kept the excluded run's verdict.
func TestALowerIDThatStandsInAnExcludedRunsPlaceIsApplied(t *testing.T) {
	for _, tc := range []struct {
		name           string
		before, after  []string
		want           []string
		wantConclusion record.RunConclusion
	}{
		{"green to red", nil, []string{"ci-ok"}, []string{"ci-ok"}, record.RunFailed},
		{"red to green", []string{"ci-ok"}, nil, []string{}, record.RunSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := exSettled(t, exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, tc.before))
			correction := exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, tc.after, exRun("ci-ok", 20))
			if got := ClassifySettlement(pr, correction); got != SettlementNewer {
				t.Fatalf("the correction is classified %s, want %s: a lower id standing in an excluded run's place is newer", got, SettlementNewer)
			}
			got, applied := ApplyCheckSettlement(pr, correction)
			got = stored(t, got)
			if !applied || got.Generation != 3 || got.Snapshot != correction.Snapshot {
				t.Fatalf("applied %t at generation %d, snapshot %q; want the correction applied at 3", applied, got.Generation, got.Snapshot)
			}
			if want := []record.AttemptRun{exFenced("ci-ok", 10, tc.wantConclusion)}; !reflect.DeepEqual(got.CheckRuns, want) {
				t.Fatalf("fence %+v, want %+v: run 10 in the excluded run's place", got.CheckRuns, want)
			}
			if !sameItems(got.Failing, tc.want) {
				t.Fatalf("failing %v, want %v", got.Failing, tc.want)
			}
			if want := []record.AttemptRun{exRun("ci-ok", 20)}; !reflect.DeepEqual(got.ExcludedRuns, want) {
				t.Fatalf("excluded runs %+v, want %+v kept", got.ExcludedRuns, want)
			}
		})
	}
}

// A check whose only run the listener excluded is no check of the head any more: the correction
// names no run of ci-ok at all, so the fence holds none, and the failure of the excluded run is no
// failure of the head. With no exclusion the same settlement keeps ci-ok failing, as a settlement
// that leaves a failing check out always has (EffectiveOutcome), which is what the exclusion
// changes.
func TestAFailureOnlyAnExcludedRunHadLeavesTheFenceAndTheFailingNames(t *testing.T) {
	pr := exSettled(t, exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20), exRun("unit", 15)}, []string{"ci-ok"}))
	correction := exSettlement(3, []record.AttemptRun{exRun("unit", 15)}, nil, exRun("ci-ok", 20))

	if outcome := EffectiveOutcome(pr, correction); len(outcome.Failing) != 0 {
		t.Fatalf("EffectiveOutcome failing %v, want none: the failure was the excluded run's", outcome.Failing)
	}
	got, applied := ApplyCheckSettlement(pr, correction)
	got = stored(t, got)
	if want := []record.AttemptRun{exFenced("unit", 15, record.RunSucceeded)}; !applied || !reflect.DeepEqual(got.CheckRuns, want) {
		t.Fatalf("applied %t, fence %+v; want %+v: no run of ci-ok left", applied, got.CheckRuns, want)
	}
	if len(got.Failing) != 0 || len(got.Cancelled) != 0 {
		t.Fatalf("failing %v, cancelled %v; want none", got.Failing, got.Cancelled)
	}
	if want := []record.AttemptRun{exRun("ci-ok", 20)}; !reflect.DeepEqual(got.ExcludedRuns, want) {
		t.Fatalf("excluded runs %+v, want %+v kept", got.ExcludedRuns, want)
	}

	control := correction
	control.Excluded = nil
	if outcome := EffectiveOutcome(pr, control); !reflect.DeepEqual(outcome.Failing, []string{"ci-ok"}) {
		t.Fatalf("with no exclusion EffectiveOutcome failing %v, want [ci-ok] kept as a check the settlement leaves out", outcome.Failing)
	}
}

// A correction that excludes a run the fence holds applies at any generation. Two records of one
// head (the bucket's TTL removed one, and the reconcile wrote it again) number their settlements
// apart, so the correction can come at a generation no higher than the kept one; with the same
// check runs left and no exclusion it would be a conflict or stale, and the excluded run's failure
// would stand.
func TestACorrectionOfTheFenceAppliesAtAGenerationNoHigher(t *testing.T) {
	pr := exSettled(t, exSettlement(5, []record.AttemptRun{exRun("ci-ok", 20), exRun("unit", 15)}, []string{"ci-ok"}))
	for _, generation := range []int64{5, 1} {
		t.Run(fmt.Sprintf("generation %d", generation), func(t *testing.T) {
			correction := exSettlement(generation, []record.AttemptRun{exRun("unit", 15)}, nil, exRun("ci-ok", 20))
			correction.Snapshot = fmt.Sprintf("recreated-%d", generation)
			if got := ClassifySettlement(pr, correction); got != SettlementNewer {
				t.Fatalf("the correction is classified %s, want %s", got, SettlementNewer)
			}
			got, applied := ApplyCheckSettlement(pr, correction)
			got = stored(t, got)
			if want := []record.AttemptRun{exFenced("unit", 15, record.RunSucceeded)}; !applied || !reflect.DeepEqual(got.CheckRuns, want) ||
				len(got.Failing) != 0 {
				t.Fatalf("applied %t, fence %+v, failing %v; want %+v and nothing failing", applied, got.CheckRuns, got.Failing, want)
			}
		})
	}
}

// A settlement newer for its commit statuses alone (SettlementStatusesOnly) adds the runs it names
// excluded to the kept ones, as a settlement applied whole does, and changes nothing of the fence.
func TestASettlementNewerForItsStatusesAloneKeepsTheRunsItExcludes(t *testing.T) {
	corrected := exSettled(t, exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, nil),
		exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, nil, exRun("ci-ok", 20)))
	late := exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, nil, exRun("ci-ok", 20), exRun("lint", 30))
	late.Statuses = []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 1)}
	if got := ClassifySettlement(corrected, late); got != SettlementStatusesOnly {
		t.Fatalf("the settlement is classified %s, want %s", got, SettlementStatusesOnly)
	}
	got, applied := ApplySettlement(corrected, late)
	if want := []record.AttemptRun{exRun("ci-ok", 20), exRun("lint", 30)}; !applied || !reflect.DeepEqual(got.ExcludedRuns, want) {
		t.Fatalf("applied %t, excluded runs %+v; want %+v", applied, got.ExcludedRuns, want)
	}
	if !reflect.DeepEqual(got.CheckRuns, corrected.CheckRuns) || got.Generation != corrected.Generation ||
		!reflect.DeepEqual(got.Statuses, []record.CommitStatus{keptStatus("deploy", record.CommitStatusSuccess, 1, exHead)}) {
		t.Fatalf("fence %+v at %d, statuses %+v; want the fence %+v at %d and deploy's status kept", got.CheckRuns, got.Generation,
			got.Statuses, corrected.CheckRuns, corrected.Generation)
	}
}

// The settlement the listener published before the correction still names the excluded run, at a
// higher id and a lower generation. Delivered after the correction it is stale: the excluded run
// is no run of the head, so what is left of it is no newer than the fence, and the record stays
// exactly as the correction left it. That holds with a lower run in the excluded run's place and
// with no run of that name left.
func TestTheSettlementBeforeACorrectionDeliveredAfterItIsStale(t *testing.T) {
	for _, tc := range []struct {
		name               string
		before, correction SettlementCandidate
	}{
		{"a lower run in its place, green to red",
			exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, nil),
			exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, []string{"ci-ok"}, exRun("ci-ok", 20))},
		{"a lower run in its place, red to green",
			exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, []string{"ci-ok"}),
			exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, nil, exRun("ci-ok", 20))},
		{"no run of its name left",
			exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20), exRun("unit", 15)}, []string{"ci-ok"}),
			exSettlement(3, []record.AttemptRun{exRun("unit", 15)}, nil, exRun("ci-ok", 20))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corrected := exSettled(t, tc.before, tc.correction)
			if got := ClassifySettlement(corrected, tc.before); got != SettlementStale {
				t.Fatalf("the settlement before the correction is classified %s, want %s", got, SettlementStale)
			}
			if got, applied := ApplySettlement(corrected, tc.before); applied || !reflect.DeepEqual(got, corrected) {
				t.Fatalf("ApplySettlement applied %t, record %+v; want nothing applied and %+v unchanged", applied, got, corrected)
			}
			if got, changed := ApplyCheckSettlement(corrected, tc.before); changed || !reflect.DeepEqual(got, corrected) {
				t.Fatalf("ApplyCheckSettlement changed %t, record %+v; want %+v unchanged", changed, got, corrected)
			}
		})
	}
}

// A settlement from before the correction can still be newer for another check: here it carries
// unit's re-run 16 beside the excluded ci-ok run 20. It applies for unit, and brings back neither
// the excluded run nor what it concluded. Where a lower run of ci-ok stands in the fence, ci-ok
// comes to what that run concluded (concludedBy), whatever the late settlement's lists say of the
// excluded run and whatever the record said before; where none is left, ci-ok is no check of the
// head.
func TestALateSettlementNewerForAnotherCheckBringsNoExcludedRunBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// excludedFailed and lowerFailed are whether the excluded run 20 and the run 10 in its
		// place failed; lower says whether the correction has a run 10 at all.
		excludedFailed, lower, lowerFailed bool
		fence                              []record.AttemptRun
		failing                            []string
	}{
		{"the excluded run failed, the run in its place passed", true, true, false,
			[]record.AttemptRun{exFenced("ci-ok", 10, record.RunSucceeded), exFenced("unit", 16, record.RunSucceeded)}, []string{}},
		{"the excluded run passed, the run in its place failed", false, true, true,
			[]record.AttemptRun{exFenced("ci-ok", 10, record.RunFailed), exFenced("unit", 16, record.RunSucceeded)}, []string{"ci-ok"}},
		{"the excluded run failed, no run in its place", true, false, false,
			[]record.AttemptRun{exFenced("unit", 16, record.RunSucceeded)}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named := func(failed bool) []string {
				if failed {
					return []string{"ci-ok"}
				}
				return nil
			}
			first := exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20), exRun("unit", 15)}, named(tc.excludedFailed))
			correctionRuns := []record.AttemptRun{exRun("unit", 15)}
			if tc.lower {
				correctionRuns = []record.AttemptRun{exRun("ci-ok", 10), exRun("unit", 15)}
			}
			corrected := exSettled(t, first, exSettlement(4, correctionRuns, named(tc.lowerFailed), exRun("ci-ok", 20)))
			late := exSettlement(3, []record.AttemptRun{exRun("ci-ok", 20), exRun("unit", 16)}, named(tc.excludedFailed))
			if got := ClassifySettlement(corrected, late); got != SettlementNewer {
				t.Fatalf("the late settlement is classified %s, want %s for unit's re-run", got, SettlementNewer)
			}
			got, applied := ApplyCheckSettlement(corrected, late)
			got = stored(t, got)
			if !applied || !reflect.DeepEqual(got.CheckRuns, tc.fence) {
				t.Fatalf("applied %t, fence %+v; want %+v: unit's re-run and no excluded run", applied, got.CheckRuns, tc.fence)
			}
			if !sameItems(got.Failing, tc.failing) {
				t.Fatalf("failing %v, want %v", got.Failing, tc.failing)
			}
			if want := []record.AttemptRun{exRun("ci-ok", 20)}; !reflect.DeepEqual(got.ExcludedRuns, want) {
				t.Fatalf("excluded runs %+v, want %+v kept", got.ExcludedRuns, want)
			}
		})
	}
}

// The control: a re-run of a counted check, at a higher id, still replaces its run and its
// verdict, with no exclusion anywhere and after an exclusion of an earlier run of the same check.
func TestAHigherIDReRunOfACountedCheckStillApplies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []SettlementCandidate
		rerun  SettlementCandidate
	}{
		{"no exclusion",
			[]SettlementCandidate{exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, []string{"ci-ok"})},
			exSettlement(3, []record.AttemptRun{exRun("ci-ok", 21)}, nil)},
		{"after an exclusion of run 20",
			[]SettlementCandidate{exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, nil),
				exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, []string{"ci-ok"}, exRun("ci-ok", 20))},
			exSettlement(4, []record.AttemptRun{exRun("ci-ok", 21)}, nil, exRun("ci-ok", 20))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := exSettled(t, tc.before...)
			if !reflect.DeepEqual(pr.Failing, []string{"ci-ok"}) {
				t.Fatalf("failing %v before the re-run, want [ci-ok]", pr.Failing)
			}
			got, applied := ApplyCheckSettlement(pr, tc.rerun)
			got = stored(t, got)
			if want := []record.AttemptRun{exFenced("ci-ok", 21, record.RunSucceeded)}; !applied || !reflect.DeepEqual(got.CheckRuns, want) || len(got.Failing) != 0 {
				t.Fatalf("applied %t, fence %+v, failing %v; want %+v and nothing failing", applied, got.CheckRuns, got.Failing, want)
			}
		})
	}
}

// Two corrections that exclude different runs leave the same fence, the same failing and
// cancelled names and the same excluded runs in either order. Each case starts from ci-ok's run
// 20 and lint's run 30, both counted and both failing; one correction excludes 20 for ci-ok's
// passing run 10, the other 30 for lint's passing run 25. A listener names every run it excluded
// before in each later settlement, so the second correction names both; two records of one head
// (the bucket's TTL removed one) can each name only their own.
func TestTwoCorrectionsInEitherOrderLeaveTheSameFence(t *testing.T) {
	first := exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20), exRun("lint", 30)}, []string{"ci-ok", "lint"})
	for _, tc := range []struct {
		name string
		a, b SettlementCandidate
	}{
		{"the later correction names both",
			exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10), exRun("lint", 30)}, []string{"lint"}, exRun("ci-ok", 20)),
			exSettlement(4, []record.AttemptRun{exRun("ci-ok", 10), exRun("lint", 25)}, nil, exRun("ci-ok", 20), exRun("lint", 30))},
		{"each names its own",
			exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10), exRun("lint", 30)}, []string{"lint"}, exRun("ci-ok", 20)),
			exSettlement(4, []record.AttemptRun{exRun("ci-ok", 20), exRun("lint", 25)}, []string{"ci-ok"}, exRun("lint", 30))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ab := exSettled(t, first)
			ab, _ = ApplyCheckSettlement(ab, tc.a)
			ab, _ = ApplyCheckSettlement(ab, tc.b)
			ba := exSettled(t, first)
			ba, _ = ApplyCheckSettlement(ba, tc.b)
			ba, _ = ApplyCheckSettlement(ba, tc.a)
			ab, ba = stored(t, ab), stored(t, ba)
			fence := []record.AttemptRun{exFenced("ci-ok", 10, record.RunSucceeded), exFenced("lint", 25, record.RunSucceeded)}
			excluded := []record.AttemptRun{exRun("ci-ok", 20), exRun("lint", 30)}
			for order, got := range map[string]record.PullRequest{"a then b": ab, "b then a": ba} {
				if !reflect.DeepEqual(got.CheckRuns, fence) || len(got.Failing) != 0 || len(got.Cancelled) != 0 ||
					!reflect.DeepEqual(got.ExcludedRuns, excluded) {
					t.Errorf("%s: fence %+v, failing %v, cancelled %v, excluded %+v; want %+v, nothing failing or cancelled, %+v",
						order, got.CheckRuns, got.Failing, got.Cancelled, got.ExcludedRuns, fence, excluded)
				}
			}
		})
	}
}

// Under an open head run (a head that arrived before its push) the correction is a step of the
// run, and its exclusion is replayed with it: the judgement changes, the fence and the failing
// names are the correction's, and the run keeps the excluded run through the store's JSON. The
// same correction delivered again is redundant, and so is the settlement from before it.
func TestAnOpenHeadRunReplaysAnExclusion(t *testing.T) {
	pr := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{}, Generation: 1, Snapshot: "h0-1",
		CheckRuns: []record.AttemptRun{{Name: "ci-ok", ID: 9, Head: "h0", Conclusion: record.RunSucceeded}}}
	pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, exHead))
	if pr.Run == nil {
		t.Fatal("no head run open after h1 arrived before its push")
	}
	before := exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, nil)
	pr, changed := ApplyCheckSettlement(pr, before)
	pr = stored(t, pr)
	if !changed || pr.Run == nil || !reflect.DeepEqual(pr.Failing, []string{}) {
		t.Fatalf("the first settlement: changed %t, run %+v, failing %v; want it a step of the open run, nothing failing", changed, pr.Run, pr.Failing)
	}

	correction := exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, []string{"ci-ok"}, exRun("ci-ok", 20))
	corrected, changed := ApplyCheckSettlement(pr, correction)
	corrected = stored(t, corrected)
	if !changed || corrected.Run == nil {
		t.Fatalf("the correction: changed %t, run %+v; want the judgement changed, the run still open", changed, corrected.Run)
	}
	if want := []record.AttemptRun{exFenced("ci-ok", 10, record.RunFailed)}; !reflect.DeepEqual(corrected.CheckRuns, want) ||
		!reflect.DeepEqual(corrected.Failing, []string{"ci-ok"}) {
		t.Fatalf("fence %+v, failing %v; want %+v and ci-ok failing", corrected.CheckRuns, corrected.Failing, want)
	}
	excluded := []record.AttemptRun{exRun("ci-ok", 20)}
	if !reflect.DeepEqual(corrected.ExcludedRuns, excluded) {
		t.Fatalf("excluded runs %+v, want %+v", corrected.ExcludedRuns, excluded)
	}
	if steps := corrected.Run.Steps; steps[len(steps)-1].Settlement == nil || !reflect.DeepEqual(steps[len(steps)-1].Settlement.Excluded, excluded) {
		t.Fatalf("the run's last step %+v, want the correction naming %+v excluded", steps[len(steps)-1], excluded)
	}

	for name, settlement := range map[string]SettlementCandidate{"the correction again": correction, "the settlement before it": before} {
		if got, changed := ApplyCheckSettlement(corrected, settlement); changed || !reflect.DeepEqual(got, corrected) {
			t.Errorf("%s: changed %t, record %+v; want it redundant and %+v unchanged", name, changed, got, corrected)
		}
	}
}

// A settlement of a new checked head, and a reopen, start the head's settlement afresh, and the
// excluded runs go with the rest of it: they were runs of the head the record no longer checks.
func TestANewCheckedHeadAndAReopenClearTheExcludedRuns(t *testing.T) {
	corrected := exSettled(t, exSettlement(2, []record.AttemptRun{exRun("ci-ok", 20)}, nil),
		exSettlement(3, []record.AttemptRun{exRun("ci-ok", 10)}, nil, exRun("ci-ok", 20)))
	if len(corrected.ExcludedRuns) != 1 {
		t.Fatalf("excluded runs %+v after the correction, want one", corrected.ExcludedRuns)
	}
	moved := corrected
	moved.HeadSHA = "h2"
	if stood, stands := SettlementFor(moved, SettlementCandidate{Head: "h2"}); !stands || stood.ExcludedRuns != nil {
		t.Fatalf("a settlement of the new head h2: stands %t, excluded runs %+v; want none", stands, stood.ExcludedRuns)
	}
	if reopened := Reopen(corrected, exHead); reopened.ExcludedRuns != nil {
		t.Fatalf("a reopen leaves excluded runs %+v, want none", reopened.ExcludedRuns)
	}
}

// Taking out no run costs nothing: withoutExcluded returns the runs themselves, with no copy,
// where no id is excluded or none of the runs is one of them.
func TestWithoutExcludedReturnsTheRunsThemselvesWhenNoneIsExcluded(t *testing.T) {
	runs := []record.AttemptRun{exRun("ci-ok", 10), exRun("unit", 15)}
	for name, excluded := range map[string]map[int64]struct{}{"no exclusion": nil, "no run excluded": {20: {}}} {
		if got := withoutExcluded(runs, excluded); &got[0] != &runs[0] || len(got) != len(runs) {
			t.Errorf("%s: got a copy %+v, want the runs themselves", name, got)
		}
		if allocs := testing.AllocsPerRun(100, func() { withoutExcluded(runs, excluded) }); allocs != 0 {
			t.Errorf("%s: %v allocations, want none", name, allocs)
		}
	}
	if got := withoutExcluded(runs, map[int64]struct{}{10: {}}); !reflect.DeepEqual(got, []record.AttemptRun{exRun("unit", 15)}) ||
		!reflect.DeepEqual(runs, []record.AttemptRun{exRun("ci-ok", 10), exRun("unit", 15)}) {
		t.Fatalf("withoutExcluded(10) = %+v with the runs left %+v; want unit's run alone and the runs unchanged", got, runs)
	}
}
