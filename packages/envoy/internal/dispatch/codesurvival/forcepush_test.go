package codesurvival

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// lowBudget is a remaining budget below the floor, a quarter of the limit, in a window that resets
// at reset.
func lowBudget(reset time.Time) codesurvivaltest.Budget {
	return codesurvivaltest.Budget{Limit: 5000, Remaining: 1000, Reset: reset}
}

// forcedRig is a rig after k3's first three runs over the 60-day branch a, with days 0 to 34
// measured, whose double then serves b: a force-push that replaced the eight commits of day 34 by
// five that remove lines of earlier days, so day 34 and every day after it hold other commits.
// The first pass after it (at twoWeeks) meets a budget below the floor once it has read the chain.
func forcedRig(t *testing.T) *rig {
	t.Helper()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	r.passAt(nextDay)
	r.passAt(weekLater)
	r.github.Serve(widgets, histories.b)
	r.github.SetBudget(lowBudget(twoWeeks.Add(30 * time.Minute)))
	return r
}

// The days a force-push at day 34 marks: each measured day whose 30 days hold a changed day, days
// 3 to 34 (34 − 31 = 3).
var forcedMarks = span(3, 35)

// codeRows is every row of project's code evals on day, each as `<cut>|<series>|<counts>`.
func (r *rig) codeRows(project string, on time.Time) []string {
	r.t.Helper()
	found, err := r.database.Pool.Query(r.ctx, `
		select repo || '|' || series || '|' || coalesce(counts::text, '') from eval_days
		where project_key = $1 and eval_id = any($2) and day = $3 order by 1`, project, codeEvalIDs(project), on)
	if err != nil {
		r.t.Fatalf("read the code rows of %s on %s: %v", project, on.Format(time.DateOnly), err)
	}
	rows, err := pgx.CollectRows(found, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the code rows of %s on %s: %v", project, on.Format(time.DateOnly), err)
	}
	return rows
}

// A force-push marks the measured days it touched before anything else (k7, R8, the day-state
// table's rows 5 and 7): the pass after it compares the stored fingerprints with the chain, finds
// day 34 and every day after it changed, and marks days 3 to 34 in a transaction of its own; then
// the budget, below the floor, stops it before it reads a pull request. The marks stand, each
// marked day holds its status rows alone, with marked 1 and the day's first_marked_day, every
// window that holds a marked day reads "no data" naming the first of them, no result is written,
// and days 0 to 2 read numbers as before.
func TestAForcePushMarksTheTouchedDaysBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	r := forcedRig(t)
	measured := r.measuredAt(widgets)
	results := r.dump("code_changes", "code_pairs")
	requests := r.passAt(twoWeeks)
	for _, request := range requests {
		if route := routeOf(request); route == "pulls" || route == "diff" || route == "compare" || route == "files" || route == "attribution" {
			t.Errorf("the pass after the force-push sent %s after the budget stop", request)
		}
	}
	states := r.days(widgets)
	for n := range 35 {
		want := survival.DayMeasured
		if slices.Contains(forcedMarks, n) {
			want = survival.DayMarked
		}
		if states[day(n)] != want {
			t.Errorf("day %d is %q after the force-push, want %q", n, states[day(n)], want)
		}
	}
	if !slices.Equal(r.dump("code_changes", "code_pairs"), results) {
		t.Error("the pass that met the budget floor wrote a result")
	}
	after := r.measuredAt(widgets)
	for measuredDay, at := range measured {
		if !after[measuredDay].Equal(at) {
			t.Errorf("%s was measured at %s and is now at %s, want it as it was", measuredDay.Format(time.DateOnly), at, after[measuredDay])
		}
	}
	for _, n := range forcedMarks {
		rows := r.codeRows(widgetsProject, day(n))
		if len(rows) != 4 {
			t.Errorf("marked day %d holds %d code rows, want the status row of each eval in each of two cuts: %v", n, len(rows), rows)
		}
		for _, row := range rows {
			if !strings.Contains(row, "|status|") || !strings.Contains(row, `"marked": 1`) ||
				!strings.Contains(row, `"first_marked_day": `+strconv.FormatInt(day(n).Unix(), 10)) {
				t.Errorf("marked day %d holds the row %q, want a status row with marked 1 and its first_marked_day", n, row)
			}
		}
	}
	windows := 0
	for start := 0; start+7 <= 35; start++ {
		read := r.read(widgetsProject, "agents", day(start), day(start+7))
		holds := start+6 >= forcedMarks[0]
		switch {
		case holds && (read.survival.Share != nil || read.rework.Share != nil):
			t.Errorf("the window from day %d holds a marked day and reads survival %s and rework %s, want no data", start, value(read.survival.Share), value(read.rework.Share))
		case holds && (read.status.FirstMarkedDay == nil || !read.status.FirstMarkedDay.Equal(day(max(start, forcedMarks[0])))):
			t.Errorf("the window from day %d names the marked day %v, want %s", start, read.status.FirstMarkedDay, day(max(start, forcedMarks[0])).Format(time.DateOnly))
		case !holds && read.survival.Share == nil:
			t.Errorf("the window from day %d holds no marked day and reads no data", start)
		}
		windows++
	}
	// Every window of seven days holds day 3, the first marked day, so days 0 to 2 are read alone.
	if read := r.read(widgetsProject, "agents", day(0), day(3)); read.survival.Share == nil || read.rework.Share == nil {
		t.Errorf("days 0 to 2 hold no marked day and read survival %s and rework %s, want a number of each", value(read.survival.Share), value(read.rework.Share))
	}
	lines := r.logs.lines(t, "codesurvival: run")
	if last := lines[len(lines)-1]; last["days_marked"] != float64(len(forcedMarks)) || last["stopped"] != stoppedBudget {
		t.Errorf("the pass logged %v, want %d days marked and a stop at the budget", last, len(forcedMarks))
	}
	t.Logf("%d days marked, %d windows read", len(forcedMarks), windows)
}

// results is every stored change, pair and day state of repo on the days of [from, to), without
// the times a run wrote: what two stores that measured those days alike hold alike.
func (r *rig) results(repo string, from, to time.Time) []string {
	r.t.Helper()
	found, err := r.database.Pool.Query(r.ctx, `
		select 'change ' || concat_ws(' ', sha, day, pr_number, head_ref, author, issue_key, added, alive, removed_by_non_pr)
		from code_changes where repo = $1 and day >= $2 and day < $3
		union all
		select 'pair ' || concat_ws(' ', p.adder_sha, p.remover_sha, p.remover_pr, p.remover_issue_key, p.lines, p.days_between)
		from code_pairs p join code_changes c on c.repo = p.repo and c.sha = p.adder_sha
		where p.repo = $1 and c.day >= $2 and c.day < $3
		union all
		select 'day ' || concat_ws(' ', day, state, commits, encode(digest, 'hex'), blocked_sha, blocked_files)
		from code_days where repo = $1 and day >= $2 and day < $3
		order by 1`, repo, from, to)
	if err != nil {
		r.t.Fatalf("read the results of %s: %v", repo, err)
	}
	rows, err := pgx.CollectRows(found, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the results of %s: %v", repo, err)
	}
	return rows
}

// A force-push is measured again and reads as a measurement from an empty store (k7, the day-state
// table's row 8): with the budget restored, the pass after the reset measures the 28 oldest marked
// days, days 3 to 30, from the new branch, and the next day's pass the rest, 31 to 34, with the
// days that wait by then. No mark is left; days 0 to 2, which nothing touched, keep their results
// and their day rows as written; and the results and the windows of days 0 to 34 are those of a
// store that measured the new branch from nothing.
func TestAForcePushIsMeasuredAgainAndEqualsAnEmptyStore(t *testing.T) {
	t.Parallel()
	r := forcedRig(t)
	untouched, untouchedRows := r.results(widgets, day(0), day(3)), [][]string{r.codeRows(widgetsProject, day(0)), r.codeRows(widgetsProject, day(1)), r.codeRows(widgetsProject, day(2))}
	r.passAt(twoWeeks)
	r.github.SetBudget(codesurvivaltest.DefaultBudget)
	again := twoWeeks.Add(40 * time.Minute)
	r.passAt(again)
	r.passAt(again.Add(24 * time.Hour))
	states, measured := r.days(widgets), r.measuredAt(widgets)
	for n := range 35 {
		want := firstPass
		switch {
		case n >= 3 && n <= 30:
			want = again
		case n >= 31:
			want = again.Add(24 * time.Hour)
		}
		if states[day(n)] != survival.DayMeasured || !measured[day(n)].Equal(want) {
			t.Errorf("day %d is %q, measured at %s, want measured at %s", n, states[day(n)], measured[day(n)], want)
		}
	}
	if !slices.Equal(r.results(widgets, day(0), day(3)), untouched) {
		t.Error("the days the force-push did not touch changed")
	}
	for n, rows := range untouchedRows {
		if !slices.Equal(r.codeRows(widgetsProject, day(n)), rows) {
			t.Errorf("the day rows of day %d, which the force-push did not touch, changed", n)
		}
	}

	histories := sixtyDays(t)
	empty := widgetsRig(t, histories.b)
	empty.passAt(firstPass)
	empty.passAt(weekLater)
	if got, want := r.results(widgets, day(0), day(35)), empty.results(widgets, day(0), day(35)); !slices.Equal(got, want) {
		t.Errorf("the results of days 0 to 34 after the force-push are not those of an empty store's measurement:\ngot  %v\nwant %v", got, want)
	}
	for start := 0; start+7 <= 35; start += 7 {
		got, want := r.read(widgetsProject, "agents", day(start), day(start+7)), empty.read(widgetsProject, "agents", day(start), day(start+7))
		if value(got.survival.Share) != value(want.survival.Share) || value(got.rework.Share) != value(want.rework.Share) || got.survival.Share == nil {
			t.Errorf("the window from day %d reads survival %s and rework %s, want the empty store's %s and %s", start,
				value(got.survival.Share), value(got.rework.Share), value(want.survival.Share), value(want.rework.Share))
		}
	}
}

// A force-push that replaces commits only in the 30 days after the measured days marks the measured
// days whose 30 days hold them too (R8, k7, the day-state table's row 6): days 0 to 27 are measured
// and days 28 to 57 hold fingerprints alone; c replaces day 50's commit and every one after it, so
// days 19 to 27 (50 − 31 = 19) are marked, and measured again in the same pass, from the new
// branch, while days 0 to 18 keep their measurement.
func TestAForcePushAfterTheMeasuredDaysMarksThemToo(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	r.github.Serve(widgets, histories.c)
	r.passAt(nextDay)
	lines := r.logs.lines(t, "codesurvival: run")
	if last := lines[len(lines)-1]; last["days_marked"] != float64(9) || last["stopped"] != stoppedNone {
		t.Fatalf("the pass after the force-push logged %v, want days 19 to 27 marked", last)
	}
	measured := r.measuredAt(widgets)
	for n := range 29 {
		want := firstPass
		if n >= 19 {
			want = nextDay
		}
		if !measured[day(n)].Equal(want) {
			t.Errorf("day %d was measured at %s, want at %s", n, measured[day(n)], want)
		}
	}
	if states := r.days(widgets); slices.ContainsFunc(span(0, 60), func(n int) bool { return states[day(n)] == survival.DayMarked }) {
		t.Error("a mark is left after the pass measured the marked days again")
	}
}

// A commit GitHub lists that no first parent of the branch reaches is never measured (k7): the
// double lists one dated day 10, a child of day 9's commit; the run reads no patch of it, stores
// its SHA nowhere, and day 10's fingerprint is that of the branch's own commits of the day.
func TestACommitOffTheBranchIsNeverMeasured(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	off := strings.Repeat("0f", 20)
	parent := commitsOn(histories.a, day(9), day(10))
	r.github.ListOffBranch(widgets, codesurvivaltest.Commit{SHA: off, Parents: []string{parent[len(parent)-1]}, CommittedAt: day(10).Add(12 * time.Hour)})
	requests := r.passAt(firstPass)
	if reads := patchReads(requests); reads[off] != 0 || len(reads) == 0 {
		t.Fatalf("the run read the patch of the commit off the branch %d times, and %d patches in all", reads[off], len(reads))
	}
	for _, row := range r.dump(resultTables...) {
		if strings.Contains(row, off) {
			t.Fatalf("the results hold the commit off the branch: %s", row)
		}
	}
	var chain []survival.Commit
	for _, commit := range histories.a.Chain {
		chain = append(chain, survival.Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt})
	}
	want := survival.Fingerprints(chain, day(10), day(10))[0]
	stored, err := FingerprintsFrom(r.ctx, r.database.Pool, widgets, day(10))
	if err != nil || len(stored) == 0 || stored[0].Day != day(10) || stored[0] != want {
		t.Fatalf("day 10's fingerprint is %+v (%v), want the branch's own %+v", stored, err, want)
	}
	if read := r.read(widgetsProject, "agents", day(7), day(14)); read.survival.Share == nil {
		t.Fatal("the window that holds day 10 reads no data")
	}
}
