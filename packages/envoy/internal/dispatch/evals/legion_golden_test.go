package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// goldenComputeFiles are the reports the base head's Legion section route answered at 02a7b3c6
// (metrics.Compute, since deleted) over one legiontest history, one file per window holding the
// three scopes' reports, recorded before the Legion folds replaced its aggregation
// (legion/metrics/golden_record_test.go, deleted with it: run after the change, it would have
// recorded the new code's answer).
var goldenComputeFiles = filepath.Join("..", "legion", "metrics", "testdata", "golden_compute", "*.json")

// goldenReports is one golden file.
type goldenReports struct {
	Head string `json:"head"`
	Spec struct {
		Project string    `json:"project"`
		Trees   int       `json:"trees"`
		Now     time.Time `json:"now"`
	} `json:"spec"`
	Window struct {
		Name string    `json:"name"`
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	Scopes  []metrics.Scope         `json:"scopes"`
	Reports map[string]goldenReport `json:"reports"`
}

// goldenReport is one scope's report in the shape the base head's route wrote it, in the fields
// this comparison reads: the issues that finished, the work time with its table of issues, the
// seven bars and the idle share, the rework figures with their table, and the splitting weeks,
// table and root figures.
type goldenReport struct {
	Finished int `json:"finished"`
	WorkTime struct {
		MedianHours float64 `json:"median_hours"`
		P90Hours    float64 `json:"p90_hours"`
		Issues      []struct {
			Key         string     `json:"key"`
			Root        bool       `json:"root"`
			StartedAt   time.Time  `json:"started_at"`
			ClosedAt    time.Time  `json:"closed_at"`
			MergedAt    *time.Time `json:"merged_at"`
			Hours       float64    `json:"hours"`
			ParkedHours float64    `json:"parked_hours"`
			QueuedHours float64    `json:"queued_hours"`
		} `json:"issues"`
	} `json:"work_time"`
	PhaseTime struct {
		Bars []struct {
			Role                string           `json:"role"`
			Hours               float64          `json:"hours"`
			Issues              int              `json:"issues"`
			MedianHoursPerIssue float64          `json:"median_hours_per_issue"`
			Segments            metrics.Segments `json:"segments"`
		} `json:"bars"`
		IdleShare float64 `json:"idle_share"`
	} `json:"phase_time"`
	Rework struct {
		MedianReviewRounds          float64 `json:"median_review_rounds"`
		SentBackShare               float64 `json:"sent_back_share"`
		HoursAfterFirstReview       float64 `json:"hours_after_first_review"`
		MedianHoursAfterFirstReview float64 `json:"median_hours_after_first_review"`
		Issues                      []struct {
			Key                         string  `json:"key"`
			ReviewRounds                int     `json:"review_rounds"`
			SentBack                    bool    `json:"sent_back"`
			ImplementingHoursAfterFirst float64 `json:"implementing_hours_after_first_review"`
			TestingHoursAfterFirst      float64 `json:"testing_hours_after_first_review"`
		} `json:"issues"`
	} `json:"rework"`
	Splitting struct {
		Weeks []struct {
			Week     time.Time      `json:"week"`
			Filed    map[string]int `json:"filed"`
			Finished int            `json:"finished"`
		} `json:"weeks"`
		Filed []struct {
			Key        string    `json:"key"`
			At         time.Time `json:"at"`
			Class      string    `json:"class"`
			Filer      string    `json:"filer"`
			FilerIssue string    `json:"filer_issue"`
			FilerPhase string    `json:"filer_phase"`
		} `json:"filed"`
		FinishedRoots                int     `json:"finished_roots"`
		ChildrenPerRoot              float64 `json:"children_per_root"`
		MedianHoursRootsWithChildren float64 `json:"median_hours_roots_with_children"`
		MedianHoursRootsNoChildren   float64 `json:"median_hours_roots_without_children"`
	} `json:"splitting"`
}

// The tolerances of the comparison: the spec's 0.1 hour for an hour figure, and a share (or the
// median of a count) to the last of the four decimals both sides are rounded to. Every count, key,
// flag and moment is compared exactly.
const (
	goldenHours = 0.1
	goldenShare = 1e-9
)

// TestTheLegionFoldsOfTheStoredDayRowsAnswerTheBaseHeadsReports (k6's and k14's oracle): over the
// history the golden reports were recorded over, with its facts and filings stored as the facts
// consumer stores them, every day's Legion rows are written; then for each window of the golden
// files and each cut, the six folds over the window's stored rows, read as the eval routes read
// them (DayWindow), answer every number the base head's report answered at the matching scope:
// work time, each bar with its segments, the idle share, rework and splitting, whose root figures
// the base answered the same at every scope and the all cut carries. The report's tables of issues
// are no folds: each is compared with the stored rows of the window and cut, which the items route
// answers. Then one day's rows are deleted, and the same comparison must find numbers that moved.
func TestTheLegionFoldsOfTheStoredDayRowsAnswerTheBaseHeadsReports(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	windows, project, since := seedGoldenLegionHistory(t, r)
	spec := windows[0].Spec

	first, last := goldenLegionDays(windows)
	days := 0
	for day := first; day.Before(last); day = day.AddDate(0, 0, 1) {
		if err := r.registry.WriteLegionDay(ctx, project, day, since, testBuild); err != nil {
			t.Fatalf("write the Legion rows of %s on %s: %v", project, day.Format(time.DateOnly), err)
		}
		days++
	}
	stored := r.count(t, `select count(*) from eval_days where project_key = $1`, project)

	check := compareLegionWindows(t, r, project, windows)
	for _, differ := range check.differ {
		t.Error(differ)
	}
	if check.issues < 200 {
		t.Errorf("the windows counted %d finished issues, want the at least 200 the check is stated over", check.issues)
	}
	t.Logf("history %s, %d trees, as of %s (recorded at %s): %d days written, %d day rows stored", project, spec.Trees,
		spec.Now.Format(time.RFC3339), windows[0].Head, days, stored)
	for _, window := range windows {
		t.Logf("window %s to %s (%s), cuts %v", window.Window.From.Format(time.DateOnly), window.Window.To.Format(time.DateOnly),
			window.Window.Name, window.Scopes)
	}
	t.Logf("%d windows x %d cuts: %d numbers of the folds and %d of the issue tables compared, %d finished issues counted, %d differ",
		len(windows), len(windows[0].Scopes), check.folded, check.listed, check.issues, len(check.differ))

	// The negative control: one day's rows gone, the same comparison must see numbers move.
	control := time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC)
	deleted := r.count(t, `with gone as (delete from eval_days where project_key = $1 and day = $2 returning 1) select count(*) from gone`,
		project, control)
	moved := compareLegionWindows(t, r, project, windows)
	if len(moved.differ) == 0 {
		t.Errorf("with the %d rows of %s deleted the comparison found no number that moved, so it cannot fail", deleted, control.Format(time.DateOnly))
	}
	t.Logf("negative control: %d rows of %s deleted, %d of %d numbers differ, the first: %s", deleted, control.Format(time.DateOnly),
		len(moved.differ), moved.folded+moved.listed, firstOf(moved.differ))
}

// seedGoldenLegionHistory writes into r's database the history the golden reports were recorded
// over, with its facts and filings stored as the facts consumer stores them, and answers the
// golden windows, the history's project and its first observations.
func seedGoldenLegionHistory(t *testing.T, r rig) ([]goldenReports, string, LegionSince) {
	t.Helper()
	files, err := filepath.Glob(goldenComputeFiles)
	if err != nil || len(files) == 0 {
		t.Fatalf("list %s: %d files, %v", goldenComputeFiles, len(files), err)
	}
	windows := make([]goldenReports, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var window goldenReports
		if err := json.Unmarshal(raw, &window); err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}
		if len(windows) > 0 && window.Spec != windows[0].Spec {
			t.Fatalf("%s was recorded over %+v and %s over %+v, want one history", file, window.Spec, files[0], windows[0].Spec)
		}
		windows = append(windows, window)
	}
	spec := windows[0].Spec

	ctx := t.Context()
	pool := r.database.Pool
	fixture := legiontest.Seed(t, pool, legiontest.Spec{Project: spec.Project, Trees: spec.Trees, Now: spec.Now})
	project := fixture.Project
	rows, err := legionfacts.ComputeProject(ctx, pool, project, spec.Now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: project, Filings: projectFilings(t, project, spec.Now, r), Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", project, err)
	}
	since, err := r.registry.ReadLegionSince(ctx, project)
	if err != nil {
		t.Fatalf("read the first observations of %s: %v", project, err)
	}
	return windows, project, since
}

// goldenLegionDays is the first day of the golden windows and the end of the last.
func goldenLegionDays(windows []goldenReports) (first, last time.Time) {
	first, last = windows[0].Window.From, windows[0].Window.To
	for _, window := range windows {
		first, last = minTime(first, window.Window.From), maxTime(last, window.Window.To)
	}
	return first, last
}

// projectFilings is every filing a Legion session of project made, as the facts consumer's whole
// read writes them (legionfacts' filingsOf): metric 4's rows over the project's whole population,
// one per filed issue.
func projectFilings(t *testing.T, project string, now time.Time, r rig) []legionfacts.Filing {
	t.Helper()
	population, err := metrics.ReadProject(t.Context(), r.database.Pool, nil, project, now)
	if err != nil {
		t.Fatalf("read the population of %s: %v", project, err)
	}
	var filings []legionfacts.Filing
	seen := map[string]bool{}
	for _, filed := range metrics.ProjectFilings(population) {
		if seen[filed.Key] {
			continue
		}
		seen[filed.Key] = true
		filings = append(filings, legionfacts.Filing{
			Project: project, Issue: filed.Key, FiledAt: filed.At, FilerRole: filed.Filer, FilerIssue: filed.FilerIssue,
			FilerPhase: filed.FilerPhase, Class: filed.Class, RootKey: filed.Root,
		})
	}
	return filings
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func firstOf(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return lines[0]
}

// legionCheck is one comparison of the folds with the golden reports: how many numbers of the
// folds and of the issue tables it compared, how many finished issues the reports counted, and
// each number that differed.
type legionCheck struct {
	folded, listed, issues int
	differ                 []string
	where                  string
	tables                 bool
}

func (c *legionCheck) count() {
	if c.tables {
		c.listed++
	} else {
		c.folded++
	}
}

// within compares a folded value with the report's, a nil value standing for the report's 0,
// which is what the base answered where the folds answer no value.
func (c *legionCheck) within(name string, got *float64, want, tolerance float64) {
	c.count()
	value := 0.0
	if got != nil {
		value = *got
	}
	if math.Abs(value-want) > tolerance {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %s, want the base head's %g", c.where, name, showValue(got), want))
	}
}

func (c *legionCheck) hours(name string, got *float64, want float64) {
	c.within(name, got, want, goldenHours)
}

func (c *legionCheck) share(name string, got *float64, want float64) {
	c.within(name, got, want, goldenShare)
}

func (c *legionCheck) exact(name string, got, want any) {
	c.count()
	if got != want {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %v, want the base head's %v", c.where, name, got, want))
	}
}

func (c *legionCheck) moment(name string, got, want *time.Time) {
	c.count()
	if (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %v, want the base head's %v", c.where, name, got, want))
	}
}

func showValue(value *float64) string {
	if value == nil {
		return "no value"
	}
	return fmt.Sprint(*value)
}

// compareLegionWindows folds the stored Legion rows of each golden window in each cut and
// compares them, and the stored issue rows and filings of the window, with the window's reports.
func compareLegionWindows(t *testing.T, r rig, project string, windows []goldenReports) *legionCheck {
	t.Helper()
	ctx := t.Context()
	entries := make([]Entry, len(legionDaySlugs))
	for i, slug := range legionDaySlugs {
		// Each eval as the seed writes it (Ensure): its catalogue definition and series.
		catalogued := entryOf(t, slug)
		entries[i] = Entry{Definition: catalogued.definition, Series: catalogued.series}
		entries[i].Definition.EvalID = EvalID(project, slug)
	}
	check := &legionCheck{}
	for _, window := range windows {
		read, err := r.registry.DayWindow(ctx, project, entries, nil, Range{From: window.Window.From, To: window.Window.To}, nil)
		if err != nil {
			t.Fatalf("read the day rows of %s: %v", window.Window.Name, err)
		}
		parts := map[string][]measures.DayPart{}
		for _, row := range read {
			slug := strings.TrimPrefix(row.EvalID, project+"/")
			parts[slug] = append(parts[slug], row.Part)
		}
		closed, err := legionfacts.ReadClosedIn(ctx, r.database.Pool, project, window.Window.From, window.Window.To)
		if err != nil {
			t.Fatalf("read the issues closed in %s: %v", window.Window.Name, err)
		}
		filed, err := legionfacts.ReadFilingsIn(ctx, r.database.Pool, project, window.Window.From, window.Window.To)
		if err != nil {
			t.Fatalf("read the filings of %s: %v", window.Window.Name, err)
		}
		for _, cut := range window.Scopes {
			report, held := window.Reports[string(cut)]
			if !held {
				t.Fatalf("%s holds no report of scope %s", window.Window.Name, cut)
			}
			check.where = fmt.Sprintf("%s to %s, %s", window.Window.From.Format(time.DateOnly), window.Window.To.Format(time.DateOnly), cut)
			check.issues += report.Finished
			check.tables = false
			compareFolds(check, parts, cut, report)
			check.tables = true
			compareTables(check, closed, filed, cut, report)
		}
	}
	return check
}

// compareFolds compares every number of report with the folds of parts in cut.
func compareFolds(check *legionCheck, parts map[string][]measures.DayPart, cut metrics.Scope, report goldenReport) {
	finished := float64(report.Finished)

	work := metrics.FoldWorkTime(parts[SlugLegionWorkTime], cut)
	if work == nil {
		work = &metrics.SampleFold{}
	}
	check.exact("work time's issues", work.N, len(report.WorkTime.Issues))
	check.hours("work time's median", work.Median, report.WorkTime.MedianHours)
	check.hours("work time's p90", work.P90, report.WorkTime.P90Hours)

	phase := metrics.FoldPhaseTime(parts[SlugLegionPhaseTime], cut)
	if phase == nil {
		phase = &metrics.PhaseTimeFold{}
	}
	for _, bar := range report.PhaseTime.Bars {
		folded := phase.Bars[bar.Role]
		name := "the " + bar.Role + " bar's "
		check.exact(name+"issues counted", float64(folded.N), finished)
		check.exact(name+"issues", folded.Issues, bar.Issues)
		check.hours(name+"hours", &folded.Hours, bar.Hours)
		check.hours(name+"median per issue", folded.MedianPerIssue, bar.MedianHoursPerIssue)
		for _, segment := range []struct {
			name      string
			got, want float64
		}{
			{"waiting on a person", folded.Segments.WaitingOnPerson, bar.Segments.WaitingOnPerson},
			{"in a turn", folded.Segments.InATurn, bar.Segments.InATurn},
			{"CI wait", folded.Segments.CIWait, bar.Segments.CIWait},
			{"waiting on children", folded.Segments.WaitingOnChildren, bar.Segments.WaitingOnChildren},
			{"idle", folded.Segments.Idle, bar.Segments.Idle},
		} {
			check.hours(name+segment.name, &segment.got, segment.want)
		}
	}
	check.share("the idle share", phase.IdleShare, report.PhaseTime.IdleShare)

	rework := metrics.FoldRework(parts[SlugLegionRework], cut)
	if rework == nil {
		rework = &metrics.ReworkFold{}
	}
	rounds, sent, after := orEmpty(rework.ReviewRounds), rework.SentBack, orEmpty(rework.HoursAfterFirstReview)
	if sent == nil {
		sent = &metrics.ShareFold{}
	}
	sentBack := 0
	for _, issue := range report.Rework.Issues {
		if issue.SentBack {
			sentBack++
		}
	}
	check.exact("review rounds' issues", rounds.N, report.Finished)
	check.share("review rounds' median", rounds.Median, report.Rework.MedianReviewRounds)
	check.exact("issues sent back", sent.Num, sentBack)
	check.exact("issues the sent-back share is over", sent.Den, report.Finished)
	check.share("the sent-back share", sent.Share, report.Rework.SentBackShare)
	check.exact("hours after the first review's issues", after.N, report.Finished)
	check.hours("hours after the first review", &after.Sum, report.Rework.HoursAfterFirstReview)
	check.hours("hours after the first review's median", after.Median, report.Rework.MedianHoursAfterFirstReview)

	split := metrics.FoldSplitting(parts[SlugLegionSplitting], cut)
	if split == nil {
		split = &metrics.SplittingFold{}
	}
	check.exact("issues finished", split.Finished, report.Finished)
	check.exact("weeks", len(split.Weeks), len(report.Splitting.Weeks))
	for index, week := range report.Splitting.Weeks {
		if index >= len(split.Weeks) {
			break
		}
		folded := split.Weeks[index]
		name := "the week of " + week.Week.Format(time.DateOnly) + "'s "
		check.exact(name+"start", folded.Week.Format(time.DateOnly), week.Week.UTC().Format(time.DateOnly))
		check.exact(name+"issues finished", folded.Finished, week.Finished)
		for _, class := range slices.Sorted(maps.Keys(week.Filed)) {
			check.exact(name+class+" filings", folded.Filed[class], week.Filed[class])
		}
	}
	byClass := map[string]int{}
	for _, filing := range report.Splitting.Filed {
		byClass[filing.Class]++
	}
	for _, class := range []string{metrics.ClassInTreeAtDesign, metrics.ClassInTreeLater, metrics.ClassOutOfTree, metrics.ClassByTheController} {
		check.exact(class+" filings", split.Filed[class], byClass[class])
	}
	// The root figures count roots alone at every scope: the all cut carries them, the roots cut
	// carries the same, and the children cut none.
	all := metrics.FoldSplitting(parts[SlugLegionSplitting], metrics.ScopeAll)
	roots := &metrics.RootsFold{}
	if all != nil && all.Roots != nil {
		roots = all.Roots
	}
	check.exact("finished roots", roots.FinishedRoots, report.Splitting.FinishedRoots)
	check.share("children per root", roots.ChildrenPerRoot, report.Splitting.ChildrenPerRoot)
	check.hours("roots with children's median", orEmpty(roots.HoursWithChildren).Median, report.Splitting.MedianHoursRootsWithChildren)
	check.hours("roots with no children's median", orEmpty(roots.HoursWithoutChildren).Median, report.Splitting.MedianHoursRootsNoChildren)
	switch {
	case cut == metrics.ScopeChildren && split.Roots != nil:
		check.exact("the children cut's root figures", "a value", "no value")
	case cut == metrics.ScopeRoots && all != nil && all.Roots != nil:
		check.exact("the roots cut's finished roots", orRoots(split.Roots).FinishedRoots, all.Roots.FinishedRoots)
		check.exact("the roots cut's children", orRoots(split.Roots).Children, all.Roots.Children)
	}
}

func orEmpty(fold *metrics.SampleFold) *metrics.SampleFold {
	if fold == nil {
		return &metrics.SampleFold{}
	}
	return fold
}

func orRoots(fold *metrics.RootsFold) *metrics.RootsFold {
	if fold == nil {
		return &metrics.RootsFold{}
	}
	return fold
}

// compareTables compares the report's tables of issues with the stored rows of the window in cut:
// the work-time and rework rows with the issue_facts rows of the issues that closed in it, and the
// splitting rows with the issue_filings rows of the filings made in it. A table's title and
// priority are the issue's own columns, which no stored row holds, so they are left out.
func compareTables(check *legionCheck, closed []legionfacts.Row, filed []legionfacts.Filing, cut metrics.Scope, report goldenReport) {
	rows := map[string]legionfacts.Row{}
	timed := 0
	for _, row := range closed {
		if inCutOf(cut, row.RootKey == row.Issue) {
			rows[row.Issue] = row
			if row.StartedAt != nil {
				timed++
			}
		}
	}
	check.exact("issues closed in the window", len(rows), report.Finished)
	check.exact("issues timed in the window", timed, len(report.WorkTime.Issues))
	for _, issue := range report.WorkTime.Issues {
		row, held := rows[issue.Key]
		if !held {
			check.exact(issue.Key+"'s stored row", "none", "one")
			continue
		}
		check.exact(issue.Key+"'s root", row.RootKey == row.Issue, issue.Root)
		check.moment(issue.Key+"'s start", row.StartedAt, &issue.StartedAt)
		check.moment(issue.Key+"'s close", row.ClosedAt, &issue.ClosedAt)
		check.moment(issue.Key+"'s merge", row.MergedAt, issue.MergedAt)
		check.hours(issue.Key+"'s hours", &row.WorkHours, issue.Hours)
		check.hours(issue.Key+"'s parked hours", &row.ParkedHours, issue.ParkedHours)
		check.hours(issue.Key+"'s queued hours", &row.QueuedHours, issue.QueuedHours)
	}
	check.exact("rework rows", len(report.Rework.Issues), len(rows))
	for _, issue := range report.Rework.Issues {
		row, held := rows[issue.Key]
		if !held {
			check.exact(issue.Key+"'s stored row", "none", "one")
			continue
		}
		check.exact(issue.Key+"'s review rounds", row.ReviewRounds, issue.ReviewRounds)
		check.exact(issue.Key+"'s send-back", row.SentBack, issue.SentBack)
		check.hours(issue.Key+"'s implementing hours after the first review", &row.ImplementingAfterFirstReview, issue.ImplementingHoursAfterFirst)
		check.hours(issue.Key+"'s testing hours after the first review", &row.TestingAfterFirstReview, issue.TestingHoursAfterFirst)
	}
	filings := map[string]legionfacts.Filing{}
	for _, filing := range filed {
		if inCutOf(cut, filing.RootKey == filing.Issue) {
			filings[filing.Issue] = filing
		}
	}
	check.exact("filings in the window", len(filings), len(report.Splitting.Filed))
	for _, row := range report.Splitting.Filed {
		filing, held := filings[row.Key]
		if !held {
			check.exact(row.Key+"'s stored filing", "none", "one")
			continue
		}
		check.moment(row.Key+"'s filing time", &filing.FiledAt, &row.At)
		check.exact(row.Key+"'s class", filing.Class, row.Class)
		check.exact(row.Key+"'s filer", filing.FilerRole, row.Filer)
		check.exact(row.Key+"'s filer issue", filing.FilerIssue, row.FilerIssue)
		check.exact(row.Key+"'s filer phase", filing.FilerPhase, row.FilerPhase)
	}
}

// inCutOf is whether an item counts in cut, by the rule the items route reads the stored root by:
// root_key = issue_key is a root.
func inCutOf(cut metrics.Scope, root bool) bool {
	switch cut {
	case metrics.ScopeRoots:
		return root
	case metrics.ScopeChildren:
		return !root
	}
	return true
}
