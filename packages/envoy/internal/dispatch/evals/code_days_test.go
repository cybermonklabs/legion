package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
)

// codeGroups is the code groups the code evals' tests name, in order: the agents by a head-branch
// prefix and a login, and a catch-all after them.
var codeGroups = []survival.Group{{Name: "agents", BranchPrefixes: []string{"agent/"}, AuthorLogins: []string{"agent-app[bot]"}}, {Name: "others"}}

// codeRepo is the repository the code evals' tests measure, each project's default repository.
const codeRepo = "acme/code"

// groupNames is the names of groups, in their order.
func groupNames(groups []survival.Group) []string {
	names := make([]string, len(groups))
	for index, group := range groups {
		names[index] = group.Name
	}
	return names
}

// codeEvalIDs is project's eval ids of the code evals.
func codeEvalIDs(project string) []string {
	return []string{EvalID(project, SlugCodeSurvival), EvalID(project, SlugPostMergeRework)}
}

// codeHistory is the stored results the code evals' tests build their rows from: project's changes
// on its default repository codeRepo over three days from first, and the pairs that removed their
// lines. Of the agents' 176 lines added, 126 are alive at their marks; 40 were removed by a later
// change of the agents for another issue (rework), 5 by one for the same issue (iteration) and 5
// by a commit that merged no pull request (other). The others' one change, which has no issue, added
// 50 lines, all alive.
type codeHistory struct {
	project string
	first   time.Time
	changes []fold.Change
	pairs   []fold.Pair
}

// newCodeHistory is project's codeHistory over the three days from first.
func newCodeHistory(project string, first time.Time) *codeHistory {
	h := &codeHistory{project: project, first: first}
	one := h.change(0, 1, "agent/one", project+"-1", 100, 80)
	h.change(0, 2, "other/two", "", 50, 50)
	two := h.change(1, 3, "agent/two", project+"-2", 40, 10)
	h.changes[len(h.changes)-1].RemovedByNonPR = 5
	three := h.change(2, 4, "agent/three", project+"-3", 30, 30)
	four := h.change(2, 5, "agent/four", project+"-2", 6, 6)
	h.pairs = append(h.pairs, h.pair(one, three, 20), h.pair(two, three, 20), h.pair(two, four, 5))
	return h
}

// day is the history's day n.
func (h *codeHistory) day(n int) time.Time { return h.first.AddDate(0, 0, n) }

// change stores a change on day n by pull request number from head, for issue of the history's
// project ("" for none), adding added lines of which alive are alive at its mark, and answers it.
func (h *codeHistory) change(n, number int, head, issue string, added, alive int) fold.Change {
	issueProject := ""
	if issue != "" {
		issueProject = h.project
	}
	change := fold.Change{Repo: codeRepo, IssueProject: issueProject, Change: survival.Change{
		SHA: fmt.Sprintf("%040x", number), Day: h.day(n), CommittedAt: h.day(n).Add(9 * time.Hour),
		ChangeFacts: survival.ChangeFacts{PR: number, HeadRef: head, Author: "someone", IssueKey: issue},
		Added:       added, AliveAtMark: alive,
	}}
	h.changes = append(h.changes, change)
	return change
}

// pair is the stored pair of lines of adder's that remover removed.
func (h *codeHistory) pair(adder, remover fold.Change, lines int) fold.Pair {
	return fold.Pair{Repo: codeRepo, Pair: survival.Pair{AdderSHA: adder.SHA, RemoverSHA: remover.SHA, Remover: remover.ChangeFacts, Lines: lines,
		DaysBetween: remover.CommittedAt.Sub(adder.CommittedAt).Hours() / 24}}
}

// rows is the code evals' rows of the history's three days under groups, each day measured but
// those states names otherwise, in the project's own cut and in codeRepo's (CodeDayRows), as
// fold.BuildDays builds them.
func (h *codeHistory) rows(groups []survival.Group, states map[int]survival.DayState) []DayRow {
	input := fold.BuildInput{Project: fold.Project{Key: h.project, DefaultRepo: codeRepo}, Groups: groups, Changes: h.changes, Pairs: h.pairs}
	for n := range 3 {
		state, held := states[n]
		if !held {
			state = survival.DayMeasured
		}
		input.Days = append(input.Days, fold.Day{Day: h.day(n), State: state, BlockedFiles: 2})
	}
	survivalParts, reworkParts := fold.BuildDays(input)
	return append(CodeDayRows(h.project, "", survivalParts, reworkParts), CodeDayRows(h.project, codeRepo, survivalParts, reworkParts)...)
}

// days is the history's three days.
func (h *codeHistory) days() []time.Time { return []time.Time{h.day(0), h.day(1), h.day(2)} }

// window is the 28 days from the history's first, a range the stored figures hold that starts on
// the first day of the code evals' series, so it reads their values (valuesOf).
func (h *codeHistory) window() Range { return Range{From: h.day(0), To: h.day(28)} }

// writeCode writes, as the code writer does, in a transaction of its own: groups' series into the
// project's code evals (WriteCodeGroupSeriesTx), then rows, the rows of the evals on days
// (WriteCodeDaysTx), and after the commit it moves the changes into the log. It answers how many
// series versions it wrote.
func (r *refresherRig) writeCode(t *testing.T, project string, groups []survival.Group, days []time.Time, rows []DayRow) int {
	t.Helper()
	ctx := t.Context()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the code rows of %s: %v", project, err)
	}
	defer tx.Rollback(ctx)
	written, err := WriteCodeGroupSeriesTx(ctx, tx, project, groupNames(groups))
	if err != nil {
		t.Fatalf("write the code group series of %s: %v", project, err)
	}
	if err := WriteCodeDaysTx(ctx, tx, project, codeEvalIDs(project), days, rows, r.build); err != nil {
		t.Fatalf("write the code rows of %s: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the code rows of %s: %v", project, err)
	}
	r.registry.drainChanges(ctx)
	return written
}

// codeRows is every stored row of project's code evals, by rowKey, each with the transaction that
// last wrote it (xmin).
func (r *refresherRig) codeRows(t *testing.T, project string) map[string]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select project_key, eval_id, series, repo, day, xmin::text from eval_days where project_key = $1 and eval_id = any($2)`, project, codeEvalIDs(project))
	if err != nil {
		t.Fatalf("read the code rows of %s: %v", project, err)
	}
	stored := map[string]string{}
	var row DayRow
	var xmin string
	if _, err := pgx.ForEachRow(rows, []any{&row.ProjectKey, &row.EvalID, &row.Series, &row.Repo, &row.Day, &xmin}, func() error {
		row.Day = row.Day.UTC()
		stored[rowKey(row)] = xmin
		return nil
	}); err != nil {
		t.Fatalf("read the code rows of %s: %v", project, err)
	}
	return stored
}

// rewrittenRows is the keys of after whose row a transaction other than before's wrote, and of the
// rows of before after no longer holds, sorted.
func rewrittenRows(before, after map[string]string) []string {
	var keys []string
	for key, xmin := range after {
		if before[key] != xmin {
			keys = append(keys, key)
		}
	}
	for key := range before {
		if _, held := after[key]; !held {
			keys = append(keys, "gone "+key)
		}
	}
	slices.Sort(keys)
	return keys
}

// codeValues is the list read's values of project's code evals over window (listValues), each
// eval's series by name, failing the test where the stored figures did not answer an eval or
// answered other than the fold of its day rows.
func (r *refresherRig) codeValues(t *testing.T, project string, repos []string, window Range) map[string]map[string]SeriesValues {
	t.Helper()
	stated, folded, fromFigures := r.listValues(t, project, repos, window, nil)
	values := map[string]map[string]SeriesValues{}
	for _, evalID := range codeEvalIDs(project) {
		if !fromFigures[evalID] || stated[evalID] != folded[evalID] {
			t.Errorf("%s over %s of %v: the stored figures answered %t, %s, and the day rows fold %s", evalID, daysText(window), repos, fromFigures[evalID],
				stated[evalID], folded[evalID])
		}
		var series []SeriesValues
		if err := json.Unmarshal([]byte(stated[evalID]), &series); err != nil {
			t.Fatalf("decode the values of %s: %v", evalID, err)
		}
		values[evalID] = map[string]SeriesValues{}
		for _, one := range series {
			values[evalID][one.Series.Series] = one
		}
	}
	return values
}

// figure is statistic of series of evalID among values as a failure states it: the number, or
// "no value" (showValue).
func figure(values map[string]map[string]SeriesValues, evalID, series, statistic string) string {
	held, found := values[evalID][series]
	if !found {
		return "no series"
	}
	return showValue(held.Window[statistic])
}

// TestWriteCodeDaysWritesOnlyTheRowsThatChanged: once EVAL's figures are built, the code writer's
// write of three measured days under two code groups writes each group's series into both code
// evals and every row of the days, in the project's own cut and in its repository's: 12 rows a day
// a cut, logged as one eval_days change of each eval. The stored figures are then the fold of the
// rows that stand, and the list read of the 28 days from the first answers from them the agents'
// survival share 126/176, their rework share 40/176, the others' change that reached the project
// through its repository, and the 25 days of the window after the newest row as waiting. A second write of the
// same rows rewrites none and logs no change. A write in which one change's lines alive at its
// mark moved rewrites the agents' survival row of that day in each cut and nothing else. A write in
// which the middle day is marked deletes that day's group rows, keeps its status rows with the
// mark, and the window reads "no data" and names the day. A row of a day not written, or of another
// eval, is refused before anything is written.
func TestWriteCodeDaysWritesOnlyTheRowsThatChanged(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.settle(t)
	project := r.project
	history := newCodeHistory(project, refresherToday.AddDate(0, 0, -40))
	survivalID, reworkID := EvalID(project, SlugCodeSurvival), EvalID(project, SlugPostMergeRework)
	window := history.window()

	heads := r.changeHeads(t)
	if written := r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil)); written != 10 {
		t.Errorf("the first write wrote %d series versions, want the 2 groups' survival series and their 8 rework series", written)
	}
	first := r.codeRows(t, project)
	if len(first) != 3*2*12 {
		t.Fatalf("the first write stored %d rows, want 12 a day (each eval's status, 2 survival series and 8 rework series) in 2 cuts over 3 days", len(first))
	}
	logged := r.loggedChanges(t, "eval_days", heads)
	var changed []string
	for _, change := range logged {
		changed = append(changed, change.evalID)
	}
	if slices.Sort(changed); !slices.Equal(changed, codeEvalIDs(project)) {
		t.Errorf("the first write logged the eval_days changes of %v, want one of each code eval", changed)
	}
	expectTruth(t, "the first write", r.storedFigures(t), r.truthFigures(t))
	values := r.codeValues(t, project, nil, window)
	for _, want := range []struct{ evalID, series, statistic, value string }{
		{survivalID, "survival:agents", "share", showValue(new(126.0 / 176))},
		{survivalID, "survival:agents", "added", "176"},
		{survivalID, "survival:agents", "changes", "4"},
		{survivalID, "survival:others", "share", "1"},
		{survivalID, "survival:others", "via_repository", "1"},
		{reworkID, "rework:agents", "share", showValue(new(40.0 / 176))},
		{reworkID, "iteration:agents", "lines", "5"},
		{reworkID, "other:agents", "lines", "5"},
		{reworkID, "noissue:agents", "lines", "0"},
		{survivalID, fold.SeriesStatus, "measured_days", "3"},
		{survivalID, fold.SeriesStatus, "waiting_days", "25"},
		{survivalID, fold.SeriesStatus, "groups", "2"},
		{reworkID, fold.SeriesStatus, "marked_days", "0"},
	} {
		if got := figure(values, want.evalID, want.series, want.statistic); got != want.value {
			t.Errorf("the 28 days of %s read %s %s %s %s, want %s", project, want.evalID, want.series, want.statistic, got, want.value)
		}
	}
	if got := values[survivalID]["survival:others"].ViaRepository; got != 1 {
		t.Errorf("the others' survival series counts %d rows through the repository, want the 1 change with no issue", got)
	}
	repository := r.codeValues(t, project, []string{codeRepo}, window)
	if got, want := figure(repository, survivalID, "survival:agents", "share"), figure(values, survivalID, "survival:agents", "share"); got != want {
		t.Errorf("the cut of %s reads the agents' share %s, want the project's %s: every change is of that repository", codeRepo, got, want)
	}

	heads = r.changeHeads(t)
	if written := r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil)); written != 0 {
		t.Errorf("a second write of the same groups wrote %d series versions, want none", written)
	}
	if moved := rewrittenRows(first, r.codeRows(t, project)); len(moved) != 0 {
		t.Errorf("a second write of the same rows rewrote %v, want none", moved)
	}
	if again := r.loggedChanges(t, "eval_days", heads); len(again) != 0 {
		t.Errorf("a second write of the same rows logged the changes %v, want none", again)
	}

	history.changes[2].AliveAtMark = 15
	heads = r.changeHeads(t)
	r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil))
	second := r.codeRows(t, project)
	day := history.day(1).Format(time.DateOnly)
	want := []string{
		fmt.Sprintf("%s %s survival:agents  %s", project, survivalID, day),
		fmt.Sprintf("%s %s survival:agents %s %s", project, survivalID, codeRepo, day),
	}
	slices.Sort(want)
	if moved := rewrittenRows(first, second); !slices.Equal(moved, want) {
		t.Errorf("a write in which one change's alive lines moved rewrote %v, want %v", moved, want)
	}
	if logged := r.loggedChanges(t, "eval_days", heads); len(logged) != 1 || logged[0].evalID != survivalID {
		t.Errorf("a write in which one change's alive lines moved logged the changes %v, want one of %s", logged, survivalID)
	}
	expectTruth(t, "a write of one moved row", r.storedFigures(t), r.truthFigures(t))
	if got := figure(r.codeValues(t, project, nil, window), survivalID, "survival:agents", "share"); got != showValue(new(131.0/176)) {
		t.Errorf("after the move the agents' survival share reads %s, want 131/176", got)
	}

	// The middle day marked: a window that holds it reads "no data", though each group series still
	// holds a row on its first day, the window's.
	r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, map[int]survival.DayState{1: survival.DayMarked}))
	marked := r.codeRows(t, project)
	var markedDay []string
	for key := range marked {
		if strings.HasSuffix(key, " "+history.day(1).Format(time.DateOnly)) {
			markedDay = append(markedDay, key)
		}
	}
	if len(markedDay) != 4 || len(marked) != len(first)-2*10 {
		t.Errorf("after the middle day was marked, it holds the rows %v and the evals %d rows, want its 4 status rows alone, %d rows", markedDay,
			len(marked), len(first)-2*10)
	}
	for _, key := range markedDay {
		if !strings.Contains(key, " "+fold.SeriesStatus+" ") {
			t.Errorf("the marked day holds %s, want its status rows alone", key)
		}
	}
	status := r.day(t, survivalID, fold.SeriesStatus, "", history.day(1))
	if status.Counts[fold.CountMarked] != 1 || status.Counts[fold.CountFirstMarkedDay] != float64(history.day(1).Unix()) || status.Counts[fold.CountMeasured] != 0 {
		t.Errorf("the marked day's status row counts %v, want marked 1, its first_marked_day and measured 0", status.Counts)
	}
	expectTruth(t, "a marked day", r.storedFigures(t), r.truthFigures(t))
	values = r.codeValues(t, project, nil, window)
	for _, series := range []struct{ evalID, name string }{{survivalID, "survival:agents"}, {reworkID, "rework:agents"}, {survivalID, "survival:others"}} {
		if share := figure(values, series.evalID, series.name, "share"); share != "no value" {
			t.Errorf("a window holding a marked day reads %s's share %s, want no data", series.name, share)
		}
	}
	if got := figure(values, reworkID, fold.SeriesStatus, "first_marked_day"); got != showValue(new(float64(history.day(1).Unix()))) {
		t.Errorf("a window holding a marked day names first_marked_day %s, want %s's", got, history.day(1).Format(time.DateOnly))
	}

	ctx := t.Context()
	for _, refused := range []struct {
		what    string
		evalIDs []string
		rows    []DayRow
	}{
		{"a row of a day not written", codeEvalIDs(project), (&codeHistory{project: project, first: history.day(3), changes: nil}).rows(codeGroups, nil)},
		{"a row of another eval", []string{survivalID}, history.rows(codeGroups, nil)},
		{"an eval id that is no code eval", []string{EvalID(project, SlugReworkShare)}, nil},
	} {
		tx, err := r.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteCodeDaysTx(ctx, tx, project, refused.evalIDs, history.days(), refused.rows, r.build); err == nil {
			t.Errorf("%s was written, want it refused", refused.what)
		}
		tx.Rollback(ctx)
	}
	if moved := rewrittenRows(marked, r.codeRows(t, project)); len(moved) != 0 {
		t.Errorf("the refused writes moved %v, want nothing", moved)
	}
	t.Logf("%d rows written, %d a day of each cut; one moved change rewrote %v; the marked day kept %d status rows", len(first), 12, want, len(markedDay))
}

// TestARemovedGroupsSeriesLeavesTheRebuiltDays: a regroup that drops the others' group, as the
// code writer rebuilds every day from the stored results under the project's groups, unlists the
// group's five series (one survival series and four rework series), deletes every row of them from
// the days it writes, and writes the status rows with the one group; the agents' series stand at
// their first version and their figures as they were, and the list read states no series of the
// dropped group. The stored figures are the fold of the rows that stand. The group named again is
// listed again, in a next version of each series, and its rows come back.
func TestARemovedGroupsSeriesLeavesTheRebuiltDays(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.settle(t)
	project := r.project
	history := newCodeHistory(project, refresherToday.AddDate(0, 0, -40))
	survivalID, reworkID := EvalID(project, SlugCodeSurvival), EvalID(project, SlugPostMergeRework)
	window := history.window()
	r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil))
	before := r.codeValues(t, project, nil, window)
	if share := figure(before, survivalID, "survival:others", "share"); share != "1" {
		t.Fatalf("before the regroup the others' survival share reads %s, want 1", share)
	}

	agents := codeGroups[:1]
	if written := r.writeCode(t, project, agents, history.days(), history.rows(agents, nil)); written != 5 {
		t.Errorf("the regroup wrote %d series versions, want the dropped group's 5 unlisted", written)
	}
	rows := r.codeRows(t, project)
	if len(rows) != 3*2*(2+5) {
		t.Errorf("the regroup left %d rows, want each eval's status row and the agents' 5 series a day of each cut, %d", len(rows), 3*2*(2+5))
	}
	for key := range rows {
		if strings.Contains(key, ":others ") {
			t.Errorf("the regroup left the row %s of the dropped group", key)
		}
	}
	for _, evalID := range codeEvalIDs(project) {
		slug := slugOf(project, evalID)
		_, series := r.currentOf(t, slug)
		for name, one := range series {
			switch {
			case strings.HasSuffix(name, ":others") && (one.Listed || one.Version != 2):
				t.Errorf("%s's series %s is listed %t at version %d, want version 2 unlisted", evalID, name, one.Listed, one.Version)
			case strings.HasSuffix(name, ":agents") && (!one.Listed || one.Version != 1):
				t.Errorf("%s's series %s is listed %t at version %d, want version 1 listed", evalID, name, one.Listed, one.Version)
			}
		}
	}
	if status := r.day(t, reworkID, fold.SeriesStatus, codeRepo, history.day(2)); status.Counts[fold.CountGroups] != 1 {
		t.Errorf("the regrouped status row counts %v groups, want 1", status.Counts[fold.CountGroups])
	}
	expectTruth(t, "the regroup", r.storedFigures(t), r.truthFigures(t))
	after := r.codeValues(t, project, nil, window)
	for _, evalID := range codeEvalIDs(project) {
		for name := range after[evalID] {
			if strings.HasSuffix(name, ":others") {
				t.Errorf("the list read of %s states the dropped group's series %s", evalID, name)
			}
		}
	}
	for _, series := range []struct{ evalID, name string }{{survivalID, "survival:agents"}, {reworkID, "rework:agents"}} {
		if got, want := figure(after, series.evalID, series.name, "share"), figure(before, series.evalID, series.name, "share"); got != want {
			t.Errorf("after the regroup %s reads %s's share %s, want %s as before", series.evalID, series.name, got, want)
		}
	}

	if written := r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil)); written != 5 {
		t.Errorf("naming the group again wrote %d series versions, want its 5 listed again", written)
	}
	if again := r.codeRows(t, project); len(again) != 3*2*12 {
		t.Errorf("with the group named again %d rows stand, want %d", len(again), 3*2*12)
	}
	_, series := r.currentOf(t, SlugCodeSurvival)
	if others := series["survival:others"]; !others.Listed || others.Version != 3 {
		t.Errorf("the named group's survival series is listed %t at version %d, want version 3 listed", others.Listed, others.Version)
	}
	expectTruth(t, "the group named again", r.storedFigures(t), r.truthFigures(t))
	if got, want := figure(r.codeValues(t, project, nil, window), survivalID, "survival:others", "share"), figure(before, survivalID, "survival:others", "share"); got != want {
		t.Errorf("the group named again reads its survival share %s, want %s as before", got, want)
	}
}

// TestTheSeedKeepsTheCodeGroupSeries: the code group series the code writer writes are no series
// of the catalogue, and the seed keeps them. After the first seed, two groups' series are written
// into both code evals; a second seed writes nothing, a write of the same groups again writes no
// version, and once a code eval's method block is deleted, the seed that writes it a new section
// and a new version leaves every group series at its version, listed, and writes no series
// version of either eval.
func TestTheSeedKeepsTheCodeGroupSeries(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.registry.Ensure(ctx, r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	writeGroups := func() int {
		t.Helper()
		tx, err := r.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		written, err := WriteCodeGroupSeriesTx(ctx, tx, r.project, groupNames(codeGroups))
		if err != nil {
			t.Fatalf("write the code group series of %s: %v", r.project, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return written
	}
	if written := writeGroups(); written != 10 {
		t.Fatalf("the first write of the groups wrote %d series versions, want 10", written)
	}
	held := func() map[string]Series {
		t.Helper()
		series := map[string]Series{}
		for _, slug := range codeDaySlugs {
			_, current := r.currentOf(t, slug)
			for name, one := range current {
				series[slug+" "+name] = one
			}
		}
		return series
	}
	groups := held()
	expect := func(what string) {
		t.Helper()
		now := held()
		if !slices.Equal(slices.Sorted(maps.Keys(now)), slices.Sorted(maps.Keys(groups))) {
			t.Errorf("%s: the code evals hold the series %v, want %v", what, slices.Sorted(maps.Keys(now)), slices.Sorted(maps.Keys(groups)))
		}
		listed := 0
		for name, one := range now {
			if was := groups[name]; one.Version != was.Version || one.Listed != was.Listed {
				t.Errorf("%s: %s stands at version %d listed %t, want version %d listed %t", what, name, one.Version, one.Listed, was.Version, was.Listed)
			}
			if one.Listed {
				listed++
			}
		}
		if listed != 12 {
			t.Errorf("%s: %d series of the code evals are listed, want each status series and the 10 group series", what, listed)
		}
	}
	state := r.seededState(t, r.project)
	if again, err := r.registry.Ensure(ctx, r.project); err != nil || again != (Seeded{}) {
		t.Fatalf("a seed after the group series wrote %+v, %v, want nothing", again, err)
	}
	if now := r.seededState(t, r.project); now.versions != state.versions || now.series != state.series {
		t.Errorf("a seed after the group series moved the versions %d -> %d and the series versions %d -> %d, want neither", state.versions, now.versions,
			state.series, now.series)
	}
	expect("a second seed")
	if written := writeGroups(); written != 0 {
		t.Errorf("a second write of the same groups wrote %d series versions, want none", written)
	}

	definition, _ := r.currentOf(t, SlugCodeSurvival)
	r.deleteBlock(t, r.documentID(t, r.project), definition.DocBlock)
	before := r.seededState(t, r.project)
	restored, err := r.registry.Ensure(ctx, r.project)
	if err != nil {
		t.Fatalf("seed %s after the delete: %v", r.project, err)
	}
	if restored.Versions != 1 || restored.Sections != 1 || restored.Series != 0 {
		t.Errorf("the seed after the delete wrote %+v, want one version and one section of %s and no series version", restored, SlugCodeSurvival)
	}
	if after := r.seededState(t, r.project); after.series != before.series {
		t.Errorf("the seed after the delete moved the series versions %d -> %d, want none", before.series, after.series)
	}
	expect("a seed that writes a version of the eval")
	t.Logf("%d code series held through two seeds; the seed after the delete wrote %+v", len(groups), restored)
}

// TestTheRefresherWritesNoRowForTheCodeEvals: over the delivery and Legion histories, settled, the
// code writer writes DELIV's three code days. The refresher's passes of two more UTC days, which
// write every family's rows of each new day, and a release's rebuild, which rebuilds every family's
// rows aside and flips them, write no row of either code eval and rewrite and delete none (R7): the
// code evals' rows are the code writer's alone. The stored figures stay the fold of the rows that
// stand, and the release's seed keeps the group series.
func TestTheRefresherWritesNoRowForTheCodeEvals(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	project := deliveries.Project
	history := newCodeHistory(project, refresherToday.AddDate(0, 0, -40))
	r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil))
	written := r.codeRows(t, project)
	if len(written) != 3*2*12 {
		t.Fatalf("the code writer stored %d rows of %s, want %d", len(written), project, 3*2*12)
	}
	codeRowsEverywhere := func() int {
		t.Helper()
		return r.count(t, `select count(*) from eval_days where substr(eval_id, length(project_key) + 2) = any($1)`, codeDaySlugs)
	}

	for day := 1; day <= 2; day++ {
		r.now = refresherNow.AddDate(0, 0, day)
		r.settle(t)
		today := refresherToday.AddDate(0, 0, day)
		if rows := r.count(t, `select count(*) from eval_days where day = $1`, today); rows == 0 {
			t.Fatalf("the passes of %s wrote no row of that day, so the check of the code evals proves nothing", today.Format(time.DateOnly))
		}
		if moved := rewrittenRows(written, r.codeRows(t, project)); len(moved) != 0 || codeRowsEverywhere() != len(written) {
			t.Errorf("the passes of %s moved the code rows %v and leave %d code rows, want none moved and the %d the code writer wrote",
				today.Format(time.DateOnly), moved, codeRowsEverywhere(), len(written))
		}
	}
	r.release("another-build")
	r.settle(t)
	if rebuilds := r.count(t, `select count(*) from eval_rebuilds where state = 'flipped'`); rebuilds == 0 {
		t.Fatalf("the release's passes flipped no rebuild, so the check of the rebuild proves nothing")
	}
	if moved := rewrittenRows(written, r.codeRows(t, project)); len(moved) != 0 || codeRowsEverywhere() != len(written) {
		t.Errorf("the release's rebuild moved the code rows %v and leaves %d code rows, want none moved and the %d the code writer wrote",
			moved, codeRowsEverywhere(), len(written))
	}
	series := map[string]Series{}
	for _, entry := range r.current(t, project).Items {
		if entry.Definition.Slug == SlugPostMergeRework {
			for _, one := range entry.Series {
				series[one.Series] = one
			}
		}
	}
	if others := series["other:others"]; !others.Listed || others.Version != 1 {
		t.Errorf("after the release's seed the series other:others is listed %t at version %d, want version 1 listed", others.Listed, others.Version)
	}
	expectTruth(t, "the release's rebuild", r.storedFigures(t), r.truthFigures(t))
	t.Logf("%d code rows stood through two days of passes and a release's rebuild", len(written))
}

// TestTheCodeEvalsViaRepositoryIsReadAndNoOtherChanges: a project reads its own rows of the code
// evals with the via_repository they store (viaAsRead): the others' change with no issue that
// reached DELIV through its repository counts 1 through the repository, in the list read's
// via-repository count and in the fold's figure. Every other eval reads a project's own rows as
// counting none through a repository, as before: the first own row of another eval that counts an
// item, stored with a count through the repository, reads 0. Over the delivery and Legion
// histories, the list read of every eval of each project, over its own rows, over the code
// repository's and over its default repository's, of the code days and of the newest days, answers
// the fold of the rows of its cut as cutValues selects them from every stored row by that rule.
func TestTheCodeEvalsViaRepositoryIsReadAndNoOtherChanges(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, legion := r.seedHistories(t)
	r.settle(t)
	project := deliveries.Project
	history := newCodeHistory(project, refresherToday.AddDate(0, 0, -40))
	r.writeCode(t, project, codeGroups, history.days(), history.rows(codeGroups, nil))
	window := history.window()
	values := r.codeValues(t, project, nil, window)
	survivalID := EvalID(project, SlugCodeSurvival)
	if got := values[survivalID]["survival:others"].ViaRepository; got != 1 {
		t.Errorf("%s's own rows count %d of the others' changes through the repository, want the 1 stored", project, got)
	}
	if got := figure(values, survivalID, "survival:others", "via_repository"); got != "1" {
		t.Errorf("%s's fold of the others' series states via_repository %s, want 1", project, got)
	}

	var other string
	var day time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `
		select eval_id, day from eval_days where project_key = $1 and repo = '' and n > 0 and listed
			and substr(eval_id, length(project_key) + 2) <> all($2)
		order by eval_id, day limit 1`, project, codeDaySlugs).Scan(&other, &day); err != nil {
		t.Fatalf("read an own row of %s's that counts an item: %v", project, err)
	}
	r.exec(t, "store a count through the repository on an own row of another eval", `
		update eval_days set via_repository = n where project_key = $1 and eval_id = $2 and repo = '' and day = $3`, project, other, day)
	deployRepos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := DayWindowIn(t.Context(), r.database.Pool, []string{project}, r.entries(t, other, survivalID), deployRepos, nil, Range{From: day, To: day.AddDate(0, 0, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, row := range rows {
		if row.EvalID == other && row.owner == project {
			read++
			if row.Part.ViaRepository != 0 {
				t.Errorf("%s's own row of %s %s on %s reads via_repository %d, want 0: the project counts its own rows of it through an issue",
					project, other, row.Part.Series, day.Format(time.DateOnly), row.Part.ViaRepository)
			}
		}
	}
	if read == 0 {
		t.Fatalf("the read of %s on %s answered no own row, so the check proves nothing", other, day.Format(time.DateOnly))
	}

	compared := 0
	for _, project := range []string{deliveries.Project, legion.Project} {
		cut := r.readCut(t, project)
		cuts := [][]string{nil, {codeRepo}}
		if cut.published != "" {
			cuts = append(cuts, []string{cut.published})
		}
		for _, repos := range cuts {
			for _, span := range []Range{window, rangeOf(8, refresherToday.AddDate(0, 0, 1))} {
				_, folded, _ := r.listValues(t, project, repos, span, nil)
				want := cut.cutValues(t, repos, span, nil)
				for _, entry := range cut.entries {
					id := entry.Definition.EvalID
					if folded[id] != want[id] {
						t.Errorf("%s of %v over %s reads %s, want the fold of its cut's rows %s", id, repos, daysText(span), folded[id], want[id])
					}
					compared++
				}
			}
		}
	}
	if compared == 0 {
		t.Fatal("compared no eval's read")
	}
	t.Logf("%d reads of an eval's cut compared; %d own rows of %s read as counting none through the repository", compared, read, other)
}
