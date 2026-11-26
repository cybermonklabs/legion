package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// evalShape is one read of the eval list the stored figures are checked over: its window, its
// compare range (nil for none), its repositories, and whether the stored figures answer it
// (evals.FiguresOffered).
type evalShape struct {
	name    string
	window  evals.Range
	compare *evals.Range
	repos   []string
	offered bool
}

// evalDaysEnding is the n whole UTC days ending at end, exclusive.
func evalDaysEnding(n int, end time.Time) evals.Range {
	return evals.Range{From: end.AddDate(0, 0, -n), To: end}
}

// storedEvalShapes is every shape of the list read a page asks for as of today, and two the stored
// figures do not answer: each preset of the Evals tab as a GET from midnight to midnight (L days
// ending today), with and without the compare range before it; each as the live view reads it at
// a moment of today (L+1 days ending tomorrow), with the compare range its query names and
// without; a repository's rows, which the stored figures of its cut answer; and, read from the day
// rows, a range of a length the tab does not offer and two repositories.
func storedEvalShapes(today time.Time, repo, other string) []evalShape {
	var shapes []evalShape
	for _, n := range []int{7, 14, 28, 84} {
		window, live, compare := evalDaysEnding(n, today), evalDaysEnding(n+1, today.AddDate(0, 0, 1)), evalDaysEnding(n, today.AddDate(0, 0, -n))
		shapes = append(shapes,
			evalShape{name: fmt.Sprintf("%d days", n), window: window, offered: true},
			evalShape{name: fmt.Sprintf("%d days with a compare range", n), window: window, compare: &compare, offered: true},
			evalShape{name: fmt.Sprintf("the live %d days", n), window: live, offered: true},
			evalShape{name: fmt.Sprintf("the live %d days with a compare range", n), window: live, compare: &compare, offered: true})
	}
	repoCompare, custom, customCompare := evalDaysEnding(28, today.AddDate(0, 0, -28)), evalDaysEnding(10, today), evalDaysEnding(10, today.AddDate(0, 0, -10))
	return append(shapes,
		evalShape{name: "28 days of " + repo + " with a compare range", window: evalDaysEnding(28, today), compare: &repoCompare, repos: []string{repo}, offered: true},
		evalShape{name: "the live 14 days of " + repo, window: evalDaysEnding(15, today.AddDate(0, 0, 1)), repos: []string{repo}, offered: true},
		evalShape{name: "10 days with a compare range", window: custom, compare: &customCompare},
		evalShape{name: "28 days of two repositories", window: evalDaysEnding(28, today), compare: &repoCompare, repos: []string{repo, other}})
}

// loggedAggs is the folds a logged series takes, each of one logged eval of the checks.
var loggedAggs = []string{evals.AggSum, evals.AggRatio, evals.AggWeightedMean, evals.AggQuantile, evals.AggMax, evals.AggLast}

// seedLoggedAggs registers, in project, one logged eval of each of loggedAggs, its one series
// folded by that agg, and logs its readings over the 100 days before today: on most days one or
// two, with an n where the fold weighs one, every third of them of repo too, and none on some days
// and none before the first, so its first day lies inside the longer ranges.
func seedLoggedAggs(t *testing.T, registry *evals.Store, project, repo string, today time.Time) {
	t.Helper()
	caller := model.Actor{Kind: "user", ID: "alice"}
	for aggIndex, agg := range loggedAggs {
		slug := "logged-" + strings.ReplaceAll(agg, "_", "-")
		if _, err := registry.Register(t.Context(), evals.Registration{Project: project, Slug: slug, Caller: caller,
			Series: []evals.SeriesChange{{Series: "value", SeriesComputationChange: evals.SeriesComputationChange{Agg: evals.Set(agg)}}}}); err != nil {
			t.Fatalf("register %s: %v", slug, err)
		}
		var rows []json.RawMessage
		for day := range 100 {
			if (day+aggIndex)%7 == 3 {
				continue
			}
			for reading := 0; reading <= day%2; reading++ {
				fields := map[string]any{"series": "value", "row_kind": "check", "row_key": fmt.Sprintf("%d-%d", day, reading),
					"value":       float64((day*37+reading*11+aggIndex*5)%97) / 4,
					"measured_at": today.AddDate(0, 0, day-100).Add(time.Duration(3+reading*7) * time.Hour)}
				if agg == evals.AggRatio || agg == evals.AggWeightedMean {
					fields["n"] = 1 + (day+reading)%5
				}
				if (day+reading)%3 == 0 {
					fields["attributes"] = map[string]any{"repo": repo}
				}
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatalf("encode a reading: %v", err)
				}
				rows = append(rows, encoded)
			}
		}
		if _, err := registry.WriteReadings(t.Context(), evals.Batch{Project: project, Slug: slug, Rows: rows, Caller: caller, BuiltBy: testServerBuild}); err != nil {
			t.Fatalf("log %s's readings: %v", slug, err)
		}
	}
}

// storedEvalsRig is the server and the moment the stored figures' read is checked over: the
// delivery project and its repositories, the golden Legion project beside it, and legion, a Legion
// project whose history begins a month before the moment.
type storedEvalsRig struct {
	served   *server
	projects []string
	now      time.Time
	today    time.Time
	delivery string
	repos    []string
	legion   string
}

// newStoredEvalsRig seeds the delivery history of 30 days, the Legion history of the golden
// windows, a Legion history of 30 trees a day apart ending at the golden history's end, its roots
// queued for a slot, and the logged evals of every agg over 100 days (seedLoggedAggs), and
// refreshes the day rows and their stored figures as the server's refresher writes them. The golden
// history begins five days before the rig's moment, after the start of every range a page asks for,
// so each of its figures reads no data; the month-long one gives the Legion evals values in the 7,
// 14 and 28 days a page reads.
func newStoredEvalsRig(t *testing.T) (*storedEvalsRig, func(statement string, args ...any)) {
	t.Helper()
	_, database, deps, served := newRegisteredTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistories(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	now := end.Add(-time.Minute)
	long := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGN", Trees: 30, Now: windows[0].Spec.Now, Spacing: 24 * time.Hour, Queued: true})
	storeLegionFacts(t, database, long.Project, windows[0].Spec.Now)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	seedLoggedAggs(t, registry, fixture.Project, fixture.DeployRepo, measures.Midnight(now))
	refreshEvalDays(t, registry, database, now)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("%s: %v", strings.Fields(statement)[0], err)
		}
	}
	return &storedEvalsRig{
		served: served, projects: []string{fixture.Project, windows[0].Spec.Project, long.Project}, now: now, today: measures.Midnight(now),
		delivery: fixture.Project, repos: fixture.Repos, legion: long.Project,
	}, exec
}

// read is the list read of every eval of the rig's projects in shape at the rig's moment
// (readEvalShape).
func (r *storedEvalsRig) read(t *testing.T, shape evalShape) (evalListResponse, []byte) {
	t.Helper()
	return readEvalShape(t, r.served, r.projects, r.now, shape)
}

// readEvalShape is the list read of every eval of projects in shape, as both list routes and the
// live view read it, at now, and its JSON body.
func readEvalShape(t *testing.T, served *server, projects []string, now time.Time, shape evalShape) (evalListResponse, []byte) {
	t.Helper()
	ranges := evalRanges{window: shape.window, compare: shape.compare, from: shape.window.From, to: shape.window.To}
	if shape.compare != nil {
		ranges.compareFrom, ranges.compareTo = shape.compare.From, shape.compare.To
	}
	answer, err := served.readEvalList(t.Context(), evalListRequest{
		evalReadQuery: evalReadQuery{ranges: ranges, repos: shape.repos, limit: evalPageMax},
		projectsIn:    func(context.Context, pgx.Tx) ([]string, error) { return projects, nil },
	}, now)
	if err != nil {
		t.Fatalf("read %s: %v", shape.name, err)
	}
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode %s: %v", shape.name, err)
	}
	return answer, body
}

// TestTheStoredFiguresAnswerWhatTheDayRowsAnswer: over the delivery, Legion and logged histories,
// with every eval of each computed source and a logged eval of each agg (the quantile among
// them), the list read of every shape a page asks for answers byte for byte what the read of the
// day rows answers, folded in day order: each preset with and without its compare range, as a GET
// and as the live view's L+1 days, a repository's rows, and ranges that start before a series'
// first day, the per-day arrays of the window and of the compare range (days, compare_days) among
// them, which hold days of their own. The read of the day rows is the same server's once no
// project's figures stand built (eval_figures_built), which a range of a length the tab does not
// offer and a read of two repositories read anyway. The month-long Legion history gives its evals values in the shorter
// ranges, so the Legion figures compared are not all null against null. The negative control
// breaks the stored
// figures of one eval, every cut of them: every shape the stored figures answer
// then answers
// otherwise, and the two the day rows answer do not.
func TestTheStoredFiguresAnswerWhatTheDayRowsAnswer(t *testing.T) {
	t.Parallel()
	r, exec := newStoredEvalsRig(t)
	shapes := storedEvalShapes(r.today, r.repos[0], r.repos[1])
	for _, shape := range shapes {
		if got := evals.FiguresOffered(shape.repos, shape.window, shape.compare); got != shape.offered {
			t.Fatalf("%s: the stored figures offered %t, want %t", shape.name, got, shape.offered)
		}
	}
	stored := map[string][]byte{}
	figures, compareDays, legionValues := 0, 0, 0
	for _, shape := range shapes {
		answer, body := r.read(t, shape)
		stored[shape.name] = body
		for _, item := range answer.Items {
			figures += len(item.Series)
			for _, series := range item.Series {
				if shape.offered {
					compareDays += len(series.CompareDays)
				}
			}
			if item.Definition.Kind != evals.KindLegion {
				continue
			}
			for _, series := range item.Series {
				for _, value := range series.Window {
					if value != nil {
						legionValues++
					}
				}
			}
		}
	}
	if compareDays == 0 {
		t.Fatal("no shape the stored figures answer holds a day of its compare range, so their compare_days are compared with nothing")
	}
	if legionValues == 0 {
		t.Fatalf("no shape states a value of a Legion eval, so the Legion figures compared are null against null")
	}
	marks := r.figuresBuilt(t)
	if len(marks) != len(r.projects) {
		t.Fatalf("the figures of %d of the %d projects stand built (%v), so a read answers from the day rows where the stored figures should", len(marks), len(r.projects), marks)
	}
	exec(`delete from eval_figures_built`)
	sample := map[string][]byte{}
	for _, shape := range shapes {
		_, sample[shape.name] = r.read(t, shape)
		if !bytes.Equal(stored[shape.name], sample[shape.name]) {
			t.Errorf("%s: the stored figures answer %d bytes that differ from the day rows' %d bytes at byte %d", shape.name,
				len(stored[shape.name]), len(sample[shape.name]), firstDifference(stored[shape.name], sample[shape.name]))
		}
	}
	t.Logf("%d shapes over %d projects, %d series in all (%d values of the Legion evals), %d days of compare ranges among the stored figures' answers, answered alike by the stored figures and the day rows",
		len(shapes), len(r.projects), figures, legionValues, compareDays)

	for project, through := range marks {
		exec(`insert into eval_figures_built (project_key, through, built_by) values ($1, $2, $3)`, project, through, testServerBuild)
	}
	broken := evals.EvalID(r.delivery, evals.SlugDeployFrequency)
	exec(`update eval_range_figures set figures = '\x0000'::bytea where eval_id = $1`, broken)
	for _, shape := range shapes {
		_, body := r.read(t, shape)
		if differs := !bytes.Equal(body, sample[shape.name]); differs != shape.offered {
			t.Errorf("%s: with %s's stored figures broken the answer differs from the day rows' %t, want %t", shape.name, broken, differs, shape.offered)
		}
	}
}

// firstDifference is the first index at which a and b differ.
func firstDifference(a, b []byte) int {
	for index := range min(len(a), len(b)) {
		if a[index] != b[index] {
			return index
		}
	}
	return min(len(a), len(b))
}

// figuresBuilt is how far each of the rig's projects' figures stand built (figuresBuiltOf).
func (r *storedEvalsRig) figuresBuilt(t *testing.T) map[string]time.Time {
	t.Helper()
	return figuresBuiltOf(t, r.served.deps.Store.Pool, r.projects)
}

// figuresBuiltOf is how far each of projects' figures stand built, read in pool: the end day
// eval_figures_built holds of each, which a read of the stored figures reads.
func figuresBuiltOf(t *testing.T, pool *store.Pool, projects []string) map[string]time.Time {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select project_key, through from eval_figures_built where project_key = any($1)`, projects)
	if err != nil {
		t.Fatalf("read how far the figures of %v are built: %v", projects, err)
	}
	built := map[string]time.Time{}
	var project string
	var through time.Time
	if _, err := pgx.ForEachRow(rows, []any{&project, &through}, func() error {
		built[project] = measures.Midnight(through)
		return nil
	}); err != nil {
		t.Fatalf("read how far the figures of %v are built: %v", projects, err)
	}
	return built
}

// TestTheStoredFiguresAnswerK12sHandFolds (k12 over the stored figures): for one count
// (deploy-frequency's deploys), one hours measure (legion-slot-wait's hours, of the month-long
// Legion history, whose roots wait for a slot), one share (deploy-run-success's runs) and one
// quantile (pr-open-to-merge's minutes), the 14-day read with its compare range, which the stored
// figures answer, states for the window and the compare range each statistic k12 folds by hand
// from the project's day rows of the range, its own and those of the owner keys of its published
// default repository (eval_project_owners, R15): sums over the days, the share's summed numerator
// over its summed denominator, the quantile's ranks of every sample of the range merged and
// sorted, interpolated here as k12 states the rule rather than by the code under test, the Legion
// figures to four decimals, and no data for a range that starts before the series' first day;
// within k12's tolerance, and bit for bit. Each measure states at least one value its hand fold
// compares, so none is null against null alone.
func TestTheStoredFiguresAnswerK12sHandFolds(t *testing.T) {
	t.Parallel()
	r, _ := newStoredEvalsRig(t)
	compare := evalDaysEnding(14, r.today.AddDate(0, 0, -14))
	shape := evalShape{name: "14 days with a compare range", window: evalDaysEnding(14, r.today), compare: &compare, offered: true}
	if !evals.FiguresOffered(nil, shape.window, shape.compare) || len(r.figuresBuilt(t)) != len(r.projects) {
		t.Fatalf("the stored figures do not answer %s: built %v", shape.name, r.figuresBuilt(t))
	}
	// quantile is p's quantile of sorted, which holds at least one sample, by k12's rule: linear
	// between the two nearest ranks, each product converted to float64 before the sum it feeds,
	// which keeps the compiler from fusing a multiply and an add into one rounding.
	quantile := func(sorted []float64, p float64) float64 {
		rank := float64(float64(len(sorted)-1) * p)
		low := int(math.Floor(rank))
		if low+1 >= len(sorted) {
			return sorted[len(sorted)-1]
		}
		return sorted[low] + float64((sorted[low+1]-sorted[low])*(rank-float64(low)))
	}
	answer, _ := r.read(t, shape)
	measured := []struct{ kind, project, slug, series string }{
		{"count", r.delivery, evals.SlugDeployFrequency, measures.SeriesDeploys},
		{"hours", r.legion, evals.SlugLegionSlotWait, "hours"},
		{"share", r.delivery, evals.SlugDeployRunSuccess, measures.SeriesRuns},
		{"quantile", r.delivery, evals.SlugPROpenToMerge, measures.SeriesMinutes},
	}
	folds, exact, examined := 0, 0, 0
	pool := r.served.deps.Store.Pool
	for _, m := range measured {
		evalID := evals.EvalID(m.project, m.slug)
		foldedRows, stated := 0, 0
		values := seriesValues(t, findEval(t, answer.Items, m.project, m.slug), m.series)
		var first *time.Time
		// The project's rows are its own and its default repository's owner keys' (evals.OwnerKey),
		// each of the eval of the same slug.
		if err := pool.QueryRow(t.Context(), `
			select min(d.day) from eval_project_owners o join eval_days d on d.project_key = o.owner
			where o.project_key = $1 and d.eval_id = o.owner || substr($2, length($1) + 1) and d.writer = 'computation' and d.series = $3 and d.repo = ''`,
			m.project, evalID, m.series).Scan(&first); err != nil {
			t.Fatalf("read %s's first day: %v", evalID, err)
		}
		for _, which := range []struct {
			label string
			days  evals.Range
			got   evals.Statistics
		}{{"window", shape.window, values.Window}, {"compare", compare, values.Compare}} {
			rows, err := pool.Query(t.Context(), `
				select d.n, d.sum, d.num, d.den, d.samples, d.counts from eval_project_owners o join eval_days d on d.project_key = o.owner
				where o.project_key = $1 and d.eval_id = o.owner || substr($2, length($1) + 1) and d.writer = 'computation' and d.series = $3
				  and d.repo = '' and d.listed and d.day >= $4::date and d.day < $5::date order by d.day, d.project_key`,
				m.project, evalID, m.series, which.days.From, which.days.To)
			if err != nil {
				t.Fatalf("read %s's day rows: %v", evalID, err)
			}
			type handRow struct {
				N             int
				Sum, Num, Den *float64
				Samples       []float64
				Counts        map[string]float64
			}
			held, err := pgx.CollectRows(rows, pgx.RowToStructByPos[handRow])
			if err != nil {
				t.Fatalf("read %s's day rows: %v", evalID, err)
			}
			examined, foldedRows = examined+len(held), foldedRows+len(held)
			var sum, num, den, withPRs float64
			var merged []float64
			for _, row := range held {
				sum += measures.ValueOrZero(row.Sum)
				num += measures.ValueOrZero(row.Num)
				den += measures.ValueOrZero(row.Den)
				withPRs += row.Counts[measures.CountWithPRs]
				merged = append(merged, row.Samples...)
			}
			slices.Sort(merged)
			hand := map[string]*float64{}
			days := float64(which.days.To.Sub(which.days.From) / (24 * time.Hour))
			fourDecimals := func(value float64) float64 { return math.Round(value*1e4) / 1e4 }
			switch m.kind {
			case "count":
				hand["deploys"], hand["with_prs"], hand["per_day"], hand["with_prs_per_day"] = &sum, &withPRs, new(sum/days), new(withPRs/days)
			case "share":
				hand["rate"] = nil
				if den != 0 {
					hand["rate"] = new(num / den)
				}
			case "quantile":
				hand["n"] = new(float64(len(merged)))
				if len(merged) > 0 {
					hand["median"], hand["p90"], hand["max"] = new(quantile(merged, 0.5)), new(quantile(merged, 0.9)), new(merged[len(merged)-1])
				}
			case "hours":
				hand["hours"], hand["n"] = new(fourDecimals(sum)), new(float64(len(merged)))
				if len(merged) > 0 {
					hand["median"], hand["p90"] = new(fourDecimals(quantile(merged, 0.5))), new(fourDecimals(quantile(merged, 0.9)))
				}
			}
			if first == nil || which.days.From.Before(first.UTC()) {
				for name := range hand {
					hand[name] = nil
				}
			}
			for name, want := range hand {
				folds++
				if want != nil {
					stated++
				}
				got := which.got[name]
				bits := want == nil && got == nil || want != nil && got != nil && math.Float64bits(*want) == math.Float64bits(*got)
				if bits {
					exact++
				}
				if !bits && (want == nil || got == nil || math.Abs(*want-*got) > 1e-9*math.Max(1, math.Max(math.Abs(*want), math.Abs(*got)))) {
					t.Errorf("%s %s.%s %s.%s: the stored figures answer %s, k12's hand fold %s", m.kind, m.slug, m.series, which.label, name, showFigure(got), showFigure(want))
				} else if !bits {
					t.Errorf("%s %s.%s %s.%s: the stored figures answer %s, k12's hand fold %s, within its tolerance and not bit for bit",
						m.kind, m.slug, m.series, which.label, name, showFigure(got), showFigure(want))
				}
			}
		}
		if foldedRows == 0 || stated == 0 {
			t.Errorf("%s's %s holds %d day rows of %s in the window and the compare range, and the hand fold states %d values of them; want rows and values, so the fold compares a value",
				m.project, m.slug, foldedRows, m.series, stated)
		}
	}
	t.Logf("k12 over the stored figures: %d of %d hand folds bit for bit, over %d day rows", exact, folds, examined)
}

// TestOneEvalListAnswerReadsItsFiguresAndItsDayRowsFromOneSnapshot (failure 5 of
// dispatch://INFRA-2916): one answer of the list read is one database moment. The refresher's pass
// after a deploy's production job fails rewrites the day rows of its day and the stored figures
// they fold in one transaction, and it commits, on another connection, the moment the read's first
// statement of the stored range figures or of how far they are built ends. The answer the commit
// landed in is the answer before it, and the next answer is the whole new state, deploy-frequency's
// window in each the sum of its own days. A read whose statements read several moments answers,
// in the answer the commit lands in, the window's old figures beside the new days: the mixed delta
// a live evals view sent for half a second after a settings rewrite.
func TestOneEvalListAnswerReadsItsFiguresAndItsDayRowsFromOneSnapshot(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	commits := storetest.NewCommitBetweenReads("eval_range_figures", "eval_figures_built")
	_, _, deps, served := newRegisteredTestServer(t, testServerOptions{store: storetest.TracedStore(t, database, commits)})
	fixture, windows := seedEvalHistories(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	now := end.Add(-time.Minute)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	shape := evalShape{name: "7 days", window: evalDaysEnding(7, measures.Midnight(now)), offered: true}
	if built := figuresBuiltOf(t, database.Pool, []string{fixture.Project}); built[fixture.Project].Before(shape.window.To) {
		t.Fatalf("%s's figures stand built through %v, so the read of %s answers from the day rows", fixture.Project, built, shape.name)
	}
	// read is the list read of the delivery project's evals in shape, and its body without its
	// freshness, which a refresher pass may move without moving a value.
	read := func() (evalListResponse, []byte) {
		t.Helper()
		answer, _ := readEvalShape(t, served, []string{fixture.Project}, now, shape)
		answer.Freshness = evalFreshness{}
		body, err := json.Marshal(answer)
		if err != nil {
			t.Fatalf("encode %s: %v", shape.name, err)
		}
		return answer, body
	}
	// deploys is deploy-frequency's deploys over the window of answer, and the sum of its days'.
	deploys := func(answer evalListResponse) (window, days float64) {
		t.Helper()
		values := seriesValues(t, findEval(t, answer.Items, fixture.Project, evals.SlugDeployFrequency), measures.SeriesDeploys)
		if values.Window["deploys"] == nil {
			t.Fatalf("deploy-frequency states no deploys over %s", shape.name)
		}
		for _, day := range values.Days {
			days += measures.ValueOrZero(day.Statistics["deploys"])
		}
		return *values.Window["deploys"], days
	}
	before, beforeBody := read()
	if window, days := deploys(before); window != days {
		t.Fatalf("before the commit deploy-frequency's window holds %v deploys and its days %v", window, days)
	}

	var runID int64
	for _, apply := range fixture.Applies {
		if !apply.CompletedAt.Before(shape.window.From) && apply.CompletedAt.Before(shape.window.To) {
			runID = apply.RunID
		}
	}
	repo := ""
	for _, run := range fixture.Runs {
		if run.RunID == runID {
			repo = run.Repo
		}
	}
	if repo == "" {
		t.Fatalf("no production apply of the history completed in %s..%s", shape.window.From, shape.window.To)
	}
	if _, err := database.Pool.Exec(t.Context(), `update delivery_run_jobs set conclusion = 'failure' where repo = $1 and run_id = $2 and name = any($3)`,
		repo, runID, fixture.Settings.ProductionJobNames); err != nil {
		t.Fatalf("fail the production job of run %d: %v", runID, err)
	}
	commits.Arm(func() { refreshEvalDays(t, registry, database, now) })
	during, duringBody := read()
	if !commits.Fired() {
		t.Fatal("no statement of the list read read eval_range_figures or eval_figures_built, so no commit landed during it")
	}
	if !bytes.Equal(duringBody, beforeBody) {
		window, days := deploys(during)
		beforeWindow, beforeDays := deploys(before)
		t.Errorf("the answer the commit landed in differs from the answer before it at byte %d of %d: deploy-frequency's window holds %v deploys and its days %v, where before it they held %v and %v",
			firstDifference(duringBody, beforeBody), len(beforeBody), window, days, beforeWindow, beforeDays)
	}

	after, _ := read()
	beforeWindow, _ := deploys(before)
	window, days := deploys(after)
	if window == beforeWindow {
		t.Errorf("after the commit deploy-frequency's window holds %v deploys, as before it: the failed production job of run %d moved nothing", window, runID)
	}
	if window != days {
		t.Errorf("after the commit deploy-frequency's window holds %v deploys and its days %v", window, days)
	}
}
