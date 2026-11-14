package evals

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// refresherNow is the moment the refresher's tests run their passes as of: noon of deliverytest's
// DefaultNow, the UTC day after the delivery history's last, so today holds no delivery input and
// every series that has begun holds a zero row of today.
var refresherNow = deliverytest.DefaultNow.Add(12 * time.Hour)

// refresherToday is refresherNow's UTC day.
var refresherToday = utcMidnight(refresherNow)

// refresherRig is one test's refresher over a database of its own: the registry rig, the event
// horizon a pass reads the log through, the moment and the build its passes run as, and the days
// a pass walks of each walk.
type refresherRig struct {
	rig
	horizon *events.Horizon
	now     time.Time
	build   string
	batch   int
}

func newRefresherRig(t *testing.T) *refresherRig {
	t.Helper()
	r := newRig(t)
	return &refresherRig{rig: r, horizon: events.NewHorizon(r.database.Pool), now: refresherNow, build: testBuild, batch: walkBatch}
}

// pass makes one refresher pass as of the rig's clock, under its build, and answers what it wrote
// and when it asks to run again. It proves the event log's safe horizon first, as the Legion facts
// consumer's tests do: a pass reads the log only up to the horizon, which a later observation
// raises past a write the test just made.
func (r *refresherRig) pass(t *testing.T) (refreshed, time.Time) {
	t.Helper()
	if _, err := r.horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	report, next, err := r.registry.refresh(t.Context(), RefresherDeps{Horizon: r.horizon, Build: r.build},
		refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatalf("the refresher's pass failed: %v", err)
	}
	return report, next
}

// settle makes passes until one leaves no walk with days left (its next pass is not asked for at
// once), and answers how many it made and every day they wrote.
func (r *refresherRig) settle(t *testing.T) (int, refreshed) {
	t.Helper()
	all := refreshed{days: map[builtDay]bool{}}
	for passes := 1; passes <= 100; passes++ {
		report, next := r.pass(t)
		maps.Copy(all.days, report.days)
		if !next.Equal(r.now) {
			return passes, all
		}
	}
	t.Fatal("100 passes left a walk with days still to build")
	return 0, all
}

// exec runs one statement of a test's own setup.
func (r *refresherRig) exec(t *testing.T, what, statement string, args ...any) {
	t.Helper()
	if _, err := r.database.Pool.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// seedHistories writes the delivery history of deliverytest (five days, the projects DELIV and
// DELIVX, ending at today's midnight) and the Legion history of legiontest (project LEGION, six
// trees ending at the same moment), with LEGION's facts and filings stored as the Legion facts
// consumer computes and writes them.
func (r *refresherRig) seedHistories(t *testing.T) (deliverytest.Fixture, legiontest.Fixture) {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	deliveries := deliverytest.Seed(t, pool, deliverytest.Spec{Days: 5})
	legion := legiontest.Seed(t, pool, legiontest.Spec{Trees: 6, Now: deliverytest.DefaultNow})
	rows, err := legionfacts.ComputeProject(ctx, pool, legion.Project, deliverytest.DefaultNow)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", legion.Project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: legion.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", legion.Project, err)
	}
	filings := projectFilings(t, legion.Project, deliverytest.DefaultNow, r.rig)
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: legion.Project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", legion.Project, err)
	}
	t.Logf("stored %d facts rows and %d filings of %s", len(rows), len(filings), legion.Project)
	return deliveries, legion
}

// rowKey is a day row's identity, `<project> <eval id> <series> <repo> <day>`.
func rowKey(row DayRow) string {
	return strings.Join([]string{row.ProjectKey, row.EvalID, row.Series, row.Repo, row.Day.Format(time.DateOnly)}, " ")
}

// familyOf is the family a pass's report names a row's day under: the delivery and CI rows one
// delivery write holds (walkDelivery), the Legion rows (walkLegion), the unowned-p0 row
// (SlugUnownedP0) and a logged eval's (walkLogged).
func familyOf(row DayRow) string {
	slug := strings.TrimPrefix(row.EvalID, row.ProjectKey+"/")
	switch {
	case slices.Contains(deliveryDaySlugs, slug):
		return walkDelivery
	case slices.Contains(legionDaySlugs, slug):
		return walkLegion
	case slug == SlugUnownedP0:
		return SlugUnownedP0
	}
	return walkLogged
}

// dayLine is a row's day as a pass's report names it, `<family> <project> <day>`.
func dayLine(row DayRow) string {
	return familyOf(row) + " " + row.ProjectKey + " " + row.Day.Format(time.DateOnly)
}

// dayRows is every stored day row, by rowKey.
func (r *refresherRig) dayRows(t *testing.T) map[string]DayRow {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select `+dayColumns+` from eval_days`)
	if err != nil {
		t.Fatalf("read the day rows: %v", err)
	}
	defer rows.Close()
	stored := map[string]DayRow{}
	for rows.Next() {
		row, err := scanDay(rows)
		if err != nil {
			t.Fatalf("read the day rows: %v", err)
		}
		stored[rowKey(row)] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the day rows: %v", err)
	}
	return stored
}

// rewritten is the days (dayLine) of every row that appeared, went or had its updated_at moved
// between before and after, sorted, and one line per such day naming how many of its rows moved
// and their updated_at before and after.
func rewritten(before, after map[string]DayRow) ([]string, []string) {
	type moved struct {
		rows          int
		before, after time.Time
	}
	days := map[string]*moved{}
	note := func(line string, was, now time.Time) {
		if days[line] == nil {
			days[line] = &moved{}
		}
		days[line].rows++
		days[line].before, days[line].after = was, now
	}
	for key, row := range after {
		old, held := before[key]
		switch {
		case !held:
			note(dayLine(row), time.Time{}, row.UpdatedAt)
		case !old.UpdatedAt.Equal(row.UpdatedAt):
			note(dayLine(row), old.UpdatedAt, row.UpdatedAt)
		}
	}
	for key, row := range before {
		if _, kept := after[key]; !kept {
			note(dayLine(row), row.UpdatedAt, time.Time{})
		}
	}
	lines := slices.Sorted(maps.Keys(days))
	described := make([]string, len(lines))
	for i, line := range lines {
		day := days[line]
		described[i] = fmt.Sprintf("%s: %d rows, updated_at %s -> %s", line, day.rows,
			day.before.UTC().Format(time.RFC3339Nano), day.after.UTC().Format(time.RFC3339Nano))
	}
	return lines, described
}

// latestSeq is the newest delivery change's seq.
func (r *refresherRig) latestSeq(t *testing.T) int64 {
	t.Helper()
	seq, err := delivery.LatestChangeSeq(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the newest delivery change: %v", err)
	}
	return seq
}

// evalDayChanges is every eval_days change logged after seq, as `<eval id> <first day>..<last
// day>`, in seq order.
func (r *refresherRig) evalDayChanges(t *testing.T, after int64) []string {
	t.Helper()
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, after, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", after, err)
	}
	var lines []string
	for _, change := range changes {
		if change.Kind == "eval_days" {
			lines = append(lines, fmt.Sprintf("%s %s..%s", change.Ref, change.TOld.UTC().Format(time.DateOnly), change.TNew.UTC().Format(time.DateOnly)))
		}
	}
	return lines
}

// expectDays makes one pass after the test changed an input, and checks that it wrote exactly the
// days want names (dayLine's form), that no row of any other day was rewritten, and that a row of
// each day of mustMove was: a day whose input the change moved. It logs what each rewritten day's
// rows held for updated_at before and after, and every eval_days change row the pass logged.
func (r *refresherRig) expectDays(t *testing.T, what string, before map[string]DayRow, want, mustMove []string) map[string]DayRow {
	t.Helper()
	seq := r.latestSeq(t)
	report, _ := r.pass(t)
	after := r.dayRows(t)
	slices.Sort(want)
	if got := report.sorted(); !slices.Equal(got, want) {
		t.Errorf("%s: the pass wrote the days\n\t%s\nwant\n\t%s", what, strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	lines, described := rewritten(before, after)
	for _, line := range lines {
		if !slices.Contains(want, line) {
			t.Errorf("%s: a row of %s was rewritten, a day the change does not touch", what, line)
		}
	}
	for _, line := range mustMove {
		if !slices.Contains(lines, line) {
			t.Errorf("%s: no row of %s was rewritten, and the change moved its input", what, line)
		}
	}
	t.Logf("%s: the pass wrote %v; rows rewritten:\n\t%s\n\teval_days changes: %v", what, report.sorted(),
		strings.Join(described, "\n\t"), r.evalDayChanges(t, seq))
	return after
}

// seriesDays is, for one project and eval, the days of its all-repositories rows of series, in
// order, with each day's n.
func seriesDays(rows map[string]DayRow, project, slug, series string) ([]time.Time, map[time.Time]int) {
	n := map[time.Time]int{}
	for _, row := range rows {
		if row.ProjectKey == project && row.EvalID == EvalID(project, slug) && row.Series == series && row.Repo == "" {
			n[row.Day] = row.N
		}
	}
	return slices.SortedFunc(maps.Keys(n), time.Time.Compare), n
}

// contiguous reports the first day missing from days, which should run from their first to today
// with no gap, and false where none is.
func contiguous(days []time.Time, today time.Time) (time.Time, bool) {
	if len(days) == 0 {
		return time.Time{}, false
	}
	for want, i := days[0], 0; !want.After(today); want, i = want.AddDate(0, 0, 1), i+1 {
		if i >= len(days) || !days[i].Equal(want) {
			return want, true
		}
	}
	return time.Time{}, false
}

// TestTheFirstPassWalksEveryFamilyFromItsFirstInputDayWithZeroRows: over the delivery and Legion
// histories and one logged reading, a refresher with no cursor walks every family in batches of
// two days, one pass after another, until today. Then each series of each family holds an
// all-repositories row on every day from its first input day to today and none before it: the
// merge-to-production rows of DELIV start on the first merge of a pull request that counts for
// it, pr-ci's on the first PR-checks run, deploy-frequency's on the first deploy run, LEGION's work
// time on its first close, its idle time on its first close on or after the first whole day after
// its first claim observation, and the logged eval's on its reading's day. The days with no input
// (today, and a day before a reading) hold zero rows, n 0. The cursors stand at the feeds' heads,
// stamped with the pass's moment, which RefresherAge reads.
func TestTheFirstPassWalksEveryFamilyFromItsFirstInputDayWithZeroRows(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, legion := r.seedHistories(t)
	readingDay := refresherToday.AddDate(0, 0, -3)
	r.write(t, "build-minutes", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "run-1", "value": 12.5, "measured_at": readingDay.Add(9 * time.Hour)}))
	if _, found, err := r.registry.RefresherAge(t.Context(), r.now); err != nil || found {
		t.Fatalf("before any pass RefresherAge answers found %t, %v, want no age", found, err)
	}

	r.batch = 2
	passes, walked := r.settle(t)
	rows := r.dayRows(t)
	t.Logf("%d passes walked %d project days into %d rows", passes, len(walked.days), len(rows))
	if passes < 3 {
		t.Errorf("the walk of six delivery days in batches of two took %d passes, want at least 3", passes)
	}

	firstMerge, firstCheck, firstDeploy := time.Time{}, time.Time{}, time.Time{}
	delivProjects := map[string]string{}
	for project, keys := range deliveries.Issues {
		for _, key := range keys {
			delivProjects[key] = project
		}
	}
	for _, pr := range deliveries.PullRequests {
		counts := pr.IssueKey == nil || delivProjects[*pr.IssueKey] == deliveries.Project
		if counts && (firstMerge.IsZero() || pr.MergedAt.Before(firstMerge)) {
			firstMerge = *pr.MergedAt
		}
	}
	for _, run := range deliveries.Runs {
		at := &firstDeploy
		if run.Kind == delivery.DeliveryRunKindPRChecks {
			at = &firstCheck
		}
		if at.IsZero() || run.StartedAt.Before(*at) {
			*at = run.StartedAt
		}
	}
	var firstClose time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(closed_at) from issue_facts where project_key = $1`, legion.Project).Scan(&firstClose); err != nil {
		t.Fatalf("read the first close of %s: %v", legion.Project, err)
	}
	since, err := r.registry.ReadLegionSince(t.Context(), legion.Project)
	if err != nil || since.Claims == nil {
		t.Fatalf("read the first observations of %s: %+v, %v", legion.Project, since, err)
	}
	var firstIdle time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(closed_at) from issue_facts where project_key = $1 and closed_at >= $2`,
		legion.Project, *since.Claims).Scan(&firstIdle); err != nil {
		t.Fatalf("read the first close of %s after its first claim: %v", legion.Project, err)
	}

	for _, series := range []struct {
		project, slug, series string
		first                 time.Time
	}{
		{deliveries.Project, SlugMergeToProduction, measures.SeriesMinutes, firstMerge},
		{deliveries.Project, SlugPRCI, measures.SeriesMinutes, firstCheck},
		{deliveries.Project, SlugDeployFrequency, measures.SeriesDeploys, firstDeploy},
		{legion.Project, SlugLegionWorkTime, "hours", firstClose},
		{legion.Project, SlugLegionIdleTime, "implementer", firstIdle},
		{"EVAL", "build-minutes", "minutes", readingDay},
	} {
		days, n := seriesDays(rows, series.project, series.slug, series.series)
		name := series.project + " " + series.slug + " " + series.series
		if len(days) == 0 {
			t.Errorf("%s holds no all-repositories row", name)
			continue
		}
		if want := utcMidnight(series.first); !days[0].Equal(want) {
			t.Errorf("%s's first row is of %s, want its first input day %s", name, days[0].Format(time.DateOnly), want.Format(time.DateOnly))
		}
		if missing, gap := contiguous(days, refresherToday); gap {
			t.Errorf("%s holds no row of %s, between its first input day %s and today", name, missing.Format(time.DateOnly), days[0].Format(time.DateOnly))
		}
		if today, ok := n[refresherToday]; !ok || today != 0 {
			t.Errorf("%s's row of today holds n %d (held %t), want the zero row of a day with no input", name, today, ok)
		}
		t.Logf("%s: rows from %s to %s, %d days, n of today %d", name, days[0].Format(time.DateOnly), days[len(days)-1].Format(time.DateOnly), len(days), n[refresherToday])
	}
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day > $2 and n = 0 and repo = ''`, EvalID("EVAL", "build-minutes"), readingDay); got != 3 {
		t.Errorf("the logged eval holds %d zero rows after its reading's day, want the 3 days through today", got)
	}
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day = $2 and repo = ''`, EvalID(deliveries.Project, SlugUnownedP0), refresherToday); got != 1 {
		t.Errorf("%s holds %d unowned-p0 rows of today, want 1", deliveries.Project, got)
	}

	// The last walk's writes logged eval_days changes after the cursor that pass moved; one more
	// pass reads them, which name no day, and leaves the cursor at the head.
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Errorf("a pass after the walk wrote %v, want nothing: the walk's own eval_days changes name no day", report.sorted())
	}
	cursors, err := r.registry.readRefresherCursors(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	head := r.latestSeq(t)
	headAt, err := delivery.ChangeAt(t.Context(), r.database.Pool, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := cursors[changesCursor]; got.position != head || got.at == nil || headAt == nil || !got.at.Equal(*headAt) {
		t.Errorf("the changes cursor stands at %d recorded %v, want the log's head %d recorded %v", got.position, got.at, head, headAt)
	}
	age, found, err := r.registry.RefresherAge(t.Context(), r.now.Add(time.Minute))
	if err != nil || !found || age != time.Minute {
		t.Errorf("RefresherAge a minute after the last pass answers %s, found %t, %v; want a minute", age, found, err)
	}
}

// TestANewBuildRewritesEveryRowOfAFamily: once a build has walked every family, a pass of another
// build walks them all again and rewrites every computed row with its own build, although no value
// moved; while the two builds run in turn, each one's pass walks the days again.
func TestANewBuildRewritesEveryRowOfAFamily(t *testing.T) {
	r := newRefresherRig(t)
	r.seedHistories(t)
	r.settle(t)
	computed := `select count(*) from eval_days where built_by <> $1 and split_part(eval_id, '/', 2) = any($2)`
	families := slices.Concat(deliveryDaySlugs, legionDaySlugs, []string{SlugUnownedP0})
	total := r.count(t, `select count(*) from eval_days where split_part(eval_id, '/', 2) = any($1)`, families)
	if stale := r.count(t, computed, testBuild, families); stale != 0 || total == 0 {
		t.Fatalf("after the first build's walk %d of %d computed rows hold another build", stale, total)
	}

	r.build = "another-build"
	_, walked := r.settle(t)
	if stale := r.count(t, computed, r.build, families); stale != 0 {
		t.Errorf("after the new build's walk %d of %d computed rows still hold another build than %s", stale, total, r.build)
	}
	if got := r.count(t, `select count(*) from eval_days where split_part(eval_id, '/', 2) = any($1)`, families); got != total {
		t.Errorf("the new build's walk left %d computed rows, want the %d the first left", got, total)
	}
	t.Logf("the new build rewrote %d computed rows over %d project days", total, len(walked.days))

	for _, build := range []string{testBuild, "another-build"} {
		r.build = build
		_, again := r.settle(t)
		if len(again.days) < len(walked.days) {
			t.Errorf("build %s's turn walked %d project days, want every one of the %d", build, len(again.days), len(walked.days))
		}
	}
}

// TestTheTickPrunesChangesOlderThanTheirRetention: a pass deletes every delivery change older
// than delivery.ChangeRetention by the database's clock and keeps a newer one, and asks for its
// next pass pruneSlack after the oldest change it kept passes its retention or at the next UTC
// midnight, whichever is first, so the log keeps its 24 hours with no GitHub App's reconcile
// pruning it, and a log changes reach every few seconds is pruned every five minutes rather than
// every RefresherGap.
func TestTheTickPrunesChangesOlderThanTheirRetention(t *testing.T) {
	r := newRefresherRig(t)
	r.now = time.Now().UTC()
	r.pass(t)
	r.exec(t, "log three changes", `
		insert into delivery_changes (kind, repo, ref, at)
		values ('session_group', null, '25h', now() - interval '25 hours'),
		       ('session_group', null, '24h1m', now() - interval '24 hours 1 minute'),
		       ('session_group', null, '23h59m', now() - interval '23 hours 59 minutes')`)
	_, next := r.pass(t)
	rows, err := r.database.Pool.Query(t.Context(), `select ref, at from delivery_changes where kind = 'session_group' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	var ref string
	var keptAt time.Time
	if _, err := pgx.ForEachRow(rows, []any{&ref, &keptAt}, func() error {
		kept = append(kept, ref)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"23h59m"}) {
		t.Errorf("after the pass the log keeps %v of the three changes, want the one 23 hours 59 minutes old", kept)
	}
	want := keptAt.Add(delivery.ChangeRetention + pruneSlack)
	if midnight := utcMidnight(r.now).AddDate(0, 0, 1); midnight.Before(want) {
		want = midnight
	}
	if !next.Equal(want) {
		t.Errorf("the pass asks for its next at %s, want %s", next.UTC(), want.UTC())
	}
	t.Logf("pruned to %v; the next pass is due at %s", kept, next.UTC().Format(time.RFC3339))
}
