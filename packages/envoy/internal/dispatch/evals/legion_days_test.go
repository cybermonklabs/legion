package evals

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// legionFact is one Legion day row as the cases state it: `<slug> <series> <repo>: n=…`, with the
// value its fold reads (the share, the samples in ascending order, or the sum) and every count
// that is not zero, by name.
func legionFact(row DayRow) string {
	slug := strings.TrimPrefix(row.EvalID, row.ProjectKey+"/")
	repo := row.Repo
	if repo == "" {
		repo = "all"
	}
	value := ""
	switch {
	case row.Num != nil:
		value = fmt.Sprintf(" share=%g/%g", *row.Num, *row.Den)
	case row.Samples != nil:
		value = fmt.Sprintf(" samples=%v", slices.Sorted(slices.Values(row.Samples)))
	case row.Sum != nil:
		value = fmt.Sprintf(" sum=%g", *row.Sum)
	}
	for _, name := range slices.Sorted(maps.Keys(row.Counts)) {
		if row.Counts[name] != 0 {
			value += fmt.Sprintf(" %s=%g", name, row.Counts[name])
		}
	}
	return fmt.Sprintf("%s %s %s: n=%d via=%d%s", slug, row.Series, repo, row.N, row.ViaRepository, value)
}

// legionFacts is the Legion rows of project on day, each as legionFact states it, and how many
// rows each eval holds in each repository's cut, as `<slug> <repo>`.
func (r rig) legionFacts(t *testing.T, project string, day time.Time) (map[string]bool, map[string]int) {
	t.Helper()
	facts, groups := map[string]bool{}, map[string]int{}
	for _, row := range r.stored(t, project) {
		slug := strings.TrimPrefix(row.EvalID, project+"/")
		if !row.Day.Equal(day) || !slices.Contains(legionDaySlugs, slug) {
			continue
		}
		facts[legionFact(row)] = true
		repo := row.Repo
		if repo == "" {
			repo = "all"
		}
		groups[slug+" "+repo]++
	}
	return facts, groups
}

// TestALegionDayWritesEachEvalInEveryCutAndRepositoryAndRecordsItsIssues: over a hand-written
// project of three issues that close on one day (a root in acme/web with two children, its child
// in acme/api, and a root that owns no repository, which the ':default' cut holds,
// legionfacts.DefaultRepo), one that closes the next day, and two filings of the first day (one
// into acme/api, one of an issue that owns none), the day's write, as the refresher's walk writes
// it, holds every Legion eval with input in every cut, in the
// all-repositories row and in each repository its input is in: idle time from the first whole day
// after the first claim observation, slot wait not before the first whole day after the first slot
// observation, and the splitting root figures with no children cut. It records the three issues on
// their close day, and leaves the day's delivery row and item alone. Written again unchanged it
// rewrites nothing; after one issue's close moves to the next day, both days are written again and
// the issue's rows and item move with it.
func TestALegionDayWritesEachEvalInEveryCutAndRepositoryAndRecordsItsIssues(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	pool := r.database.Pool
	day, next := at(3, 0, 0), at(4, 0, 0)
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
	}
	actor := `{"kind":"system","id":"legion-day-test"}`
	// An issue whose chain names no repository resolves to legionfacts.DefaultRepo, which stands
	// for the project's default repository. Each other issue's repository is its own, which the day
	// resolves when it is built.
	for _, issue := range []struct {
		number int
		repo   any
	}{{1, "acme/web"}, {2, "acme/api"}, {3, nil}, {4, "acme/web"}, {10, "acme/api"}, {11, nil}, {12, nil}} {
		exec(fmt.Sprintf("the issue EVAL-%d", issue.number), `
			insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
			values ($1, 'EVAL', $2, 'An issue', 'triage', $3, $4, $5::jsonb, $6)
		`, fmt.Sprintf("EVAL-%d", issue.number), issue.number, fmt.Sprintf("%08d", issue.number), issue.repo, actor, at(2, 0, 0))
	}
	exec("the first claim and slot observations", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ('EVAL', 'EVAL-1', 'claim.implementer', '"working"', $1), ('EVAL', 'EVAL-1', 'slot', '0', $2)
	`, at(2, 10, 0), at(3, 6, 0))

	bar := func(hours float64, segments metrics.Segments) metrics.BarPart {
		return metrics.BarPart{Hours: hours, HoldsHours: true, Segments: segments}
	}
	closed := func(key, root, repo string, closedAt time.Time, hours float64) legionfacts.Row {
		started := closedAt.Add(-time.Duration(hours) * time.Hour)
		return legionfacts.Row{Project: "EVAL", Issue: key, Repo: repo, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: at(1, 0, 0), ClosedAt: &closedAt, Root: root == key, Tree: root,
			StartedAt: &started, WorkHours: hours, PhaseHours: map[string]float64{}, Bars: map[string]metrics.BarPart{},
			IdleByRole: map[string]float64{},
		}}}
	}
	root := closed("EVAL-1", "EVAL-1", "acme/web", at(3, 10, 0), 10)
	root.Bars["implementer"] = bar(6, metrics.Segments{InATurn: 4, Idle: 2})
	root.Bars["reviewer"] = bar(4, metrics.Segments{InATurn: 4})
	root.IdleByRole["implementer"], root.SlotWaitHours, root.ReviewRounds, root.Children = 1.5, 2, 1, 2
	child := closed("EVAL-2", "EVAL-1", "acme/api", at(3, 12, 0), 4)
	child.Bars["implementer"] = bar(4, metrics.Segments{InATurn: 4})
	child.IdleByRole["tester"], child.SlotWaitHours, child.ReviewRounds, child.SentBack = 0.5, 1, 2, true
	child.ImplementingHoursAfterFirstReview, child.TestingHoursAfterFirstReview = 1, 0.5
	lone := closed("EVAL-3", "EVAL-3", "", at(3, 14, 0), 2)
	later := closed("EVAL-4", "EVAL-4", "acme/web", at(4, 9, 0), 3)
	rows := []legionfacts.Row{root, child, lone, later}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: "EVAL", Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	filing := func(key string, filedAt time.Time, filer, filerIssue, phase, class, root string) legionfacts.Filing {
		return legionfacts.Filing{Project: "EVAL", IssueFiling: metrics.IssueFiling{FiledIssue: metrics.FiledIssue{
			Key: key, At: filedAt, Filer: filer, FilerIssue: filerIssue, FilerPhase: phase, Class: class,
		}, Root: root}}
	}
	filings := []legionfacts.Filing{
		filing("EVAL-10", at(3, 8, 0), "architect", "EVAL-1", "design", metrics.ClassInTreeAtDesign, "EVAL-1"),
		filing("EVAL-11", at(3, 9, 0), "implementer", "EVAL-2", "implementing", metrics.ClassOutOfTree, "EVAL-11"),
		filing("EVAL-12", at(4, 1, 0), "controller", "", "", metrics.ClassByTheController, "EVAL-12"),
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: "EVAL", Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings: %v", err)
	}
	delivery := ProjectDay{Project: "EVAL", Day: day, Rows: []DayRow{{
		ProjectKey: "EVAL", EvalID: EvalID("EVAL", SlugDeployFrequency), DeployRepo: "acme/web", Listed: true,
		DayPart: measures.DayPart{Series: "deploys", Day: day, N: 1, Sum: new(1.0)},
	}}, Members: []Member{{Family: KindDelivery, DeployRepo: "acme/web", RowKey: RunMember("acme/web", 7), Repo: "acme/web"}}}
	if err := r.registry.writeProjectDay(ctx, deliveryFamily, delivery, testBuild, true); err != nil {
		t.Fatalf("write the day's delivery row: %v", err)
	}

	since, err := r.registry.ReadLegionSince(ctx, "EVAL")
	if err != nil {
		t.Fatalf("read the first observations: %v", err)
	}
	if since.Claims == nil || !since.Claims.Equal(day) || since.Slots == nil || !since.Slots.Equal(next) {
		t.Fatalf("since = claims %v, slots %v, want %s (the first claim was the day before) and %s (the first slot was that day)",
			since.Claims, since.Slots, day.Format(time.DateOnly), next.Format(time.DateOnly))
	}
	r.refreshDays(t, []string{"EVAL"}, day)
	facts, groups := r.legionFacts(t, "EVAL", day)
	// Each eval with input holds 3 rows of work time, 21 of phase time (seven bars), 18 of idle
	// time (six roles), 9 of rework and 21 of splitting (four classes and the finished issues in
	// three cuts, and three root figures in two), in every cut its input is in.
	wantGroups := map[string]int{}
	for _, repo := range []string{"all", "acme/web", "acme/api", legionfacts.DefaultRepo} {
		for slug, count := range map[string]int{
			SlugLegionWorkTime: 3, SlugLegionPhaseTime: 21, SlugLegionIdleTime: 18, SlugLegionRework: 9, SlugLegionSplitting: 21,
		} {
			wantGroups[slug+" "+repo] = count
		}
	}
	if !maps.Equal(groups, wantGroups) {
		t.Errorf("the day's rows by eval and repository:\n%v\nwant:\n%v", groups, wantGroups)
	}
	for _, want := range []string{
		"legion-work-time hours all: n=3 via=0 samples=[2 4 10]",
		"legion-work-time hours:roots all: n=2 via=0 samples=[2 10]",
		"legion-work-time hours:children all: n=1 via=0 samples=[4]",
		"legion-work-time hours acme/web: n=1 via=0 samples=[10]",
		"legion-work-time hours:children acme/web: n=0 via=0 samples=[]",
		"legion-work-time hours:children acme/api: n=1 via=0 samples=[4]",
		"legion-phase-time implementer all: n=3 via=0 samples=[0 4 6] idle=2 in_a_turn=8 issues=2",
		"legion-phase-time reviewer:roots all: n=2 via=0 samples=[0 4] in_a_turn=4 issues=1",
		"legion-phase-time architect all: n=3 via=0 samples=[0 0 0]",
		"legion-idle-time implementer all: n=3 via=0 samples=[0 0 1.5]",
		"legion-idle-time tester:children acme/api: n=1 via=0 samples=[0.5]",
		"legion-rework review_rounds all: n=3 via=0 samples=[0 1 2]",
		"legion-rework sent_back all: n=3 via=0 share=1/3",
		"legion-rework sent_back:roots all: n=2 via=0 share=0/2",
		"legion-rework hours_after_first_review all: n=3 via=0 samples=[0 0 1.5]",
		"legion-splitting filed:in_tree_at_design all: n=1 via=0 sum=1",
		"legion-splitting filed:in_tree_at_design:children all: n=1 via=0 sum=1",
		"legion-splitting filed:out_of_tree:roots all: n=1 via=0 sum=1",
		"legion-splitting filed:in_tree_at_design acme/api: n=1 via=0 sum=1",
		"legion-splitting filed:out_of_tree acme/api: n=0 via=0 sum=0",
		"legion-splitting finished all: n=3 via=0 sum=3",
		"legion-splitting finished:children all: n=1 via=0 sum=1",
		"legion-splitting finished acme/api: n=1 via=0 sum=1",
		"legion-splitting roots all: n=2 via=0 sum=2 children=2",
		"legion-splitting roots:roots all: n=2 via=0 sum=2 children=2",
		"legion-splitting hours_with_children all: n=1 via=0 samples=[10]",
		"legion-splitting hours_without_children:roots all: n=1 via=0 samples=[2]",
		"legion-splitting roots acme/api: n=0 via=0 sum=0",
	} {
		if !facts[want] {
			t.Errorf("the day's rows lack %q", want)
		}
	}
	for fact := range facts {
		if strings.HasPrefix(fact, SlugLegionSlotWait+" ") || strings.Contains(fact, "roots:children") ||
			strings.Contains(fact, "_children:children") {
			t.Errorf("the day holds %q: slot wait has no input before %s, and the root figures have no children cut", fact, next.Format(time.DateOnly))
		}
	}
	wantMembers := []string{
		"delivery run:acme/web/7 acme/web 2026-11-03 as acme/web",
		"legion EVAL-1 acme/web 2026-11-03", "legion EVAL-2 acme/api 2026-11-03", "legion EVAL-3 " + legionfacts.DefaultRepo + " 2026-11-03",
	}
	if got := r.members(t, "EVAL"); !slices.Equal(got, wantMembers) {
		t.Errorf("the items of the day: %v, want %v", got, wantMembers)
	}
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day = $2`, EvalID("EVAL", SlugDeployFrequency), day); got != 1 {
		t.Errorf("the day's delivery rows after the Legion write: %d, want the one it held", got)
	}

	// The same day written again holds the same values: no row is rewritten.
	var newest time.Time
	if err := pool.QueryRow(ctx, `select max(updated_at) from eval_days`).Scan(&newest); err != nil {
		t.Fatalf("read the newest write: %v", err)
	}
	r.refreshDays(t, []string{"EVAL"}, day)
	if rewritten := r.count(t, `select count(*) from eval_days where updated_at > $1`, newest); rewritten != 0 {
		t.Errorf("writing the unchanged day again rewrote %d rows, want none", rewritten)
	}

	// EVAL-2 closes again on the next day: its first day's rows in acme/api, which held EVAL-2
	// alone, are deleted but for splitting's, whose filing is still there, as a whole walk over the
	// moved close holds none, and the next day, which slot wait now has input on, gains them.
	exec("EVAL-2's new close", `update issue_facts set closed_at = $1 where issue_key = 'EVAL-2'`, at(4, 11, 0))
	r.refreshDays(t, []string{"EVAL"}, day, next)
	firstFacts, firstGroups := r.legionFacts(t, "EVAL", day)
	kept := maps.Clone(groups)
	for _, slug := range []string{SlugLegionWorkTime, SlugLegionPhaseTime, SlugLegionIdleTime, SlugLegionRework} {
		delete(kept, slug+" acme/api")
	}
	if !maps.Equal(firstGroups, kept) {
		t.Errorf("the first day after EVAL-2 left it: %v, want every row it held but acme/api's of the evals EVAL-2 alone was input of there, %v", firstGroups, kept)
	}
	for fact := range firstFacts {
		if strings.Contains(fact, " acme/api: ") && !strings.HasPrefix(fact, SlugLegionSplitting+" ") {
			t.Errorf("the first day after EVAL-2 left it holds %q, want no row of acme/api but splitting's, whose filing is still there", fact)
		}
	}
	if !firstFacts["legion-splitting filed:in_tree_at_design acme/api: n=1 via=0 sum=1"] {
		t.Errorf("the first day after EVAL-2 left it lacks acme/api's filing")
	}
	nextFacts, nextGroups := r.legionFacts(t, "EVAL", next)
	for _, want := range []string{
		"legion-work-time hours all: n=2 via=0 samples=[3 4]",
		"legion-slot-wait hours all: n=2 via=0 samples=[0 1]",
		"legion-slot-wait hours:children acme/api: n=1 via=0 samples=[1]",
		"legion-splitting filed:by_the_controller:roots all: n=1 via=0 sum=1",
	} {
		if !nextFacts[want] {
			t.Errorf("the next day's rows lack %q", want)
		}
	}
	if nextGroups[SlugLegionSlotWait+" all"] != 3 || nextGroups[SlugLegionSlotWait+" acme/api"] != 3 {
		t.Errorf("the next day's rows by eval and repository: %v, want slot wait in every cut of all and acme/api", nextGroups)
	}
	wantMembers = []string{
		"delivery run:acme/web/7 acme/web 2026-11-03 as acme/web",
		"legion EVAL-1 acme/web 2026-11-03", "legion EVAL-2 acme/api 2026-11-04", "legion EVAL-3 " + legionfacts.DefaultRepo + " 2026-11-03",
		"legion EVAL-4 acme/web 2026-11-04",
	}
	if got := r.members(t, "EVAL"); !slices.Equal(got, wantMembers) {
		t.Errorf("the items after EVAL-2 moved: %v, want %v", got, wantMembers)
	}
	t.Logf("first day: %d Legion rows in %d eval and repository cuts; next day: %d", len(facts), len(groups), len(nextFacts))
}

// TestTheZeroLegionDayIsTheBuildersDayWithNoInput: ZeroLegionDay holds the all-repositories row of
// every series of the evals named, in every cut but the root figures' children cut, each n 0 with
// a zero value; the catalogue names every one of those and, beside them, the root figures'
// children cut, which the eval has and no day row is ever written of; it holds nothing of a slug
// of no Legion eval; and the Legion write refuses a row of a delivery eval before writing
// anything.
func TestTheZeroLegionDayIsTheBuildersDayWithNoInput(t *testing.T) {
	t.Parallel()
	day := at(5, 0, 0)
	zero := ZeroLegionDay("EVAL", day, legionDaySlugs...)
	series := 0
	for _, slug := range legionDaySlugs {
		series += len(seriesNames(entryOf(t, slug)))
	}
	if len(zero.Rows) != 3+21+18+3+9+21 || series != len(zero.Rows)+len(metrics.RootFigures) {
		t.Errorf("the zero day holds %d rows and the six evals %d series, want 75 rows and those 75 series with the %d root figures' children cut beside them",
			len(zero.Rows), series, len(metrics.RootFigures))
	}
	for _, row := range zero.Rows {
		fact := legionFact(row)
		slug := strings.TrimPrefix(row.EvalID, "EVAL/")
		if row.N != 0 || row.Repo != "" || !row.Listed || !row.Day.Equal(day) || strings.Contains(fact, "=1") ||
			strings.HasSuffix(row.Series, ":children") && slices.ContainsFunc(metrics.RootFigures, func(figure string) bool { return strings.HasPrefix(row.Series, figure+":") }) ||
			!slices.Contains(seriesNames(entryOf(t, slug)), row.Series) {
			t.Errorf("a zero row is %s (repo %q, listed %t, %s), want n 0 and a zero value in the all-repositories row of %s of a series the catalogue names",
				fact, row.Repo, row.Listed, row.Day.Format(time.DateOnly), day.Format(time.DateOnly))
		}
	}
	splitting := seriesNames(entryOf(t, SlugLegionSplitting))
	for _, figure := range metrics.RootFigures {
		if !slices.Contains(splitting, metrics.CutSeries(figure, metrics.ScopeRoots)) ||
			!slices.Contains(splitting, metrics.CutSeries(figure, metrics.ScopeChildren)) {
			t.Errorf("legion-splitting's series %v, want %s in all three cuts", splitting, figure)
		}
	}
	if got := ZeroLegionDay("EVAL", day, SlugDeployFrequency); len(got.Rows) != 0 {
		t.Errorf("the zero day of deploy-frequency holds %d Legion rows, want none", len(got.Rows))
	}

	r := newRig(t)
	refused := ProjectDay{Project: "EVAL", Day: day, Rows: []DayRow{{
		ProjectKey: "EVAL", EvalID: EvalID("EVAL", SlugDeployFrequency), Listed: true, DayPart: measures.DayPart{Series: "deploys", Day: day},
	}}}
	if err := r.registry.writeProjectDay(t.Context(), legionFamily, refused, testBuild, true); err == nil {
		t.Errorf("the Legion write took a deploy-frequency row, want it refused")
	}
	if got := r.count(t, `select count(*) from eval_days`); got != 0 {
		t.Errorf("the refused write left %d rows, want none", got)
	}
}
