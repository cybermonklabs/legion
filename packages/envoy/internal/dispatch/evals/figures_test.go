package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The checks of the stored figures (figures.go): after each change, the stored rows are the
// fold of the day rows that stand, as a read of those rows folds them (truthFigures, which reads
// them through DayWindow and folds each range with FoldStatistics over its rows in day order,
// merging and sorting every quantile's samples), and the rows the change wrote (by the
// transaction that last wrote each, xmin) are only those its moved days reach (figureReach).

// figureRow is one stored figures row as the checks read it: the transaction that last wrote it
// (xmin, "" for a row truthFigures folds), the build that folded it, its fold key and figures, and
// its oldest updated_at (a range row's) or its stamp (a day row's).
type figureRow struct {
	xmin    string
	builtBy string
	foldKey int64
	figures []byte
	oldest  time.Time
	stamp   int64
}

// same reports whether two rows hold the same figures, whichever transaction and build wrote them.
func (f figureRow) same(other figureRow) bool {
	return f.foldKey == other.foldKey && slices.Equal(f.figures, other.figures) && f.oldest.Equal(other.oldest) && f.stamp == other.stamp
}

// rangeLine names a stored range row: `range <project> <repo> <deploy repo> <days> <end> <eval>`.
func rangeLine(project, repo, deployRepo string, days int, end time.Time, evalID string) string {
	return fmt.Sprintf("range %s %q %q %d %s %s", project, repo, deployRepo, days, end.Format(time.DateOnly), evalID)
}

// dayFigureLine names a stored day row: `day <project> <repo> <deploy repo> <day> <eval>`.
func dayFigureLine(project, repo, deployRepo string, day time.Time, evalID string) string {
	return fmt.Sprintf("day %s %q %q %s %s", project, repo, deployRepo, day.Format(time.DateOnly), evalID)
}

// storedFigures is every stored figures row, by rangeLine and dayFigureLine.
func (r *refresherRig) storedFigures(t *testing.T) map[string]figureRow {
	t.Helper()
	stored := map[string]figureRow{}
	rows, err := r.database.Pool.Query(t.Context(), `
		select xmin::text, built_by, project_key, repo, deploy_repo, days, end_day, eval_id, fold_key, figures, oldest_updated_at
		from eval_range_figures`)
	if err != nil {
		t.Fatalf("read the stored range figures: %v", err)
	}
	var row figureRow
	var project, repo, deployRepo, evalID string
	var days int
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&row.xmin, &row.builtBy, &project, &repo, &deployRepo, &days, &at, &evalID, &row.foldKey, &row.figures, &row.oldest}, func() error {
		kept := row
		kept.oldest = kept.oldest.UTC()
		stored[rangeLine(project, repo, deployRepo, days, at.UTC(), evalID)] = kept
		return nil
	}); err != nil {
		t.Fatalf("read the stored range figures: %v", err)
	}
	rows, err = r.database.Pool.Query(t.Context(), `
		select xmin::text, built_by, project_key, repo, deploy_repo, day, eval_id, fold_key, figures, rows_stamp from eval_day_figures`)
	if err != nil {
		t.Fatalf("read the stored day figures: %v", err)
	}
	row = figureRow{}
	if _, err := pgx.ForEachRow(rows, []any{&row.xmin, &row.builtBy, &project, &repo, &deployRepo, &at, &evalID, &row.foldKey, &row.figures, &row.stamp}, func() error {
		stored[dayFigureLine(project, repo, deployRepo, at.UTC(), evalID)] = row
		return nil
	}); err != nil {
		t.Fatalf("read the stored day figures: %v", err)
	}
	return stored
}

// figuresBuilt is the last end day every project whose figures are built is built through
// (FiguresThrough), the marker a read of the stored figures reads.
func (r *refresherRig) figuresBuilt(t *testing.T) map[string]time.Time {
	t.Helper()
	projects, err := r.registry.Projects(t.Context())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	built, err := r.registry.FiguresThrough(t.Context(), projects)
	if err != nil {
		t.Fatalf("read how far the figures are built: %v", err)
	}
	return built
}

// figuresFolded is how far each project's figures are folded, by any build (readFigureScope):
// every range ending on or before it may be stored, and each write keeps them true.
func (r *refresherRig) figuresFolded(t *testing.T) map[string]time.Time {
	t.Helper()
	projects, err := r.registry.Projects(t.Context())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	folded := map[string]time.Time{}
	for _, project := range projects {
		scope, ok, err := readFigureScope(t.Context(), r.database.Pool, r.build, project)
		if err != nil {
			t.Fatalf("read how far %s's figures are folded: %v", project, err)
		}
		if ok {
			folded[project] = scope.through
		}
	}
	return folded
}

// truthFigures is every figure the walks have folded, folded from the day rows that stand as a
// read of the rows folds them: for each project folded through an end day, each repository's
// listed rows of each eval of each deploy repository read through DayWindow (the evals keyed by
// the deploy repository read with that repository, every other eval with none), in day order, and
// every range of FigureLengths ending on or before that day that holds one of them folded by
// FoldStatistics, and every day before it that holds one folded alone over its headline series. No
// list is sorted before the fold, so every quantile merges and sorts its samples.
func (r *refresherRig) truthFigures(t *testing.T) map[string]figureRow {
	t.Helper()
	ctx := t.Context()
	truth := map[string]figureRow{}
	for project, through := range r.figuresFolded(t) {
		page, err := r.registry.Current(ctx, []string{project}, "", 1000, 0)
		if err != nil {
			t.Fatalf("read the evals of %s: %v", project, err)
		}
		rows, err := r.database.Pool.Query(ctx, `select distinct repo, deploy_repo from eval_days where project_key = $1 and listed`, project)
		if err != nil {
			t.Fatalf("read the repositories of %s: %v", project, err)
		}
		cuts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (figureCut, error) {
			var cut figureCut
			return cut, row.Scan(&cut.repo, &cut.deployRepo)
		})
		if err != nil {
			t.Fatalf("read the repositories of %s: %v", project, err)
		}
		for _, cut := range cuts {
			repo := cut.repo
			var named []string
			if repo != "" {
				named = []string{repo}
			}
			var entries []Entry
			for _, entry := range page.Items {
				if KeyedByDeployRepo(entry.Definition) == (cut.deployRepo != "") {
					entries = append(entries, entry)
				}
			}
			read, err := r.registry.DayWindow(ctx, []string{project}, entries, cut.deployRepo, named, Range{From: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), To: through}, nil)
			if err != nil {
				t.Fatalf("read the day rows of %s %q of %q: %v", project, repo, cut.deployRepo, err)
			}
			byEval := map[string][]WindowRow{}
			for _, row := range read {
				byEval[row.EvalID] = append(byEval[row.EvalID], row)
			}
			for _, entry := range entries {
				held := byEval[entry.Definition.EvalID]
				listed, headline := listedAndHeadline(entry)
				if len(held) == 0 {
					continue
				}
				slices.SortStableFunc(held, func(a, b WindowRow) int { return a.Part.Day.Compare(b.Part.Day) })
				key := foldKeyOf(entry)
				within := func(from, to time.Time) ([]WindowRow, []measures.DayPart) {
					var rows []WindowRow
					var parts []measures.DayPart
					for _, row := range held {
						if !row.Part.Day.Before(from) && row.Part.Day.Before(to) {
							rows, parts = append(rows, row), append(parts, row.Part)
						}
					}
					return rows, parts
				}
				for end := held[0].Part.Day.AddDate(0, 0, 1); !end.After(through); end = end.AddDate(0, 0, 1) {
					for _, n := range FigureLengths {
						span := rangeOf(n, end)
						rows, parts := within(span.From, span.To)
						if len(rows) == 0 {
							continue
						}
						via, oldest := map[string]int{}, rows[0].UpdatedAt
						for _, row := range rows {
							via[row.Part.Series] += row.Part.ViaRepository
							if row.UpdatedAt.Before(oldest) {
								oldest = row.UpdatedAt
							}
						}
						truth[rangeLine(project, repo, cut.deployRepo, n, end, entry.Definition.EvalID)] = figureRow{
							foldKey: key, figures: encodeFigures(FoldStatistics(entry.Definition, listed, parts, span), via), oldest: oldest,
						}
					}
				}
				for day := held[0].Part.Day; day.Before(through); day = day.AddDate(0, 0, 1) {
					rows, parts := within(day, day.AddDate(0, 0, 1))
					if len(rows) == 0 {
						continue
					}
					folded := FoldStatistics(entry.Definition, headline, parts, Range{From: day, To: day.AddDate(0, 0, 1)})
					kept := map[string]Statistics{}
					stamped := []stampedRow{}
					for _, series := range headline {
						if holds(parts, series.Series) {
							kept[series.Series] = folded[series.Series]
						}
					}
					for _, row := range rows {
						stamped = append(stamped, stampedRow{series: row.Part.Series, updatedAt: row.UpdatedAt})
					}
					truth[dayFigureLine(project, repo, cut.deployRepo, day, entry.Definition.EvalID)] = figureRow{
						foldKey: key, figures: encodeFigures(kept, nil), stamp: rowsStamp(stamped),
					}
				}
			}
		}
	}
	return truth
}

// expectTruth checks that stored holds exactly truth's rows, each with truth's figures.
func expectTruth(t *testing.T, what string, stored, truth map[string]figureRow) {
	t.Helper()
	var wrong []string
	for _, line := range slices.Sorted(maps.Keys(truth)) {
		if row, held := stored[line]; !held {
			wrong = append(wrong, "missing "+line)
		} else if !row.same(truth[line]) {
			wrong = append(wrong, "stale "+line)
		}
	}
	for _, line := range slices.Sorted(maps.Keys(stored)) {
		if _, held := truth[line]; !held {
			wrong = append(wrong, "extra "+line)
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%s: %d of the %d stored figures rows are not the fold of the day rows that stand (%d expected), first ones:\n\t%s",
			what, len(wrong), len(stored), len(truth), strings.Join(wrong[:min(len(wrong), 12)], "\n\t"))
	}
}

// writtenFigures is every stored figures row a transaction wrote, made or deleted between before
// and after.
func writtenFigures(before, after map[string]figureRow) []string {
	var written []string
	for line, row := range after {
		if old, held := before[line]; !held || old.xmin != row.xmin {
			written = append(written, line)
		}
	}
	for line := range before {
		if _, held := after[line]; !held {
			written = append(written, line)
		}
	}
	slices.Sort(written)
	return written
}

// figureReach is what a change of the day rows from before to after may write of the stored
// figures, with through how far each project's figures are folded: for each listed row that
// appeared, went or had its updated_at moved, on day D of its eval and repository, every range of
// FigureLengths whose span holds D and that ends no later than through, and D's own day row; and
// those day rows alone, each of which a write of its moved rows must write, since its stamp moved.
func figureReach(before, after map[string]DayRow, through map[string]time.Time) (map[string]bool, []string) {
	reach := map[string]bool{}
	var days []string
	note := func(row DayRow) {
		limit, built := through[row.ProjectKey]
		if !row.Listed || !built || !row.Day.Before(limit) {
			return
		}
		for _, n := range FigureLengths {
			for end := row.Day.AddDate(0, 0, 1); !end.After(row.Day.AddDate(0, 0, n)) && !end.After(limit); end = end.AddDate(0, 0, 1) {
				reach[rangeLine(row.ProjectKey, row.Repo, row.DeployRepo, n, end, row.EvalID)] = true
			}
		}
		line := dayFigureLine(row.ProjectKey, row.Repo, row.DeployRepo, row.Day, row.EvalID)
		if !reach[line] {
			reach[line] = true
			days = append(days, line)
		}
	}
	for key, row := range after {
		if old, held := before[key]; !held || !old.UpdatedAt.Equal(row.UpdatedAt) || old.Listed != row.Listed {
			note(row)
			if held {
				note(old)
			}
		}
	}
	for key, row := range before {
		if _, held := after[key]; !held {
			note(row)
		}
	}
	slices.Sort(days)
	return reach, days
}

// figureState is the day rows and the stored figures at one moment.
type figureState struct {
	days    map[string]DayRow
	figures map[string]figureRow
}

func (r *refresherRig) figureState(t *testing.T) figureState {
	t.Helper()
	return figureState{days: r.dayRows(t), figures: r.storedFigures(t)}
}

// expectRewrites checks a change against the state before it, once the passes after it ran: the
// stored figures are the fold of the day rows that stand, every row written is one the day rows
// the change moved reach (figureReach), or one extra allows, each recording the pass's build as the
// build that folded it, and every moved day's own row was written; where moves is set, the change
// moved a listed day row and wrote a stored row; then answers the state after it. It logs how many
// rows the change wrote of how many its moved days reach.
func (r *refresherRig) expectRewrites(t *testing.T, what string, before figureState, moves bool, extra func(line string) bool) figureState {
	t.Helper()
	after := r.figureState(t)
	expectTruth(t, what, after.figures, r.truthFigures(t))
	reach, days := figureReach(before.days, after.days, r.figuresFolded(t))
	written := writtenFigures(before.figures, after.figures)
	if moves && (len(days) == 0 || len(written) == 0) {
		t.Errorf("%s moved %d listed day rows and wrote %d stored rows, and the check needs it to move one", what, len(days), len(written))
	}
	var outside []string
	for _, line := range written {
		if !reach[line] && (extra == nil || !extra(line)) {
			outside = append(outside, line)
		}
		if row, held := after.figures[line]; held && row.builtBy != r.build {
			t.Errorf("%s: the stored row of %s, written by the pass of %s, records %s as the build that folded it", what, line, r.build, row.builtBy)
		}
	}
	if len(outside) > 0 {
		t.Errorf("%s: %d stored figures rows were written that no moved day reaches, first ones:\n\t%s", what, len(outside), strings.Join(outside[:min(len(outside), 12)], "\n\t"))
	}
	for _, line := range days {
		if !slices.Contains(written, line) {
			t.Errorf("%s: the stored row of %s was not written, and its day rows moved", what, line)
		}
	}
	t.Logf("%s: %d moved days reach %d stored rows; the change wrote %d of them", what, len(days), len(reach), len(written))
	return after
}

// expectFoldedInTx checks that each day whose listed rows moved from before to after had its
// stored row written by a transaction that wrote one of those rows: the day rows' own, which
// folds the figures of the days it moves (figures.go), and not a later one. It answers after.
func (r *refresherRig) expectFoldedInTx(t *testing.T, what string, before, after figureState) figureState {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `select xmin::text, project_key, repo, deploy_repo, day, eval_id from eval_days where listed`)
	if err != nil {
		t.Fatalf("read the day rows' transactions: %v", err)
	}
	wrote := map[string][]string{}
	var xmin, project, repo, deployRepo, evalID string
	var day time.Time
	if _, err := pgx.ForEachRow(rows, []any{&xmin, &project, &repo, &deployRepo, &day, &evalID}, func() error {
		line := dayFigureLine(project, repo, deployRepo, day.UTC(), evalID)
		wrote[line] = append(wrote[line], xmin)
		return nil
	}); err != nil {
		t.Fatalf("read the day rows' transactions: %v", err)
	}
	_, days := figureReach(before.days, after.days, r.figuresFolded(t))
	for _, line := range days {
		if stored, held := after.figures[line]; held && !slices.Contains(wrote[line], stored.xmin) {
			t.Errorf("%s: the stored row of %s was written by transaction %s, and its day rows by %v", what, line, stored.xmin, wrote[line])
		}
	}
	return after
}

// seedLoggedQuantile registers EVAL's review-latency, a logged eval whose one series folds its
// readings' minutes as a quantile, and logs readings of it on three days of the history, some of
// them of a repository: the logged fold, a quantile, and a repository's rows beside the
// project's own.
func (r *refresherRig) seedLoggedQuantile(t *testing.T) {
	t.Helper()
	if _, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "review-latency", Caller: alice, Kind: Set(KindExperiment),
		Series: []SeriesChange{{Series: "minutes", SeriesComputationChange: SeriesComputationChange{Agg: Set(AggQuantile)}}},
	}); err != nil {
		t.Fatalf("register review-latency: %v", err)
	}
	var rows []json.RawMessage
	for index, minutes := range []float64{12, 30, 30, 7.5, 61, 2, 45, 18} {
		fields := map[string]any{"series": "minutes", "row_kind": "review", "row_key": fmt.Sprint(index), "value": minutes,
			"measured_at": refresherToday.AddDate(0, 0, -3+index%3).Add(time.Duration(index) * time.Hour)}
		if index%2 == 0 {
			fields["attributes"] = map[string]any{"repo": "acme/web"}
		}
		rows = append(rows, row(t, fields))
	}
	if _, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "review-latency", Rows: rows, Caller: alice, BuiltBy: r.build}); err != nil {
		t.Fatalf("log review-latency's readings: %v", err)
	}
}

// expectBuilt checks that every project's figures are built through the last end day of today
// (FiguresThrough), so a read of an offered range folds the stored figures.
func (r *refresherRig) expectBuilt(t *testing.T, what string, today time.Time) {
	t.Helper()
	projects, err := r.registry.Projects(t.Context())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	built := r.figuresBuilt(t)
	for _, project := range projects {
		if through, held := built[project]; !held || !through.Equal(today.AddDate(0, 0, FigureLead)) {
			t.Errorf("%s: %s's figures stand built %t through %s, want through %s", what, project, held, through.Format(time.DateOnly),
				today.AddDate(0, 0, FigureLead).Format(time.DateOnly))
		}
	}
}

// TestEachChangeRewritesExactlyTheFiguresItsMovedDaysReach: over the delivery, Legion and logged
// histories, once the first whole build has folded every figure, each change of an input is folded
// into the stored figures by the pass after it, in the day rows' own transaction where a write of
// its rows moved them, which writes only the rows its moved days reach and leaves the stored
// figures the fold of the rows that stand: a pull request merged a day earlier, a deploy run that
// is cancelled, a production job that fails, an issue's facts, a logged reading (in the readings
// write's own transaction, after which the pass writes nothing), a register write that gives the
// logged eval another fold, a settings change (every day of the eval it moves, in one
// transaction), and the daily tick (the new day's rows and the ranges of the new last end day). A
// new build whose folds agree then walks every figure and writes none, the stored figures built
// throughout; and one that meets figures another build folded otherwise rewrites exactly those.
func TestEachChangeRewritesExactlyTheFiguresItsMovedDaysReach(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	state := r.figureState(t)
	expectTruth(t, "the first whole build", state.figures, r.truthFigures(t))
	r.expectBuilt(t, "the first whole build", refresherToday)
	t.Logf("the first whole build stored %d figures rows", len(state.figures))

	// A pull request of another repository than the deploy repository's, merged a day earlier.
	var moved delivery.DeliveryPullRequest
	for _, pr := range deliveries.PullRequests {
		if pr.Repo != deliveries.DeployRepo && pr.MergedAt != nil && measures.Midnight(*pr.MergedAt).After(deliveries.From) && moved.Number == 0 {
			moved = pr
		}
	}
	if moved.Number == 0 {
		t.Fatal("the history holds no pull request of another repository merged after its first day")
	}
	r.exec(t, "merge a pull request a day earlier", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour') where repo = $1 and number = $2
	`, moved.Repo, moved.Number, moved.MergedAt.AddDate(0, 0, -1))
	r.pass(t)
	state = r.expectFoldedInTx(t, "a pull request merged a day earlier", state, r.expectRewrites(t, "a pull request merged a day earlier", state, true, nil))

	// A deploy run that succeeded is cancelled: deploy-run-success counts it among the cancelled
	// runs rather than the concluded ones.
	var run delivery.DeliveryRun
	for _, candidate := range deliveries.Runs {
		if candidate.Kind == delivery.DeliveryRunKindDeploy && candidate.Conclusion != nil && *candidate.Conclusion == delivery.DeliveryRunConclusionSuccess {
			run = candidate
		}
	}
	if run.RunID == 0 {
		t.Fatal("the history holds no deploy run that succeeded")
	}
	r.exec(t, "cancel a deploy run", `update delivery_runs set conclusion = 'cancelled' where repo = $1 and run_id = $2`, run.Repo, run.RunID)
	r.pass(t)
	state = r.expectFoldedInTx(t, "a deploy run that is cancelled", state, r.expectRewrites(t, "a deploy run that is cancelled", state, true, nil))

	// The production job of a deploy run that reached production fails: the run stops reaching
	// it, and the pull requests it shipped ship with a later one or wait.
	var job delivery.DeliveryRunJob
	for _, candidate := range deliveries.Runs {
		for _, one := range deliveries.Jobs[candidate.RunID] {
			if candidate.Kind == delivery.DeliveryRunKindDeploy && candidate.RunID != run.RunID && slices.Contains(deliveries.Settings.ProductionJobNames, one.Name) &&
				one.Conclusion != nil && *one.Conclusion == delivery.DeliveryJobConclusionSuccess {
				job, run = one, candidate
			}
		}
	}
	if job.Name == "" {
		t.Fatal("the history holds no production job that succeeded")
	}
	r.exec(t, "fail a production job", `update delivery_run_jobs set conclusion = 'failure' where repo = $1 and run_id = $2 and name = $3`, run.Repo, run.RunID, job.Name)
	r.pass(t)
	state = r.expectFoldedInTx(t, "a production job that fails", state, r.expectRewrites(t, "a production job that fails", state, true, nil))

	// An issue's facts: more work hours and a review round.
	r.exec(t, "move an issue's facts", `
		update issue_facts set work_hours = work_hours + 1.5, review_rounds = review_rounds + 1
		where project_key = 'LEGION' and issue_key = (
			select issue_key from issue_facts where project_key = 'LEGION' and closed_at is not null order by closed_at desc, issue_key limit 1)`)
	r.pass(t)
	state = r.expectFoldedInTx(t, "an issue's facts", state, r.expectRewrites(t, "an issue's facts", state, true, nil))

	// A logged reading, folded in the readings write's own transaction; the pass after it finds
	// the figures of its day fresh and writes none.
	r.write(t, "review-latency", alice, row(t, map[string]any{"series": "minutes", "row_kind": "review", "row_key": "late",
		"value": 90, "measured_at": refresherToday.AddDate(0, 0, -2).Add(5 * time.Hour), "attributes": map[string]any{"repo": "acme/web"}}))
	state = r.expectFoldedInTx(t, "a logged reading's write", state, r.expectRewrites(t, "a logged reading's write", state, true, nil))
	r.pass(t)
	if after := r.storedFigures(t); len(writtenFigures(state.figures, after)) != 0 {
		t.Errorf("the pass after a logged reading wrote the stored figures %v, which the reading's write folded", writtenFigures(state.figures, after))
	}

	// A register write that folds the logged eval by its largest value: its fold key moves, and
	// every one of its stored rows is folded again.
	evalID := EvalID(r.project, "review-latency")
	if _, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-latency", Caller: alice,
		Series: []SeriesChange{{Series: "minutes", SeriesComputationChange: SeriesComputationChange{Agg: Set(AggMax)}}}}); err != nil {
		t.Fatalf("fold review-latency by its largest value: %v", err)
	}
	r.pass(t)
	state = r.expectRewrites(t, "a register write of the logged eval's fold", state, false, func(line string) bool { return strings.HasSuffix(line, " "+evalID) })

	// A settings change: the staging e2e job is named one no run holds, and one pass rewrites every
	// day of staging-e2e, folding the figures of the days it moved in its own transaction.
	settings := deliveries.Settings
	settings.StagingE2EJobName = new("no-such-job")
	if _, err := delivery.PutSettings(t.Context(), r.database.Pool, settings, model.Actor{Kind: "system", ID: "figures-test"}); err != nil {
		t.Fatalf("rename the staging e2e job: %v", err)
	}
	r.settle(t)
	state = r.expectRewrites(t, "a settings change", state, true, nil)

	// The daily tick: a pass of the next UTC day writes its rows, and the ranges of the end day it
	// adds, two days after the new today, of every eval.
	added := refresherToday.AddDate(0, 0, 1+FigureLead).Format(time.DateOnly)
	r.now = r.now.Add(24 * time.Hour)
	r.settle(t)
	state = r.expectRewrites(t, "the daily tick", state, true, func(line string) bool {
		return strings.HasPrefix(line, "range ") && strings.Contains(line, " "+added+" ")
	})
	r.expectBuilt(t, "after the daily tick", refresherToday.AddDate(0, 0, 1))

	// A new build whose folds agree with the build before it: its first pass still reads the
	// stored figures as built, and its whole walk of them writes none.
	first := r.build
	r.build = "another-build"
	r.pass(t)
	r.expectBuilt(t, "a new build's first pass", refresherToday.AddDate(0, 0, 1))
	r.settle(t)
	after := r.figureState(t)
	expectTruth(t, "a new build's whole walk", after.figures, r.truthFigures(t))
	if written := writtenFigures(state.figures, after.figures); len(written) != 0 {
		t.Errorf("the whole walk of a new build whose folds agree wrote %d stored rows, first %v", len(written), written[:min(len(written), 5)])
	}
	state = after

	// Figures another build folded otherwise (the stored rows of one eval, as a build of other
	// folds leaves them): the next build's whole walk rewrites exactly those.
	other := EvalID(deliveries.Project, SlugDeployFrequency)
	r.exec(t, "fold one eval's figures otherwise", `
		update eval_range_figures set figures = '\x00'::bytea, built_by = 'other-folds' where eval_id = $1`, other)
	state = r.figureState(t)
	r.build = "third-build"
	r.settle(t)
	after = r.figureState(t)
	expectTruth(t, "a build's walk over figures another build folded otherwise", after.figures, r.truthFigures(t))
	for _, line := range writtenFigures(state.figures, after.figures) {
		if !strings.HasPrefix(line, "range ") || !strings.HasSuffix(line, " "+other) {
			t.Errorf("a build's walk over %s's figures folded otherwise wrote %s too", other, line)
		}
	}
	for line, row := range after.figures {
		if strings.HasPrefix(line, "range ") && strings.HasSuffix(line, " "+other) && row.builtBy != r.build {
			t.Errorf("the walk left %s recorded as %s's fold", line, row.builtBy)
		}
	}
	t.Logf("after %s and %s, the third build's walk rewrote the %d stored rows another build had folded otherwise",
		first, "another-build", len(writtenFigures(state.figures, after.figures)))
}

// TestADayRowABuildWithNoFiguresWritesIsFoldedByTheNextPass: a build that holds the day rows and
// not the stored figures writes day rows and no figures, and its refresher's pass reads the
// refresher's own change cursor past the eval_days changes its writes log; the next pass of a
// build with the figures reads those changes from the figures' own cursor and folds the figures of
// the days they moved, and only those.
func TestADayRowABuildWithNoFiguresWritesIsFoldedByTheNextPass(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	state := r.figureState(t)
	day := deliveries.To.AddDate(0, 0, -3)
	// That build's write of one day row, as its refresher writes a value, and its pass's read of
	// the refresher's own cursor past the change the write logs.
	r.exec(t, "write a day row as a build with no figures does", `
		update eval_days set n = n + 1, sum = sum + 1, updated_at = now()
		where project_key = $1 and eval_id = $2 and series = $3 and repo = '' and day = $4`,
		deliveries.Project, EvalID(deliveries.Project, SlugDeployFrequency), measures.SeriesDeploys, day)
	r.exec(t, "move the change into the log", `select delivery_changes_drain(1000)`)
	r.exec(t, "read the refresher's own cursor past it", `
		update eval_days_cursor set position = head.seq, at = head.at
		from (select seq, at from delivery_changes order by seq desc limit 1) as head where name = $1`, changesCursor)
	stale := r.figureState(t)
	if reach, days := figureReach(state.days, stale.days, r.figuresFolded(t)); len(days) == 0 {
		t.Fatalf("the write moved no day row (%d figures reached)", len(reach))
	}
	truth := r.truthFigures(t)
	staleRows := 0
	for line, row := range truth {
		if stored, held := stale.figures[line]; !held || !stored.same(row) {
			staleRows++
		}
	}
	if staleRows == 0 {
		t.Fatal("the write left every stored figure the fold of the rows that stand, so nothing shows the next pass folds it")
	}
	t.Logf("the write of a build with no figures left %d stored rows stale", staleRows)
	r.pass(t)
	r.expectRewrites(t, "the next pass of a build with the figures", state, true, nil)
}

// TestTheMarkerHoldsTheReadOffUntilTheFirstWholeBuild: a read of the stored figures reads them only
// of a project whose figures are built (FiguresThrough). While the first pass's walks of the day
// rows have days left the figures walk waits, and no project is built; the figures walk then folds
// a batch of end days a pass, what it has folded is at every pass the fold of the rows that stand,
// and a project stands built only once its walk has folded every end day through today+2, which no
// project with rows does while its walk has batches left. A project made later is built at the next
// pass, holding no row. The marker then holds whichever build serves: a new build's first pass, a
// roll of the two, and a rollback to the first all read the figures as built.
func TestTheMarkerHoldsTheReadOffUntilTheFirstWholeBuild(t *testing.T) {
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	last := refresherToday.AddDate(0, 0, FigureLead)
	heldOff := 0
	for passes := 1; passes <= 100; passes++ {
		_, next := r.pass(t)
		built := r.figuresBuilt(t)
		if passes == 1 && len(built) != 0 {
			t.Errorf("the first pass, whose walks of the day rows have days left, left figures built: %v", built)
		}
		expectTruth(t, fmt.Sprintf("pass %d", passes), r.storedFigures(t), r.truthFigures(t))
		walks, err := r.registry.readWalks(t.Context(), r.build)
		if err != nil {
			t.Fatalf("read the walks: %v", err)
		}
		for key, walk := range walks {
			if key.family != walkFigures || walk.next == nil || walk.next.After(last) {
				continue
			}
			if through, held := built[key.project]; held {
				t.Errorf("pass %d left %s's figures walk at %s with end days to fold, and its figures stand built through %s",
					passes, key.project, walk.next.Format(time.DateOnly), through.Format(time.DateOnly))
			}
			heldOff++
		}
		if !next.Equal(r.now) {
			break
		}
	}
	if heldOff == 0 {
		t.Error("no pass left a project's figures walk part of the way, so nothing shows a read held off while they are folded")
	}
	r.expectBuilt(t, "the first whole build", refresherToday)
	expectTruth(t, "the first whole build", r.storedFigures(t), r.truthFigures(t))

	r.exec(t, "make a project later", `insert into projects (key, name, default_assignee) values ('LATER', 'Later', 'alice')`)
	r.pass(t)
	r.expectBuilt(t, "a pass after a project is made", refresherToday)

	first := r.build
	r.build = "another-build"
	r.pass(t)
	r.expectBuilt(t, "a new build's first pass", refresherToday)
	r.build = first
	r.pass(t)
	r.expectBuilt(t, "a pass of the build before it, as in a roll or a rollback", refresherToday)
	r.build = "another-build"
	r.settle(t)
	r.build = first
	r.pass(t)
	r.expectBuilt(t, "a rollback once the new build's walk is done", refresherToday)
	expectTruth(t, "after the roll and the rollback", r.storedFigures(t), r.truthFigures(t))
}

// TestTheDayRowsAreFoldedInDayOrderWhateverOrderTheReadAnswers: a read of the day rows answers
// them in its plan's order, and a sum of floats depends on the order of its parts, so
// SeriesValuesOf folds them in day order, as the stored figures are folded (newFigureGroup): three
// days of a logged sum, 1e16 then 1 then 1, sum to 1e16 in day order (each 1 is half an ulp of
// 1e16 and rounds to the even neighbour) and to 1e16+2 the other way round, and the read answers
// the day order's bits whichever order its rows come in.
func TestTheDayRowsAreFoldedInDayOrderWhateverOrderTheReadAnswers(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	entry := Entry{Definition: Definition{EvalID: "EVAL/spend", ProjectKey: "EVAL", Source: SourceLogged},
		Series: []Series{{EvalID: "EVAL/spend", Series: "dollars", SeriesComputation: SeriesComputation{Role: RoleHeadline, Listed: true, Agg: AggSum}}}}
	rows := func(order ...int) []WindowRow {
		values := []float64{1e16, 1, 1}
		var held []WindowRow
		for _, index := range order {
			sum := values[index]
			held = append(held, WindowRow{Range: RangeWindow, EvalID: entry.Definition.EvalID,
				Part: measures.DayPart{Series: "dollars", Day: day.AddDate(0, 0, index), N: 1, Sum: &sum, Samples: []float64{sum}}})
		}
		return held
	}
	window := Range{From: day, To: day.AddDate(0, 0, 3)}
	firsts := map[string]time.Time{"dollars": day}
	inOrder := SeriesValuesOf(entry, rows(0, 1, 2), firsts, window, nil)
	reversed := SeriesValuesOf(entry, rows(2, 1, 0), firsts, window, nil)
	want, got := inOrder[0].Window["sum"], reversed[0].Window["sum"]
	if want == nil || *want != 1e16 || got == nil || math.Float64bits(*got) != math.Float64bits(*want) {
		t.Errorf("the sum of rows answered in day order is %s and of rows answered the other way round %s, want 1e16 for both", showValue(want), showValue(got))
	}
}

// foldMovedDays is the whole fold, on up to workers goroutines at once, of the stored figures of
// each day moved names (by eval, deploy repository and repository) and of every range whose span
// holds one, with no write: what refoldFigures reads (readFigurePlan) and folds while it writes
// (figurePlan.foldAndWrite), answered as writeFigures takes it, in no order. The checks compare its
// rows on many workers to its rows on one, and a write's statements to its rows on one.
func foldMovedDays(ctx context.Context, q delivery.Querier, scope figureScope, entries map[string]Entry, moved map[figureGroupKey]map[time.Time]bool, workers int) (figureWrites, error) {
	plan, err := readFigurePlan(ctx, q, scope, entries, moved)
	if err != nil {
		return figureWrites{}, err
	}
	// No chunk is sent, so the fold answers no error.
	if err := foldInChunks(len(plan.ranges)+len(plan.days), workers, plan.fold, nil, nil); err != nil {
		return figureWrites{}, err
	}
	writes := figureWrites{goneRanges: plan.goneRanges, goneDays: plan.goneDays}
	for _, target := range plan.ranges {
		if target.held {
			writes.ranges = append(writes.ranges, target.rangeFigureRow)
		} else {
			writes.goneRanges = append(writes.goneRanges, target.rangeFigureKey)
		}
	}
	for _, target := range plan.days {
		if target.held {
			writes.days = append(writes.days, target.dayFigureRow)
		} else {
			writes.goneDays = append(writes.goneDays, target.dayFigureKey)
		}
	}
	return writes, nil
}

// The four kinds of statement that write the stored figures, as figureStatementKind names them.
const (
	rangeUpsertKind = "insert into eval_range_figures"
	rangeDeleteKind = "delete from eval_range_figures"
	dayUpsertKind   = "insert into eval_day_figures"
	dayDeleteKind   = "delete from eval_day_figures"
)

// figureStatementKinds is the four, in the order a write sends them (writeFigures).
var figureStatementKinds = []string{rangeUpsertKind, rangeDeleteKind, dayUpsertKind, dayDeleteKind}

// figureStatementKind is which of figureStatementKinds sql is, "" for none.
func figureStatementKind(sql string) string {
	for _, kind := range figureStatementKinds {
		if strings.Contains(sql, kind) {
			return kind
		}
	}
	return ""
}

// statementOIDs is the type of each parameter of each of figureStatementKinds, as its casts name
// it: what pgx encodes each argument as.
var statementOIDs = map[string][]uint32{
	rangeUpsertKind: {pgtype.TextOID, pgtype.TextArrayOID, pgtype.Int4ArrayOID, pgtype.DateArrayOID, pgtype.TextArrayOID,
		pgtype.Int8ArrayOID, pgtype.ByteaArrayOID, pgtype.TimestamptzArrayOID, pgtype.TextOID, pgtype.TextArrayOID},
	rangeDeleteKind: {pgtype.TextOID, pgtype.TextArrayOID, pgtype.Int4ArrayOID, pgtype.DateArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID},
	dayUpsertKind: {pgtype.TextOID, pgtype.TextArrayOID, pgtype.DateArrayOID, pgtype.TextArrayOID, pgtype.Int8ArrayOID,
		pgtype.ByteaArrayOID, pgtype.Int8ArrayOID, pgtype.TextOID, pgtype.TextArrayOID},
	dayDeleteKind: {pgtype.TextOID, pgtype.TextArrayOID, pgtype.DateArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID},
}

// encodedBytes is the bytes args, the arguments of a statement, take as pgx sends them as the
// types oids: each encoded as its parameter's type, in the format pgx sends that type in, with 4
// bytes of length before it.
func encodedBytes(t *testing.T, oids []uint32, args []any) int {
	t.Helper()
	if len(oids) != len(args) {
		t.Fatalf("a statement of %d arguments, where its parameters are the %d types %v", len(args), len(oids), oids)
	}
	types, total := pgtype.NewMap(), 0
	for index, arg := range args {
		encoded, err := types.Encode(oids[index], types.FormatCodeForOID(oids[index]), arg, nil)
		if err != nil {
			t.Fatalf("encode argument %d of a statement of the types %v: %v", index+1, oids, err)
		}
		total += 4 + len(encoded)
	}
	return total
}

// paramEstimate is the most bytes args, the arguments of a statement, take as pgx encodes them,
// estimated from the arguments as statementChunks' callers estimate a statement's (paramBytes,
// and upsertBytes, deleteBytes or dayRowBytes): 4 bytes of length before each argument, a
// string's bytes, and an array's header and, for each element, 4 bytes of length and its bytes, a
// string's or a byte slice's length (none for a null string), 1 for a bool and at most 8 for a
// number, a date or a time, null or not.
func paramEstimate(t *testing.T, args []any) int {
	t.Helper()
	bytes := 4 * len(args)
	for _, arg := range args {
		switch value := arg.(type) {
		case string:
			bytes += len(value)
		case []string:
			bytes += arrayHeaderBytes
			for _, one := range value {
				bytes += 4 + len(one)
			}
		case []*string:
			bytes += arrayHeaderBytes + 4*len(value)
			for _, one := range value {
				if one != nil {
					bytes += len(*one)
				}
			}
		case [][]byte:
			bytes += arrayHeaderBytes
			for _, one := range value {
				bytes += 4 + len(one)
			}
		case []bool:
			bytes += arrayHeaderBytes + (4+1)*len(value)
		case []int32:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []int64:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []*float64:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []time.Time:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []*time.Time:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		default:
			t.Fatalf("an argument of %T, which the estimate does not know", arg)
		}
	}
	return bytes
}

// rowsIn is how many rows a statement of the stored figures takes, args its arguments: the length
// of its first array, its rows' repositories.
func rowsIn(args []any) int {
	return len(args[1].([]string))
}

// figureStatements is a tracer (storetest.Traced) that records, in the order they start, the
// statements on the pool it traces that write the stored figures (figureStatementKinds): each one's
// kind and arguments, and once it ends, the rows it wrote and its error.
type figureStatements struct {
	mu   sync.Mutex
	sent []figureStatement
}

// figureStatement is one statement figureStatements records.
type figureStatement struct {
	kind string
	args []any
	rows int64
	err  error
}

// figureStatementAt marks the context of a recorded statement with its index among them.
type figureStatementAt struct{}

func (f *figureStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	kind := figureStatementKind(data.SQL)
	if kind == "" {
		return ctx
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, figureStatement{kind: kind, args: data.Args})
	return context.WithValue(ctx, figureStatementAt{}, len(f.sent)-1)
}

func (f *figureStatements) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if index, ok := ctx.Value(figureStatementAt{}).(int); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sent[index].rows, f.sent[index].err = data.CommandTag.RowsAffected(), data.Err
	}
}

// take answers the statements recorded since the last take, each of them ended, and forgets them.
func (f *figureStatements) take() []figureStatement {
	f.mu.Lock()
	defer f.mu.Unlock()
	taken := f.sent
	f.sent = nil
	return taken
}

// writesOf is the rows statements take, by kind, each list in the order they take its rows.
func writesOf(statements []figureStatement) figureWrites {
	var w figureWrites
	for _, statement := range statements {
		args := statement.args
		switch statement.kind {
		case rangeUpsertKind:
			repos, ns, ends, evalIDs, keys := args[1].([]string), args[2].([]int32), args[3].([]time.Time), args[4].([]string), args[5].([]int64)
			figures, oldest, deployRepos := args[6].([][]byte), args[7].([]time.Time), args[9].([]string)
			for index := range repos {
				w.ranges = append(w.ranges, rangeFigureRow{
					rangeFigureKey: rangeFigureKey{repo: repos[index], days: int(ns[index]), end: ends[index], eval: evalIDs[index], deployRepo: deployRepos[index]},
					key:            keys[index], figures: figures[index], oldest: oldest[index],
				})
			}
		case rangeDeleteKind:
			repos, ns, ends, evalIDs, deployRepos := args[1].([]string), args[2].([]int32), args[3].([]time.Time), args[4].([]string), args[5].([]string)
			for index := range repos {
				w.goneRanges = append(w.goneRanges, rangeFigureKey{repo: repos[index], days: int(ns[index]), end: ends[index], eval: evalIDs[index], deployRepo: deployRepos[index]})
			}
		case dayUpsertKind:
			repos, days, evalIDs, keys := args[1].([]string), args[2].([]time.Time), args[3].([]string), args[4].([]int64)
			figures, stamps, deployRepos := args[5].([][]byte), args[6].([]int64), args[8].([]string)
			for index := range repos {
				w.days = append(w.days, dayFigureRow{
					dayFigureKey: dayFigureKey{repo: repos[index], day: days[index], eval: evalIDs[index], deployRepo: deployRepos[index]},
					key:          keys[index], figures: figures[index], stamp: stamps[index],
				})
			}
		case dayDeleteKind:
			repos, days, evalIDs, deployRepos := args[1].([]string), args[2].([]time.Time), args[3].([]string), args[4].([]string)
			for index := range repos {
				w.goneDays = append(w.goneDays, dayFigureKey{repo: repos[index], day: days[index], eval: evalIDs[index], deployRepo: deployRepos[index]})
			}
		}
	}
	return w
}

// rangeKeyOrder and dayFigureKeyOrder each answer the positions, from 0, of the keys its arrays
// hold (range rows' and day rows' of the stored figures, each array a column of the keys), in
// the byte order of the keys, every text `collate "C"`: the order of the table's primary key that
// every writer of its rows takes them in, which upsertRangeFigures' and writeAfterRanges'
// statements sorted their rows by before they took them in the order sent (`with ordinality`).
const (
	rangeKeyOrder = `
		select (k.sent - 1)::int from unnest($1::text[], $2::int[], $3::date[], $4::text[], $5::text[]) with ordinality
			as k(repo, days, end_day, eval_id, deploy_repo, sent)
		order by k.repo collate "C", k.days, k.end_day, k.eval_id collate "C", k.deploy_repo collate "C"`
	dayFigureKeyOrder = `
		select (k.sent - 1)::int from unnest($1::text[], $2::date[], $3::text[], $4::text[]) with ordinality
			as k(repo, day, eval_id, deploy_repo, sent)
		order by k.repo collate "C", k.day, k.eval_id collate "C", k.deploy_repo collate "C"`
)

// rangeKeyArrays is keys as rangeKeyOrder's arrays.
func rangeKeyArrays(keys []rangeFigureKey) []any {
	repos, ns, ends, evalIDs, deployRepos := make([]string, len(keys)), make([]int32, len(keys)), make([]time.Time, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for index, key := range keys {
		repos[index], ns[index], ends[index], evalIDs[index], deployRepos[index] = key.repo, int32(key.days), key.end, key.eval, key.deployRepo
	}
	return []any{repos, ns, ends, evalIDs, deployRepos}
}

// dayFigureKeyArrays is keys as dayFigureKeyOrder's arrays.
func dayFigureKeyArrays(keys []dayFigureKey) []any {
	repos, days, evalIDs, deployRepos := make([]string, len(keys)), make([]time.Time, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for index, key := range keys {
		repos[index], days[index], evalIDs[index], deployRepos[index] = key.repo, key.day, key.eval, key.deployRepo
	}
	return []any{repos, days, evalIDs, deployRepos}
}

// rangeKeyText and dayFigureKeyText are a key as a failure names it.
func rangeKeyText(key rangeFigureKey) string {
	return fmt.Sprintf("%q, %d days to %s, %q, %q", key.repo, key.days, key.end.Format(time.DateOnly), key.eval, key.deployRepo)
}

func dayFigureKeyText(key dayFigureKey) string {
	return fmt.Sprintf("%q, %s, %q, %q", key.repo, key.day.Format(time.DateOnly), key.eval, key.deployRepo)
}

// keyOrderOf is the positions, from 0, of the n keys the arrays args hold, in the byte order
// query (rangeKeyOrder, dayFigureKeyOrder or dayRowKeyOrder) answers them in.
func keyOrderOf(t *testing.T, q delivery.Querier, n int, query string, args ...any) []int {
	t.Helper()
	rows, err := q.Query(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("order %d keys by their bytes: %v", n, err)
	}
	order, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("order %d keys by their bytes: %v", n, err)
	}
	if len(order) != n {
		t.Fatalf("%d keys ordered by their bytes came back as %d", n, len(order))
	}
	return order
}

// firstOutOfOrder is the first index of order (keyOrderOf's) that holds another position, -1 for
// none: keys sent in the byte order of their keys come back in the order they were sent.
func firstOutOfOrder(order []int) int {
	for index, at := range order {
		if at != index {
			return index
		}
	}
	return -1
}

// TestEachWritesGoKeyOrderIsPostgresCollateCOrder: the range upsert and the day upsert of the
// stored figures (upsertRangeFigures, writeAfterRanges) and the statement of day rows
// (writeDayRows) take their rows in the order they are sent them, so Go's sort of each is the
// order they lock rows in, where every writer of those rows takes them in their keys' byte order,
// the order `collate "C"` gives them. For each of the three, keys whose texts differ at every
// collation edge a byte order has (upper against lower case, accented against plain letters,
// digits against letters, punctuation, a key that is a prefix of another, and the empty string),
// in one field of the key with keys equal in their leading fields, given in a shuffled order and
// sorted by the write's own sort (figureWrites.sortByKey, compareDayRows), come back in the same
// sequence from Postgres's `order by` of the same keys, each text `collate "C"`. The test
// database collates text by ICU's en-US, under which many of those pairs order the other way.
func TestEachWritesGoKeyOrderIsPostgresCollateCOrder(t *testing.T) {
	r := newRig(t)
	// edges is texts whose byte order is not the order a collation by language gives them.
	edges := []string{"", "a", "A", "b", "B", "e", "é", "É", "f", "1", "9", "a1", "aa", "a b", "a-b", "a/b", "a_b",
		"acme/web", "Acme/web", "acme/web2", "acme/wéb", "~"}
	// few is some of them, for the other text fields of a key.
	few := []string{"", "B", "a", "é", "1"}
	// Day rows' days are instants, which their statement reads as the UTC date each falls on.
	instants := []time.Time{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 10, 12, 0, 0, 0, time.UTC)}
	days := make([]time.Time, len(instants))
	for index, instant := range instants {
		days[index] = measures.Midnight(instant)
	}
	shuffle := rand.New(rand.NewPCG(1, 2))

	var writes figureWrites
	for _, repo := range edges {
		for _, n := range []int{7, 8, 14, 85} {
			for _, end := range days {
				for _, evalID := range few {
					for _, deployRepo := range few {
						writes.ranges = append(writes.ranges, rangeFigureRow{rangeFigureKey: rangeFigureKey{repo: repo, days: n, end: end, eval: evalID, deployRepo: deployRepo}})
					}
				}
			}
		}
	}
	for _, repo := range few {
		for _, day := range days {
			for _, evalID := range edges {
				for _, deployRepo := range few {
					writes.days = append(writes.days, dayFigureRow{dayFigureKey: dayFigureKey{repo: repo, day: day, eval: evalID, deployRepo: deployRepo}})
				}
			}
		}
	}
	var rows []DayRow
	for _, project := range few[:3] {
		for _, evalID := range few {
			for _, deployRepo := range few[:3] {
				for _, series := range edges {
					for _, repo := range few {
						for _, day := range instants {
							rows = append(rows, DayRow{ProjectKey: project, EvalID: evalID, DeployRepo: deployRepo, Repo: repo, DayPart: measures.DayPart{Series: series, Day: day}})
						}
					}
				}
			}
		}
	}
	shuffle.Shuffle(len(writes.ranges), func(i, j int) { writes.ranges[i], writes.ranges[j] = writes.ranges[j], writes.ranges[i] })
	shuffle.Shuffle(len(writes.days), func(i, j int) { writes.days[i], writes.days[j] = writes.days[j], writes.days[i] })
	shuffle.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	writes.sortByKey()
	slices.SortFunc(rows, func(a, b DayRow) int { return compareDayRows(&a, &b) })

	rangeKeys := make([]rangeFigureKey, len(writes.ranges))
	for index, row := range writes.ranges {
		rangeKeys[index] = row.rangeFigureKey
	}
	order := keyOrderOf(t, r.database.Pool, len(rangeKeys), rangeKeyOrder, rangeKeyArrays(rangeKeys)...)
	if at := firstOutOfOrder(order); at >= 0 {
		t.Errorf("Go sorts %d range keys (compareRangeKeys) otherwise than Postgres's collate \"C\": at %d Go puts %s, and Postgres %s",
			len(rangeKeys), at, rangeKeyText(rangeKeys[at]), rangeKeyText(rangeKeys[order[at]]))
	}
	dayKeys := make([]dayFigureKey, len(writes.days))
	for index, row := range writes.days {
		dayKeys[index] = row.dayFigureKey
	}
	order = keyOrderOf(t, r.database.Pool, len(dayKeys), dayFigureKeyOrder, dayFigureKeyArrays(dayKeys)...)
	if at := firstOutOfOrder(order); at >= 0 {
		t.Errorf("Go sorts %d day figure keys (compareDayFigureKeys) otherwise than Postgres's collate \"C\": at %d Go puts %s, and Postgres %s",
			len(dayKeys), at, dayFigureKeyText(dayKeys[at]), dayFigureKeyText(dayKeys[order[at]]))
	}
	order = keyOrderOf(t, r.database.Pool, len(rows), dayRowKeyOrder, dayRowKeyArrays(rows)...)
	if at := firstOutOfOrder(order); at >= 0 {
		key := func(row DayRow) string {
			return dayKeyLine(row.ProjectKey, row.EvalID, row.DeployRepo, row.Series, row.Repo, row.Day)
		}
		t.Errorf("Go sorts %d day rows (compareDayRows) otherwise than Postgres's collate \"C\": at %d Go puts %q, and Postgres %q",
			len(rows), at, key(rows[at]), key(rows[order[at]]))
	}
	t.Logf("%d range keys, %d day figure keys and %d day row keys, each sorted by its write's sort, held to Postgres's collate \"C\" order of them",
		len(rangeKeys), len(dayKeys), len(rows))
}

// TestARangeChunkClosesAtARepositoryOrLengthOnceItHoldsTheLeast: the chunks a write folds and
// sends of its ranges, in the byte order of their keys, close at the end of a (repository, length)
// prefix of the keys only once they hold at least the least ranges, and at the limit wherever it
// falls (rangeChunks): at a least of one, a chunk each prefix, cut again at the limit; at a larger
// least, chunks of several prefixes, which pass the end of a prefix inside them by; and at
// rangeChunkLeast, a write of a few ranges is one chunk.
func TestARangeChunkClosesAtARepositoryOrLengthOnceItHoldsTheLeast(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var ranges []rangeTarget
	for _, prefix := range []struct {
		repo        string
		days, count int
	}{{"", 7, 3}, {"", 8, 5}, {"acme/api", 7, 1}, {"acme/api", 8, 2}} {
		for end := range prefix.count {
			key := rangeFigureKey{repo: prefix.repo, days: prefix.days, end: day.AddDate(0, 0, end), eval: "EVAL/latency"}
			ranges = append(ranges, rangeTarget{rangeFigureRow: rangeFigureRow{rangeFigureKey: key}})
		}
	}
	for _, check := range []struct {
		least, limit int
		want         []int
	}{
		{1, 2, []int{2, 3, 5, 7, 8, 9, 11}},
		{1, statementRows, []int{3, 8, 9, 11}},
		{4, statementRows, []int{8, 11}},
		{3, 4, []int{3, 7, 11}},
		{rangeChunkLeast, statementRows, []int{11}},
	} {
		if got := rangeChunks(ranges, check.least, check.limit); !slices.Equal(got, check.want) {
			t.Errorf("the chunks of %d ranges at a least of %d and a limit of %d end at %v, want %v", len(ranges), check.least, check.limit, got, check.want)
		}
	}
	if got := rangeChunks(nil, rangeChunkLeast, statementRows); len(got) != 0 {
		t.Errorf("no range is cut into the chunks %v", got)
	}
}

// TestThePipelinedFoldFoldsOnItsWorkersAndSendsEachChunkOnceItIsFolded: foldInChunks on
// figureFoldWorkers workers runs that many folds at once and never more; sends each chunk once and
// in order, on its calling goroutine, once every fold of it is done, while the workers go on with
// the chunks after it; and folds every index once, those after the last chunk (a write's days)
// included. Once a send fails it sends no other chunk, leaves indexes unfolded, and answers that
// error once no fold runs.
func TestThePipelinedFoldFoldsOnItsWorkersAndSendsEachChunkOnceItIsFolded(t *testing.T) {
	bound := testwait.Bounded(t, time.Minute, time.Second)
	var folding, most, begun atomic.Int64
	enter := func() {
		now := folding.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		begun.Add(1)
	}
	const n = 64
	ends := []int{8, 9, 20, 41, 56}
	var folds [n]atomic.Int32
	var short atomic.Bool
	fold := func(index int) {
		enter()
		// The first folds wait until figureFoldWorkers of them run at once, which a pool of fewer
		// workers never reaches, and then a while longer, in which a worker past the cap would take
		// the next index: none does, since every worker is held.
		if index < figureFoldWorkers {
			if !testwait.Holds(bound, testwait.Poll, func() bool { return most.Load() >= figureFoldWorkers }) {
				short.Store(true)
			}
			testwait.Holds(200*time.Millisecond, testwait.Poll, func() bool { return begun.Load() > figureFoldWorkers })
		}
		folds[index].Add(1)
		folding.Add(-1)
	}
	var sent []int
	if err := foldInChunks(n, figureFoldWorkers, fold, ends, func(chunk int) error {
		from := 0
		if chunk > 0 {
			from = ends[chunk-1]
		}
		for index := from; index < ends[chunk]; index++ {
			if got := folds[index].Load(); got != 1 {
				t.Errorf("chunk %d is sent with its index %d folded %d times", chunk, index, got)
			}
		}
		if chunk == 0 && !testwait.Holds(bound, testwait.Poll, func() bool { return begun.Load() > int64(ends[0]) }) {
			t.Error("no fold past the first chunk began while it was sent")
		}
		sent = append(sent, chunk)
		return nil
	}); err != nil {
		t.Fatalf("a fold whose every send succeeds answers %v", err)
	}
	if short.Load() {
		t.Errorf("fewer than %d folds ever ran at once", figureFoldWorkers)
	}
	if got := most.Load(); got != figureFoldWorkers {
		t.Errorf("at most %d folds ran at once, want %d", got, figureFoldWorkers)
	}
	if !slices.Equal(sent, []int{0, 1, 2, 3, 4}) {
		t.Errorf("the chunks were sent in the order %v, want each once in order", sent)
	}
	for index := range folds {
		if got := folds[index].Load(); got != 1 {
			t.Errorf("index %d was folded %d times, want once", index, got)
		}
	}

	// A send that fails, while the folds past its chunk wait for it.
	const many = 1 << 16
	failed := errors.New("the statement of chunk 1 fails")
	release := make(chan struct{})
	begun.Store(0)
	sent = nil
	err := foldInChunks(many, figureFoldWorkers, func(index int) {
		enter()
		if index >= 20 {
			<-release
		}
		folding.Add(-1)
	}, []int{10, 20, 30}, func(chunk int) error {
		sent = append(sent, chunk)
		if chunk == 1 {
			close(release)
			return failed
		}
		return nil
	})
	if !errors.Is(err, failed) {
		t.Errorf("a fold whose second send fails answers %v, want that failure", err)
	}
	if !slices.Equal(sent, []int{0, 1}) {
		t.Errorf("a fold whose second send fails sent the chunks %v, want the first two alone", sent)
	}
	if running := folding.Load(); running != 0 {
		t.Errorf("a fold whose send failed answered while %d folds ran", running)
	}
	if got := begun.Load(); got >= many {
		t.Errorf("a fold whose send failed went on to fold all %d indexes", got)
	}
	t.Logf("%d folds ran at once; once the second send failed, %d of %d indexes had been folded", most.Load(), begun.Load(), many)
}

// TestAStatementOfTheStoredFiguresHoldsAtMostItsBoundAsPgxEncodesIt: rows whose every field takes
// the most bytes its encoding can (the largest numbers and the farthest dates, whose binary
// encodings are as long as any, and long strings and figures, which the estimate counts byte for
// byte), and short rows, of which a statement holds thousands, are cut into statements of each of
// the four kinds (rangeUpserts, rangeDeletes, dayUpserts, dayDeletes) whose parameters, as pgx
// encodes them, take no more than the estimate (paramEstimate, statementChunks') and no more than
// statementBytes, holding every row once and in its order: each statement but the last as many
// rows as that estimate lets under the bound, or statementRows, and no fewer. A row larger than
// the bound alone is sent in a statement of its own, between the statements of the rows around it.
func TestAStatementOfTheStoredFiguresHoldsAtMostItsBoundAsPgxEncodesIt(t *testing.T) {
	long := func(index, size int) string { return fmt.Sprintf("%06d", index) + strings.Repeat("é", size/2) }
	figures := func(size int) []byte {
		filled := make([]byte, size)
		for index := range filled {
			filled[index] = byte(index)
		}
		return filled
	}
	far := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	scope := figureScope{build: long(0, 300), project: long(0, 200)}
	var ranges []rangeFigureRow
	for _, shape := range []struct {
		name                 string
		count, text, figures int
	}{{"rows whose fields take the most bytes", 400, 600, 8 << 10}, {"short rows", 12000, 0, 40}} {
		ranges = make([]rangeFigureRow, shape.count)
		goneRanges, days, goneDays := make([]rangeFigureKey, shape.count), make([]dayFigureRow, shape.count), make([]dayFigureKey, shape.count)
		repos := make([]string, shape.count)
		for index := range shape.count {
			repos[index] = long(index, shape.text)
			goneRanges[index] = rangeFigureKey{repo: repos[index], days: math.MaxInt32, end: far, eval: long(index, shape.text*3/2), deployRepo: long(index, shape.text)}
			goneDays[index] = dayFigureKey{repo: repos[index], day: far, eval: long(index, shape.text*3/2), deployRepo: long(index, shape.text)}
			ranges[index] = rangeFigureRow{rangeFigureKey: goneRanges[index], key: math.MinInt64, figures: figures(shape.figures), oldest: far}
			days[index] = dayFigureRow{dayFigureKey: goneDays[index], key: math.MinInt64, figures: figures(shape.figures), stamp: math.MaxInt64}
		}
		for _, kind := range []struct {
			name string
			// statements is the statements of the first n rows of the kind.
			statements func(n int) iter.Seq[[]any]
		}{
			{rangeUpsertKind, func(n int) iter.Seq[[]any] { return rangeUpserts(scope, ranges[:n]) }},
			{rangeDeleteKind, func(n int) iter.Seq[[]any] { return rangeDeletes(scope.project, goneRanges[:n]) }},
			{dayUpsertKind, func(n int) iter.Seq[[]any] { return dayUpserts(scope, days[:n]) }},
			{dayDeleteKind, func(n int) iter.Seq[[]any] { return dayDeletes(scope.project, goneDays[:n]) }},
		} {
			// Every row of the shape estimates alike, so the rows the bound lets into one statement
			// are what is left of it past a statement's own parameters, over a row's.
			estimateOf := func(n int) int {
				for args := range kind.statements(n) {
					return paramEstimate(t, args)
				}
				return 0
			}
			row := estimateOf(2) - estimateOf(1)
			fits := min((statementBytes-(estimateOf(1)-row))/row, statementRows)
			var held []string
			var sizes []int
			largest := 0
			for args := range kind.statements(shape.count) {
				encoded, estimated := encodedBytes(t, statementOIDs[kind.name], args), paramEstimate(t, args)
				if encoded > estimated {
					t.Errorf("%s, %q: a statement of %d rows encodes to %d bytes of parameters, past its estimate of %d", shape.name, kind.name, rowsIn(args), encoded, estimated)
				}
				if estimated > statementBytes {
					t.Errorf("%s, %q: a statement of %d rows takes an estimated %d bytes of parameters, past %d", shape.name, kind.name, rowsIn(args), estimated, statementBytes)
				}
				held, sizes, largest = append(held, args[1].([]string)...), append(sizes, rowsIn(args)), max(largest, encoded)
			}
			if !slices.Equal(held, repos) {
				t.Errorf("%s, %q: the statements take %d rows, other rows or in another order than the %d given", shape.name, kind.name, len(held), len(repos))
			}
			for index, size := range sizes {
				if size != fits && (index < len(sizes)-1 || size > fits) {
					t.Errorf("%s, %q: the statements hold %v rows, want %d in each but the last, as many as the estimate lets under the bound or statementRows",
						shape.name, kind.name, sizes, fits)
					break
				}
			}
			if len(sizes) < 2 {
				t.Errorf("%s, %q: %d rows went in %d statement, and the check needs them cut", shape.name, kind.name, shape.count, len(sizes))
			}
			t.Logf("%s, %q: %d rows in %d statements of %d rows, the largest %d bytes of parameters as pgx encodes them, of %d",
				shape.name, kind.name, shape.count, len(sizes), fits, largest, statementBytes)
		}
	}

	huge := ranges[1]
	huge.figures = figures(statementBytes + 1)
	var sizes []int
	var alone []byte
	for args := range rangeUpserts(scope, []rangeFigureRow{ranges[0], huge, ranges[2], ranges[3]}) {
		if sizes = append(sizes, rowsIn(args)); len(sizes) == 2 {
			alone = args[6].([][]byte)[0]
		}
	}
	if !slices.Equal(sizes, []int{1, 1, 2}) || len(alone) != len(huge.figures) {
		t.Errorf("a row of %d bytes of figures between three others went in statements of %v rows, the second's figures %d bytes; want 1, 1 and 2, the row alone in the second",
			len(huge.figures), sizes, len(alone))
	}
}

// TestARowPastTheStatementBoundIsWrittenAlone: a range row whose figures alone are past
// statementBytes is written by a statement of its own, between those of the rows around it, and
// stored bit for bit, as they are (writeFigures).
func TestARowPastTheStatementBoundIsWrittenAlone(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	statements := &figureStatements{}
	traced := storetest.Traced(t, r.database.Pool, statements, nil)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]rangeFigureRow, 3)
	for index := range rows {
		figures := make([]byte, 1000)
		if index == 1 {
			figures = make([]byte, statementBytes+1)
		}
		for k := range figures {
			figures[k] = byte(index + k)
		}
		rows[index] = rangeFigureRow{rangeFigureKey: rangeFigureKey{days: 7, end: day.AddDate(0, 0, index), eval: EvalID(r.project, "wide")}, key: 1, figures: figures, oldest: day}
	}
	tx, err := traced.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := writeFigures(ctx, tx, figureScope{build: testBuild, project: r.project}, figureWrites{ranges: rows}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("write three range rows, one past the bound of a statement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var sizes []int
	for _, statement := range statements.take() {
		sizes = append(sizes, rowsIn(statement.args))
	}
	if !slices.Equal(sizes, []int{1, 1, 1}) {
		t.Errorf("three rows, the second past the bound alone, went in statements of %v rows, want one each", sizes)
	}
	stored, err := r.database.Pool.Query(ctx, `select figures from eval_range_figures where project_key = $1 order by end_day`, r.project)
	if err != nil {
		t.Fatalf("read the range rows back: %v", err)
	}
	figures, err := pgx.CollectRows(stored, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the range rows back: %v", err)
	}
	if !slices.EqualFunc(figures, rows, func(read []byte, row rangeFigureRow) bool { return slices.Equal(read, row.figures) }) {
		t.Errorf("the three rows read back as %d rows, other figures than those written", len(figures))
	}
}

// TestARefoldSendsTheWholeFoldsRowsInKeyOrderOnceEachInBoundedStatements: over a settled history of
// thirty days whose stored figures went stale (every range row's figures garbled, every day row
// gone, and a range and a day row of a repository no row holds stored), a refold of every eval of
// the project (refoldFigures, its statements recorded on a traced pool) folds the ranges of several
// evals and of both cuts, the deploy repository's rows and the rows no deploy repository moves,
// while it writes them. Its range upserts take the rows the whole fold on one worker with no write
// answers (foldMovedDays), each once, in the byte order of their keys, each statement within one
// chunk of the ranges it reads (readFigurePlan), as rangeChunks cuts them at rangeChunkLeast; its
// gone ranges, day rows and gone days are that fold's too, in their order, after every range; the
// range rows and the day rows its upserts take, across their statements, are in the order
// Postgres's `collate "C"` gives their keys (rangeKeyOrder, dayFigureKeyOrder), which each
// statement takes them in as sent; no statement holds more than statementBytes of parameters as
// statementChunks estimates them, nor more than that estimate as pgx encodes them; and the stored
// figures it leaves are the fold of the rows that stand, bit for bit.
func TestARefoldSendsTheWholeFoldsRowsInKeyOrderOnceEachInBoundedStatements(t *testing.T) {
	r := newRefresherRig(t)
	deliveries := deliverytest.Seed(t, r.database.Pool, deliverytest.Spec{Days: 30})
	r.settle(t)
	ctx, project := t.Context(), deliveries.Project
	scope, folded, err := readFigureScope(ctx, r.database.Pool, r.build, project)
	if err != nil || !folded {
		t.Fatalf("read how far the figures of %s are folded: folded %t, %v", project, folded, err)
	}
	entries, err := readEntries(ctx, r.database.Pool, project, nil)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	byID := entriesByID(entries)
	gone, goneDay := EvalID(project, SlugReworkShare), measures.Midnight(deliveries.From).AddDate(0, 0, 3)
	r.exec(t, "garble every stored range row", `update eval_range_figures set figures = '\x00'::bytea where project_key = $1`, project)
	r.exec(t, "forget every stored day row", `delete from eval_day_figures where project_key = $1`, project)
	r.exec(t, "store a day row of a repository no row holds", `
		insert into eval_day_figures (project_key, repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp, built_by)
		values ($1, 'zz/gone', $2, $3, '', 0, '\x00', 0, 'an older build')`, project, goneDay, gone)
	r.exec(t, "store a range row of a repository no row holds", `
		insert into eval_range_figures (project_key, repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest_updated_at, built_by)
		values ($1, 'zz/gone', 7, $2::date + 1, $3, '', 0, '\x00', now(), 'an older build')`, project, goneDay, gone)
	spans := map[string]daySpan{}
	for evalID := range byID {
		spans[evalID] = daySpan{first: deliveries.From.AddDate(0, 0, -1), last: scope.through}
	}
	moved, err := movedFigureDays(ctx, r.database.Pool, scope, byID, spans)
	if err != nil {
		t.Fatalf("read the moved days of %s: %v", project, err)
	}
	whole, err := foldMovedDays(ctx, r.database.Pool, scope, byID, moved, 1)
	if err != nil {
		t.Fatalf("fold the figures of %s on one worker: %v", project, err)
	}
	whole.sortByKey()
	plan, err := readFigurePlan(ctx, r.database.Pool, scope, byID, moved)
	if err != nil {
		t.Fatalf("read the ranges of %s: %v", project, err)
	}
	// chunkOf is the chunk of the refold's ranges (rangeChunks) each range falls in, by its key.
	ends := rangeChunks(plan.ranges, rangeChunkLeast, statementRows)
	chunkOf, prefixes := map[rangeFigureKey]int{}, map[rangeFigureKey]bool{}
	from := 0
	for chunk, end := range ends {
		for _, target := range plan.ranges[from:end] {
			chunkOf[target.rangeFigureKey] = chunk
			prefixes[rangeFigureKey{repo: target.repo, days: target.days}] = true
		}
		from = end
	}

	statements := &figureStatements{}
	tx, err := storetest.Traced(t, r.database.Pool, statements, nil).Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := refoldFigures(ctx, tx, scope, byID, spans); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("refold the figures of %s: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sent := statements.take()
	kinds, upserts, largest := 0, 0, 0
	for index, statement := range sent {
		for kinds < len(figureStatementKinds) && figureStatementKinds[kinds] != statement.kind {
			kinds++
		}
		if kinds == len(figureStatementKinds) {
			t.Fatalf("statement %d, %q, came after a later kind of statement", index, statement.kind)
		}
		encoded, estimated := encodedBytes(t, statementOIDs[statement.kind], statement.args), paramEstimate(t, statement.args)
		if estimated > statementBytes || encoded > estimated {
			t.Errorf("statement %d, %q of %d rows, takes %d bytes of parameters as pgx encodes them and an estimated %d, where the bound is %d",
				index, statement.kind, rowsIn(statement.args), encoded, estimated, statementBytes)
		}
		largest = max(largest, encoded)
		if statement.kind != rangeUpsertKind {
			continue
		}
		upserts++
		rows := writesOf(sent[index : index+1]).ranges
		first := chunkOf[rows[0].rangeFigureKey]
		for _, row := range rows {
			if chunk, read := chunkOf[row.rangeFigureKey]; !read || chunk != first {
				t.Errorf("range upsert %d holds rows of chunk %d of the ranges and %+v, of chunk %d (a range read: %t)", index, first, row.rangeFigureKey, chunk, read)
				break
			}
		}
	}
	written := writesOf(sent)
	same := func(a, b any) bool { return reflect.DeepEqual(a, b) }
	for _, list := range []struct {
		name      string
		got, want int
		equal     bool
	}{
		{"range rows", len(written.ranges), len(whole.ranges), slices.EqualFunc(written.ranges, whole.ranges, func(a, b rangeFigureRow) bool { return same(a, b) })},
		{"gone ranges", len(written.goneRanges), len(whole.goneRanges), slices.EqualFunc(written.goneRanges, whole.goneRanges, func(a, b rangeFigureKey) bool { return same(a, b) })},
		{"day rows", len(written.days), len(whole.days), slices.EqualFunc(written.days, whole.days, func(a, b dayFigureRow) bool { return same(a, b) })},
		{"gone days", len(written.goneDays), len(whole.goneDays), slices.EqualFunc(written.goneDays, whole.goneDays, func(a, b dayFigureKey) bool { return same(a, b) })},
	} {
		if !list.equal {
			t.Errorf("the refold's statements take %d %s, the whole fold on one worker %d, or other rows, or in another order", list.got, list.name, list.want)
		}
	}
	for index := 1; index < len(written.ranges); index++ {
		if compareRangeKeys(written.ranges[index-1].rangeFigureKey, written.ranges[index].rangeFigureKey) >= 0 {
			t.Fatalf("range row %d of the refold's upserts, %+v, does not follow %+v in key order", index, written.ranges[index].rangeFigureKey, written.ranges[index-1].rangeFigureKey)
		}
	}
	rangeKeys := make([]rangeFigureKey, len(written.ranges))
	for index, row := range written.ranges {
		rangeKeys[index] = row.rangeFigureKey
	}
	if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(rangeKeys), rangeKeyOrder, rangeKeyArrays(rangeKeys)...)); at >= 0 {
		t.Errorf("range row %d of the refold's upserts, %s, is out of the byte order Postgres's collate \"C\" gives their keys", at, rangeKeyText(rangeKeys[at]))
	}
	dayKeys := make([]dayFigureKey, len(written.days))
	for index, row := range written.days {
		dayKeys[index] = row.dayFigureKey
	}
	if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(dayKeys), dayFigureKeyOrder, dayFigureKeyArrays(dayKeys)...)); at >= 0 {
		t.Errorf("day row %d of the refold's upserts, %s, is out of the byte order Postgres's collate \"C\" gives their keys", at, dayFigureKeyText(dayKeys[at]))
	}
	evals, deployRepos := map[string]bool{}, map[string]bool{}
	for _, row := range written.ranges {
		evals[row.eval], deployRepos[row.deployRepo] = true, true
	}
	if len(evals) < 2 || !deployRepos[""] || !deployRepos[deliveries.DeployRepo] || upserts < 2 || len(written.goneRanges) == 0 || len(written.goneDays) == 0 {
		t.Errorf("the refold upserted the ranges of %d evals and of the deploy repositories %v in %d statements, and deleted %d ranges and %d days; the check needs several evals, %q and %q, several statements and a range and a day deleted",
			len(evals), slices.Sorted(maps.Keys(deployRepos)), upserts, len(written.goneRanges), len(written.goneDays), "", deliveries.DeployRepo)
	}
	expectTruth(t, "a refold of every stale figure", r.storedFigures(t), r.truthFigures(t))
	t.Logf("the refold sent %d statements, %d of them range upserts of %d rows of %d evals, of %d ranges in %d chunks over %d (repository, length) prefixes, the largest holding %d bytes of parameters as pgx encodes them",
		len(sent), upserts, len(written.ranges), len(evals), len(plan.ranges), len(ends), len(prefixes), largest)
}

// TestAFailureInALaterChunkRollsTheWholeWriteBack: over a settled history of a logged eval whose
// readings fold into its project's own rows and into acme/web's, a write of a reading of acme/web
// on each of the 180 days before today folds the ranges those days moved (of each length and
// repository, those ending on each of the 182 days from the first of them through the last end
// day folded) in its own transaction while it writes them, chunk by chunk
// (figurePlan.foldAndWrite): the first chunk closes once it holds rangeChunkLeast ranges, at the
// end of one of the project's own lengths before its last, and the upsert of a later chunk fails:
// a trigger of the test refuses every range row of 85 days, the last length of the project's own
// rows, once the upserts of the first chunk, one of them holding ranges of several shorter
// lengths, have written theirs. The write answers that error, sends nothing after it, and stores
// nothing: no reading, no day row, no change row, and none of the stored figures the chunks before
// the failure wrote.
func TestAFailureInALaterChunkRollsTheWholeWriteBack(t *testing.T) {
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	statements := &figureStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events)
	r.exec(t, "make a refusal of a range row", `
		create function evals_test_refuse_range() returns trigger language plpgsql as $$
		begin
			raise exception 'evals test: the range of % days ending % is refused', new.days, new.end_day;
		end $$`)
	r.exec(t, "refuse every range row of 85 days", `
		create trigger evals_test_refuse_range before insert on eval_range_figures
		for each row when (new.days = 85) execute function evals_test_refuse_range()`)
	stored := func() (int, int) {
		return r.count(t, `select count(*) from eval_readings`),
			r.count(t, `select (select count(*) from delivery_changes_pending) + (select count(*) from delivery_changes)`)
	}
	before := r.figureState(t)
	readings, changes := stored()
	var rows []json.RawMessage
	for back := 1; back <= 180; back++ {
		rows = append(rows, row(t, map[string]any{"series": "minutes", "row_kind": "review", "row_key": fmt.Sprintf("refused-%d", back), "value": 33,
			"measured_at": refresherToday.AddDate(0, 0, -back).Add(3 * time.Hour), "attributes": map[string]any{"repo": "acme/web"}}))
	}
	_, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "review-latency", Caller: alice, BuiltBy: r.build, Rows: rows})
	if err == nil || !strings.Contains(err.Error(), "is refused") || !strings.Contains(err.Error(), "write the stored range figures") {
		t.Fatalf("a write whose later range upsert is refused answers %v, want the refusal", err)
	}
	sent := statements.take()
	failing := slices.IndexFunc(sent, func(statement figureStatement) bool { return statement.err != nil })
	wrote, mixed := 0, false
	for index, statement := range sent[:max(failing, 0)] {
		if statement.kind != rangeUpsertKind || slices.Contains(statement.args[2].([]int32), 85) {
			t.Errorf("statement %d before the refused one is %q of the lengths %v", index, statement.kind, statement.args[2])
		}
		if statement.kind == rangeUpsertKind {
			lengths := statement.args[2].([]int32)
			mixed = mixed || slices.ContainsFunc(lengths, func(n int32) bool { return n != lengths[0] })
		}
		wrote += int(statement.rows)
	}
	if failing < 1 || failing != len(sent)-1 || sent[failing].kind != rangeUpsertKind || wrote == 0 || !mixed {
		t.Fatalf("the write sent %d statements, the refused one at %d, and the earlier ones wrote %d rows, one of them ranges of several lengths: %t; want range upserts that wrote rows, one of several lengths, then the refused one, last",
			len(sent), failing, wrote, mixed)
	}
	after := r.figureState(t)
	if written := writtenFigures(before.figures, after.figures); len(written) != 0 {
		t.Errorf("the refused write left %d stored figures written, first %v", len(written), written[:min(len(written), 5)])
	}
	if !reflect.DeepEqual(before.days, after.days) {
		t.Errorf("the refused write left the day rows moved: %d rows before, %d after", len(before.days), len(after.days))
	}
	if gotReadings, gotChanges := stored(); gotReadings != readings || gotChanges != changes {
		t.Errorf("the refused write left %d readings and %d change rows, where %d and %d stood before it", gotReadings, gotChanges, readings, changes)
	}
	t.Logf("the write's %d range upserts before the refused one wrote %d rows, none of which stands", failing, wrote)
}

// TestAWriteOfAFewRangesOfEveryLengthSendsOneRangeUpsert: over a settled history of a logged eval
// whose readings fold into its project's own rows and into acme/web's, a reading's write folds the
// ranges its day moved, a few of each length of FigureLengths in each of the two repositories, far
// fewer than rangeChunkLeast, as one chunk, and upserts them in one statement
// (figurePlan.foldAndWrite), not in one a (repository, length) prefix.
func TestAWriteOfAFewRangesOfEveryLengthSendsOneRangeUpsert(t *testing.T) {
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	statements := &figureStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events)
	if _, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: "review-latency", Caller: alice, BuiltBy: r.build,
		Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "review", "row_key": "few", "value": 33,
			"measured_at": refresherToday.AddDate(0, 0, -2).Add(3 * time.Hour), "attributes": map[string]any{"repo": "acme/web"}})}}); err != nil {
		t.Fatalf("write a reading of review-latency: %v", err)
	}
	upserts, ranges := 0, 0
	lengths, repos := map[int32]bool{}, map[string]bool{}
	for _, statement := range statements.take() {
		if statement.kind != rangeUpsertKind {
			continue
		}
		upserts++
		ranges += rowsIn(statement.args)
		for index, repo := range statement.args[1].([]string) {
			repos[repo], lengths[statement.args[2].([]int32)[index]] = true, true
		}
	}
	if upserts != 1 || len(lengths) != len(FigureLengths) || !repos[""] || !repos["acme/web"] {
		t.Errorf("a reading's write upserted %d ranges of the lengths %v and the repositories %q in %d statements, want ranges of all %d lengths and of both repositories in one",
			ranges, slices.Sorted(maps.Keys(lengths)), slices.Sorted(maps.Keys(repos)), upserts, len(FigureLengths))
	}
	t.Logf("a reading's write upserted %d ranges of %d lengths and %d repositories in %d statements", ranges, len(lengths), len(repos), upserts)
}

// figureFoldTimingEnv names the variable that runs TestTheFoldOfOneCutOfSixEvalsIsTimed: set to 1,
// it times the fold; unset, as in CI, the test is skipped.
const figureFoldTimingEnv = "EVALS_FIGURE_FOLD_TIMING"

// timedFoldInput is one cut built in memory, as a production rule's rewrite folds it: the deploy
// repository's rows of the six delivery evals that rewrite moves, over the 200 days through
// 2026-11-01, each lead time and the production deploy holding 10 to 60 samples a day, the deploys
// counted and the runs shared, as each eval's group of rows (newFigureGroup); the key of each
// group; every day, which the rewrite moves; and the end day the ranges are folded through.
func timedFoldInput(t *testing.T) ([]*figureGroup, []figureGroupKey, []time.Time, time.Time) {
	t.Helper()
	const project, deployRepo, span = "BENCH", "acme/deploy", 200
	last := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	random := rand.New(rand.NewPCG(2916, 8))
	minutes := func() []float64 {
		samples := make([]float64, 10+random.IntN(51))
		for index := range samples {
			samples[index] = math.Round(math.Exp(3+random.NormFloat64()*1.2)*10) / 10
		}
		return samples
	}
	days := make([]time.Time, span)
	for index := range days {
		days[index] = last.AddDate(0, 0, index+1-span)
	}
	var groups []*figureGroup
	var keys []figureGroupKey
	for _, slug := range []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugMergeToProduction, SlugFirstCommitToProduction, SlugOpenToProduction, SlugProductionDeploy} {
		catalogued := entryOf(t, slug)
		definition := catalogued.definition
		definition.EvalID, definition.ProjectKey = EvalID(project, slug), project
		entry := Entry{Definition: definition, Series: catalogued.series}
		rows := make([]WindowRow, 0, span)
		for _, day := range days {
			var part measures.DayPart
			switch slug {
			case SlugDeployFrequency:
				deploys := random.IntN(9)
				part = measures.CountPart(day, measures.SeriesDeploys, deploys, map[string]float64{measures.CountWithPRs: float64(random.IntN(deploys + 1))})
			case SlugDeployRunSuccess:
				concluded := 1 + random.IntN(12)
				part = measures.RatioPart(day, measures.SeriesRuns, random.IntN(concluded+1), concluded, map[string]float64{measures.CountCancelled: float64(random.IntN(3))})
			case SlugProductionDeploy:
				samples := minutes()
				failure := random.IntN(3)
				part = measures.SamplesPart(day, measures.SeriesMinutes, samples, map[string]float64{
					measures.CountSuccess: float64(len(samples) - failure), measures.CountFailure: float64(failure), measures.CountCancelled: float64(random.IntN(2)),
				})
			default:
				part = measures.SamplesPart(day, measures.SeriesMinutes, minutes(), nil)
			}
			rows = append(rows, WindowRow{Range: RangeWindow, EvalID: definition.EvalID, Part: part, UpdatedAt: day.Add(time.Hour)})
		}
		groups = append(groups, newFigureGroup(entry, foldKeyOf(entry), rows))
		keys = append(keys, figureGroupKey{eval: definition.EvalID, deployRepo: deployRepo})
	}
	return groups, keys, days, last.AddDate(0, 0, 1+FigureLead)
}

// foldTimedCut folds every range and day of groups (each of the cut and eval its key names) that
// holds one of days, up to through, on workers goroutines at once, as a write folds them
// (figurePlan.foldAndWrite, each chunk sent to no statement), and answers how many it folded.
func foldTimedCut(t *testing.T, groups []*figureGroup, keys []figureGroupKey, days []time.Time, through time.Time, workers int) int {
	t.Helper()
	var plan figurePlan
	for index, group := range groups {
		plan.add(group, keys[index], days, through)
	}
	plan.sortByKey()
	folds := len(plan.ranges) + len(plan.days)
	if err := foldInChunks(folds, workers, plan.fold, rangeChunks(plan.ranges, rangeChunkLeast, statementRows), func(int) error { return nil }); err != nil {
		t.Fatalf("fold the timed cut: %v", err)
	}
	return folds
}

// TestTheFoldOfOneCutOfSixEvalsIsTimed logs the wall and CPU time, the bytes allocated and the
// collections of a fold of one cut of six delivery evals over 200 days (timedFoldInput, folded by
// foldTimedCut on as many workers as a write folds on), seven times after one unmeasured fold: the
// in-memory part of what the farm measured in a production rule's rewrite. It is skipped unless
// figureFoldTimingEnv is 1.
func TestTheFoldOfOneCutOfSixEvalsIsTimed(t *testing.T) {
	if os.Getenv(figureFoldTimingEnv) != "1" {
		t.Skipf("set %s=1 to time the fold of one cut of six evals over 200 days", figureFoldTimingEnv)
	}
	groups, keys, days, through := timedFoldInput(t)
	workers := min(runtime.GOMAXPROCS(0), figureFoldWorkers)
	folds := foldTimedCut(t, groups, keys, days, through, workers)
	var walls, cpus []time.Duration
	var allocated []uint64
	var collections []uint32
	for range 7 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		cpu, start := cputest.Process(), time.Now()
		foldTimedCut(t, groups, keys, days, through, workers)
		wall, used := time.Since(start), cputest.Process()-cpu
		runtime.ReadMemStats(&after)
		walls, cpus = append(walls, wall), append(cpus, used)
		allocated, collections = append(allocated, after.TotalAlloc-before.TotalAlloc), append(collections, after.NumGC-before.NumGC)
	}
	t.Logf("a fold of %d ranges and days of %d evals on %d workers (GOMAXPROCS %d): wall median %v, min %v; CPU median %v, min %v; allocated %v bytes, %v collections; walls %v, CPU %v",
		folds, len(groups), workers, runtime.GOMAXPROCS(0), median(walls), slices.Min(walls), median(cpus), slices.Min(cpus), allocated, collections, walls, cpus)
}
