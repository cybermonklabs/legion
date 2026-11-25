package evals

import (
	"cmp"
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
	"strconv"
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
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The checks of the stored figures (figures.go): after each change, the stored rows are the
// fold of the day rows that stand, as a read of those rows folds them (truthFigures, which reads
// them through DayWindowIn and folds each range with FoldStatistics over its rows in the folds'
// order, merging and sorting every quantile's samples), and the rows the change wrote (by the
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

// rangeLine names a stored range row: `range <project> <cut> <deploy repo> <days> <end> <eval>`.
func rangeLine(project, repo, deployRepo string, days int, end time.Time, evalID string) string {
	return fmt.Sprintf("range %s %q %q %d %s %s", project, repo, deployRepo, days, end.Format(time.DateOnly), evalID)
}

// dayFigureLine names a stored day row: `day <project> <cut> <deploy repo> <day> <eval>`.
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

// publishedDefaults is the default repository each project publishes (eval_days_defaults), by
// project: a project that publishes none is in none.
func (r *refresherRig) publishedDefaults(t *testing.T) map[string]string {
	t.Helper()
	published, err := readPublishedDefaults(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

// cutOfRow is the repository of the cut of project's stored figures a listed day row of owner
// (project itself, or an owner key it reads) of repo folds into, as a read of that one repository
// reads it (DayWindowIn's repoCut), defaults being each project's published default: the row's
// repository, and for a row of the project's own stored under legionfacts.DefaultRepo the
// repository it publishes as its default, false where it publishes none or the row is an owner
// key's.
func cutOfRow(owner, project, repo string, defaults map[string]string) (string, bool) {
	if repo != legionfacts.DefaultRepo {
		return repo, true
	}
	published := defaults[project]
	return published, owner == project && published != ""
}

// figuresBuilt is the last end day every project whose figures are built is built through
// (figuresThrough), the marker a read of the stored figures reads.
func (r *refresherRig) figuresBuilt(t *testing.T) map[string]time.Time {
	t.Helper()
	projects, err := r.registry.Projects(t.Context())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	cuts, err := figuresThrough(t.Context(), r.database.Pool, projects, "")
	if err != nil {
		t.Fatalf("read how far the figures are built: %v", err)
	}
	built := make(map[string]time.Time, len(cuts))
	for project, cut := range cuts {
		built[project] = cut.through
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
// read of the rows folds them: for each project folded through an end day, each cut of its listed
// rows, its own and those of the owner keys it reads (eval_project_owners, owners.go): its own rows
// (repo ""), and each repository's, which a read naming that one repository reads, the rows of the
// repository and, of its published default, its own rows stored under legionfacts.DefaultRepo; of
// each eval of each deploy repository read through DayWindowIn, with no repository named or that
// one (the evals keyed by the deploy repository read with that repository, every other eval with
// none), in the folds' order (by day, then by the key each row is stored under, its repository and
// its series), and every range of FigureLengths ending on or before that day that holds one of them
// folded by FoldStatistics, and every day before it that holds one folded alone over its headline
// series. No list is sorted before the fold, so every quantile merges and sorts its samples.
func (r *refresherRig) truthFigures(t *testing.T) map[string]figureRow {
	t.Helper()
	ctx := t.Context()
	truth := map[string]figureRow{}
	for project, through := range r.figuresFolded(t) {
		page, err := r.registry.Current(ctx, []string{project}, "", 1000, 0)
		if err != nil {
			t.Fatalf("read the evals of %s: %v", project, err)
		}
		rows, err := r.database.Pool.Query(ctx, `
			select distinct d.repo, d.deploy_repo from eval_project_owners o join eval_days d on d.project_key = o.owner
			where o.project_key = $1 and d.listed and d.repo <> $2
			union
			select p.repo, d.deploy_repo from eval_days d join eval_days_defaults p on p.project_key = d.project_key
			where d.project_key = $1 and d.listed and d.repo = $2`, project, legionfacts.DefaultRepo)
		if err != nil {
			t.Fatalf("read the cuts of %s: %v", project, err)
		}
		cuts, err := pgx.CollectRows(rows, scanFigureCut)
		if err != nil {
			t.Fatalf("read the cuts of %s: %v", project, err)
		}
		for _, cut := range cuts {
			repo, deployRepo := cut.repo, cut.deployRepo
			var named []string
			if repo != "" {
				named = []string{repo}
			}
			var entries []Entry
			for _, entry := range page.Items {
				if KeyedByDeployRepo(entry.Definition) == (deployRepo != "") {
					entries = append(entries, entry)
				}
			}
			read, err := DayWindowIn(ctx, r.database.Pool, []string{project}, entries, deployRepo, named, Range{From: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), To: through}, nil)
			if err != nil {
				t.Fatalf("read the day rows of %s %q of %q: %v", project, repo, deployRepo, err)
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
				slices.SortFunc(held, func(a, b WindowRow) int {
					return cmp.Or(a.Part.Day.Compare(b.Part.Day), strings.Compare(a.owner, b.owner), strings.Compare(a.repo, b.repo),
						strings.Compare(a.Part.Series, b.Part.Series))
				})
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
						truth[rangeLine(project, repo, deployRepo, n, end, entry.Definition.EvalID)] = figureRow{
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
						stamped = append(stamped, stampedRow{series: row.Part.Series, updatedAt: row.UpdatedAt, via: row.Part.ViaRepository})
					}
					truth[dayFigureLine(project, repo, deployRepo, day, entry.Definition.EvalID)] = figureRow{
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
// figures, with through how far each project's figures are folded, readers the projects that read
// each owner key (ownerReaders) and defaults each project's published default: for each listed row
// that appeared, went or had its updated_at moved, on day D of its eval and deploy repository, as
// each project that reads it reads it (asReaders), in the cut of the row's repository the project
// reads it in (cutOfRow: a row stored under legionfacts.DefaultRepo the cut of the project's
// published default), every range of FigureLengths whose span holds D and that ends no later than
// through, and D's own day row; and those day rows alone, each of which a write of its moved rows
// must write, since its stamp moved.
func figureReach(before, after map[string]DayRow, through map[string]time.Time, readers map[string][]string, defaults map[string]string) (map[string]bool, []string) {
	reach := map[string]bool{}
	var days []string
	note := func(moved DayRow) {
		for _, row := range asReaders(moved, readers) {
			limit, built := through[row.ProjectKey]
			cut, held := cutOfRow(moved.ProjectKey, row.ProjectKey, row.Repo, defaults)
			if !row.Listed || !held || !built || !row.Day.Before(limit) {
				continue
			}
			for _, n := range FigureLengths {
				for end := row.Day.AddDate(0, 0, 1); !end.After(row.Day.AddDate(0, 0, n)) && !end.After(limit); end = end.AddDate(0, 0, 1) {
					reach[rangeLine(row.ProjectKey, cut, row.DeployRepo, n, end, row.EvalID)] = true
				}
			}
			line := dayFigureLine(row.ProjectKey, cut, row.DeployRepo, row.Day, row.EvalID)
			if !reach[line] {
				reach[line] = true
				days = append(days, line)
			}
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
	reach, days := figureReach(before.days, after.days, r.figuresFolded(t), r.ownerReaders(t), r.publishedDefaults(t))
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

// recordDeletes makes every deletion of a listed day row record, in the test database's own table
// evals_test_deleted, the transaction that deleted it beside the row's day key, as xmin names a
// transaction (its 32 bits): a day a project reads holds rows of several owners (owners.go), each
// written in its own owner's transaction, so the one that last moved a day's rows can be one that
// deleted a row of it and wrote none that stands (expectFoldedInTx).
func recordDeletes(t *testing.T, r rig) {
	t.Helper()
	for _, statement := range []string{
		`create table evals_test_deleted (txid text not null, project_key text not null, repo text not null, deploy_repo text not null,
			day date not null, eval_id text not null)`,
		`create function evals_test_record_delete() returns trigger language plpgsql as $$
		begin
			if old.listed then
				insert into evals_test_deleted values ((txid_current() % 4294967296)::text, old.project_key, old.repo, old.deploy_repo, old.day, old.eval_id);
			end if;
			return null;
		end $$`,
		`create trigger evals_test_record_delete after delete on eval_days for each row execute function evals_test_record_delete()`,
	} {
		if _, err := r.database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("record the transactions that delete day rows: %v", err)
		}
	}
}

// expectFoldedInTx checks that each day whose listed rows moved from before to after had its
// stored row of each cut the rows reach written by a transaction that wrote one of those rows or
// deleted one since the last check (recordDeletes, which the test makes first), as the projects
// that read them read them (asReaders, cutOfRow): the day rows' own, which folds the figures of
// the days it moves (figures.go), and not a later one. It answers after.
func (r *refresherRig) expectFoldedInTx(t *testing.T, what string, before, after figureState) figureState {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		with taken as (delete from evals_test_deleted returning txid, project_key, repo, deploy_repo, day, eval_id)
		select xmin::text, project_key, repo, deploy_repo, day, eval_id from eval_days where listed
		union all
		select txid, project_key, repo, deploy_repo, day, eval_id from taken`)
	if err != nil {
		t.Fatalf("read the day rows' transactions: %v", err)
	}
	readers, defaults := r.ownerReaders(t), r.publishedDefaults(t)
	wrote := map[string][]string{}
	var xmin string
	var stored DayRow
	if _, err := pgx.ForEachRow(rows, []any{&xmin, &stored.ProjectKey, &stored.Repo, &stored.DeployRepo, &stored.Day, &stored.EvalID}, func() error {
		for _, row := range asReaders(stored, readers) {
			if cut, held := cutOfRow(stored.ProjectKey, row.ProjectKey, row.Repo, defaults); held {
				line := dayFigureLine(row.ProjectKey, cut, row.DeployRepo, row.Day.UTC(), row.EvalID)
				wrote[line] = append(wrote[line], xmin)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the day rows' transactions: %v", err)
	}
	_, days := figureReach(before.days, after.days, r.figuresFolded(t), readers, defaults)
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
// (figuresThrough), so a read of an offered range folds the stored figures.
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
// release whose folds agree then rebuilds every figure aside and its flip writes none, the stored
// figures read as built throughout; and the rebuild of one that meets figures another build folded
// otherwise rewrites exactly those, in its flip.
func TestEachChangeRewritesExactlyTheFiguresItsMovedDaysReach(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	recordDeletes(t, r.rig)
	state := r.figureState(t)
	expectTruth(t, "the first whole build", state.figures, r.truthFigures(t))
	r.expectBuilt(t, "the first whole build", refresherToday)
	t.Logf("the first whole build stored %d figures rows", len(state.figures))

	// A pull request of another repository than the deploy repository's that names an issue of the
	// main project (namesIssueOf), which it counts for through that issue, merged a day earlier: one
	// that names none counts for its repository's owner key (owners.go), which a project reads only
	// where it publishes that repository as its default.
	var moved delivery.DeliveryPullRequest
	for _, pr := range deliveries.PullRequests {
		if pr.Repo != deliveries.DeployRepo && namesIssueOf(deliveries, pr) && pr.MergedAt != nil && measures.Midnight(*pr.MergedAt).After(deliveries.From) && moved.Number == 0 {
			moved = pr
		}
	}
	if moved.Number == 0 {
		t.Fatal("the history holds no pull request of another repository that names an issue of the main project merged after its first day")
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

	// A release whose folds agree with the build before it: its first pass still reads the stored
	// figures as built, and its rebuild, which folds them all aside, writes none.
	first := r.build
	r.release("another-build")
	r.pass(t)
	r.expectBuilt(t, "a release's first pass", refresherToday.AddDate(0, 0, 1))
	r.settle(t)
	after := r.figureState(t)
	expectTruth(t, "a release's rebuild", after.figures, r.truthFigures(t))
	if written := writtenFigures(state.figures, after.figures); len(written) != 0 {
		t.Errorf("the rebuild of a release whose folds agree wrote %d stored rows, first %v", len(written), written[:min(len(written), 5)])
	}
	state = after

	// Figures another build folded otherwise (the stored rows of one eval, as a build of other
	// folds leaves them): the next release's rebuild rewrites exactly those, in its flip.
	other := EvalID(deliveries.Project, SlugDeployFrequency)
	r.exec(t, "fold one eval's figures otherwise", `
		update eval_range_figures set figures = '\x00'::bytea, built_by = 'other-folds' where eval_id = $1`, other)
	state = r.figureState(t)
	r.release("third-build")
	r.settle(t)
	after = r.figureState(t)
	expectTruth(t, "a release's rebuild over figures another build folded otherwise", after.figures, r.truthFigures(t))
	for _, line := range writtenFigures(state.figures, after.figures) {
		if !strings.HasPrefix(line, "range ") || !strings.HasSuffix(line, " "+other) {
			t.Errorf("a release's rebuild over %s's figures folded otherwise wrote %s too", other, line)
		}
	}
	for line, row := range after.figures {
		if strings.HasPrefix(line, "range ") && strings.HasSuffix(line, " "+other) && row.builtBy != r.build {
			t.Errorf("the rebuild left %s recorded as %s's fold", line, row.builtBy)
		}
	}
	t.Logf("after %s and %s, the third build's rebuild rewrote the %d stored rows another build had folded otherwise",
		first, "another-build", len(writtenFigures(state.figures, after.figures)))
}

// TestADayRowABuildWithNoFiguresWritesIsFoldedByTheNextPass: a build that holds the day rows and
// not the stored figures writes day rows and no figures, and its refresher's pass reads the
// refresher's own change cursor past the eval_days changes its writes log; the next pass of a
// build with the figures reads those changes from the figures' own cursor and folds the figures of
// the days they moved, and only those.
func TestADayRowABuildWithNoFiguresWritesIsFoldedByTheNextPass(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
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
	if reach, days := figureReach(state.days, stale.days, r.figuresFolded(t), r.ownerReaders(t), r.publishedDefaults(t)); len(days) == 0 {
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

// TestAWriteFoldsEachReaderOverEverySpanItsKeysWrote: one write of the day rows of several keys
// (writeProjectDays) folds a project's stored figures, in its own transaction, over every span its
// own write and the writes of the owner keys it reads name, each span as it was written. The
// write moves a project's own row of one day and a row two days later of an owner key the project
// reads: the project's stored day figures of both days are then the fold of the rows that stand;
// and those of the day between, whose row a build that writes no figures moved before the write,
// stand as they stood, since no write named that day.
func TestAWriteFoldsEachReaderOverEverySpanItsKeysWrote(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.settle(t)
	ctx := t.Context()
	stored := r.dayRows(t)
	keys := slices.Sorted(maps.Keys(stored))
	// folded is key's listed all-repositories delivery rows of day, the rows its figures fold.
	folded := func(key string, day time.Time) []DayRow {
		var rows []DayRow
		for _, line := range keys {
			row := stored[line]
			if row.ProjectKey == key && row.Day.Equal(day) && row.Listed && row.Repo == "" && slices.Contains(deliveryDaySlugs, slugOf(key, row.EvalID)) {
				rows = append(rows, row)
			}
		}
		return rows
	}
	readers := r.ownerReaders(t)
	var owner, project string
	var first, between, last time.Time
search:
	for _, key := range slices.Sorted(maps.Keys(readers)) {
		for _, reader := range readers[key] {
			for day := refresherToday; !day.Before(refresherToday.AddDate(0, 0, -10)); day = day.AddDate(0, 0, -1) {
				if len(folded(key, day)) > 0 && len(folded(reader, day.AddDate(0, 0, -1))) > 0 && len(folded(reader, day.AddDate(0, 0, -2))) > 0 {
					owner, project, first, between, last = key, reader, day.AddDate(0, 0, -2), day.AddDate(0, 0, -1), day
					break search
				}
			}
		}
	}
	if owner == "" {
		t.Fatal("no project reads an owner key holding a row two days after a day of the project's own rows, with a day of them between")
	}
	// dayOf is key's stored rows and items of the delivery family on day, as a write of the day
	// sends them again, with the value of one of the rows its figures fold moved, which it answers.
	dayOf := func(key string, day time.Time) (ProjectDay, DayRow) {
		written := ProjectDay{Project: key, Day: day}
		for _, line := range keys {
			if row := stored[line]; row.ProjectKey == key && row.Day.Equal(day) && slices.Contains(deliveryDaySlugs, slugOf(key, row.EvalID)) {
				written.Rows = append(written.Rows, row)
			}
		}
		moved := folded(key, day)[0]
		for i := range written.Rows {
			if rowKey(written.Rows[i]) == rowKey(moved) {
				written.Rows[i].N++
			}
		}
		rows, err := r.database.Pool.Query(ctx, `
			select family, deploy_repo, row_key, repo, shipped_by from eval_day_members where project_key = $1 and day = $2 and family = any($3)
		`, key, day, deliveryDayFamilies)
		if err != nil {
			t.Fatalf("read the items of %s on %s: %v", key, day.Format(time.DateOnly), err)
		}
		var member Member
		if _, err := pgx.ForEachRow(rows, []any{&member.Family, &member.DeployRepo, &member.RowKey, &member.Repo, &member.ShippedBy}, func() error {
			written.Members = append(written.Members, member)
			return nil
		}); err != nil {
			t.Fatalf("read the items of %s on %s: %v", key, day.Format(time.DateOnly), err)
		}
		return written, moved
	}
	own, ownMoved := dayOf(project, first)
	owned, ownedMoved := dayOf(owner, last)
	gap := folded(project, between)[0]
	r.exec(t, "move a row of the day between as a build with no figures does", `
		update eval_days set n = n + 1, updated_at = now()
		where project_key = $1 and eval_id = $2 and writer = $3 and deploy_repo = $4 and series = $5 and repo = '' and day = $6
	`, project, gap.EvalID, writerComputation, gap.DeployRepo, gap.Series, between)
	before := r.storedFigures(t)
	if err := r.registry.writeProjectDays(ctx, deliveryFamily, []ProjectDay{own, owned}, r.build, true); err != nil {
		t.Fatalf("write %s's day %s and %s's day %s: %v", project, first.Format(time.DateOnly), owner, last.Format(time.DateOnly), err)
	}
	after, truth := r.storedFigures(t), r.truthFigures(t)
	for _, line := range []string{
		dayFigureLine(project, "", ownMoved.DeployRepo, first, ownMoved.EvalID),
		dayFigureLine(project, "", ownedMoved.DeployRepo, last, asReader(owner, project, ownedMoved.EvalID)),
	} {
		if row, held := after[line]; !held || !row.same(truth[line]) || before[line].same(truth[line]) {
			t.Errorf("the stored row of %s is held %t and the fold of the rows that stand %t after the write, and was before it %t; want it folded by the write",
				line, held, row.same(truth[line]), before[line].same(truth[line]))
		}
	}
	gapLine := dayFigureLine(project, "", gap.DeployRepo, between, gap.EvalID)
	if was, held := before[gapLine]; !held || was.same(truth[gapLine]) || after[gapLine].xmin != was.xmin {
		t.Errorf("the stored row of %s, of the day between that no write named, is held %t and stale %t before the write, written by %s and then by %s; want it left as it stood",
			gapLine, held, !was.same(truth[gapLine]), was.xmin, after[gapLine].xmin)
	}
	t.Logf("%s reads %s: one write of %s's day %s and %s's day %s folded both, and left %s's day %s stale",
		project, owner, project, first.Format(time.DateOnly), owner, last.Format(time.DateOnly), project, between.Format(time.DateOnly))
}

// listValues is how the eval list's read states each of project's evals over window and compare
// (nil for none) of repos (api's readEvalValues): from the stored figures of the cut repos names
// (none or one repository) where FiguresOffered offers the read and they answer the eval
// (StoredFigures.SeriesValues), else from the day rows of repos (DayWindowIn, SeriesValuesOf); and
// beside it the fold of those day rows alone, which the read must answer. Each eval's values are
// encoded as the list route sends them, by eval id. It answers which evals the stored figures
// answered too. It reads the cut's state over window and compare (readListState) and makes the one
// read of it (listState.values); a check of many reads of one cut reads the state once for all of
// them (expectReads).
func (r *refresherRig) listValues(t *testing.T, project string, repos []string, window Range, compare *Range) (stated, folded map[string]string, fromFigures map[string]bool) {
	t.Helper()
	ranges := []Range{window}
	if compare != nil {
		ranges = append(ranges, *compare)
	}
	return r.readListState(t, project, repos, ranges...).values(t, window, compare)
}

// listState is what the eval list's read of project's cut repos (none or one repository) folds,
// read once for any number of reads of the cut (readListState): the project's evals, the deploy
// repository their rows stand recorded under (DeployRepoIn), the first day of each series
// (FirstDaysIn), and, by eval, the day rows of repos over the span of every range the reads name,
// from the earliest range's first day to the latest range's end, as one DayWindowIn read with no
// compare range answers them (each a RangeWindow row). A check of every read of a first paint
// (expectReads over firstPaintReads, 216 reads of each cut) reads it once for each cut. When it was
// read again for each read, those DayWindowIn statements kept each first-paint test waiting
// 15 minutes on CI.
type listState struct {
	q                   delivery.Querier
	project, deployRepo string
	repos               []string
	entries             []Entry
	firsts              map[string]map[string]time.Time
	rows                map[string][]WindowRow
}

// readListState reads project's listState of repos for reads whose windows and compare ranges are
// ranges (at least one).
func (r *refresherRig) readListState(t *testing.T, project string, repos []string, ranges ...Range) listState {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	state := listState{q: pool, project: project, repos: repos, rows: map[string][]WindowRow{}}
	page, err := r.registry.Current(ctx, []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read %s's evals: %v", project, err)
	}
	state.entries = page.Items
	if state.deployRepo, err = DeployRepoIn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if state.firsts, err = FirstDaysIn(ctx, pool, state.entries, state.deployRepo); err != nil {
		t.Fatalf("read the first days of %s's evals: %v", project, err)
	}
	span := ranges[0]
	for _, one := range ranges[1:] {
		if one.From.Before(span.From) {
			span.From = one.From
		}
		if one.To.After(span.To) {
			span.To = one.To
		}
	}
	rows, err := DayWindowIn(ctx, pool, []string{project}, state.entries, state.deployRepo, repos, span, nil)
	if err != nil {
		t.Fatalf("read %s's day rows of %v over %s: %v", project, repos, daysText(span), err)
	}
	for _, row := range rows {
		state.rows[row.EvalID] = append(state.rows[row.EvalID], row)
	}
	return state
}

// values is listValues' answer to one read of s's cut over window and compare (nil for none), each
// within the span s read. Of each eval's rows it folds those whose day window holds (dayIn), as
// RangeWindow rows, and copies of those whose day compare holds, as RangeCompare rows: the rows a
// DayWindowIn read of window and compare answers, which answers a day of both ranges twice, once for
// each, since a compare range may overlap its window. Each read folds a slice of its own, since
// SeriesValuesOf sorts its rows in place. Where FiguresOffered offers the read, it reads the stored
// figures of the read (ReadStoredFiguresIn).
func (s listState) values(t *testing.T, window Range, compare *Range) (stated, folded map[string]string, fromFigures map[string]bool) {
	t.Helper()
	var stored StoredFigures
	offered := FiguresOffered(s.repos, window, compare)
	if offered {
		var err error
		if stored, err = ReadStoredFiguresIn(t.Context(), s.q, []string{s.project}, s.entries, s.deployRepo, s.repos, window, compare); err != nil {
			t.Fatalf("read %s's stored figures: %v", s.project, err)
		}
	}
	encode := func(values []SeriesValues) string {
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatalf("encode the values of %s's evals: %v", s.project, err)
		}
		return string(encoded)
	}
	stated, folded, fromFigures = map[string]string{}, map[string]string{}, map[string]bool{}
	for _, entry := range s.entries {
		id := entry.Definition.EvalID
		var rows []WindowRow
		for _, row := range s.rows[id] {
			if dayIn(row.Part.Day, window) {
				row.Range = RangeWindow
				rows = append(rows, row)
			}
			if compare != nil && dayIn(row.Part.Day, *compare) {
				row.Range = RangeCompare
				rows = append(rows, row)
			}
		}
		folded[id] = encode(SeriesValuesOf(entry, rows, s.firsts[id], window, compare))
		stated[id] = folded[id]
		if !offered {
			continue
		}
		if values, _, ok := stored.SeriesValues(entry, s.firsts[id], window, compare); ok {
			stated[id] = encode(values)
			fromFigures[id] = true
		}
	}
	return stated, folded, fromFigures
}

// dayIn reports whether of holds day, a UTC midnight, as DayWindowIn's statement selects a range's
// days: from the UTC date of of.From, inclusive, to the UTC date of of.To, exclusive.
func dayIn(day time.Time, of Range) bool {
	return !day.Before(measures.Midnight(of.From)) && day.Before(measures.Midnight(of.To))
}

// cutRead is what cutValues selects the rows of a read's cut from, read once for the reads of one
// state: project's evals, the deploy repository their rows stand recorded under, the default
// repository the project publishes ("" for none), the first day of each series (FirstDaysIn, which
// the fold reads as the list read's does), each key whose rows the project reads with whether it
// reads them through the repository (eval_project_owners: its own key, false, and its owner keys),
// and every stored day row.
type cutRead struct {
	project, deployRepo, published string
	entries                        []Entry
	firsts                         map[string]map[string]time.Time
	via                            map[string]bool
	rows                           map[string]DayRow
}

// readCut reads project's cutRead.
func (r *refresherRig) readCut(t *testing.T, project string) cutRead {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	read := cutRead{project: project, via: map[string]bool{}, rows: r.dayRows(t)}
	page, err := r.registry.Current(ctx, []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read %s's evals: %v", project, err)
	}
	read.entries = page.Items
	if read.deployRepo, err = DeployRepoIn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if read.published, err = PublishedDefaultIn(ctx, pool, project); err != nil {
		t.Fatal(err)
	}
	if read.firsts, err = FirstDaysIn(ctx, r.database.Pool, read.entries, read.deployRepo); err != nil {
		t.Fatalf("read the first days of %s's evals: %v", project, err)
	}
	owners, err := pool.Query(ctx, `select owner, via from eval_project_owners where project_key = $1`, project)
	if err != nil {
		t.Fatalf("read the keys %s reads: %v", project, err)
	}
	var owner string
	var via bool
	if _, err := pgx.ForEachRow(owners, []any{&owner, &via}, func() error {
		read.via[owner] = via
		return nil
	}); err != nil {
		t.Fatalf("read the keys %s reads: %v", project, err)
	}
	return read
}

// cutValues is, by eval id and encoded as listValues encodes them, the values the eval list's read
// of c's project over window and compare (nil for none) of repos must answer: the fold
// (SeriesValuesOf) of the rows of its cut, selected here from every stored day row and not by the
// read (DayWindowIn). The cut is the listed rows of the days of the ranges, of the project's own key
// and of each owner key it reads, whose repository is one repos names, or the empty string (the
// project's whole)
// where repos names none; and the project's own rows stored under legionfacts.DefaultRepo, where repos
// names the repository the project publishes as its default (owners.go). An owner key's row is read
// under the project's eval id of its slug (asReader), its via_repository counted only where the
// project reads the key through the repository (viaAsRead). Of each eval it keeps the rows of the
// deploy repository its read reads (DeployRepoOf), and of each row what the read sends of it: the
// owner key and the repository it is stored under, which the fold orders a day's rows by
// (sortWindowRows); the samples of a series whose fold reads them (readsSamples), the write of a
// last value only in a read of more than one repository, and no counts where it holds none. The
// stored rows hold no writer, and none is needed: no eval of these histories changes its source, so
// every row of an eval is its own writer's.
func (c cutRead) cutValues(t *testing.T, repos []string, window Range, compare *Range) map[string]string {
	t.Helper()
	cut := repos
	if len(cut) == 0 {
		cut = []string{""}
	}
	defaultCut := c.published != "" && slices.Contains(repos, c.published)
	lastPairs := len(repos) > 1
	entries := map[string]Entry{}
	for _, entry := range c.entries {
		entries[entry.Definition.EvalID] = entry
	}
	byEval := map[string][]WindowRow{}
	for _, key := range slices.Sorted(maps.Keys(c.rows)) {
		row := c.rows[key]
		owner := row.ProjectKey
		throughRepository, reads := c.via[owner]
		if !reads || !row.Listed {
			continue
		}
		if !slices.Contains(cut, row.Repo) && (row.Repo != legionfacts.DefaultRepo || owner != c.project || !defaultCut) {
			continue
		}
		evalID := asReader(owner, c.project, row.EvalID)
		entry, listed := entries[evalID]
		if !listed || row.DeployRepo != DeployRepoOf(entry.Definition, c.deployRepo) {
			continue
		}
		part := row.DayPart
		if !throughRepository {
			part.ViaRepository = 0
		}
		at := slices.IndexFunc(entry.Series, func(series Series) bool { return series.Series == part.Series })
		if at < 0 || !readsSamples(entry.Definition, entry.Series[at]) {
			part.Samples = nil
		}
		if at < 0 || !lastPairs || entry.Series[at].Agg != AggLast {
			part.LastRecordedAt, part.LastReading = nil, ""
		}
		if len(part.Counts) == 0 {
			part.Counts = nil
		}
		if dayIn(row.Day, window) {
			byEval[evalID] = append(byEval[evalID], WindowRow{Range: RangeWindow, EvalID: evalID, Part: part, UpdatedAt: row.UpdatedAt, owner: owner, repo: row.Repo})
		}
		if compare != nil && dayIn(row.Day, *compare) {
			byEval[evalID] = append(byEval[evalID], WindowRow{Range: RangeCompare, EvalID: evalID, Part: part, UpdatedAt: row.UpdatedAt, owner: owner, repo: row.Repo})
		}
	}
	values := map[string]string{}
	for _, entry := range c.entries {
		id := entry.Definition.EvalID
		encoded, err := json.Marshal(SeriesValuesOf(entry, byEval[id], c.firsts[id], window, compare))
		if err != nil {
			t.Fatalf("encode the values of %s: %v", id, err)
		}
		values[id] = string(encoded)
	}
	return values
}

// TestARepositoryFilterAnswersTheFoldOfTheDayRowsOfItsCut: LEGION publishes acme/legion as its
// default repository, two of its trees own acme/legion and the rest own none, so their Legion rows
// stand under ':default' (legionfacts.DefaultRepo), which the project's cut of acme/legion reads
// (repoCut) and its stored figures fold (cutOf). Once its stored figures are built, the facts of a
// closed issue of a ':default' tree move, and then the project's default moves to acme/other,
// which holds no row of its own, and the refresher settles, which folds again the cuts the move
// marked stale (refoldStaleCuts): after each, the eval list's read of every offered range (each
// preset as a GET and as the live view reads it, with its compare range) of the project's own
// rows, of the default repository, and after the move of the default before it, is answered by the
// stored figures of its cut (listValues states some evals from them), with what the fold of its day
// rows answers (DayWindowIn, SeriesValuesOf) and the fold of the rows of its cut as this test
// selects them from every stored day row (cutValues); and the stored figures are the fold of the
// rows that stand, every cut's (expectTruth). Red where the read's cut of the published default
// leaves out the ':default' rows, or reads them under a repository the project no longer
// publishes. Red without the ':default' fix too, where a ':default' row is in a cut of its own
// (cutOf answering the row's own repository): that cut, which no read names, is stored beside the
// default's, and once the default has moved the stored cut of acme/other holds none of the moved
// ':default' rows, so the read of the published default the stored figures answer differs from
// the fold of its day rows.
func TestARepositoryFilterAnswersTheFoldOfTheDayRowsOfItsCut(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	defaultRepo, nextDefault := "acme/legion", "acme/other"
	project := r.seedLegionDefault(t, defaultRepo)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	held := map[string]int{}
	for _, row := range r.dayRows(t) {
		if row.ProjectKey == project && row.Listed {
			held[row.Repo]++
		}
	}
	if held[defaultRepo] == 0 || held[legionfacts.DefaultRepo] == 0 {
		t.Fatalf("%s holds listed day rows by repository %v, and the check needs rows of %s and of %s", project, held, defaultRepo, legionfacts.DefaultRepo)
	}

	type shape struct {
		name            string
		window, compare Range
	}
	var shapes []shape
	for _, n := range []int{7, 14, 28, 84} {
		compare := rangeOf(n, refresherToday.AddDate(0, 0, -n))
		shapes = append(shapes, shape{fmt.Sprintf("%d days", n), rangeOf(n, refresherToday), compare},
			shape{fmt.Sprintf("the live %d days", n), rangeOf(n+1, refresherToday.AddDate(0, 0, 1)), compare})
	}
	check := func(what string, repos ...string) {
		t.Helper()
		filters := [][]string{nil}
		for _, repo := range repos {
			filters = append(filters, []string{repo})
		}
		read := r.readCut(t, project)
		for _, one := range shapes {
			for _, filter := range filters {
				stated, folded, fromFigures := r.listValues(t, project, filter, one.window, &one.compare)
				cut := read.cutValues(t, filter, one.window, &one.compare)
				if len(fromFigures) == 0 {
					t.Errorf("%s: the stored figures answered none of %s's evals over %s of %v", what, project, one.name, filter)
				}
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s of %v states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, one.name, filter, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
					if stated[id] != cut[id] {
						at := firstDiffering(stated[id], cut[id])
						t.Errorf("%s: the read of %s of %v states %s's values in %d bytes that differ from the fold of the rows of its cut's %d at byte %d: %q, want %q",
							what, one.name, filter, id, len(stated[id]), len(cut[id]), at, stated[id][at:min(at+80, len(stated[id]))], cut[id][at:min(at+80, len(cut[id]))])
					}
				}
			}
		}
		expectTruth(t, what, r.storedFigures(t), r.truthFigures(t))
	}
	check("the first whole build", defaultRepo)

	before := r.dayRows(t)
	r.exec(t, "move the facts of an issue of a ':default' tree", `
		update issue_facts set work_hours = work_hours + 1.5, review_rounds = review_rounds + 1
		where project_key = $1 and issue_key = (
			select issue_key from issue_facts where project_key = $1 and repo = $2 and closed_at is not null order by closed_at desc, issue_key limit 1)`,
		project, legionfacts.DefaultRepo)
	r.pass(t)
	movedDefault := 0
	for key, row := range r.dayRows(t) {
		if old, found := before[key]; row.ProjectKey == project && row.Repo == legionfacts.DefaultRepo && row.Listed && (!found || !old.UpdatedAt.Equal(row.UpdatedAt)) {
			movedDefault++
		}
	}
	if movedDefault == 0 {
		t.Fatalf("the change of an issue's facts moved no listed day row of %s under %s", project, legionfacts.DefaultRepo)
	}
	check(fmt.Sprintf("after %d day rows of %s moved", movedDefault, legionfacts.DefaultRepo), defaultRepo)

	r.exec(t, "move the project's default repository", `update projects set default_repo = $2 where key = $1`, project, nextDefault)
	r.settle(t)
	if published, err := PublishedDefaultIn(ctx, pool, project); err != nil || published != nextDefault {
		t.Fatalf("%s publishes %q as its default (%v), want %s", project, published, err, nextDefault)
	}
	check("after the default moves to "+nextDefault, nextDefault, defaultRepo)
}

// seedLegionDefault seeds the delivery and Legion histories (seedHistories), gives the Legion
// project defaultRepo as its default repository and as the repository of its first two trees, and
// stores the project's facts again, so its Legion rows stand under defaultRepo for those two trees
// and under ':default' (legionfacts.DefaultRepo) for the others, whose chains name no repository.
// It answers the Legion project.
func (r *refresherRig) seedLegionDefault(t *testing.T, defaultRepo string) string {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	_, legion := r.seedHistories(t)
	project := legion.Project
	r.exec(t, "give the project a default repository", `update projects set default_repo = $2 where key = $1`, project, defaultRepo)
	r.exec(t, "give two trees the default repository", `update issues set repo = $2 where key = any($1)`, legion.Roots[:2], defaultRepo)
	facts, filings, err := legionfacts.ComputeProject(ctx, pool, project, deliverytest.DefaultNow)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: project, Rows: facts, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", project, err)
	}
	return project
}

// TestTheReadOfOneCutsDeployRepositoriesReadsItsRowsAlone: readFigureCuts' read of one cut
// (cutDeployReposQuery), which every batch of a stale cut's refold makes, answers the cuts of that
// repository the read of every cut answers (cutOf), and reads the cut's rows of its days and no
// other. EVAL publishes acme/main as its default and reads the plain owner key of acme/main; over
// 120 days of two series, EVAL's rows stand under acme/main (those of one deploy repository
// unlisted), acme/side, "" and ':default', the key's under acme/main, "" and ':default', and
// another project's under acme/main, each deploy repository but "" that of one cut's rows alone.
// Over 30 of those days, each of acme/main, acme/side and acme/none is answered as the read of
// every cut answers it: acme/main with the deploy repository of EVAL's ':default' rows, and with
// none of the unlisted rows, the key's ':default' rows or the other project's. Its generic plan
// reads eval_days through eval_days_window alone, each range bounded by its key, a repository (the
// cut's or ':default') and the days, and its analyzed plan of acme/main reads exactly EVAL's and the
// key's listed rows of acme/main and EVAL's of ':default' in those days. Red against the statement
// before it, which read every key's ':default' rows of those days beside them (240 rows here, where
// the cut holds 180) and, on the farm at INFRA's size, every listed row of the keys in the range,
// 703 ms a call; and red without the lateral's `offset 0`, whose generic plan reads
// eval_days_window by the repository and the days alone, every project's rows of the repository.
func TestTheReadOfOneCutsDeployRepositoriesReadsItsRowsAlone(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	owner := OwnerKey("acme/main", nil, nil)
	r.exec(t, "publish EVAL's default", `insert into eval_days_defaults (project_key, repo) values ($1, 'acme/main')`, r.project)
	r.exec(t, "seed the day rows", `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, built_by)
		select k.key, k.key || '/a', k.deploy_repo, s.series, k.repo, date '2026-07-01' + n, k.listed, 'test'
		from (values ($1::text, 'acme/main', '', true), ($1, 'acme/main', 'acme/unlisted', false), ($1, 'acme/side', 'acme/deploy', true),
			($1, '', '', true), ($1, ':default', 'acme/legacy', true), ($2, 'acme/main', 'acme/deploy', true), ($2, '', '', true),
			($2, ':default', 'acme/owner-only', true), ('QUAY', 'acme/main', 'acme/quay-only', true)) as k(key, repo, deploy_repo, listed)
		cross join (values ('n'), ('m')) as s(series)
		cross join generate_series(0, 119) as n`, r.project, owner)
	r.exec(t, "analyze the day rows", `analyze eval_days`)
	read := Range{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)}
	every, err := readFigureCuts(ctx, pool, r.project, read, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/main", "acme/side", "acme/none"} {
		got, err := readFigureCuts(ctx, pool, r.project, read, &repo)
		if err != nil {
			t.Fatal(err)
		}
		var want []figureCut
		for _, cut := range every {
			if cut.repo == repo {
				want = append(want, cut)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("the read of %s's cut of %s answers %v, and the read of every cut %v of it", r.project, repo, got, want)
		}
	}
	if !slices.Contains(every, figureCut{repo: "acme/main", deployRepo: "acme/legacy"}) {
		t.Fatalf("the read of every cut answers %v, and the check needs the cut of the ':default' rows under acme/main", every)
	}
	var rows float64
	if err := pool.QueryRow(ctx, `
		select count(*)::float8 from eval_days d where d.listed and d.day >= $3 and d.day < $4
			and (d.project_key = any(array[$1::text, $2::text]) and d.repo = 'acme/main' or d.project_key = $1 and d.repo = $5)`,
		r.project, owner, read.From, read.To, legionfacts.DefaultRepo).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := explaintest.GenericPlan(t, ctx, connection, "cut_deploy_repos", cutDeployReposQuery, []string{"text", "date", "date", "text"},
		[]string{"'" + r.project + "'", "'2026-08-01'", "'2026-08-31'", "'acme/main'"}, "set local enable_seqscan = off")
	if explaintest.SeqScansRelation(t, plan, "eval_days") {
		t.Errorf("the read of one cut scans eval_days under the generic plan:\n%s", plan)
	}
	explaintest.NewIndexKeys(pool).RequireIndexKeyed(t, plan, "eval_days_window", "the read of one cut", explaintest.CondRequired)
	defaults := 0
	for _, scan := range explaintest.IndexScans(t, plan) {
		if !slices.Contains(evalDaysIndexes, scan.Index) {
			continue
		}
		if scan.Index != "eval_days_window" || !strings.Contains(scan.Cond, "repo = ") || !strings.Contains(scan.Cond, "day >= ") || !strings.Contains(scan.Cond, "day < ") {
			t.Errorf("the read of one cut reads %s under %q under the generic plan, want a range of eval_days_window bounded by the key, a repository and the days:\n%s",
				scan, scan.Cond, explaintest.Outline(t, plan))
		}
		if strings.Contains(scan.Cond, "'"+legionfacts.DefaultRepo+"'") {
			defaults++
		}
	}
	if defaults == 0 {
		t.Errorf("the read of one cut reads no range of %s under the generic plan:\n%s", legionfacts.DefaultRepo, explaintest.Outline(t, plan))
	}
	analyzed, raw := explaintest.AnalyzedPlan(t, ctx, pool, cutDeployReposQuery, r.project, read.From, read.To, "acme/main")
	readRows := 0.0
	for _, node := range explaintest.Nodes(t, analyzed) {
		if node.Relation == "eval_days" {
			readRows += node.Rows*node.Loops + node.RowsRemovedByFilter*node.Loops
		}
	}
	if math.Abs(readRows-rows) > 0.5 {
		t.Errorf("the read of the cut of acme/main read %.0f rows of eval_days, want its %.0f:\n%s", readRows, rows, raw)
	}
	t.Logf("the read of the cut of acme/main read %.0f rows of eval_days, its own:\n%s", readRows, explaintest.Outline(t, analyzed))
}

// evalDaysIndexes is every index of eval_days (migration 20261117100006), by which a plan test tells
// a scan of eval_days from one of another table: a bitmap index scan names no relation.
var evalDaysIndexes = []string{"eval_days_pkey", "eval_days_window", "eval_days_stamps"}

// firstDiffering is the first index at which a and b differ.
func firstDiffering(a, b string) int {
	for index := range min(len(a), len(b)) {
		if a[index] != b[index] {
			return index
		}
	}
	return min(len(a), len(b))
}

// TestTheMarkerHoldsTheReadOffUntilTheFirstWholeBuild: a read of the stored figures reads them only
// of a project whose figures are built (figuresThrough). While the first pass's walks of the day
// rows have days left the figures walk waits, and no project is built; the figures walk then folds
// a batch of end days a pass, what it has folded is at every pass the fold of the rows that stand,
// and a project stands built only once its walk has folded every end day through today+2, which no
// project with rows does while its walk has batches left. A project made later is built at the next
// pass, holding no row. The marker then holds whichever build serves: a release's passes, which
// rebuild aside (rebuild.go), a pass of the build before it in the roll, the release's flip, and a
// rollback to the first build all read the figures as built.
func TestTheMarkerHoldsTheReadOffUntilTheFirstWholeBuild(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.batch = 2
	r.seedHistories(t)
	last := refresherToday.AddDate(0, 0, FigureLead)
	heldOff := 0
	for passes := 1; passes <= 100; passes++ {
		report, _ := r.pass(t)
		built := r.figuresBuilt(t)
		if passes == 1 && len(built) != 0 {
			t.Errorf("the first pass, whose walks of the day rows have days left, left figures built: %v", built)
		}
		expectTruth(t, fmt.Sprintf("pass %d", passes), r.storedFigures(t), r.truthFigures(t))
		walks, err := r.registry.readWalks(t.Context())
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
		if !report.walksLeft {
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

	old := *r
	r.release("another-build")
	r.pass(t)
	r.expectBuilt(t, "a release's first pass", refresherToday)
	old.pass(t)
	r.expectBuilt(t, "a pass of the build before it, as in a roll", refresherToday)
	r.settle(t)
	r.expectBuilt(t, "the release's flip", refresherToday)
	old.release(old.build)
	old.settle(t)
	r.expectBuilt(t, "a rollback once the release has flipped", refresherToday)
	expectTruth(t, "after the roll and the rollback", r.storedFigures(t), r.truthFigures(t))
}

// TestTheDayRowsAreFoldedInDayOrderWhateverOrderTheReadAnswers: a read of the day rows answers
// them in its plan's order, and a sum of floats depends on the order of its parts, so
// SeriesValuesOf folds them in day order, as the stored figures are folded (newFigureGroup): three
// days of a logged sum, 1e16 then 1 then 1, sum to 1e16 in day order (each 1 is half an ulp of
// 1e16 and rounds to the even neighbour) and to 1e16+2 the other way round, and the read answers
// the day order's bits whichever order its rows come in.
func TestTheDayRowsAreFoldedInDayOrderWhateverOrderTheReadAnswers(t *testing.T) {
	t.Parallel()
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

// TestADaysRowsAreFoldedInOneOrderWhateverOrderTheReadAnswers: a read of the cut of a project's
// default repository answers, on one day, rows of one series that differ only in the key each is
// stored under or its repository: the project's own row of the repository, its own row stored
// under legionfacts.DefaultRepo (repoCut) and the repository's owner key's row (owners.go), in
// whatever order the read's plan answers them; and a sum of floats depends on the order of its
// parts: 1e16, -1e16 and 1 sum to 1 in that order and to 0 in another (1 is half an ulp of 1e16
// and rounds to the even neighbour). So every fold sorts a day's rows by the key each is stored
// under and then by its repository (sortWindowRows): in each of the six orders the read can answer
// the rows in, the read of the day rows (SeriesValuesOf) answers the same bytes, and so do the
// stored figures (newFigureGroup) of the range and of the day, the range's sum the read's bit for
// bit.
func TestADaysRowsAreFoldedInOneOrderWhateverOrderTheReadAnswers(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	entry := Entry{Definition: Definition{EvalID: "EVAL/spend", ProjectKey: "EVAL", Source: SourceLogged},
		Series: []Series{{EvalID: "EVAL/spend", Series: "dollars", SeriesComputation: SeriesComputation{Role: RoleHeadline, Listed: true, Agg: AggSum}}}}
	stored := []struct {
		owner, repo string
		sum         float64
	}{
		{"EVAL", "acme/legion", 1e16},
		{"EVAL", legionfacts.DefaultRepo, -1e16},
		{OwnerKey("acme/legion", nil, nil), "acme/legion", 1},
	}
	rows := func(order []int) []WindowRow {
		held := make([]WindowRow, len(order))
		for at, index := range order {
			sum := stored[index].sum
			held[at] = WindowRow{Range: RangeWindow, EvalID: entry.Definition.EvalID, owner: stored[index].owner, repo: stored[index].repo,
				Part: measures.DayPart{Series: "dollars", Day: day, N: 1, Sum: &sum, Samples: []float64{sum}}}
		}
		return held
	}
	end := day.AddDate(0, 0, 1)
	window := rangeOf(7, end)
	firsts := map[string]time.Time{"dollars": window.From}
	unsorted := map[uint64]bool{}
	var read, ranged, daily []byte
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		var parts []measures.DayPart
		for _, row := range rows(order) {
			parts = append(parts, row.Part)
		}
		unsorted[math.Float64bits(*FoldStatistics(entry.Definition, entry.Series, parts, window)["dollars"]["sum"])] = true

		values := SeriesValuesOf(entry, rows(order), firsts, window, nil)
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatalf("encode the values of the rows in the order %v: %v", order, err)
		}
		group := newFigureGroup(entry, foldKeyOf(entry), rows(order))
		rangeFigures, _, rangeHeld := group.rangeFigure(7, end)
		dayFigures, _, dayHeld := group.dayFigure(day)
		if !rangeHeld || !dayHeld {
			t.Fatalf("the rows in the order %v hold the range (%t) and the day (%t), want both", order, rangeHeld, dayHeld)
		}
		folded, _, err := decodeFigures(rangeFigures)
		if err != nil {
			t.Fatalf("decode the stored range of the rows in the order %v: %v", order, err)
		}
		if got, want := folded["dollars"]["sum"], values[0].Window["sum"]; got == nil || want == nil || math.Float64bits(*got) != math.Float64bits(*want) {
			t.Errorf("the rows in the order %v: the stored range sums to %s and the read of the day rows to %s, want the same bits", order, showValue(got), showValue(want))
		}
		if read == nil {
			read, ranged, daily = encoded, rangeFigures, dayFigures
			continue
		}
		if !slices.Equal(encoded, read) || !slices.Equal(rangeFigures, ranged) || !slices.Equal(dayFigures, daily) {
			t.Errorf("the rows in the order %v fold to other bytes than in the order [0 1 2]: the read of the day rows %s, want %s; the stored range %x, want %x; the stored day %x, want %x",
				order, encoded, read, rangeFigures, ranged, dayFigures, daily)
		}
	}
	if len(unsorted) < 2 {
		t.Errorf("the rows summed in the order they come in answer one sum whatever the order, want two: the check would show nothing")
	}
}

// foldMovedDays is the whole fold, on up to workers goroutines at once, of the stored figures of
// each day moved names (by eval and deploy repository) and of every range whose span
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

// dayUpdateOIDs is the type of each parameter of writeDays' update of its rows (dayUpdates), as
// the server describes it (TestAReadingsWritesDayRowsGoInBoundedStatementsInKeyOrder holds it to
// the description): what pgx encodes each argument as.
var dayUpdateOIDs = []uint32{
	pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.DateArrayOID,
	pgtype.BoolArrayOID, pgtype.Int4ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID,
	pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.TimestamptzArrayOID, pgtype.Int4ArrayOID, pgtype.TimestamptzArrayOID,
	pgtype.TextArrayOID,
}

// encodedBytes is the bytes args, the arguments of a statement, take as pgx sends them as the
// types oids: each encoded as its parameter's type, in the format pgx sends it in (a string as
// text, as pgx's ExtendedQueryBuilder chooses, any other argument in its type's format), with 4
// bytes of length before it.
func encodedBytes(t *testing.T, oids []uint32, args []any) int {
	t.Helper()
	if len(oids) != len(args) {
		t.Fatalf("a statement of %d arguments, where its parameters are the %d types %v", len(args), len(oids), oids)
	}
	types, total := pgtype.NewMap(), 0
	for index, arg := range args {
		format := types.FormatCodeForOID(oids[index])
		switch arg.(type) {
		case string, *string:
			format = pgtype.TextFormatCode
		}
		encoded, err := types.Encode(oids[index], format, arg, nil)
		if err != nil {
			t.Fatalf("encode argument %d of a statement of the types %v: %v", index+1, oids, err)
		}
		total += 4 + len(encoded)
	}
	return total
}

// paramEstimate is the most bytes args, the arguments of a statement, take as pgx encodes them,
// estimated from the arguments as statementChunks' callers estimate a statement's (paramBytes,
// and upsertBytes, deleteBytes, dayRowBytes or dayUpdateBytes): 4 bytes of length before each
// argument, a string's bytes, and an array's header and, for each element, 4 bytes of length and
// its bytes, a string's or a byte slice's length (none for a null string), 1 for a bool and at
// most 8 for a number, a date or a time, null or not.
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
		case []int:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []float64:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case []*float64:
			bytes += arrayHeaderBytes + (4+8)*len(value)
		case time.Time:
			bytes += 8
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
// of its last array, its rows' deploy repositories.
func rowsIn(args []any) int {
	return len(args[len(args)-1].([]string))
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

// sentStatements is a tracer (storetest.Traced) that records, in the order they start, every
// statement on the pool it traces that takes parameters: each one's SQL and arguments.
type sentStatements struct {
	mu   sync.Mutex
	sent []pgx.TraceQueryStartData
}

func (s *sentStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if len(data.Args) > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sent = append(s.sent, data)
	}
	return ctx
}

func (s *sentStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take answers the statements recorded since the last take, and forgets them.
func (s *sentStatements) take() []pgx.TraceQueryStartData {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := s.sent
	s.sent = nil
	return taken
}

// sentStatement is one statement sentStatements recorded, measured: its SQL and arguments, the
// types the server describes its parameters as, and the bytes its parameters take as pgx encodes
// them as those types (encodedBytes).
type sentStatement struct {
	sql   string
	args  []any
	oids  []uint32
	bytes int
}

// measured is each of sent measured (sentStatement), the types of each SQL's parameters described
// once, by the server, on a connection of the rig's own pool.
func (r rig) measured(t *testing.T, sent []pgx.TraceQueryStartData) []sentStatement {
	t.Helper()
	ctx := t.Context()
	conn, err := r.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to describe the statements: %v", err)
	}
	defer conn.Release()
	described := map[string][]uint32{}
	statements := make([]sentStatement, len(sent))
	for index, data := range sent {
		oids, held := described[data.SQL]
		if !held {
			description, err := conn.Conn().PgConn().Prepare(ctx, "", data.SQL, nil)
			if err != nil {
				t.Fatalf("describe the statement %.120q: %v", strings.Join(strings.Fields(data.SQL), " "), err)
			}
			oids, described[data.SQL] = description.ParamOIDs, description.ParamOIDs
		}
		// A statement that names its exec mode carries it as its first argument (pgx.QueryExecMode),
		// which pgx takes and sends no parameter for.
		args := data.Args
		if len(args) > 0 {
			if _, mode := args[0].(pgx.QueryExecMode); mode {
				args = args[1:]
			}
		}
		statements[index] = sentStatement{sql: data.SQL, args: args, oids: oids, bytes: encodedBytes(t, oids, args)}
	}
	return statements
}

// largestStatement is the largest of statements (sentStatement.bytes) and its SQL's first words,
// as a failure or a log names it.
func largestStatement(statements []sentStatement) (int, string) {
	largest, what := 0, ""
	for _, statement := range statements {
		if statement.bytes > largest {
			largest, what = statement.bytes, statement.sql
		}
	}
	if words := strings.Fields(what); len(words) > 6 {
		what = strings.Join(words[:6], " ")
	}
	return largest, what
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
// every writer of its rows takes them in, and so the order upsertRangeFigures' and
// writeAfterRanges' statements must be sent their rows in, since they take them as sent (`with
// ordinality`).
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

// rangeKeyArrays is the keys of rows as rangeKeyOrder's arrays.
func rangeKeyArrays(rows []rangeFigureRow) []any {
	repos, ns, ends, evalIDs, deployRepos := make([]string, len(rows)), make([]int32, len(rows)), make([]time.Time, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for index, row := range rows {
		repos[index], ns[index], ends[index], evalIDs[index], deployRepos[index] = row.repo, int32(row.days), row.end, row.eval, row.deployRepo
	}
	return []any{repos, ns, ends, evalIDs, deployRepos}
}

// dayFigureKeyArrays is the keys of rows as dayFigureKeyOrder's arrays.
func dayFigureKeyArrays(rows []dayFigureRow) []any {
	repos, days, evalIDs, deployRepos := make([]string, len(rows)), make([]time.Time, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for index, row := range rows {
		repos[index], days[index], evalIDs[index], deployRepos[index] = row.repo, row.day, row.eval, row.deployRepo
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
// in one field of the key with keys equal in their leading fields, sorted by the write's own sort
// (figureWrites.sortByKey; dayRowStatements, by compareDayRows), come back in the same sequence
// from Postgres's `order by` of the same keys, each text `collate "C"`. The test database collates
// text by a language collation (ICU en-US on legion-testpg, en_US.utf8 in CI), under which many of
// those pairs order the other way.
func TestEachWritesGoKeyOrderIsPostgresCollateCOrder(t *testing.T) {
	t.Parallel()
	pool := storetest.Open(t).Pool
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
			for _, evalID := range few {
				for _, deployRepo := range edges {
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
	writes.sortByKey()
	order := keyOrderOf(t, pool, len(writes.ranges), rangeKeyOrder, rangeKeyArrays(writes.ranges)...)
	if at := firstOutOfOrder(order); at >= 0 {
		t.Errorf("Go sorts %d range keys (compareRangeKeys) otherwise than Postgres's collate \"C\": at %d Go puts %s, and Postgres %s",
			len(writes.ranges), at, rangeKeyText(writes.ranges[at].rangeFigureKey), rangeKeyText(writes.ranges[order[at]].rangeFigureKey))
	}
	order = keyOrderOf(t, pool, len(writes.days), dayFigureKeyOrder, dayFigureKeyArrays(writes.days)...)
	if at := firstOutOfOrder(order); at >= 0 {
		t.Errorf("Go sorts %d day figure keys (compareDayFigureKeys) otherwise than Postgres's collate \"C\": at %d Go puts %s, and Postgres %s",
			len(writes.days), at, dayFigureKeyText(writes.days[at].dayFigureKey), dayFigureKeyText(writes.days[order[at]].dayFigureKey))
	}
	statements, err := dayRowStatements(rows, testBuild, false)
	if err != nil {
		t.Fatalf("encode %d day rows: %v", len(rows), err)
	}
	sent := slices.Collect(statements)
	order = keyOrderOf(t, pool, len(rows), dayRowKeyOrder, dayRowKeyArraysIn(sent)...)
	if at := firstOutOfOrder(order); at >= 0 {
		var held []string
		for _, args := range sent {
			held = append(held, dayKeysIn(args)...)
		}
		t.Errorf("Go sorts %d day rows (compareDayRows) otherwise than Postgres's collate \"C\": at %d Go puts %q, and Postgres %q",
			len(rows), at, held[at], held[order[at]])
	}
	t.Logf("%d range keys, %d day figure keys and %d day row keys, each sorted by its write's sort, held to Postgres's collate \"C\" order of them",
		len(writes.ranges), len(writes.days), len(rows))
}

// TestARangeChunkClosesAtARepositoryOrLengthOnceItHoldsTheLeast: the chunks a write folds and
// sends of its ranges, in the byte order of their keys, close at the end of a (repository, length)
// prefix of the keys only once they hold at least the least ranges, and at the limit wherever it
// falls (rangeChunks): at a least of one, a chunk each prefix, cut again at the limit; at a larger
// least, chunks of several prefixes, which pass the end of a prefix inside them by; and at
// rangeChunkLeast, a write of a few ranges is one chunk. The third prefix keeps the second's
// length, so only the repository ends the second: a split that compared lengths alone would not
// close a chunk there.
func TestARangeChunkClosesAtARepositoryOrLengthOnceItHoldsTheLeast(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var ranges []rangeTarget
	for _, prefix := range []struct {
		repo        string
		days, count int
	}{{"", 7, 3}, {"", 8, 5}, {"acme/api", 8, 1}, {"acme/api", 14, 2}} {
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
	t.Parallel()
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
		deployRepos := make([]string, shape.count)
		for index := range shape.count {
			deployRepos[index] = long(index, shape.text)
			goneRanges[index] = rangeFigureKey{repo: long(index, shape.text), days: math.MaxInt32, end: far, eval: long(index, shape.text*3/2), deployRepo: deployRepos[index]}
			goneDays[index] = dayFigureKey{repo: long(index, shape.text), day: far, eval: long(index, shape.text*3/2), deployRepo: deployRepos[index]}
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
				held, sizes, largest = append(held, args[len(args)-1].([]string)...), append(sizes, rowsIn(args)), max(largest, encoded)
			}
			if !slices.Equal(held, deployRepos) {
				t.Errorf("%s, %q: the statements take %d rows, other rows or in another order than the %d given", shape.name, kind.name, len(held), len(deployRepos))
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

// TestTheRoutesBoundsAreTheStatementBoundAtTheLargestKeys: the bounds the routes hold a row to
// are each the most whose one row, built by the package's own folds and encoders with every key,
// the build and each series' name at their largest, its statement sends within statementBytes.
// An eval of MaxEvalSeries series, each folded by any agg with every statistic set and with a
// via-repository count a varint takes the most bytes for: its range row is upserted (rangeUpserts)
// in one statement whose parameters take at most the bound as estimated and as pgx encodes them,
// and so is its day row of headline series (dayUpserts); the widest fold, a quantile's, takes the
// bytes for each series that MaxEvalSeries' derivation counts (seriesFigureBytes); and with one
// series more that range row's statement passes the bound, as estimated and as encoded. A day row
// of MaxDaySamples values is updated (dayUpdates) in one statement whose parameters take at most
// the bound, as estimated (dayUpdateBytes) and as encoded; with one value more the estimate, the
// bound statementChunks holds a statement to, passes it (the estimate counts the row's day, n and
// first at 8 bytes, where pgx encodes each in 4, so that row would still encode within it).
func TestTheRoutesBoundsAreTheStatementBoundAtTheLargestKeys(t *testing.T) {
	t.Parallel()
	largest := func(index int, fill string) string {
		prefix := fmt.Sprintf("%06d-", index)
		return prefix + strings.Repeat(fill, maxKeyBytes-len(prefix))
	}
	project := "P" + strings.Repeat("9", maxProjectKeyBytes-1)
	evalID := EvalID(project, strings.Repeat("s", maxKeyBytes))
	scope := figureScope{build: strings.Repeat("b", maxKeyBytes), project: project}
	repo, deployRepo := strings.Repeat("r", maxKeyBytes), strings.Repeat("d", maxKeyBytes)
	if len(evalID) != maxEvalIDBytes || len(project) != maxProjectKeyBytes {
		t.Fatalf("an eval id of %d bytes and a project key of %d, want %d and %d", len(evalID), len(project), maxEvalIDBytes, maxProjectKeyBytes)
	}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	definition := Definition{EvalID: evalID, ProjectKey: project, Source: SourceLogged}
	// rows is the stored range row and day row of an eval of n series, each named at the largest
	// a name holds and folded by agg, over one day whose part of each sets every value a fold reads.
	rows := func(agg string, n int) (rangeFigureRow, dayFigureRow) {
		series, parts, via := make([]Series, n), make([]measures.DayPart, n), make(map[string]int, n)
		for index := range n {
			name, value, at := largest(index, "x"), 1.5+float64(index), day.Add(time.Hour)
			series[index] = Series{EvalID: evalID, Series: name, SeriesComputation: SeriesComputation{Role: RoleHeadline, Listed: true, Agg: agg}}
			parts[index] = measures.DayPart{Series: name, Day: day, N: 2, Sum: new(2 * value), Num: new(value), Den: new(1.0), Max: new(value),
				Samples: []float64{value, value}, LastValue: new(value), LastAt: &at}
			via[name] = math.MaxInt64
		}
		folded := FoldStatistics(definition, series, parts, Range{From: day, To: day.AddDate(0, 0, 1)})
		for name, stats := range folded {
			for statistic, value := range stats {
				if value == nil {
					t.Fatalf("%s: series %.12s... folds no value of %s, and the check needs every statistic set", agg, name, statistic)
				}
			}
		}
		return rangeFigureRow{rangeFigureKey: rangeFigureKey{repo: repo, days: math.MaxInt32, end: day.AddDate(0, 0, 1), eval: evalID, deployRepo: deployRepo},
				key: math.MinInt64, figures: encodeFigures(folded, via), oldest: day},
			dayFigureRow{dayFigureKey: dayFigureKey{repo: repo, day: day, eval: evalID, deployRepo: deployRepo},
				key: math.MinInt64, figures: encodeFigures(foldDay(definition, series, parts, day), nil), stamp: math.MaxInt64}
	}
	// one is the size of the one statement statements sends, as estimated and as pgx encodes it as
	// oids, failing the test where it sends more than one.
	one := func(what string, statements iter.Seq[[]any], oids []uint32) (int, int) {
		sent := slices.Collect(statements)
		if len(sent) != 1 {
			t.Fatalf("%s went in %d statements, want one", what, len(sent))
		}
		return paramEstimate(t, sent[0]), encodedBytes(t, oids, sent[0])
	}

	widest, widestAgg := 0, ""
	for _, agg := range aggs {
		ranged, daily := rows(agg, MaxEvalSeries)
		for _, statement := range []struct {
			name       string
			statements iter.Seq[[]any]
			oids       []uint32
		}{
			{"its range row", rangeUpserts(scope, []rangeFigureRow{ranged}), statementOIDs[rangeUpsertKind]},
			{"its day row", dayUpserts(scope, []dayFigureRow{daily}), statementOIDs[dayUpsertKind]},
		} {
			what := fmt.Sprintf("%s of an eval of %d series folded by %s", statement.name, MaxEvalSeries, agg)
			estimated, encoded := one(what, statement.statements, statement.oids)
			if estimated > statementBytes || encoded > estimated {
				t.Errorf("%s takes %d bytes of parameters as pgx encodes them and an estimated %d, where the bound is %d", what, encoded, estimated, statementBytes)
			}
			t.Logf("%s: %d bytes of parameters as pgx encodes them, an estimated %d, of %d", what, encoded, estimated, statementBytes)
		}
		if len(ranged.figures) > widest {
			widest, widestAgg = len(ranged.figures), agg
		}
	}
	// The figures' two counts take 2 bytes each at this many series, as figureRowReserve counts them.
	if want := 2 + MaxEvalSeries*seriesFigureBytes + 2; widestAgg != AggQuantile || widest != want {
		t.Errorf("the widest range row's figures are %s's, %d bytes, want a quantile's, %d bytes: %d of %d bytes and the two counts", widestAgg, widest, want, MaxEvalSeries, seriesFigureBytes)
	}
	past, _ := rows(AggQuantile, MaxEvalSeries+1)
	what := fmt.Sprintf("the range row of an eval of %d series folded by %s", MaxEvalSeries+1, AggQuantile)
	if estimated, encoded := one(what, rangeUpserts(scope, []rangeFigureRow{past}), statementOIDs[rangeUpsertKind]); estimated <= statementBytes || encoded <= statementBytes {
		t.Errorf("%s takes %d bytes of parameters as pgx encodes them and an estimated %d, want both past the bound %d", what, encoded, estimated, statementBytes)
	} else {
		t.Logf("%s: %d bytes of parameters as pgx encodes them, an estimated %d, past %d", what, encoded, estimated, statementBytes)
	}

	dayRow := func(n int) DayRow {
		lastAt, recordedAt := day.Add(23*time.Hour), day.Add(24*time.Hour)
		row := DayRow{ProjectKey: project, EvalID: evalID, Repo: repo, Listed: true, DayPart: measures.DayPart{
			Series: largest(0, "v"), Day: day, N: n, Sum: new(math.MaxFloat64), Num: new(-math.MaxFloat64), Den: new(math.SmallestNonzeroFloat64),
			Max: new(math.MaxFloat64), LastValue: new(math.Pi), LastAt: &lastAt, LastRecordedAt: &recordedAt, LastReading: "0190c9a4-5c1e-7a00-8000-000000000000",
		}}
		row.Samples = make([]float64, n)
		for index := range row.Samples {
			row.Samples[index] = math.Pi * float64(index)
		}
		return row
	}
	what = fmt.Sprintf("a day row of %d values", MaxDaySamples)
	estimated, encoded := one(what, dayUpdates(project, evalID, scope.build, []DayRow{dayRow(MaxDaySamples)}), dayUpdateOIDs)
	if estimated > statementBytes || encoded > estimated {
		t.Errorf("%s takes %d bytes of parameters as pgx encodes them and an estimated %d, where the bound is %d", what, encoded, estimated, statementBytes)
	}
	t.Logf("%s: %d bytes of parameters as pgx encodes them, an estimated %d, of %d", what, encoded, estimated, statementBytes)
	what = fmt.Sprintf("a day row of %d values", MaxDaySamples+1)
	if estimated, encoded := one(what, dayUpdates(project, evalID, scope.build, []DayRow{dayRow(MaxDaySamples + 1)}), dayUpdateOIDs); estimated <= statementBytes || encoded > estimated {
		t.Errorf("%s takes %d bytes of parameters as pgx encodes them and an estimated %d, want the estimate past the bound %d", what, encoded, estimated, statementBytes)
	} else {
		t.Logf("%s: %d bytes of parameters as pgx encodes them, an estimated %d, past %d", what, encoded, estimated, statementBytes)
	}
}

// TestARowPastTheStatementBoundIsWrittenAlone: a range row whose figures alone are past
// statementBytes is written by a statement of its own, between those of the rows around it, and
// stored bit for bit, as they are (writeFigures).
func TestARowPastTheStatementBoundIsWrittenAlone(t *testing.T) {
	t.Parallel()
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
// gone, and a range and a day row of a repository and of a deploy repository no row holds
// stored), a refold of every eval of the project (refoldFigures, its statements recorded on a
// traced pool) folds the ranges of several evals and of several cuts, the project's own rows and a
// repository's, the deploy repository's and the rows no deploy repository moves, while it writes
// them. Its range upserts take the rows the whole fold on one worker with no
// write answers (foldMovedDays), each once, in the byte order of their keys, each statement within
// one chunk of the ranges it reads (readFigurePlan), as rangeChunks cuts them at rangeChunkLeast;
// its gone ranges, day rows and gone days are that fold's too, in their order, after every range;
// the range rows and the day rows its upserts take, across their statements, are in the order
// Postgres's `collate "C"` gives their keys (rangeKeyOrder, dayFigureKeyOrder), which each
// statement takes them in as sent; no statement holds more than statementBytes of parameters as
// statementChunks estimates them, nor more than that estimate as pgx encodes them; and the stored
// figures it leaves are the fold of the rows that stand, bit for bit.
func TestARefoldSendsTheWholeFoldsRowsInKeyOrderOnceEachInBoundedStatements(t *testing.T) {
	t.Parallel()
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
	goneEval, goneDay := EvalID(project, SlugReworkShare), measures.Midnight(deliveries.From).AddDate(0, 0, 3)
	r.exec(t, "garble every stored range row", `update eval_range_figures set figures = '\x00'::bytea where project_key = $1`, project)
	r.exec(t, "forget every stored day row", `delete from eval_day_figures where project_key = $1`, project)
	for _, gone := range []struct{ what, repo, deployRepo string }{{"a repository", "zz/gone", ""}, {"a deploy repository", "", "zz/gone"}} {
		r.exec(t, "store a day row of "+gone.what+" no row holds", `
			insert into eval_day_figures (project_key, repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp, built_by)
			values ($1, $4, $2, $3, $5, 0, '\x00', 0, 'an older build')`, project, goneDay, goneEval, gone.repo, gone.deployRepo)
		r.exec(t, "store a range row of "+gone.what+" no row holds", `
			insert into eval_range_figures (project_key, repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest_updated_at, built_by)
			values ($1, $4, 7, $2::date + 1, $3, $5, 0, '\x00', now(), 'an older build')`, project, goneDay, goneEval, gone.repo, gone.deployRepo)
	}
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
	if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(written.ranges), rangeKeyOrder, rangeKeyArrays(written.ranges)...)); at >= 0 {
		t.Errorf("range row %d of the refold's upserts, %s, is out of the byte order Postgres's collate \"C\" gives their keys", at, rangeKeyText(written.ranges[at].rangeFigureKey))
	}
	if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(written.days), dayFigureKeyOrder, dayFigureKeyArrays(written.days)...)); at >= 0 {
		t.Errorf("day row %d of the refold's upserts, %s, is out of the byte order Postgres's collate \"C\" gives their keys", at, dayFigureKeyText(written.days[at].dayFigureKey))
	}
	evals, repos, deployRepos := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, row := range written.ranges {
		evals[row.eval], repos[row.repo], deployRepos[row.deployRepo] = true, true, true
	}
	if len(evals) < 2 || !repos[""] || len(repos) < 2 || !deployRepos[""] || !deployRepos[deliveries.DeployRepo] || upserts < 2 ||
		len(written.goneRanges) < 2 || len(written.goneDays) < 2 {
		t.Errorf("the refold upserted the ranges of %d evals, of the cuts %q and of the deploy repositories %v in %d statements, and deleted %d ranges and %d days; the check needs several evals, the project's own rows and a repository's, %q and %q, several statements and the two gone ranges and days deleted",
			len(evals), slices.Sorted(maps.Keys(repos)), slices.Sorted(maps.Keys(deployRepos)), upserts, len(written.goneRanges), len(written.goneDays), "", deliveries.DeployRepo)
	}
	expectTruth(t, "a refold of every stale figure", r.storedFigures(t), r.truthFigures(t))
	t.Logf("the refold sent %d statements, %d of them range upserts of %d rows of %d evals, of %d ranges in %d chunks over %d (repository, length) prefixes, the largest holding %d bytes of parameters as pgx encodes them",
		len(sent), upserts, len(written.ranges), len(evals), len(plan.ranges), len(ends), len(prefixes), largest)
}

// TestAFailureInALaterChunkRollsTheWholeWriteBack: over a settled history of a logged eval whose
// readings fold into its project's own rows and into acme/web's, a write of a reading of acme/web
// on each of the 180 days before today folds the ranges those days moved, of the project's own rows
// and of the cut of acme/web (of each length, those ending on each of the 182 days from the first
// of them through the last end day folded), in its own transaction while it writes them, chunk by
// chunk (figurePlan.foldAndWrite): the first chunk closes once it holds rangeChunkLeast ranges, at
// the end of one of the project's own lengths before its last, and the upsert of a later chunk
// fails: a trigger of the test refuses every range row of 85 days, the last length of the
// project's own rows, once the upserts of the first chunk, one of them holding ranges of several
// shorter lengths, have written theirs. The write answers that error, sends nothing after it, and
// stores nothing: no reading, no day row, no change row, and none of the stored figures the chunks
// before the failure wrote.
func TestAFailureInALaterChunkRollsTheWholeWriteBack(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	statements := &figureStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
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
// whose readings fold into its project's own rows and into acme/web's, a reading's write of
// acme/web folds the ranges its day moved, a few of each length of FigureLengths in the project's
// own rows and in the cut of acme/web, far fewer than rangeChunkLeast, as one chunk, and upserts
// them in one statement (figurePlan.foldAndWrite), not in one a (repository, length) prefix.
func TestAWriteOfAFewRangesOfEveryLengthSendsOneRangeUpsert(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	statements := &figureStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
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
		t.Errorf("a reading's write upserted %d ranges of the lengths %v and the cuts %q in %d statements, want ranges of all %d lengths and of both cuts in one",
			ranges, slices.Sorted(maps.Keys(lengths)), slices.Sorted(maps.Keys(repos)), upserts, len(FigureLengths))
	}
	t.Logf("a reading's write upserted %d ranges of %d lengths and %d cuts in %d statements", ranges, len(lengths), len(repos), upserts)
}

// TestTheLargestEvalTheRoutesAcceptIsWrittenWithinTheStatementBound: over a settled history whose
// figures are built, a logged eval takes series of the largest names a name holds, each folded as
// a quantile, listed and headline, 40 at a time and then one at a time, until a register write is
// refused: it holds MaxEvalSeries of them, and the refusal (CodeSeriesLimit) names that bound. A
// readings write of one reading of each series then folds its day and every range that holds it
// into the stored figures, in its own transaction, and no statement the write sends takes more
// than statementBytes of parameters as pgx encodes them as the types the server describes; the
// stored figures it leaves are the fold of the rows that stand.
func TestTheLargestEvalTheRoutesAcceptIsWrittenWithinTheStatementBound(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLoggedQuantile(t)
	r.settle(t)
	const slug, most = "big-names", 1600
	name := func(index int) string {
		prefix := fmt.Sprintf("s%04d-", index)
		return prefix + strings.Repeat("x", maxKeyBytes-len(prefix))
	}
	register := func(from, to int) error {
		change := Registration{Project: r.project, Slug: slug, Caller: alice}
		for index := from; index < to; index++ {
			change.Series = append(change.Series, SeriesChange{Series: name(index),
				SeriesComputationChange: SeriesComputationChange{Agg: Set(AggQuantile), Role: Set(RoleHeadline), Listed: Set(true)}})
		}
		_, err := r.registry.Register(t.Context(), change)
		return err
	}
	held, step := 0, 40
	var refused *Error
	for {
		if held >= most {
			t.Fatalf("a logged eval took %d series of %d-byte names and refused none", held, maxKeyBytes)
		}
		err := register(held, held+step)
		if err == nil {
			held += step
			continue
		}
		if !errors.As(err, &refused) || refused.Code != CodeSeriesLimit {
			t.Fatalf("a register write of series %d to %d was answered %v, want %s", held, held+step, err, CodeSeriesLimit)
		}
		if step == 1 {
			break
		}
		step = 1
	}
	if held != MaxEvalSeries || !strings.Contains(refused.Message, strconv.Itoa(held)) || refused.Field != "series[0].series" {
		t.Fatalf("the eval took %d series and then refused series[0].series? %q: %s, want %d taken and the bound named", held, refused.Field, refused.Message, MaxEvalSeries)
	}
	t.Logf("the eval took %d series; then: %s %s: %s", held, refused.Code, refused.Field, refused.Message)

	statements := &sentStatements{}
	r.registry = New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	rows := make([]json.RawMessage, held)
	for index := range held {
		rows[index] = row(t, map[string]any{"series": name(index), "row_kind": "review", "row_key": fmt.Sprint(index), "value": 1.5 + float64(index),
			"measured_at": refresherToday.Add(time.Duration(index) * time.Second)})
	}
	if _, err := r.registry.WriteReadings(t.Context(), Batch{Project: r.project, Slug: slug, Rows: rows, Caller: alice, BuiltBy: r.build}); err != nil {
		t.Fatalf("write a reading of each of %d series: %v", held, err)
	}
	sent := r.measured(t, statements.take())
	upserts := 0
	for _, statement := range sent {
		if statement.bytes > statementBytes {
			t.Errorf("the write sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
		if figureStatementKind(statement.sql) == rangeUpsertKind {
			upserts += rowsIn(statement.args)
		}
	}
	if upserts == 0 {
		t.Errorf("the write upserted no range row of %s, and the check needs the widest rows written", slug)
	}
	stored := r.storedFigures(t)
	expectTruth(t, "after the write of the largest eval", stored, r.truthFigures(t))
	widest := 0
	for line, figure := range stored {
		if strings.HasSuffix(line, EvalID(r.project, slug)) {
			widest = max(widest, len(figure.figures))
		}
	}
	largest, what := largestStatement(sent)
	t.Logf("%d statements, the largest %d bytes of parameters (%s), of %d; %d range rows upserted, the widest stored figures of %s %d bytes",
		len(sent), largest, what, statementBytes, upserts, slug, widest)
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
		keys = append(keys, figureGroupKey{eval: definition.EvalID, figureCut: figureCut{deployRepo: deployRepo}})
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
