package evals

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// refresherNow is the moment the refresher's tests run their passes as of: noon of deliverytest's
// DefaultNow, the UTC day after the delivery history's last, so today holds no delivery input and
// every series that has begun holds a zero row of today.
var refresherNow = deliverytest.DefaultNow.Add(12 * time.Hour)

// refresherToday is refresherNow's UTC day.
var refresherToday = measures.Midnight(refresherNow)

// refresherRig is one test's refresher over a database of its own: the registry rig, the event
// horizon a pass reads the log through, the moment and the build its passes run as, the days a
// pass walks of each walk, and the seeds its passes keep, as the loop keeps them.
type refresherRig struct {
	rig
	horizon *events.Horizon
	now     time.Time
	build   string
	batch   int
	seeds   *seeding
}

func newRefresherRig(t *testing.T) *refresherRig {
	t.Helper()
	r := newRig(t)
	return &refresherRig{rig: r, horizon: events.NewHorizon(r.database.Pool), now: refresherNow, build: testBuild, batch: walkBatch, seeds: &seeding{}}
}

// pass makes one refresher pass as of the rig's clock, under its build, with a batch of its own
// (gapBatch), and answers what it wrote and when it asks to run again (refreshIn).
func (r *refresherRig) pass(t *testing.T) (refreshed, time.Time) {
	t.Helper()
	return r.refreshIn(t.Context(), t, &gapBatch{})
}

// refreshIn makes one refresher pass in ctx as of the rig's clock, under its build, with batch
// (refresh; nil for none), and answers what it wrote and when it asks to run again. It proves the
// event log's safe horizon first, as the Legion facts consumer's tests do: a pass reads the log
// only up to the horizon, which a later observation raises past a write the test just made.
func (r *refresherRig) refreshIn(ctx context.Context, t *testing.T, batch *gapBatch) (refreshed, time.Time) {
	t.Helper()
	if _, err := r.horizon.Await(ctx); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	report, next, err := r.registry.refresh(ctx, RefresherDeps{Horizon: r.horizon, Build: r.build},
		refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds, batch)
	if err != nil {
		t.Fatalf("the refresher's pass failed: %v", err)
	}
	return report, next
}

// settle makes passes until one leaves no walk with days left, and answers how many it made and
// every day they wrote.
func (r *refresherRig) settle(t *testing.T) (int, refreshed) {
	t.Helper()
	all := refreshed{days: map[builtDay]bool{}}
	for passes := 1; passes <= 100; passes++ {
		report, _ := r.pass(t)
		maps.Copy(all.days, report.days)
		if !report.walksLeft {
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
	rows, filings, err := legionfacts.ComputeProject(ctx, pool, legion.Project, deliverytest.DefaultNow)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", legion.Project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: legion.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", legion.Project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: legion.Project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", legion.Project, err)
	}
	t.Logf("stored %d facts rows and %d filings of %s", len(rows), len(filings), legion.Project)
	return deliveries, legion
}

// deployRule is the rule of the deploy source that reads repo as a pass reads it
// (delivery.DeployRules over every project's settings), its DeployRepo repo.
func deployRule(t testing.TB, pool *store.Pool, repo string) delivery.DeliverySettings {
	t.Helper()
	projects, err := delivery.ListProjectSettings(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	for _, rule := range delivery.DeployRules(projects) {
		if strings.EqualFold(rule.DeployRepo, repo) {
			rule.DeployRepo = repo
			return rule
		}
	}
	t.Fatalf("no deploy source reads %s", repo)
	return delivery.DeliverySettings{}
}

// eventsSource is a source that reads the project's posted deploys.
var eventsSource = delivery.SourceInput{Kind: delivery.SourceKindEvents}

// putSources saves project's delivery settings through delivery.PutProjectSettings, in a
// transaction of its own: delivery on, authors as the population rule, and sources.
func putSources(t testing.TB, pool *store.Pool, project string, authors []string, sources ...delivery.SourceInput) delivery.ProjectSettings {
	t.Helper()
	return putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: project, Delivery: true, Sources: sources, PopulationAuthors: authors, ExcludedRepos: []string{},
	})
}

// putProjectSettings saves input through delivery.PutProjectSettings, in a transaction of its own.
func putProjectSettings(t testing.TB, pool *store.Pool, input delivery.ProjectSettingsInput) delivery.ProjectSettings {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the delivery settings of %s: %v", input.Project, err)
	}
	defer tx.Rollback(ctx)
	saved, err := delivery.PutProjectSettings(ctx, tx, input, model.Actor{Kind: "system", ID: "evals-test"})
	if err != nil {
		t.Fatalf("save the delivery settings of %s: %v", input.Project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the delivery settings of %s: %v", input.Project, err)
	}
	return saved
}

// leaveInputsUnread clears every stored pull request's attribution inputs, as a row stored before
// its GitHub facts were read for them holds none, so the issue its row names (issue_key) stands as
// its attribution: no inputs were read, and a row with none counts by the one list its issue_key
// gives (delivery_days.go's attribution). A test whose oracle is deliverytest's issue_key, the
// project of the issue it names each pull request after, reads its rows this way: the history's
// issues own no repository and only its project's default is the deploy repository, so with the
// inputs read most of its pull requests would count by the published defaults (under owner keys
// whose lists name the issue's project), and those of its other repositories, and every one
// naming an issue of its second project, for no project the issue names.
func (r *refresherRig) leaveInputsUnread(t *testing.T) {
	t.Helper()
	r.exec(t, "leave the pull requests' attribution inputs unread", `
		update delivery_pull_requests
		set attribution_title_keys = null, attribution_cited_issues = null, attribution_branch_keys = null, attribution_commit_keys = null`)
}

// rowKey is a day row's identity, `<project> <eval id> <series> <repo> <day>`, and ` as <deploy
// repo>` after it for a row of an eval keyed by the deploy repository (deploy_repos.go).
func rowKey(row DayRow) string {
	key := strings.Join([]string{row.ProjectKey, row.EvalID, row.Series, row.Repo, row.Day.Format(time.DateOnly)}, " ")
	if row.DeployRepo != "" {
		key += " as " + row.DeployRepo
	}
	return key
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

// ownerReaders is, for each owner key (owners.go) any project reads, the projects whose reads fold
// its rows (eval_project_owners), in key order: those whose published default is its repository,
// less its sharers. A key no project reads is in none.
func (r *refresherRig) ownerReaders(t *testing.T) map[string][]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select owner, project_key from eval_project_owners where owner <> project_key order by owner, project_key collate "C"`)
	if err != nil {
		t.Fatalf("read the projects that read each owner key: %v", err)
	}
	readers := map[string][]string{}
	var owner, project string
	if _, err := pgx.ForEachRow(rows, []any{&owner, &project}, func() error {
		readers[owner] = append(readers[owner], project)
		return nil
	}); err != nil {
		t.Fatalf("read the projects that read each owner key: %v", err)
	}
	return readers
}

// asReaders is row as each project whose reads fold it reads it: the row itself where it is a
// project's, and, where it is an owner key's, one copy for each of the key's readers, under that
// project's key and its eval id of the row's slug (asReader); none where no project reads it.
func asReaders(row DayRow, readers map[string][]string) []DayRow {
	if !isOwner(row.ProjectKey) {
		return []DayRow{row}
	}
	read := make([]DayRow, 0, len(readers[row.ProjectKey]))
	for _, project := range readers[row.ProjectKey] {
		one := row
		one.ProjectKey, one.EvalID = project, asReader(row.ProjectKey, project, row.EvalID)
		read = append(read, one)
	}
	return read
}

// readerEvals is evalIDs as their readers read them, sorted and each once: a project's eval id as
// it is, and an owner key's (owners.go) as the eval of the same slug of each project that reads
// the key, none where no project does. An eval id is its owner's key, '/' and its slug, and no
// slug holds '/'.
func readerEvals(evalIDs []string, readers map[string][]string) []string {
	read := map[string]bool{}
	for _, evalID := range evalIDs {
		owner := evalID[:strings.LastIndex(evalID, "/")]
		if !isOwner(owner) {
			read[evalID] = true
			continue
		}
		for _, project := range readers[owner] {
			read[asReader(owner, project, evalID)] = true
		}
	}
	return slices.Sorted(maps.Keys(read))
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
	return r.changesHead(t).Seq
}

// changesHead is the change cursor at the newest delivery change: its seq and when it was
// recorded.
func (r *refresherRig) changesHead(t *testing.T) delivery.ChangeCursor {
	t.Helper()
	head, err := delivery.ChangesHead(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the newest delivery change: %v", err)
	}
	return head
}

// evalDayChanges is every eval_days change of an eval logged after seq, as `<eval id> <first
// day>..<last day>`, in seq order. An eval_days change that names no day is left out: the
// refresher's word that the day rows stand at the delivery settings (delivery.SettingsApplied),
// that it recorded the settings they stand built under (recordBuiltUnder), or that the project
// judging the delivery views moved (delivery.JudgingMoved).
func (r *refresherRig) evalDayChanges(t *testing.T, after int64) []string {
	t.Helper()
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, after, 100000)
	if err != nil {
		t.Fatalf("read the delivery changes after %d: %v", after, err)
	}
	var lines []string
	for _, change := range changes {
		if change.Kind == "eval_days" && change.TOld != nil && change.TNew != nil {
			lines = append(lines, fmt.Sprintf("%s %s..%s", change.Ref, change.TOld.UTC().Format(time.DateOnly), change.TNew.UTC().Format(time.DateOnly)))
		}
	}
	return lines
}

// dayProblems is what is wrong with the days a pass wrote, as expectDays judges them: the days it
// wrote (report) other than the days want names (dayLine's form), a row of a day outside want that
// was rewritten between before and after, and no row of a day of mustMove, whose input the change
// moved, rewritten.
func dayProblems(what string, report refreshed, before, after map[string]DayRow, want, mustMove []string) []string {
	var problems []string
	want = slices.Sorted(slices.Values(want))
	if got := report.sorted(); !slices.Equal(got, want) {
		problems = append(problems, fmt.Sprintf("%s: the pass wrote the days\n\t%s\nwant\n\t%s", what, strings.Join(got, "\n\t"), strings.Join(want, "\n\t")))
	}
	lines, _ := rewritten(before, after)
	for _, line := range lines {
		if !slices.Contains(want, line) {
			problems = append(problems, fmt.Sprintf("%s: a row of %s was rewritten, a day the change does not touch", what, line))
		}
	}
	for _, line := range mustMove {
		if !slices.Contains(lines, line) {
			problems = append(problems, fmt.Sprintf("%s: no row of %s was rewritten, and the change moved its input", what, line))
		}
	}
	return problems
}

// expectDays makes one pass after the test changed an input, and checks that it wrote exactly the
// days want names (dayLine's form), that no row of any other day was rewritten, and that a row of
// each day of mustMove was: a day whose input the change moved (dayProblems). It logs what each
// rewritten day's rows held for updated_at before and after, and every eval_days change row the
// pass logged.
func (r *refresherRig) expectDays(t *testing.T, what string, before map[string]DayRow, want, mustMove []string) map[string]DayRow {
	t.Helper()
	seq := r.latestSeq(t)
	report, _ := r.pass(t)
	after := r.dayRows(t)
	for _, problem := range dayProblems(what, report, before, after, want, mustMove) {
		t.Error(problem)
	}
	_, described := rewritten(before, after)
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

// readRows is every stored day row as the projects that read it read it (asReaders): a project's
// own row, and an owner key's (owners.go) once under each project that reads the key, by the
// stored row's rowKey and the project. A project's day can hold a row of one series of its own and
// rows of owner keys it reads.
func (r *refresherRig) readRows(t *testing.T) map[string]DayRow {
	t.Helper()
	readers := r.ownerReaders(t)
	read := map[string]DayRow{}
	for key, row := range r.dayRows(t) {
		for _, one := range asReaders(row, readers) {
			read[key+" for "+one.ProjectKey] = one
		}
	}
	return read
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
// stamped with the pass's moment, which RefresherAgeIn reads.
func TestTheFirstPassWalksEveryFamilyFromItsFirstInputDayWithZeroRows(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, legion := r.seedHistories(t)
	r.leaveInputsUnread(t)
	readingDay := refresherToday.AddDate(0, 0, -3)
	r.write(t, "build-minutes", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "run-1", "value": 12.5, "measured_at": readingDay.Add(9 * time.Hour)}))
	if _, found, err := RefresherAgeIn(t.Context(), r.database.Pool, r.now); err != nil || found {
		t.Fatalf("before any pass RefresherAgeIn answers found %t, %v, want no age", found, err)
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
	// The first claim observation, read from the table as the project's observations are stored:
	// legion-idle-time has input from the first UTC midnight at or after it.
	var firstClaim time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(observed_at) from legion_observations where project = $1 and field like 'claim.%'`,
		legion.Project).Scan(&firstClaim); err != nil {
		t.Fatalf("read the first claim observation of %s: %v", legion.Project, err)
	}
	idleFrom := measures.Midnight(firstClaim)
	if idleFrom.Before(firstClaim) {
		idleFrom = idleFrom.AddDate(0, 0, 1)
	}
	var firstIdle time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(closed_at) from issue_facts where project_key = $1 and closed_at >= $2`,
		legion.Project, idleFrom).Scan(&firstIdle); err != nil {
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
		if want := measures.Midnight(series.first); !days[0].Equal(want) {
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
	// The write filled the zero rows through the database's today, past the rig's (writeLoggedZeros).
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day > $2 and day <= $3 and n = 0 and repo = ''`,
		EvalID("EVAL", "build-minutes"), readingDay, refresherToday); got != 3 {
		t.Errorf("the logged eval holds %d zero rows after its reading's day through today, want the 3 days", got)
	}
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day = $2 and repo = ''`, EvalID(deliveries.Project, SlugUnownedP0), refresherToday); got != 1 {
		t.Errorf("%s holds %d unowned-p0 rows of today, want 1", deliveries.Project, got)
	}

	// An event of a type the refresher reads no day of is written after the walk, which the pass
	// below reads past. The last walk's writes logged eval_days changes after the cursor that pass
	// moved; one more pass reads them, which name no day, and leaves the cursor at the head.
	r.exec(t, "write an event that names no day", `
		insert into events (seq, type, actor, payload, notify) values (0, 'refresher.test', '{"kind":"user","id":"alice"}', '{}', false)`)
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Errorf("a pass after the walk wrote %v, want nothing: the walk's own eval_days changes name no day", report.sorted())
	}
	cursors, _, err := r.registry.readRefresherCursors(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	head := r.changesHead(t)
	if got := cursors.changes; got.Seq != head.Seq || got.At == nil || head.At == nil || !got.At.Equal(*head.At) {
		t.Errorf("the changes cursor stands at %d recorded %v, want the log's head %d recorded %v", got.Seq, got.At, head.Seq, head.At)
	}
	// The event cursor stands at the safe horizon the last pass read the log through, past the
	// event written after the walk.
	if events := int64(r.count(t, `select coalesce(max(id), 0) from events`)); cursors.events != r.horizon.Proven() || cursors.events < events {
		t.Errorf("the events cursor stands at %d, want the safe horizon %d, no less than the newest event %d", cursors.events, r.horizon.Proven(), events)
	}
	age, found, err := RefresherAgeIn(t.Context(), r.database.Pool, r.now.Add(time.Minute))
	if err != nil || !found || age != time.Minute {
		t.Errorf("RefresherAgeIn a minute after the last pass answers %s, found %t, %v; want a minute", age, found, err)
	}
}

// TestTheTickPrunesChangesOlderThanTheirRetention: a pass deletes the delivery changes older than
// delivery.ChangeRetention and keeps a newer one (where the cut falls is PruneChanges' own test's),
// and asks for its next pass pruneSlack after the oldest change it kept passes its retention or at
// the next UTC midnight, whichever is first, so the log keeps its 24 hours with no GitHub App's
// reconcile pruning it, and a loop no change wakes prunes it within five minutes of a change's
// expiry and no sooner.
func TestTheTickPrunesChangesOlderThanTheirRetention(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.now = time.Now().UTC()
	r.pass(t)
	r.exec(t, "log two changes", `
		insert into delivery_changes (kind, repo, ref, at)
		values ('session_group', null, '25h', now() - interval '25 hours'),
		       ('session_group', null, '23h', now() - interval '23 hours')`)
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
	if !slices.Equal(kept, []string{"23h"}) {
		t.Errorf("after the pass the log keeps %v of the two changes, want the one 23 hours old", kept)
	}
	want := keptAt.Add(delivery.ChangeRetention + pruneSlack)
	if midnight := measures.Midnight(r.now).AddDate(0, 0, 1); midnight.Before(want) {
		want = midnight
	}
	if !next.Equal(want) {
		t.Errorf("the pass asks for its next at %s, want %s", next.UTC(), want.UTC())
	}
	t.Logf("pruned to %v; the next pass is due at %s", kept, next.UTC().Format(time.RFC3339))
}

// loopGap is the gap the tests of the loop run it at: an hour, which no test waits out. A pass a
// change woke then runs at once, or the test's wait for it gives up by name; and the loop begins
// the next batch of the walks only once the test has moved the loop's clock a gap on (advance).
const loopGap = time.Hour

// refresherLoop is the refresher's loop as the server runs it (runRefresher, under the eval-days
// lease), over a rig's database, at loopGap and as of the rig's clock. deliveries is its delivery
// wake where the test signals it by hand (wake), nil where a hub signals it; log holds every
// record logged while the loop runs; ahead is how far the loop's own clock stands ahead of the
// wall clock (advance).
type refresherLoop struct {
	deliveries chan struct{}
	log        *logtest.Recorder
	ahead      atomic.Int64
}

// runLoop starts the loop over the rig, woken by the test by hand rather than by a hub, once the
// event log's safe horizon covers every event written so far; it runs until the test ends.
func (r *refresherRig) runLoop(t *testing.T) *refresherLoop {
	t.Helper()
	if _, err := r.horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	deliveries := make(chan struct{}, 1)
	loop := r.startLoop(t, deliveries)
	loop.deliveries = deliveries
	return loop
}

// startLoop starts the loop over the rig with deliveries as its delivery wake and an events wake
// nothing signals; it runs until the test ends. Its boot pass reads the event log through the
// horizon as a pass of the server does (events.Horizon.SafeOrAwait).
func (r *refresherRig) startLoop(t *testing.T, deliveries <-chan struct{}) *refresherLoop {
	t.Helper()
	loop := &refresherLoop{log: &logtest.Recorder{}}
	logtest.SetDefault(t, loop.log)
	deps := RefresherDeps{Horizon: r.horizon, Build: r.build, Leases: storetest.Leases(t, r.database.Pool),
		Events: make(chan struct{}, 1), Deliveries: deliveries}
	now := r.now
	sched := refresherSchedule{gap: loopGap, page: refresherPage, batch: r.batch, now: func() time.Time { return now }, wall: loop.wall}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.registry.runRefresher(ctx, deps, sched)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return loop
}

// changesWake runs a pglisten.Hub on the rig's listener with delivery.ChangesChannel registered,
// until the test ends, and answers its wake once the hub has connected: the delivery wake the
// server gives the refresher (cmd/dispatch's daysChangesWake), which the commit of every write
// that logs a change signals, the refresher's own day writes among them.
func (r *refresherRig) changesWake(t *testing.T) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "eval days refresher test", Dial: r.database.Pool.Listener}
	wake := hub.Wake(delivery.ChangesChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	testwait.Receive(t, "the hub to connect", connected)
	return wake
}

// nudge logs a change of an issue's facts that names no day (no t_old and no t_new, as a change of
// an open issue's facts does) and moves it into the change log, which a hub wakes the loop for: a
// wake of a loop whose delivery wake is a hub's.
func (r *refresherRig) nudge(t *testing.T) {
	t.Helper()
	r.exec(t, "log a change that names no day", `select delivery_change_log_pending('issue_facts', null, 'EVAL', 'EVAL-4', null, null)`)
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("move the change into the log: %v", err)
	}
}

// wall is the loop's own clock: the wall clock, ahead as far as the test has moved it.
func (l *refresherLoop) wall() time.Time {
	return time.Now().Add(time.Duration(l.ahead.Load()))
}

// advance moves the loop's clock d on, so the next pass begins another batch of the walks where d
// reaches the gap, which the loop would otherwise wait for.
func (l *refresherLoop) advance(d time.Duration) {
	l.ahead.Add(int64(d))
}

// wake signals the loop's delivery wake as the hub does, a send that never blocks into a buffer of
// one: the wake a change in the change log gives, a stored fact's or the refresher's own.
func (l *refresherLoop) wake() {
	select {
	case l.deliveries <- struct{}{}:
	default:
	}
}

// passEnds is when each of the loop's passes that has run so far ended: duty.Exclusive logs each
// one that ran under the refresher's lease, as it ends.
func (l *refresherLoop) passEnds() []time.Time {
	var ends []time.Time
	for _, record := range l.log.Records() {
		if record.Message != "dispatch duty: leased pass ran" {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "lease" && attr.Value.String() == RefresherLease {
				ends = append(ends, record.Time)
				return false
			}
			return true
		})
	}
	return ends
}

// ran is how many of the loop's passes have run so far (passEnds).
func (l *refresherLoop) ran() int {
	return len(l.passEnds())
}

// awaitRan waits until n of the loop's passes have run.
func (l *refresherLoop) awaitRan(t *testing.T, n int) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("pass %d of the loop", n), func() bool { return l.ran() >= n })
}

// tracedStore is a registry over the rig's database whose statements tracer traces, beside the
// rig's documents, events and attributor: the registry of a check that reads what a write sends.
func (r rig) tracedStore(t *testing.T, tracer pgx.QueryTracer) *Store {
	t.Helper()
	return New(storetest.TracedStore(t, r.database, tracer), r.documents, r.registry.events, r.registry.attributor)
}

// workTime is EVAL's legion-work-time n of each day, of its all-repositories rows of hours, by the
// day's date: one read, cheap enough to ask every 10 ms.
func (r *refresherRig) workTime(t *testing.T) map[string]int {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select day, n from eval_days where eval_id = $1 and series = 'hours' and repo = '' and deploy_repo = ''`,
		EvalID("EVAL", SlugLegionWorkTime))
	if err != nil {
		t.Fatalf("read legion-work-time's rows: %v", err)
	}
	n := map[string]int{}
	var day time.Time
	var count int
	if _, err := pgx.ForEachRow(rows, []any{&day, &count}, func() error {
		n[day.Format(time.DateOnly)] = count
		return nil
	}); err != nil {
		t.Fatalf("read legion-work-time's rows: %v", err)
	}
	return n
}

// factsCursorUpsert moves the Legion facts consumer's events cursor to $1, as its pass does
// (legionfacts' advanceCursors).
const factsCursorUpsert = `
	insert into issue_facts_cursor (name, position, updated_at) values ('events', $1, now())
	on conflict (name) do update set position = excluded.position, updated_at = excluded.updated_at`

// storeFacts stores the facts of keys as history holds them and moves the Legion facts consumer's
// events cursor to through, in one transaction, then drains the change log's outbox: what the
// consumer's pass commits, and then does (legionfacts' writePass). It answers the moment the
// transaction committed.
func (r *refresherRig) storeFacts(t *testing.T, history *legionEval, through int64, keys ...string) time.Time {
	t.Helper()
	ctx := t.Context()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the facts of %v: %v", keys, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var rows []legionfacts.Row
	for _, key := range keys {
		rows = append(rows, history.rows[key])
	}
	if _, err := legionfacts.WriteProject(ctx, tx, legionfacts.ProjectWrite{Project: "EVAL", Rows: rows, Stated: keys}); err != nil {
		t.Fatalf("write the facts of %v: %v", keys, err)
	}
	if _, err := tx.Exec(ctx, factsCursorUpsert, through); err != nil {
		t.Fatalf("move the facts consumer's events cursor to %d: %v", through, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the facts of %v: %v", keys, err)
	}
	committed := time.Now()
	if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	return committed
}

// TestTheFactsAnEventMovesReachTheDayRowsAtTheirCommit: R14 gives a Legion facts write 2 s to
// reach every evals view, and a view wakes once the refresher has rewritten its day rows. The
// write's event wakes the refresher at once, and that pass runs before the Legion facts consumer
// has stored the facts the event moves, so it rewrites nothing; the consumer's commit wakes the
// loop again. Here the loop runs as the server runs it, at loopGap, woken by hand: its first pass
// reads EVAL-3's status event and rewrites nothing; a wake before the consumer commits, as the
// refresher's own writes give, runs a pass at once; then the consumer stores EVAL-3's close moved
// from the 3rd day to the 5th, and the wake of that commit runs the pass that rewrites both days.
// EVAL-2's close, moved after that pass with no event, reaches the day rows by the wake of its own
// commit: no gap holds a pass a change woke, which a gap of an hour would hold past any wait.
func TestTheFactsAnEventMovesReachTheDayRowsAtTheirCommit(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	d1, d2 := history.d1.Format(time.DateOnly), history.d2.Format(time.DateOnly)
	d3, d4 := legionHistoryDay(4).Format(time.DateOnly), legionHistoryDay(5).Format(time.DateOnly)
	if n := r.workTime(t); n[d1] != 2 || n[d2] != 1 || n[d3] != 0 || n[d4] != 0 {
		t.Fatalf("after the walk legion-work-time holds n %v, want 2 on %s, 1 on %s and 0 on %s and %s", n, d1, d2, d3, d4)
	}
	r.exec(t, "store the facts of every event so far", factsCursorUpsert, r.count(t, `select coalesce(max(id), 0) from events`))
	r.writeIssueEvent(t, "EVAL-3", "issue.updated", `{"key":"EVAL-3","status":"in_review","changed":["status"]}`)
	event := int64(r.count(t, `select max(id) from events`))

	loop := r.runLoop(t)
	loop.awaitRan(t, 1)
	if read := int64(r.count(t, `select position from eval_days_cursor where name = $1`, eventsCursor)); read < event {
		t.Fatalf("the first pass left the events cursor at %d, short of EVAL-3's event %d", read, event)
	}
	if n := r.workTime(t); n[d2] != 1 || n[d4] != 0 {
		t.Fatalf("the first pass moved EVAL-3's close before its facts were stored: n %v", n)
	}
	loop.wake()
	loop.awaitRan(t, 2)

	moved := history.rows["EVAL-3"]
	moved.ClosedAt = new(legionHistoryDay(5).Add(9 * time.Hour))
	history.rows["EVAL-3"] = moved
	r.storeFacts(t, history, event, "EVAL-3")
	loop.wake()
	testwait.Eventually(t, "the day rows of EVAL-3's moved close", func() bool {
		n := r.workTime(t)
		return n[d2] == 0 && n[d4] == 1
	})

	loop.awaitRan(t, 3)
	moved = history.rows["EVAL-2"]
	moved.ClosedAt = new(legionHistoryDay(4).Add(12 * time.Hour))
	history.rows["EVAL-2"] = moved
	r.storeFacts(t, history, event, "EVAL-2")
	loop.wake()
	testwait.Eventually(t, "the day rows of EVAL-2's moved close", func() bool {
		n := r.workTime(t)
		return n[d1] == 1 && n[d3] == 1
	})
}

// TestAChangeInsideWhatWasTheGapReachesTheDayRowsAtOnce: the loop runs as the server runs it, at
// loopGap, woken by hand, over a settled Legion history of EVAL whose facts consumer stands at the
// head of the events. EVAL-3's close, moved from the 3rd day to the 5th after the boot pass's end,
// reaches the day rows by the wake of the facts' commit, and EVAL-2's, moved from the 2nd to the
// 4th at once, with no wait for the pass of the first, by the wake of its own commit: no gap that
// began with the boot pass holds a pass a change woke. Before, both waited for RefresherGap after
// the boot pass began.
func TestAChangeInsideWhatWasTheGapReachesTheDayRowsAtOnce(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	head := int64(r.count(t, `select coalesce(max(id), 0) from events`))
	r.exec(t, "store the facts of every event so far", factsCursorUpsert, head)
	d1, d2 := history.d1.Format(time.DateOnly), history.d2.Format(time.DateOnly)
	d3, d4 := legionHistoryDay(4).Format(time.DateOnly), legionHistoryDay(5).Format(time.DateOnly)

	loop := r.runLoop(t)
	loop.awaitRan(t, 1)
	moved := history.rows["EVAL-3"]
	moved.ClosedAt = new(legionHistoryDay(5).Add(9 * time.Hour))
	history.rows["EVAL-3"] = moved
	r.storeFacts(t, history, head, "EVAL-3")
	loop.wake()
	testwait.Eventually(t, "the day rows of EVAL-3's moved close", func() bool {
		n := r.workTime(t)
		return n[d2] == 0 && n[d4] == 1
	})

	moved = history.rows["EVAL-2"]
	moved.ClosedAt = new(legionHistoryDay(4).Add(12 * time.Hour))
	history.rows["EVAL-2"] = moved
	r.storeFacts(t, history, head, "EVAL-2")
	loop.wake()
	testwait.Eventually(t, "the day rows of EVAL-2's moved close", func() bool {
		n := r.workTime(t)
		return n[d1] == 1 && n[d3] == 1
	})
}

// TestABurstDuringAPassMakesOneOwedPass: changes that commit while a pass runs earn one pass,
// which starts as soon as the running one ends. The loop runs at loopGap, woken by hand, over a
// settled Legion history of EVAL. EVAL-3's close moves to the 5th day, and the pass that rewrites
// it is held at its write of the 5th (pauseInserts); while it is held, five more closes move, each
// onto another day, each with its wake, and the held write then goes on. The next pass holds all
// five moves, and no other pass runs in the 2 s after it, since the wakes are by hand and no echo
// comes. Before, that pass waited for RefresherGap after the held pass began.
func TestABurstDuringAPassMakesOneOwedPass(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	head := int64(r.count(t, `select coalesce(max(id), 0) from events`))
	r.exec(t, "store the facts of every event so far", factsCursorUpsert, head)
	day := func(n int) string { return legionHistoryDay(n).Format(time.DateOnly) }

	loop := r.runLoop(t)
	loop.awaitRan(t, 1)
	move := func(key string, n int) {
		row := history.rows[key]
		row.ClosedAt = new(legionHistoryDay(n).Add(10 * time.Hour))
		history.rows[key] = row
		r.storeFacts(t, history, head, key)
		loop.wake()
	}
	holder := pauseInserts(t, r.rig, "eval_days", fmt.Sprintf("new.project_key = 'EVAL' and new.day = '%s'", day(5)))
	move("EVAL-3", 5)
	storetest.WaitForLockWaiters(t, holder, 1)
	burst := []struct {
		key string
		day int
	}{{"EVAL-1", 6}, {"EVAL-2", 7}, {"EVAL-4", 8}, {"EVAL-1", 4}, {"EVAL-2", 3}}
	for _, m := range burst {
		move(m.key, m.day)
	}
	if ran := loop.ran(); ran != 1 {
		t.Fatalf("%d passes ended while the held pass ran, want the boot pass alone", ran)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("let the held write on: %v", err)
	}
	loop.awaitRan(t, 3)
	n := r.workTime(t)
	want := map[string]int{day(2): 0, day(3): 1, day(4): 1, day(5): 1, day(6): 0, day(7): 0, day(8): 1}
	for d, count := range want {
		if n[d] != count {
			t.Errorf("after the owed pass legion-work-time holds n %d on %s, want %d: n %v", n[d], d, count, n)
		}
	}
	// A span's check, which keeps its body: it ends at the first pass beyond the owed one, or
	// after the 2 s no such pass came in.
	if testwait.Holds(2*time.Second, 50*time.Millisecond, func() bool { return loop.ran() != 3 }) {
		t.Fatalf("%d passes ran within 2 s of the owed pass's end, want the boot pass, the held pass and the owed one", loop.ran())
	}
}

// TestTheWalkKeepsItsGapWhileChangesRunAtOnce: a pass that begins a batch of the walks keeps the
// gap from the one before it, while the changes that arrive meanwhile run at once. Over the
// settled Legion history of EVAL, a new build's loop rebuilds it (a release, rebuild.go), a day a
// batch at loopGap, woken by hand: its boot pass begins the rebuild and builds the walk's first
// day. EVAL-3's close then moves between the 3rd and the 5th day six times, each move woken and
// awaited, and the rebuild's walk stands where the boot pass left it after every one: no change's
// pass begins a batch. Once the loop's own clock has moved a gap on, the next wake's pass builds
// the walk's next day. Before, each change waited for the gap.
func TestTheWalkKeepsItsGapWhileChangesRunAtOnce(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	head := int64(r.count(t, `select coalesce(max(id), 0) from events`))
	r.release("walk-gap-build")
	r.batch = 1
	d2, d4 := legionHistoryDay(3).Format(time.DateOnly), legionHistoryDay(5).Format(time.DateOnly)
	// walkNext is where the release's rebuild's walk of EVAL stands: "" until the rebuild has
	// staged a batch, the stored rows' own walk standing where the settled history left it.
	walkNext := func() string {
		next, rebuilding := r.rebuildNext(t, walkLegion, "EVAL")
		if !rebuilding || next == nil {
			return ""
		}
		return next.Format(time.DateOnly)
	}

	loop := r.runLoop(t)
	loop.awaitRan(t, 1)
	stood := walkNext()
	if stood == "" {
		t.Fatal("the boot pass staged no day of the rebuild's walk, and the test needs one")
	}
	for move := 1; move <= 6; move++ {
		onFifth := move%2 == 1
		closed := legionHistoryDay(3)
		if onFifth {
			closed = legionHistoryDay(5)
		}
		moved := history.rows["EVAL-3"]
		moved.ClosedAt = new(closed.Add(9 * time.Hour))
		history.rows["EVAL-3"] = moved
		r.storeFacts(t, history, head, "EVAL-3")
		loop.wake()
		testwait.Eventually(t, fmt.Sprintf("move %d of EVAL-3's close in the day rows", move), func() bool {
			n := r.workTime(t)
			return onFifth && n[d2] == 0 && n[d4] == 1 || !onFifth && n[d2] == 1 && n[d4] == 0
		})
		loop.awaitRan(t, 1+move)
		if got := walkNext(); got != stood {
			t.Fatalf("the rebuild's walk stands at %s after move %d of EVAL-3's close, want %s, where the boot pass left it: a change's pass began a batch", got, move, stood)
		}
	}

	next, err := time.Parse(time.DateOnly, stood)
	if err != nil {
		t.Fatalf("read the walk's day %q: %v", stood, err)
	}
	loop.advance(loopGap)
	loop.wake()
	loop.awaitRan(t, 8)
	if got, want := walkNext(), next.AddDate(0, 0, 1).Format(time.DateOnly); got != want {
		t.Errorf("the rebuild's walk stands at %s after the gap passed, want %s, one day on", got, want)
	}
}

// TestAWalkStopsBetweenDaysWhenAWakeIsOwed: a pass whose loop owes a wake (duty.WakeOwed) stops its
// walks between two days once it has built one, and leaves the batch's other days to the next
// pass, which builds them after its own pages (gapBatch). Over the Legion history of EVAL, walked
// from its first input day in batches of four days: a pass under an owed wake builds one day of
// the batch and says it stopped; a second pass under the wake, with the same batch, builds one
// more; a third, with no wake owed, builds the batch's last two and stops nowhere; a fourth with
// the same batch builds no day, the batch spent, and says the walk has days left; and a pass with a
// new batch builds four days again.
func TestAWalkStopsBetweenDaysWhenAWakeIsOwed(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLegionEval(t)
	r.batch = 4
	owed := make(chan struct{}, 1)
	owed <- struct{}{}
	woken := duty.WithWake(t.Context(), owed)
	sched := refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}
	pass := func(ctx context.Context, batch *gapBatch) refreshed {
		t.Helper()
		if _, err := r.horizon.Await(ctx); err != nil {
			t.Fatalf("await the event log's safe horizon: %v", err)
		}
		report, _, err := r.registry.refresh(ctx, RefresherDeps{Horizon: r.horizon, Build: r.build}, sched, r.seeds, batch)
		if err != nil {
			t.Fatalf("the refresher's pass failed: %v", err)
		}
		return report
	}
	walked := func(report refreshed) int {
		n := 0
		for day := range report.days {
			if day.family == walkLegion && day.project == "EVAL" {
				n++
			}
		}
		return n
	}
	batch := &gapBatch{}
	for _, step := range []struct {
		what    string
		ctx     context.Context
		batch   *gapBatch
		days    int
		yielded bool
		// left is set where the step's pass stopped or spent its batch, so the walk has days left.
		left bool
	}{
		{"the first pass under an owed wake", woken, batch, 1, true, true},
		{"the second pass under the wake, with the same batch", woken, batch, 1, true, true},
		{"the third pass, with no wake owed", t.Context(), batch, 2, false, false},
		{"the fourth pass, with the batch spent", t.Context(), batch, 0, false, true},
		{"a pass with a new batch", t.Context(), &gapBatch{}, 4, false, false},
	} {
		report := pass(step.ctx, step.batch)
		if got := walked(report); got != step.days || report.yielded != step.yielded || step.left && !report.walksLeft {
			t.Errorf("%s built %d days of EVAL's Legion walk, stopped for the wake %t, and left days %t; want %d, %t and, %t, days left",
				step.what, got, report.yielded, report.walksLeft, step.days, step.yielded, step.left)
			continue
		}
		t.Logf("%s built %d days of EVAL's Legion walk (%v), stopped for the wake %t", step.what, walked(report), report.sorted(), report.yielded)
	}
	if !duty.WakeOwed(woken) {
		t.Error("the passes took the owed wake, which is the loop's to take")
	}
}

// TestAWalkBatchThatStopsForAWakeKeepsItsDaysForTheNextPass: woken as the server wakes it, by a hub
// on the change log's channel, the refresher is woken by its own day writes too, and a batch of the
// walks stops between two days whenever a wake is owed; the owed pass, which starts at once, does
// its changes' work and then builds the days the batch has left. Over the Legion history of EVAL,
// walked from its first input day in batches of three at loopGap, the walk's write of its second
// day is held (pauseInserts) while EVAL-4 closes on the 8th day, then goes on. EVAL-4's close
// reaches the day rows, and the walk stands a whole batch further after each gap the loop's own
// clock is moved on (three days, six, then every day through today), not a day a gap, as it would
// if each stop dropped the batch's days, nor more, as it would with no gap: a pass that ends after
// the walk has reached a batch's last day finds it still there. Before, the change waited for the
// batch and for the gap after its start.
func TestAWalkBatchThatStopsForAWakeKeepsItsDaysForTheNextPass(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.batch = 3
	first, err := r.registry.legionInputStart(t.Context(), "EVAL")
	if err != nil || first == nil {
		t.Fatalf("read EVAL's first Legion input day: %v (%v)", err, first)
	}
	start := measures.Midnight(*first)
	days := int(refresherToday.Sub(start)/(24*time.Hour)) + 1
	closed := legionHistoryDay(8)
	if !start.AddDate(0, 0, 1).Before(closed) || days <= 2*r.batch {
		t.Fatalf("EVAL's walk from %s holds %d days, too few for the test's three batches and its held day", start.Format(time.DateOnly), days)
	}
	built := func() int {
		next, _ := r.rebuildNext(t, walkLegion, "EVAL")
		if next == nil {
			return 0
		}
		return int(measures.Midnight(*next).Sub(start) / (24 * time.Hour))
	}
	holder := pauseInserts(t, r.rig, "eval_days", fmt.Sprintf("new.project_key = 'EVAL' and new.day = '%s'", start.AddDate(0, 0, 1).Format(time.DateOnly)))

	loop := r.startLoop(t, r.changesWake(t))
	storetest.WaitForLockWaiters(t, holder, 1)
	row := history.rows["EVAL-4"]
	row.ClosedAt = new(closed.Add(10 * time.Hour))
	history.rows["EVAL-4"] = row
	r.storeFacts(t, history, 0, "EVAL-4")
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("let the held write on: %v", err)
	}
	testwait.Eventually(t, "EVAL-4's close in the day rows", func() bool { return r.workTime(t)[closed.Format(time.DateOnly)] == 1 })
	batches := (days + r.batch - 1) / r.batch
	for k := range batches {
		want := min(days, (k+1)*r.batch)
		if k > 0 {
			loop.advance(loopGap)
			r.nudge(t)
		}
		testwait.Eventually(t, fmt.Sprintf("the walk to have built %d of its %d days, batch %d", want, days, k+1), func() bool { return built() >= want })
		// A pass that ends after the walk reached the batch's last day finds the walk standing
		// there, where a loop with no gap would have built on.
		ran := loop.ran()
		r.nudge(t)
		loop.awaitRan(t, ran+1)
		if got := built(); got != want {
			t.Errorf("after gap %d of the loop's clock the walk had built %d of its %d days, want %d: a whole batch of %d a gap", k+1, got, days, want, r.batch)
		}
	}
	t.Logf("EVAL-4's close reached the day rows, and the walk built %d days in %d batches of %d, in %d passes", days, batches, r.batch, loop.ran())
}
