package fold_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
)

// epoch is the UTC midnight the tests count their days from.
var epoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// day is the UTC midnight n days after epoch.
func day(n int) time.Time { return epoch.AddDate(0, 0, n) }

// groups is the two groups the tests name: the agents by their branch prefix, and the rest.
var groups = []survival.Group{{Name: "agents", BranchPrefixes: []string{"agent/"}}, {Name: "others", BranchPrefixes: []string{"other/"}}}

// project is the project the tests build rows for, whose default repository is owner/repo.
var project = fold.Project{Key: "PROJ", DefaultRepo: "owner/repo"}

// change is a stored change of owner/repo on day n: pull request number of head, adding added
// lines of which alive live at the mark, for issue (of PROJ; none when "").
func change(n, number int, head, issue string, added, alive int) fold.Change {
	issueProject := ""
	if issue != "" {
		issueProject = project.Key
	}
	return fold.Change{Repo: project.DefaultRepo, IssueProject: issueProject, Change: survival.Change{
		SHA: fmt.Sprintf("%040x", number), Day: day(n), CommittedAt: day(n).Add(9 * time.Hour),
		ChangeFacts: survival.ChangeFacts{PR: number, HeadRef: head, Author: "someone", IssueKey: issue},
		Added:       added, AliveAtMark: alive,
	}}
}

// pairOf is a stored pair of adder's lines removed by the change remover.
func pairOf(adder, remover fold.Change, lines int) fold.Pair {
	return fold.Pair{Repo: adder.Repo, Pair: survival.Pair{AdderSHA: adder.SHA, RemoverSHA: remover.SHA,
		Remover: remover.ChangeFacts, Lines: lines, DaysBetween: remover.CommittedAt.Sub(adder.CommittedAt).Hours() / 24}}
}

// measured is the project's day n, measured.
func measured(n int) fold.Day { return fold.Day{Day: day(n), State: survival.DayMeasured} }

// partOf is the part of series on day n among parts; it fails the test when there is none.
func partOf(t *testing.T, parts []measures.DayPart, series string, n int) measures.DayPart {
	t.Helper()
	for _, part := range parts {
		if part.Series == series && part.Day.Equal(day(n)) {
			return part
		}
	}
	t.Fatalf("no part of %s on day %d among %d parts", series, n, len(parts))
	return measures.DayPart{}
}

// ratio is a part's numerator, denominator and N, failing the test when it holds no value.
func ratio(t *testing.T, part measures.DayPart) (float64, float64, int) {
	t.Helper()
	if part.Num == nil || part.Den == nil {
		t.Fatalf("the part %s of %s holds no value: %+v", part.Series, part.Day.Format(time.DateOnly), part)
	}
	return *part.Num, *part.Den, part.N
}

func TestEachRemovalFallsInItsKind(t *testing.T) {
	t.Parallel()
	repo := survivaltest.New(t)
	ledger := strings.Split(strings.TrimSpace(func() string {
		var text string
		for n := range 20 {
			text += fmt.Sprintf("ledger entry %02d: reconcile the account for the closing period\n", n)
		}
		return text
	}()), "\n")
	at := func(n int) time.Time { return day(n).Add(9 * time.Hour) }
	repo.Write("unrelated.txt", "background\n")
	repo.Commit("base", at(0))
	repo.Write("ledger.txt", strings.Join(ledger, "\n")+"\n")
	facts := map[string]survival.ChangeFacts{}
	cohort := repo.Commit("add the ledger", at(1))
	facts[cohort] = survival.ChangeFacts{PR: 1, HeadRef: "agent/ledger", Author: "agent-app[bot]", IssueKey: "PROJ-1"}
	// One removal of each kind of R5, by a change of the cohort's group: for its issue
	// (iteration), for another (rework), and for none (no issue); then the kind other, by a change
	// of the other group with an issue, by one with no issue, and by a commit that merged no pull
	// request. Each removes a block of its own size, so a removal counted in the wrong kind moves a
	// count.
	removers := []struct {
		facts survival.ChangeFacts
		count int
	}{
		{survival.ChangeFacts{PR: 2, HeadRef: "agent/tighten", Author: "agent-app[bot]", IssueKey: "PROJ-1"}, 3},
		{survival.ChangeFacts{PR: 3, HeadRef: "agent/rewrite", Author: "agent-app[bot]", IssueKey: "PROJ-2"}, 5},
		{survival.ChangeFacts{PR: 4, HeadRef: "agent/tidy", Author: "agent-app[bot]"}, 7},
		{survival.ChangeFacts{PR: 5, HeadRef: "other/fix", Author: "someone", IssueKey: "PROJ-3"}, 2},
		{survival.ChangeFacts{PR: 6, HeadRef: "other/trim", Author: "someone"}, 1},
		{survival.ChangeFacts{}, 1},
	}
	kept := ledger
	for index, remover := range removers {
		kept = kept[remover.count:]
		repo.Write("ledger.txt", strings.Join(kept, "\n")+"\n")
		sha := repo.Commit(fmt.Sprintf("remover %d", index), at(2+index))
		if remover.facts.PR != 0 {
			facts[sha] = remover.facts
		}
	}
	var chain []survival.Commit
	patches := map[string][]survival.FileDiff{}
	for _, commit := range repo.FirstParents(repo.Head()) {
		files, err := survival.ParseDiff(commit.Patch)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, survival.Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt, Change: facts[commit.SHA]})
		patches[commit.SHA] = files
	}
	rangeChain := survival.ReplayRange(chain, day(1), day(1))
	out, err := survival.Measure(survival.MeasureInput{Days: []time.Time{day(1)}, Chain: rangeChain,
		Patch: func(i int) (survival.Patch, error) { return survival.Patch{Files: patches[rangeChain[i].SHA]}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	in := fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{measured(1)}}
	for _, c := range out.Changes {
		in.Changes = append(in.Changes, fold.Change{Repo: project.DefaultRepo, IssueProject: project.Key, Change: c})
	}
	for _, p := range out.Pairs {
		in.Pairs = append(in.Pairs, fold.Pair{Repo: project.DefaultRepo, Pair: p})
	}
	if len(in.Changes) != 1 || len(in.Pairs) != 5 {
		t.Fatalf("measured %d changes and %d pairs, want the cohort and its five removers that are changes", len(in.Changes), len(in.Pairs))
	}
	_, rework := fold.BuildDays(in)
	for series, want := range map[string]float64{
		fold.SeriesRework: 5, fold.SeriesIteration: 3, fold.SeriesNoIssue: 7, fold.SeriesOther: 2 + 1 + 1,
	} {
		num, den, n := ratio(t, partOf(t, rework, fold.GroupSeries(series, "agents"), 1))
		if num != want || den != 20 || n != 1 {
			t.Errorf("%s reads %v of %v over %d changes, want %v of 20 over 1", fold.GroupSeries(series, "agents"), num, den, n, want)
		}
	}
	// Only rework counts in the rework number.
	if got := fold.FoldRework(rework, fold.GroupSeries(fold.SeriesRework, "agents")).Share; got == nil || *got != 5.0/20 {
		t.Errorf("the rework share reads %v, want 5 of 20", got)
	}
}

func TestAChangeCountsForItsIssuesProject(t *testing.T) {
	t.Parallel()
	c := fold.Change{Repo: "owner/shared", IssueProject: "ALPHA", Change: survival.Change{SHA: fmt.Sprintf("%040x", 1),
		Day: day(1), ChangeFacts: survival.ChangeFacts{PR: 1, HeadRef: "agent/x", IssueKey: "ALPHA-1"}, Added: 10, AliveAtMark: 6}}
	alpha := fold.Project{Key: "ALPHA", DefaultRepo: "owner/alpha"}
	beta := fold.Project{Key: "BETA", DefaultRepo: "owner/shared"}
	none := fold.Project{Key: "GAMMA"}
	if !fold.CountsFor(c, alpha) || fold.CountsFor(c, beta) || fold.CountsFor(c, none) {
		t.Errorf("a change of ALPHA's issue in owner/shared counts for ALPHA %v, for BETA (whose default repository "+
			"holds it) %v and for GAMMA %v: want only ALPHA", fold.CountsFor(c, alpha), fold.CountsFor(c, beta), fold.CountsFor(c, none))
	}
	for _, row := range []struct {
		project fold.Project
		den     float64
	}{{alpha, 10}, {beta, 0}} {
		parts, _ := fold.BuildDays(fold.BuildInput{Project: row.project, Groups: groups, Days: []fold.Day{measured(1)}, Changes: []fold.Change{c}})
		_, den, _ := ratio(t, partOf(t, parts, fold.GroupSeries(fold.SeriesSurvival, "agents"), 1))
		if den != row.den {
			t.Errorf("%s's survival row counts %v added lines, want %v", row.project.Key, den, row.den)
		}
	}
}

func TestAChangeWithNoIssueCountsForEachProjectOfItsRepository(t *testing.T) {
	t.Parallel()
	c := fold.Change{Repo: "owner/shared", Change: survival.Change{SHA: fmt.Sprintf("%040x", 1), Day: day(1),
		ChangeFacts: survival.ChangeFacts{PR: 1, HeadRef: "agent/x"}, Added: 10, AliveAtMark: 6}}
	for _, row := range []struct {
		project fold.Project
		counts  bool
	}{
		{fold.Project{Key: "ALPHA", DefaultRepo: "owner/shared"}, true},
		{fold.Project{Key: "BETA", DefaultRepo: "Owner/Shared"}, true},
		{fold.Project{Key: "GAMMA", DefaultRepo: "owner/other"}, false},
		{fold.Project{Key: "DELTA"}, false},
	} {
		if got := fold.CountsFor(c, row.project); got != row.counts {
			t.Errorf("a change with no issue in owner/shared counts for %s (default repository %q): %v, want %v",
				row.project.Key, row.project.DefaultRepo, got, row.counts)
		}
		parts, _ := fold.BuildDays(fold.BuildInput{Project: row.project, Groups: groups, Days: []fold.Day{measured(1)}, Changes: []fold.Change{c}})
		part := partOf(t, parts, fold.GroupSeries(fold.SeriesSurvival, "agents"), 1)
		if want := map[bool]int{true: 1, false: 0}[row.counts]; part.ViaRepository != want || part.N != want {
			t.Errorf("%s's survival row counts %d changes, %d of them through the repository: want %d and %d",
				row.project.Key, part.N, part.ViaRepository, want, want)
		}
	}
}

func TestBuildDaysWritesRowsOnlyForDecidedDays(t *testing.T) {
	t.Parallel()
	in := fold.BuildInput{Project: project, Groups: groups,
		Days: []fold.Day{measured(1), {Day: day(2), State: survival.DayMarked}, {Day: day(3), State: survival.DayBlocked, BlockedFiles: 3}},
		Changes: []fold.Change{
			change(1, 1, "agent/a", "PROJ-1", 10, 8), change(2, 2, "agent/b", "", 5, 5), change(4, 3, "other/c", "", 7, 7),
		},
	}
	survivalParts, reworkParts := fold.BuildDays(in)
	for name, parts := range map[string][]measures.DayPart{"survival": survivalParts, "rework": reworkParts} {
		byDay := map[time.Time][]string{}
		for _, part := range parts {
			byDay[part.Day] = append(byDay[part.Day], part.Series)
		}
		bases := map[string][]string{
			"survival": {fold.SeriesSurvival},
			"rework":   {fold.SeriesRework, fold.SeriesIteration, fold.SeriesNoIssue, fold.SeriesOther},
		}[name]
		var day1 []string
		for _, group := range groups {
			for _, base := range bases {
				day1 = append(day1, fold.GroupSeries(base, group.Name))
			}
		}
		day1 = append(day1, fold.SeriesStatus)
		want := map[time.Time][]string{day(1): day1, day(2): {fold.SeriesStatus}, day(3): {fold.SeriesStatus}}
		if !reflect.DeepEqual(byDay, want) {
			t.Errorf("%s: the parts by day are %v, want %v: rows for the decided days alone, a marked or blocked day's status alone", name, byDay, want)
		}
		wantCounts := map[int]map[string]float64{
			1: {fold.CountMeasured: 1, fold.CountMarked: 0, fold.CountBlocked: 0, fold.CountBlockedFiles: 0, fold.CountGroups: 2, fold.CountRepoUnreadable: 0},
			2: {fold.CountMeasured: 0, fold.CountMarked: 1, fold.CountBlocked: 0, fold.CountBlockedFiles: 0, fold.CountGroups: 2, fold.CountRepoUnreadable: 0,
				fold.CountFirstMarkedDay: float64(day(2).Unix())},
			3: {fold.CountMeasured: 0, fold.CountMarked: 0, fold.CountBlocked: 1, fold.CountBlockedFiles: 3, fold.CountGroups: 2, fold.CountRepoUnreadable: 0},
		}
		for n, counts := range wantCounts {
			status := partOf(t, parts, fold.SeriesStatus, n)
			if !reflect.DeepEqual(status.Counts, counts) || status.Num != nil || status.Den != nil || status.Sum != nil {
				t.Errorf("%s: day %d's status is %+v, want the counts %v and no value", name, n, status, counts)
			}
		}
	}
}

func TestBuildDaysWritesAZeroRowForAGroupWithNoChange(t *testing.T) {
	t.Parallel()
	survivalParts, reworkParts := fold.BuildDays(fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{measured(1)},
		Changes: []fold.Change{change(1, 1, "agent/a", "PROJ-1", 10, 8)}})
	for _, series := range []string{fold.SeriesSurvival} {
		num, den, n := ratio(t, partOf(t, survivalParts, fold.GroupSeries(series, "others"), 1))
		if num != 0 || den != 0 || n != 0 {
			t.Errorf("%s reads %v of %v over %d, want a zero row", fold.GroupSeries(series, "others"), num, den, n)
		}
	}
	for _, series := range []string{fold.SeriesRework, fold.SeriesIteration, fold.SeriesNoIssue, fold.SeriesOther} {
		num, den, n := ratio(t, partOf(t, reworkParts, fold.GroupSeries(series, "others"), 1))
		if num != 0 || den != 0 || n != 0 {
			t.Errorf("%s reads %v of %v over %d, want a zero row", fold.GroupSeries(series, "others"), num, den, n)
		}
	}
	num, den, n := ratio(t, partOf(t, survivalParts, fold.GroupSeries(fold.SeriesSurvival, "agents"), 1))
	if num != 8 || den != 10 || n != 1 {
		t.Errorf("the agents' survival row reads %v of %v over %d, want 8 of 10 over 1", num, den, n)
	}
}
