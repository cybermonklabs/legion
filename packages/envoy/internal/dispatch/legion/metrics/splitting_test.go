// splitting_test.go holds the tests of metric 4, splitting and follow-ups: each filing's class and
// its filer's phase, the weeks the filings fall in, and children per root.
package metrics

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestEachClassOfFiledIssue is metric 4: a child filed before the filer's first implementer event,
// one filed after it, an issue filed outside the filer's tree, and one the controller filed, each
// counted in its own week against the issues that finished.
func TestEachClassOfFiledIssue(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("implementer", hour(5)).
		closedAt(hour(20))
	atDesign := newIssue("INFRA-2").under("INFRA-1")
	later := newIssue("INFRA-3").under("INFRA-1")
	outside := newIssue("CORE-9")
	filed := newIssue("INFRA-4")
	population := fixture(hour(-24), hour(48), root, atDesign, later, outside, filed)
	population.Filed = []Filed{
		{Key: "INFRA-2", At: hour(1), Filer: legion.SessionTitle{Role: "architect", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-3", At: hour(6), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "CORE-9", At: hour(7), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}},
		{Key: "INFRA-4", At: hour(8), Filer: legion.SessionTitle{Role: "controller", Project: "INFRA", Controller: true}},
	}
	report := Compute(population, ScopeAll, hour(48))
	classOf := map[string]string{}
	phaseOf := map[string]string{}
	for _, row := range report.Splitting.Filed {
		classOf[row.Key], phaseOf[row.Key] = row.Class, row.FilerPhase
	}
	want := map[string]string{
		"INFRA-2": ClassInTreeAtDesign, "INFRA-3": ClassInTreeLater,
		"CORE-9": ClassOutOfTree, "INFRA-4": ClassByTheController,
	}
	if !reflect.DeepEqual(classOf, want) {
		t.Fatalf("the filings are classed %v, want %v", classOf, want)
	}
	// "For every class, the table names the filer's phase at the filing": the in-tree filings and
	// the one outside the tree all name it, and only the controller's, which works no issue,
	// leaves it empty.
	if phaseOf["INFRA-3"] != legion.PhaseImplementing {
		t.Errorf("the later filing names the filer's phase %q, want implementing", phaseOf["INFRA-3"])
	}
	if phaseOf["INFRA-2"] != phaseDesign {
		t.Errorf("the at-design filing names the filer's phase %q, want design", phaseOf["INFRA-2"])
	}
	if phaseOf["CORE-9"] != legion.PhaseImplementing {
		t.Errorf("the out-of-tree filing names the filer's phase %q, want implementing", phaseOf["CORE-9"])
	}
	if phaseOf["INFRA-4"] != "" {
		t.Errorf("the controller's filing names the phase %q, want none: the controller works no issue", phaseOf["INFRA-4"])
	}
	if len(report.Splitting.Weeks) != 1 {
		t.Fatalf("the filings fall in %d weeks, want one", len(report.Splitting.Weeks))
	}
	week := report.Splitting.Weeks[0]
	if week.Finished != 1 || week.Filed[ClassInTreeAtDesign] != 1 || week.Filed[ClassByTheController] != 1 {
		t.Fatalf("the week is %+v, want one finished issue and one filing of each class", week)
	}
	if report.Splitting.FinishedRoots != 1 || report.Splitting.ChildrenPerRoot != 2 {
		t.Fatalf("finished roots %d with %g children each, want one root with its two children",
			report.Splitting.FinishedRoots, report.Splitting.ChildrenPerRoot)
	}
	if report.Splitting.MedianHoursRootsWithChildren != 20 || report.Splitting.MedianHoursRootsNoChildren != 0 {
		t.Fatalf("the medians are %g with children and %g without, want 20 and 0",
			report.Splitting.MedianHoursRootsWithChildren, report.Splitting.MedianHoursRootsNoChildren)
	}
}

// TestAFilingUnderAnotherTreeIsOutOfTheFilersTree: "Out of the tree: any other issue an architect
// or phase worker filed", where "The new issue is in the filer's tree when both walks reach the
// same top issue" (the spec's metric 4 and "The tree of a filed issue"). The implementer below
// files an issue under the root of another tree: the filing has a parent, and that parent's walk
// reaches another top issue than the filer's.
func TestAFilingUnderAnotherTreeIsOutOfTheFilersTree(t *testing.T) {
	filer := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(20))
	otherRoot := newIssue("INFRA-5").worked("architect", hour(0))
	filed := newIssue("INFRA-6").under("INFRA-5")
	population := fixture(hour(-24), hour(48), filer, otherRoot, filed)
	population.Filed = []Filed{
		{Key: "INFRA-6", At: hour(6), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}, Parent: "INFRA-5", Ancestors: []string{"INFRA-5"}},
	}
	report := Compute(population, ScopeAll, hour(48))
	if len(report.Splitting.Filed) != 1 || report.Splitting.Filed[0].Class != ClassOutOfTree {
		t.Fatalf("the filings are %+v, want INFRA-6 out of the filer's tree: its parent INFRA-5 tops a tree of its own", report.Splitting.Filed)
	}
}

// TestTheScopeNarrowsTheFilingsByTheIssueFiled: the splitting table's rows and each week's four
// class counts follow the scope, read by the issue each filing created — the one implementer
// files a child of its own tree and a follow-up that is a root, and `roots` keeps one and
// `children` the other, whichever session filed them. That week's finished count and the table's
// root figures stand where they are: "These root figures count the project's finished roots
// whatever the all, roots or children toggle says, since they describe roots, while the weekly
// counts follow the toggle" (the spec's metric 4).
func TestTheScopeNarrowsTheFilingsByTheIssueFiled(t *testing.T) {
	// The root and its two children finish in the window's second week, so the week the filings
	// fall in has no finished issue of either kind and its finished count is the same under every
	// scope, while the filings in it are not.
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("implementer", hour(5)).
		closedAt(hour(100))
	inTree := newIssue("INFRA-2").under("INFRA-1")
	// Filed under no parent at all: an issue of its own tree, and so a root.
	followUp := newIssue("INFRA-7")
	finishedChild := newIssue("INFRA-3").under("INFRA-1").worked("implementer", hour(50)).closedAt(hour(101))
	population := fixture(hour(-24), hour(120), root, inTree, followUp, finishedChild)
	population.Filed = []Filed{
		{Key: "INFRA-2", At: hour(6), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-7", At: hour(7), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}},
	}
	filingsWeek := weekStart(hour(6))
	for _, one := range []struct {
		scope  Scope
		filed  []string
		counts map[string]int
	}{
		{ScopeAll, []string{"INFRA-2", "INFRA-7"}, map[string]int{ClassInTreeLater: 1, ClassOutOfTree: 1}},
		{ScopeRoots, []string{"INFRA-7"}, map[string]int{ClassOutOfTree: 1}},
		{ScopeChildren, []string{"INFRA-2"}, map[string]int{ClassInTreeLater: 1}},
	} {
		t.Run(string(one.scope), func(t *testing.T) {
			report := Compute(population, one.scope, hour(120))
			var filed []string
			for _, row := range report.Splitting.Filed {
				filed = append(filed, row.Key)
			}
			if !reflect.DeepEqual(filed, one.filed) {
				t.Fatalf("scope %s holds the filings %v, want %v: a filing is read by the tree of the issue it created, not by the filer's",
					one.scope, filed, one.filed)
			}
			var week SplittingWeek
			for _, row := range report.Splitting.Weeks {
				if row.Week.Equal(filingsWeek) {
					week = row
				}
			}
			want := map[string]int{ClassInTreeAtDesign: 0, ClassInTreeLater: 0, ClassOutOfTree: 0, ClassByTheController: 0}
			for class, count := range one.counts {
				want[class] = count
			}
			if !reflect.DeepEqual(week.Filed, want) {
				t.Fatalf("scope %s counts the filings' week %v, want %v: the week counts hold the rows of the same table",
					one.scope, week.Filed, want)
			}
			if week.Finished != 0 {
				t.Errorf("scope %s finished %d issues in the filings' week, want none: nothing closed in it under any scope",
					one.scope, week.Finished)
			}
			if report.Splitting.FinishedRoots != 1 || report.Splitting.ChildrenPerRoot != 2 {
				t.Errorf("scope %s answers %d finished roots with %g children each, want the one root with its two children: the root figures are the project's under every scope",
					one.scope, report.Splitting.FinishedRoots, report.Splitting.ChildrenPerRoot)
			}
			if report.Splitting.MedianHoursRootsWithChildren != 100 || report.Splitting.MedianHoursRootsNoChildren != 0 {
				t.Errorf("scope %s answers medians of %g with children and %g without, want the root's 100 hours and no root without children",
					one.scope, report.Splitting.MedianHoursRootsWithChildren, report.Splitting.MedianHoursRootsNoChildren)
			}
		})
	}
}

// TestEachWeeksFinishedCountFollowsTheScope: metric 4 counts each week's filings "against issues
// finished per week", and "the weekly counts follow the toggle" (the spec's metric 4). A root and
// its child finish in one week, which counts both under all, the root under roots and the child
// under children.
func TestEachWeeksFinishedCountFollowsTheScope(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(10))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(1)).closedAt(hour(8))
	population := fixture(hour(-24), hour(24), root, child)
	for _, one := range []struct {
		scope    Scope
		finished int
	}{
		{ScopeAll, 2},
		{ScopeRoots, 1},
		{ScopeChildren, 1},
	} {
		t.Run(string(one.scope), func(t *testing.T) {
			weeks := Compute(population, one.scope, hour(24)).Splitting.Weeks
			if len(weeks) != 1 || weeks[0].Finished != one.finished {
				t.Fatalf("scope %s answers the weeks %+v, want one week that finished %d", one.scope, weeks, one.finished)
			}
		})
	}
}

// TestAWeekRunsFromMondayToMondayInUTC: "A week is a calendar week in UTC, from Monday 00:00 to
// the next Monday 00:00 (ISO 8601)" (the spec's metric 4). The three filings below fall on the
// Saturday and the Sunday before Monday 2026-10-05 and half an hour into that Monday: the first
// two are the week of Monday 2026-09-28, and the third the week that Monday starts. The weeks are
// written out rather than computed, so a week that starts on a Sunday, or at another hour, fails.
func TestAWeekRunsFromMondayToMondayInUTC(t *testing.T) {
	population := fixture(hour(-24), hour(24*14))
	controller := legion.SessionTitle{Role: "controller", Project: "INFRA", Controller: true}
	population.Filed = []Filed{
		{Key: "INFRA-11", At: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), Filer: controller},
		{Key: "INFRA-12", At: time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC), Filer: controller},
		{Key: "INFRA-13", At: time.Date(2026, 10, 5, 0, 30, 0, 0, time.UTC), Filer: controller},
	}
	filed := map[string]int{}
	for _, week := range Compute(population, ScopeAll, hour(24*14)).Splitting.Weeks {
		filed[week.Week.Format(time.RFC3339)] = week.Filed[ClassByTheController]
	}
	want := map[string]int{"2026-09-28T00:00:00Z": 2, "2026-10-05T00:00:00Z": 1}
	if !reflect.DeepEqual(filed, want) {
		t.Fatalf("the filings fall in the weeks %v, want %v: Saturday 2026-10-03 12:00 and Sunday 2026-10-04 23:00 in the week of Monday 2026-09-28, and Monday 2026-10-05 00:30 in the week it starts",
			filed, want)
	}
}

// TestChildrenPerRootCountsEveryIssueOfTheTreeUnderItsRoot: children per root is "the mean over
// the finished roots (every issue of the tree under its root, not only direct children ...)" (the
// spec's metric 4). The root below has a child, and the child an issue of its own: two issues of
// the tree under one root.
func TestChildrenPerRootCountsEveryIssueOfTheTreeUnderItsRoot(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(10))
	// The child carries Legion phase work, so the walk from the grandchild climbs through it.
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(1))
	grandchild := newIssue("INFRA-3").under("INFRA-2")
	report := Compute(fixture(hour(-24), hour(24), root, child, grandchild), ScopeAll, hour(24))
	if report.Splitting.FinishedRoots != 1 || report.Splitting.ChildrenPerRoot != 2 {
		t.Fatalf("%d finished roots with %g children each, want the one root with its child and its grandchild",
			report.Splitting.FinishedRoots, report.Splitting.ChildrenPerRoot)
	}
}

// TestTheSplittingTableNamesAFilerByThePhaseWordsOfTables: "Every table names a phase by the bar
// it falls on: design, planning, implementing, testing, reviewing, accepting, retro, merging (for
// `merging` and `awaiting_merge`). So splitting and follow-ups names a filer in `awaiting_merge`
// as merging" (the spec's "Phase words in tables"). Retro is on the implementer's bar and
// keeps its own word, and both phases of the merger's bar are named merging.
func TestTheSplittingTableNamesAFilerByThePhaseWordsOfTables(t *testing.T) {
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(20)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(10), "retro").
		observeString(legion.FieldPhase, hour(12), "merging").
		observeString(legion.FieldPhase, hour(14), "awaiting_merge")
	inRetro := newIssue("INFRA-5").under("INFRA-1")
	whileImplementing := newIssue("INFRA-6").under("INFRA-1")
	whileMerging := newIssue("INFRA-7").under("INFRA-1")
	awaitingMerge := newIssue("INFRA-8").under("INFRA-1")
	population := fixture(hour(-24), hour(48), root, inRetro, whileImplementing, whileMerging, awaitingMerge)
	implementer := legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}
	merger := legion.SessionTitle{Role: "merger", Issue: "INFRA-1"}
	population.Filed = []Filed{
		{Key: "INFRA-6", At: hour(6), Filer: implementer, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-5", At: hour(11), Filer: implementer, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-7", At: hour(13), Filer: merger, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-8", At: hour(15), Filer: merger, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
	}

	report := Compute(population, ScopeAll, hour(48))

	phaseOf := map[string]string{}
	for _, row := range report.Splitting.Filed {
		phaseOf[row.Key] = row.FilerPhase
	}
	want := map[string]string{
		"INFRA-5": legion.PhaseRetro,
		"INFRA-6": legion.PhaseImplementing,
		"INFRA-7": legion.PhaseMerging,
		"INFRA-8": legion.PhaseMerging,
	}
	if !reflect.DeepEqual(phaseOf, want) {
		t.Fatalf("the filings name the filer's phases %v, want %v: a filer in retro is named retro, and one in merging or awaiting_merge merging",
			phaseOf, want)
	}
}
