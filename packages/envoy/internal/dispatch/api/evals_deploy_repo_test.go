package api

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The checks of a move of the deploy repository (evals/deploy_repos.go): the evals that read it
// are kept for every repository that holds a delivery run, so a move is a selection at read time,
// which writes nothing, answers what a whole walk under the new settings answers, reads the new
// repository's own history from its first day, and finds the old repository's as it left it on the
// way back.

// deployRepoRig is a server over the delivery history of 30 days (seedEvalHistories), whose second
// repository, other, holds copies of the deploy repository's deploy runs from first on and of its
// PR-checks runs from three days before: a repository with a history of its own, which begins
// before its first deploy.
type deployRepoRig struct {
	served   *server
	database *store.Store
	registry *evals.Store
	fixture  deliverytest.Fixture
	projects []string
	now      time.Time
	today    time.Time
	other    string
}

// deployRepoAlice is who saves the settings of the checks.
var deployRepoAlice = model.Actor{Kind: "user", ID: "alice"}

// newDeployRepoRig seeds the rig, other's runs copied from the twelfth day of the history on where
// copied is set, runs before (where it is not nil) and then refreshes the day rows and their stored
// figures as the server's refresher writes them: one whole walk.
func newDeployRepoRig(t *testing.T, copied bool, before func(*deployRepoRig)) *deployRepoRig {
	t.Helper()
	_, database, deps, served := newRegisteredTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistories(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	now := end.Add(-time.Minute)
	r := &deployRepoRig{
		served: served, database: database, registry: evals.New(database, deps.Docs, deps.Events, deps.Attributor), fixture: fixture,
		projects: []string{fixture.Project, windows[0].Spec.Project}, now: now, today: measures.Midnight(now), other: fixture.Repos[1],
	}
	if copied {
		first := fixture.From.AddDate(0, 0, 12)
		r.copyRuns(t, delivery.DeliveryRunKindDeploy, first)
		r.copyRuns(t, delivery.DeliveryRunKindPRChecks, first.AddDate(0, 0, -3))
	}
	if before != nil {
		before(r)
	}
	r.refresh(t)
	return r
}

// copyRuns writes, as runs of other, every run of the deploy repository of kind started at or after
// from, with its jobs.
func (r *deployRepoRig) copyRuns(t *testing.T, kind delivery.DeliveryRunKind, from time.Time) {
	t.Helper()
	r.exec(t, `
		insert into delivery_runs
		select (jsonb_populate_record(null::delivery_runs, to_jsonb(run) || jsonb_build_object('repo', $2::text))).*
		from delivery_runs run where run.repo = $1 and run.kind = $3 and run.started_at >= $4`, r.fixture.DeployRepo, r.other, string(kind), from)
	r.exec(t, `
		insert into delivery_run_jobs
		select (jsonb_populate_record(null::delivery_run_jobs, to_jsonb(job) || jsonb_build_object('repo', $2::text))).*
		from delivery_run_jobs job join delivery_runs run on run.repo = job.repo and run.run_id = job.run_id
		where job.repo = $1 and run.kind = $3 and run.started_at >= $4`, r.fixture.DeployRepo, r.other, string(kind), from)
}

func (r *deployRepoRig) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := r.database.Pool.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(statement)[0], err)
	}
}

// refresh runs the refresher's passes as of the rig's moment until no walk has days left.
func (r *deployRepoRig) refresh(t *testing.T) {
	t.Helper()
	refreshEvalDays(t, r.registry, r.database, r.now)
}

// moveTo saves the delivery settings with repo as the deploy repository and runs the passes after.
func (r *deployRepoRig) moveTo(t *testing.T, repo string) {
	t.Helper()
	settings, err := delivery.GetSettings(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	settings.DeployRepo = repo
	if _, err := delivery.PutSettings(t.Context(), r.database.Pool, settings, deployRepoAlice); err != nil {
		t.Fatalf("save the delivery settings: %v", err)
	}
	r.refresh(t)
}

// read is the list read of every eval of the rig's projects in shape at the rig's moment
// (readEvalShape).
func (r *deployRepoRig) read(t *testing.T, shape evalShape) (evalListResponse, []byte) {
	t.Helper()
	return readEvalShape(t, r.served, r.projects, r.now, shape)
}

// values is what answer states of each eval from its rows, one JSON line per eval: its id, its
// series' values and its verdict, each series' version time (created_at) left out. A second
// database answers the same values under definitions written at other moments.
func values(t *testing.T, answer evalListResponse) []string {
	t.Helper()
	lines := make([]string, len(answer.Items))
	for index, item := range answer.Items {
		series := slices.Clone(item.Series)
		for i := range series {
			series[i].CreatedAt = time.Time{}
		}
		line, err := json.Marshal(struct {
			ID      string               `json:"id"`
			Series  []evals.SeriesValues `json:"series"`
			Verdict *evals.Verdict       `json:"verdict"`
		}{item.ID, series, item.Verdict})
		if err != nil {
			t.Fatalf("encode %s: %v", item.ID, err)
		}
		lines[index] = string(line)
	}
	return lines
}

// written is the transaction that last wrote each day row, member, stored figure and walk record,
// by its key: a write moves its xmin, and a row it makes or deletes comes or goes.
func (r *deployRepoRig) written(t *testing.T) map[string]string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select 'day ' || project_key || ' ' || eval_id || ' ' || deploy_repo || ' ' || series || ' ' || repo || ' ' || day, xmin::text from eval_days
		union all
		select 'member ' || project_key || ' ' || family || ' ' || deploy_repo || ' ' || row_key, xmin::text from eval_day_members
		union all
		select 'range ' || project_key || ' ' || repo || ' ' || deploy_repo || ' ' || days || ' ' || end_day || ' ' || eval_id, xmin::text from eval_range_figures
		union all
		select 'figure ' || project_key || ' ' || repo || ' ' || deploy_repo || ' ' || day || ' ' || eval_id, xmin::text from eval_day_figures
		union all
		select 'walk ' || family || ' ' || project_key || ' ' || built_by, xmin::text from eval_days_walks`)
	if err != nil {
		t.Fatalf("read what the passes wrote: %v", err)
	}
	held := map[string]string{}
	var key, xmin string
	if _, err := pgx.ForEachRow(rows, []any{&key, &xmin}, func() error {
		held[key] = xmin
		return nil
	}); err != nil {
		t.Fatalf("read what the passes wrote: %v", err)
	}
	return held
}

// rewritten is the keys of before and after whose rows a write made, deleted or moved, the first
// ten, sorted.
func rewritten(before, after map[string]string) []string {
	var moved []string
	for key, xmin := range after {
		if was, held := before[key]; !held || was != xmin {
			moved = append(moved, key)
		}
	}
	for key := range before {
		if _, held := after[key]; !held {
			moved = append(moved, key)
		}
	}
	slices.Sort(moved)
	return moved[:min(len(moved), 10)]
}

// firstDeployDay is the UTC day of other's first deploy run.
func (r *deployRepoRig) firstDeployDay(t *testing.T) time.Time {
	t.Helper()
	var first time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(started_at) from delivery_runs where repo = $1 and kind = $2`,
		r.other, string(delivery.DeliveryRunKindDeploy)).Scan(&first); err != nil {
		t.Fatalf("read %s's first deploy: %v", r.other, err)
	}
	return measures.Midnight(first)
}

// TestAMoveOfTheDeployRepositoryReadsTheNewRepositorysOwnHistory (root R11, edge a): a second
// repository holds deploy runs from a day of the history on and PR-checks runs from three days
// before it. Moved to it, the deploy repository's evals read its own rows: deploy-frequency reads
// "no data" over a window before its first deploy day and names that day (data_since); a window
// that straddles it reads "no data" too and draws only the days from it on; a window after it reads
// real values, a point of every day, the days with no deploy among them as 0. Each series names its
// own first day: pr-ci that of the repository's first PR-checks run, and merge-to-production the
// merge day of a pull request of the repository merged before its first deploy and shipped by it
// (the history's attribution inputs left unread, so the issue its row names is its attribution,
// leaveAttributionInputsUnread),
// which reads its real lead time that day while deploy-frequency holds no point of it. No row of
// the repository is older than its first run, the start of the history the reconcile holds for it.
// Read from the stored figures and then from the day rows (no figures built), the answers are the
// same bytes.
func TestAMoveOfTheDeployRepositoryReadsTheNewRepositorysOwnHistory(t *testing.T) {
	t.Parallel()
	r := newDeployRepoRig(t, true, func(r *deployRepoRig) { leaveAttributionInputsUnread(t, r.database.Pool) })
	first := r.firstDeployDay(t)
	var since time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `select min(started_at) from delivery_runs where repo = $1`, r.other).Scan(&since); err != nil {
		t.Fatalf("read %s's first run: %v", r.other, err)
	}
	since = measures.Midnight(since)
	r.moveTo(t, r.other)

	project, other := r.fixture.Project, r.other
	// shipped is the merge day of the first pull request of other that other's deploys ship, read
	// from the pull requests themselves: one of other's own (a pull request elsewhere is shipped by
	// no deploy of it, delivery.ContainingRun), created and merged as merge-to-production counts it,
	// merged on or after the day of other's first run, before which other holds no row, and before
	// the day of its first deploy, every apply of which is created after that merge.
	var shipped time.Time
	if err := r.database.Pool.QueryRow(t.Context(), `
		select min(merged_at) from delivery_pull_requests
		where repo = $1 and created_at is not null and merged_at >= $2 and merged_at < $3`,
		other, since, first).Scan(&shipped); err != nil {
		t.Fatalf("read the first pull request %s shipped before its first deploy: %v", other, err)
	}
	shipped = measures.Midnight(shipped)
	if older := countRows(t, r.database, `select count(*) from eval_days where deploy_repo = $1 and day < $2`, other, since); older != 0 {
		t.Errorf("%s holds %d rows before %s, its first run", other, older, since.Format(time.DateOnly))
	}
	shapes := []evalShape{
		{name: "the week before the first deploy", window: evalDaysEnding(7, first), offered: true},
		{name: "a week that straddles it", window: evalDaysEnding(7, first.AddDate(0, 0, 3)), offered: true},
		{name: "a week after it", window: evalDaysEnding(7, r.today), offered: true},
	}
	day := func(d time.Time) string { return d.Format(time.DateOnly) }
	stored := map[string][]byte{}
	for _, shape := range shapes {
		answer, body := r.read(t, shape)
		stored[shape.name] = body
		deploys := seriesValues(t, findEval(t, answer.Items, project, evals.SlugDeployFrequency), measures.SeriesDeploys)
		if deploys.DataSince == nil || *deploys.DataSince != day(first) {
			t.Errorf("%s: deploy-frequency names %v as its first day, want %s, %s's first deploy", shape.name, deploys.DataSince, day(first), other)
		}
		before := shape.window.From.Before(first)
		if got := deploys.Window["deploys"] == nil; got != before {
			t.Errorf("%s: deploy-frequency reads no data %t, want %t", shape.name, got, before)
		}
		for _, point := range deploys.Days {
			if point.Day < day(first) {
				t.Errorf("%s: deploy-frequency draws %s, before its first day %s", shape.name, point.Day, day(first))
			}
		}
		if !before && len(deploys.Days) != 7 {
			t.Errorf("%s: deploy-frequency draws %d days, want a point of each of the 7, 0 on a day with no deploy", shape.name, len(deploys.Days))
		}
		lead := seriesValues(t, findEval(t, answer.Items, project, evals.SlugMergeToProduction), measures.SeriesMinutes)
		if lead.DataSince == nil || *lead.DataSince != day(shipped) {
			t.Errorf("%s: merge-to-production names %v as its first day, want %s, the merge day of the first pull request %s ships", shape.name, lead.DataSince, day(shipped), other)
		}
		ci := seriesValues(t, findEval(t, answer.Items, project, evals.SlugPRCI), measures.SeriesMinutes)
		if ci.DataSince == nil || *ci.DataSince != day(since) {
			t.Errorf("%s: pr-ci names %v as its first day, want %s, %s's first PR-checks run", shape.name, ci.DataSince, day(since), other)
		}
		if inRange(shape.window, shipped) {
			if !slices.ContainsFunc(lead.Days, func(point evals.DayPoint) bool { return point.Day == day(shipped) && point.Statistics["median"] != nil }) {
				t.Errorf("%s: merge-to-production draws no lead time on %s, the merge day of a pull request %s's first deploy ships", shape.name, day(shipped), other)
			}
			if slices.ContainsFunc(deploys.Days, func(point evals.DayPoint) bool { return point.Day == day(shipped) }) {
				t.Errorf("%s: deploy-frequency draws %s, before its first day", shape.name, day(shipped))
			}
		}
	}
	if !inRange(shapes[0].window, shipped) {
		t.Fatalf("the pull request %s first shipped merged on %s, outside %s", other, day(shipped), shapes[0].name)
	}
	r.exec(t, `delete from eval_figures_built`)
	for _, shape := range shapes {
		if _, body := r.read(t, shape); !bytes.Equal(body, stored[shape.name]) {
			t.Errorf("%s: the day rows answer otherwise than the stored figures, at byte %d", shape.name, firstDifference(body, stored[shape.name]))
		}
	}
	t.Logf("%s's history begins %s, its first deploy %s; a pull request merged %s reads its lead time", other, day(since), day(first), day(shipped))
}

// TestAMoveBackToTheOldDeployRepositoryAnswersWhatItAnsweredBefore (root R11, edge b): the list
// read of every shape a page asks for, before a move of the deploy repository to a second
// repository with runs of its own, equals byte for byte the read after the move back, though while
// the second was the deploy repository the first commit of a pull request of its own that its
// deploys ship moved an hour earlier, and the pass that wrote that pull request's day moved what
// some shape read under the second (its first-commit-to-production; a first commit moves no eval
// that reads no deploy repository). The history's attribution inputs are left unread, so the issue
// each pull request names is its attribution (leaveAttributionInputsUnread) and the second
// repository's pull requests count for the project read.
func TestAMoveBackToTheOldDeployRepositoryAnswersWhatItAnsweredBefore(t *testing.T) {
	t.Parallel()
	r := newDeployRepoRig(t, true, func(r *deployRepoRig) { leaveAttributionInputsUnread(t, r.database.Pool) })
	shapes := storedEvalShapes(r.today, r.fixture.Repos[0], r.fixture.Repos[1])
	before := map[string][]byte{}
	for _, shape := range shapes {
		_, before[shape.name] = r.read(t, shape)
	}
	r.moveTo(t, r.other)
	away := map[string][]byte{}
	differs := 0
	for _, shape := range shapes {
		if _, away[shape.name] = r.read(t, shape); !bytes.Equal(away[shape.name], before[shape.name]) {
			differs++
		}
	}
	if differs == 0 {
		t.Fatalf("no shape reads otherwise with %s as the deploy repository, so the move back shows nothing", r.other)
	}
	moved, err := r.database.Pool.Exec(t.Context(), `
		update delivery_pull_requests set first_commit_at = first_commit_at - interval '1 hour'
		where (repo, number) = (
			select repo, number from delivery_pull_requests
			where repo = $1 and merged_at < $2 and created_at is not null and first_commit_at is not null
			order by merged_at desc limit 1)`,
		r.other, r.today.AddDate(0, 0, -1))
	if err != nil || moved.RowsAffected() != 1 {
		t.Fatalf("move the first commit of %s's newest pull request merged before yesterday: %v rows, %v", r.other, moved.RowsAffected(), err)
	}
	r.refresh(t)
	changed := 0
	for _, shape := range shapes {
		if _, body := r.read(t, shape); !bytes.Equal(body, away[shape.name]) {
			changed++
		}
	}
	if changed == 0 {
		t.Fatalf("the moved first commit of %s's pull request moved no shape's read, so the move back shows nothing of it", r.other)
	}
	r.moveTo(t, r.fixture.DeployRepo)
	for _, shape := range shapes {
		if _, body := r.read(t, shape); !bytes.Equal(body, before[shape.name]) {
			t.Errorf("%s: the read after the move back differs from the read before the move at byte %d", shape.name, firstDifference(body, before[shape.name]))
		}
	}
	t.Logf("%d shapes read alike before the move and after the move back; %d read otherwise in between, %d of them moved by the pull request's first commit", len(shapes), differs, changed)
}

// TestAMoveOfTheDeployRepositoryAwayAndBackWritesNothing: moved to a second repository with runs
// of its own and back, the passes write no day row, member, stored figure or walk record, and the
// list read of every shape a page asks for states, under the second repository, each eval's
// values as a second database answers them whose whole walk ran under it, and after the move back
// the values it stated before.
func TestAMoveOfTheDeployRepositoryAwayAndBackWritesNothing(t *testing.T) {
	t.Parallel()
	r := newDeployRepoRig(t, true, nil)
	walked := newDeployRepoRig(t, true, func(w *deployRepoRig) {
		settings, err := delivery.GetSettings(t.Context(), w.database.Pool)
		if err != nil {
			t.Fatalf("read the delivery settings: %v", err)
		}
		settings.DeployRepo = w.other
		if _, err := delivery.PutSettings(t.Context(), w.database.Pool, settings, deployRepoAlice); err != nil {
			t.Fatalf("save the delivery settings: %v", err)
		}
	})
	shapes := storedEvalShapes(r.today, r.fixture.Repos[0], r.fixture.Repos[1])
	original := map[string][]string{}
	for _, shape := range shapes {
		answer, _ := r.read(t, shape)
		original[shape.name] = values(t, answer)
	}
	for _, move := range []struct {
		repo string
		want func(evalShape) []string
	}{
		{r.other, func(shape evalShape) []string { answer, _ := walked.read(t, shape); return values(t, answer) }},
		{r.fixture.DeployRepo, func(shape evalShape) []string { return original[shape.name] }},
	} {
		held := r.written(t)
		r.moveTo(t, move.repo)
		if moved := rewritten(held, r.written(t)); len(moved) > 0 {
			t.Errorf("the move to %s wrote %v", move.repo, moved)
		}
		for _, shape := range shapes {
			answer, _ := r.read(t, shape)
			if got, want := values(t, answer), move.want(shape); !slices.Equal(got, want) {
				index := firstDifferingLine(got, want)
				t.Errorf("under %s, %s states otherwise than it should, first at item %d of %d:\n\tgot  %s\n\twant %s",
					move.repo, shape.name, index, len(want), lineAt(got, index), lineAt(want, index))
			}
		}
	}
}

// firstDifferingLine is the first index at which a and b differ.
func firstDifferingLine(a, b []string) int {
	for index := range min(len(a), len(b)) {
		if a[index] != b[index] {
			return index
		}
	}
	return min(len(a), len(b))
}

// lineAt is lines[index], or "(none)" past its end.
func lineAt(lines []string, index int) string {
	if index < len(lines) {
		return lines[index]
	}
	return "(none)"
}

// TestAMoveToARepositoryWithNoRunReadsNoDataOfItsDeployEvals: moved to a repository that holds no
// delivery run, every eval that reads the deploy repository states each of its series and reads
// "no data" in each over every shape a page asks for, naming no first day and drawing no day, and
// every other eval states what it stated before, each compared with its own statement by its id.
func TestAMoveToARepositoryWithNoRunReadsNoDataOfItsDeployEvals(t *testing.T) {
	t.Parallel()
	r := newDeployRepoRig(t, false, nil)
	shapes := storedEvalShapes(r.today, r.fixture.Repos[0], r.fixture.Repos[1])
	before := map[string]evalListResponse{}
	for _, shape := range shapes {
		before[shape.name], _ = r.read(t, shape)
	}
	r.moveTo(t, r.other)
	keyed, kept := 0, 0
	for _, shape := range shapes {
		answer, _ := r.read(t, shape)
		was := map[string]string{}
		for index, line := range values(t, before[shape.name]) {
			was[before[shape.name].Items[index].ID] = line
		}
		now := values(t, answer)
		if len(now) != len(was) {
			t.Errorf("%s: %d evals stated after the move, %d before", shape.name, len(now), len(was))
		}
		for index, item := range answer.Items {
			if !evals.KeyedByDeployRepo(item.Definition) {
				kept++
				if now[index] != was[item.ID] {
					t.Errorf("%s: %s states otherwise after the move:\n\t%s\nwant\n\t%s", shape.name, item.ID, now[index], was[item.ID])
				}
				continue
			}
			keyed++
			if len(item.Series) == 0 {
				t.Errorf("%s: %s states no series, so none of them reads no data", shape.name, item.ID)
			}
			for _, series := range item.Series {
				for name, value := range series.Window {
					if value != nil {
						t.Errorf("%s: %s %s reads %s %g, want no data", shape.name, item.ID, series.Series.Series, name, *value)
					}
				}
				if series.DataSince != nil || len(series.Days) != 0 {
					t.Errorf("%s: %s %s names first day %v and draws %d days, want none", shape.name, item.ID, series.Series.Series, series.DataSince, len(series.Days))
				}
			}
		}
	}
	if keyed == 0 || kept == 0 {
		t.Fatalf("the shapes state %d evals that read the deploy repository and %d others, and the check needs both", keyed, kept)
	}
	t.Logf("%d statements of evals that read the deploy repository read no data, %d of other evals stand", keyed, kept)
}

// inRange reports whether day is one of r's days.
func inRange(r evals.Range, day time.Time) bool {
	return !day.Before(r.From) && day.Before(r.To)
}
