package metrics

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// goldenComputeFiles are the reports the base's Legion metrics computation (metrics.Compute, which
// this package no longer holds) answers over one legiontest history, one file per window holding
// the three scopes' reports. They are 8f6e8562's answers, recorded on a checkout of that base head
// with the seeder (legion/legiontest) of 02a7b3c6 and a recorder test of 8a78cf55, which is gone.
// They were 02a7b3c6's answers until the base changed its rules on purpose (8beac41a07: retro is
// on the implementer's bar, a handoff starts the work time only of an issue no worker wrote on, an
// issue first observed in flight keeps its earlier phases; 687c7048d5: after a send-back a role's
// first event starts no phase), so the oracle is the base's own code over the same history. The
// base's later rules (2eed8f979e: an issue Dispatch observed stays a Legion issue; 22bd759ecb: a
// null phase mark ends no phase; e2a409a4f5: an issue the daemon drops keeps the slot it held;
// 9f994275a7: an hour out of the workflow before the close is outside the work time and on no bar)
// moved no value of this history, which holds no null phase mark, no reopen, no `triage` write
// inside a work time and no `done` observed before a close.
var goldenComputeFiles = filepath.Join("testdata", "golden_compute", "*.json")

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
	Scopes  []Scope                 `json:"scopes"`
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
			Role                string   `json:"role"`
			Hours               float64  `json:"hours"`
			Issues              int      `json:"issues"`
			MedianHoursPerIssue float64  `json:"median_hours_per_issue"`
			Segments            Segments `json:"segments"`
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
// median of a count) to 1e-9. Every count, key, flag and moment is compared exactly.
const (
	goldenHours = 0.1
	goldenShare = 1e-9
)

// TestTheLegionFoldsAnswerTheBaseHeadsReports, the oracle of the Legion day builders and folds:
// over the history the golden reports were recorded over (legiontest.Seed of the files' spec),
// the parts of every day are built from ReadProject at the files' now (windowOf: each issue's
// facts on its close day, each filing on its day), and for each window of the golden files the
// parts whose day is in [from, to) are combined in each cut by the folds. They answer every number
// the base head's report answered at the matching scope: work time, each bar with its segments,
// the idle share, rework and splitting, whose root figures the base answered the same at every
// scope and the all cut carries. The report's tables of issues are no folds: each is compared with
// the facts of the issues that closed in the window (IssueFacts) and the filings made in it
// (IssueFiling). An hour matches to 0.1 hour and a share to 1e-9; a count, a key, a flag and a
// time match exactly. The test fails below 200 finished issues. Then the parts of 2026-09-28 are
// removed, and the same comparison must find numbers that moved.
func TestTheLegionFoldsAnswerTheBaseHeadsReports(t *testing.T) {
	windows := readGoldenReports(t)
	spec := windows[0].Spec
	database := storetest.Open(t)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: spec.Project, Trees: spec.Trees, Now: spec.Now})
	population, err := ReadProject(t.Context(), database.Pool, unrenamed(), fixture.Project, spec.Now)
	if err != nil {
		t.Fatalf("read %s: %v", fixture.Project, err)
	}
	built := windowOf(population, ScopeAll)

	check := compareGoldenWindows(built, windows, nil)
	for _, differ := range check.differ {
		t.Error(differ)
	}
	if len(check.finished) < 200 {
		t.Errorf("the windows compared %d distinct finished issues, want the at least 200 the check is stated over", len(check.finished))
	}
	t.Logf("history %s, %d trees, as of %s (recorded at %s): %d issues closed with facts, %d filings",
		fixture.Project, spec.Trees, spec.Now.Format(time.RFC3339), windows[0].Head, len(built.facts), len(built.filed))
	t.Logf("%d windows x %d cuts: %d numbers of the folds and %d of the issue tables compared, %d distinct finished issues, %d differ",
		len(windows), len(windows[0].Scopes), check.folded, check.listed, len(check.finished), len(check.differ))

	// The negative control: one day's parts gone, the same comparison must see numbers move.
	control := time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC)
	moved := compareGoldenWindows(built, windows, &control)
	if len(moved.differ) == 0 {
		t.Errorf("with the parts of %s removed the comparison found no number that moved, so it cannot fail", control.Format(time.DateOnly))
	}
	t.Logf("negative control: the parts of %s removed, %d of %d numbers differ, the first: %s", control.Format(time.DateOnly),
		len(moved.differ), moved.folded+moved.listed, goldenFirstOf(moved.differ))
}

// readGoldenReports reads every golden file, which must all be of one history.
func readGoldenReports(t *testing.T) []goldenReports {
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
	return windows
}

func goldenFirstOf(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return lines[0]
}

// goldenCheck is one comparison of the folds with the golden reports: how many numbers of the
// folds and of the issue tables it compared, the distinct issues that closed with facts in the
// windows it compared in the all cut, and each number that differed.
type goldenCheck struct {
	folded, listed int
	finished       map[string]struct{}
	differ         []string
	where          string
	tables         bool
}

func (c *goldenCheck) count() {
	if c.tables {
		c.listed++
	} else {
		c.folded++
	}
}

// within compares a folded value with the report's, a nil value standing for the report's 0,
// which is what the base answered where the folds answer no value.
func (c *goldenCheck) within(name string, got *float64, want, tolerance float64) {
	c.count()
	value := 0.0
	if got != nil {
		value = *got
	}
	if math.Abs(value-want) > tolerance {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %s, want the base head's %g", c.where, name, goldenValue(got), want))
	}
}

func (c *goldenCheck) hours(name string, got *float64, want float64) {
	c.within(name, got, want, goldenHours)
}

func (c *goldenCheck) share(name string, got *float64, want float64) {
	c.within(name, got, want, goldenShare)
}

func (c *goldenCheck) exact(name string, got, want any) {
	c.count()
	if got != want {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %v, want the base head's %v", c.where, name, got, want))
	}
}

func (c *goldenCheck) moment(name string, got, want *time.Time) {
	c.count()
	if (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
		c.differ = append(c.differ, fmt.Sprintf("%s: %s is %v, want the base head's %v", c.where, name, got, want))
	}
}

func goldenValue(value *float64) string {
	if value == nil {
		return "no value"
	}
	return fmt.Sprint(*value)
}

// goldenParts are the four evals' parts of one window, each the parts of built whose day is in
// [from, to), and none of the day removed where removed is set.
type goldenParts struct {
	workTime, phaseTime, rework, splitting []measures.DayPart
}

func goldenPartsOf(built window, from, to time.Time, removed *time.Time) goldenParts {
	keep := func(parts []measures.DayPart) []measures.DayPart {
		var kept []measures.DayPart
		for _, part := range parts {
			if part.Day.Before(from) || !part.Day.Before(to) || removed != nil && part.Day.Equal(*removed) {
				continue
			}
			kept = append(kept, part)
		}
		return kept
	}
	return goldenParts{
		workTime: keep(built.workTimeParts), phaseTime: keep(built.phaseTimeParts),
		rework: keep(built.reworkParts), splitting: keep(built.splittingParts),
	}
}

// compareGoldenWindows folds built's parts of each golden window in each cut and compares them,
// and the facts and filings of the window, with the window's reports. With removed set, the parts
// of that day are left out of every window, and the facts and filings of the window stay whole.
func compareGoldenWindows(built window, windows []goldenReports, removed *time.Time) *goldenCheck {
	check := &goldenCheck{finished: map[string]struct{}{}}
	for _, window := range windows {
		parts := goldenPartsOf(built, window.Window.From, window.Window.To, removed)
		var closed []string
		for _, key := range slices.Sorted(maps.Keys(built.facts)) {
			if at := built.facts[key].ClosedAt; at != nil && !at.Before(window.Window.From) && at.Before(window.Window.To) {
				closed = append(closed, key)
			}
		}
		var filed []IssueFiling
		for _, filing := range built.filed {
			if !filing.At.Before(window.Window.From) && filing.At.Before(window.Window.To) {
				filed = append(filed, filing)
			}
		}
		for _, cut := range window.Scopes {
			report, held := window.Reports[string(cut)]
			if !held {
				check.differ = append(check.differ, fmt.Sprintf("%s holds no report of scope %s", window.Window.Name, cut))
				continue
			}
			check.where = fmt.Sprintf("%s to %s, %s", window.Window.From.Format(time.DateOnly), window.Window.To.Format(time.DateOnly), cut)
			if cut == ScopeAll {
				for _, key := range closed {
					check.finished[key] = struct{}{}
				}
			}
			check.tables = false
			compareGoldenFolds(check, parts, cut, report)
			check.tables = true
			compareGoldenTables(check, built.facts, closed, filed, cut, report)
		}
	}
	return check
}

// compareGoldenFolds compares every number of report with the folds of parts in cut.
func compareGoldenFolds(check *goldenCheck, parts goldenParts, cut Scope, report goldenReport) {
	finished := float64(report.Finished)

	work := FoldWorkTime(parts.workTime, cut)
	if work == nil {
		work = &SampleFold{}
	}
	check.exact("work time's issues", work.N, len(report.WorkTime.Issues))
	check.hours("work time's median", work.Median, report.WorkTime.MedianHours)
	check.hours("work time's p90", work.P90, report.WorkTime.P90Hours)

	phase := FoldPhaseTime(parts.phaseTime, cut)
	if phase == nil {
		phase = &PhaseTimeFold{}
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

	rework := FoldRework(parts.rework, cut)
	if rework == nil {
		rework = &ReworkFold{}
	}
	rounds, sent, after := goldenSamples(rework.ReviewRounds), rework.SentBack, goldenSamples(rework.HoursAfterFirstReview)
	if sent == nil {
		sent = &ShareFold{}
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

	split := FoldSplitting(parts.splitting, cut)
	if split == nil {
		split = &SplittingFold{}
	}
	check.exact("issues finished", split.Finished, report.Finished)
	// The base head's report counted splitting by the UTC week, the week metric 4 has always
	// counted by, where the folds answer it by the day: each of the report's weeks is the fold of
	// the window's parts of that week's days.
	weeks := splittingWeeks(parts.splitting, cut)
	check.exact("weeks", len(weeks), len(report.Splitting.Weeks))
	for index, week := range report.Splitting.Weeks {
		if index >= len(weeks) {
			break
		}
		folded := weeks[index]
		name := "the week of " + week.Week.Format(time.DateOnly) + "'s "
		check.exact(name+"start", folded.start.Format(time.DateOnly), week.Week.UTC().Format(time.DateOnly))
		check.exact(name+"issues finished", folded.fold.Finished, week.Finished)
		for _, class := range slices.Sorted(maps.Keys(week.Filed)) {
			check.exact(name+class+" filings", folded.fold.Filed[class], week.Filed[class])
		}
	}
	byClass := map[string]int{}
	for _, filing := range report.Splitting.Filed {
		byClass[filing.Class]++
	}
	for _, class := range []string{ClassInTreeAtDesign, ClassInTreeLater, ClassOutOfTree, ClassByTheController} {
		check.exact(class+" filings", split.Filed[class], byClass[class])
	}
	// The root figures count roots alone at every scope: the all cut carries them, the roots cut
	// carries the same, and the children cut none.
	all := FoldSplitting(parts.splitting, ScopeAll)
	roots := &RootsFold{}
	if all != nil && all.Roots != nil {
		roots = all.Roots
	}
	check.exact("finished roots", roots.FinishedRoots, report.Splitting.FinishedRoots)
	check.share("children per root", roots.ChildrenPerRoot, report.Splitting.ChildrenPerRoot)
	check.hours("roots with children's median", goldenSamples(roots.HoursWithChildren).Median, report.Splitting.MedianHoursRootsWithChildren)
	check.hours("roots with no children's median", goldenSamples(roots.HoursWithoutChildren).Median, report.Splitting.MedianHoursRootsNoChildren)
	switch {
	case cut == ScopeChildren && split.Roots != nil:
		check.exact("the children cut's root figures", "a value", "no value")
	case cut == ScopeRoots && all != nil && all.Roots != nil:
		check.exact("the roots cut's finished roots", goldenRoots(split.Roots).FinishedRoots, all.Roots.FinishedRoots)
		check.exact("the roots cut's children", goldenRoots(split.Roots).Children, all.Roots.Children)
	}
}

func goldenSamples(fold *SampleFold) *SampleFold {
	if fold == nil {
		return &SampleFold{}
	}
	return fold
}

func goldenRoots(fold *RootsFold) *RootsFold {
	if fold == nil {
		return &RootsFold{}
	}
	return fold
}

// compareGoldenTables compares the report's tables of issues with the facts of the issues that
// closed in the window in cut (closed, keys of facts) and the filings made in it: the work-time and
// rework rows with IssueFacts, and the splitting rows with IssueFiling. A table's title and
// priority are the issue's own columns, which the facts do not hold, so they are left out.
func compareGoldenTables(check *goldenCheck, facts map[string]IssueFacts, closed []string, filed []IssueFiling, cut Scope, report goldenReport) {
	rows := map[string]IssueFacts{}
	timed := 0
	for _, key := range closed {
		if issue := facts[key]; InCut(cut, issue.Root) {
			rows[key] = issue
			if issue.StartedAt != nil {
				timed++
			}
		}
	}
	check.exact("issues closed in the window", len(rows), report.Finished)
	check.exact("issues timed in the window", timed, len(report.WorkTime.Issues))
	for _, issue := range report.WorkTime.Issues {
		row, held := rows[issue.Key]
		if !held {
			check.exact(issue.Key+"'s facts", "none", "one")
			continue
		}
		check.exact(issue.Key+"'s root", row.Root, issue.Root)
		check.moment(issue.Key+"'s start", row.StartedAt, &issue.StartedAt)
		check.moment(issue.Key+"'s close", row.ClosedAt, &issue.ClosedAt)
		check.moment(issue.Key+"'s merge", row.MergedAt, issue.MergedAt)
		check.hours(issue.Key+"'s hours", &row.WorkHours, issue.Hours)
		check.hours(issue.Key+"'s parked hours", &row.ParkedHours, issue.ParkedHours)
		check.hours(issue.Key+"'s queued hours", &row.QueuedHours, issue.QueuedHours)
	}
	check.exact("rework rows", len(rows), len(report.Rework.Issues))
	for _, issue := range report.Rework.Issues {
		row, held := rows[issue.Key]
		if !held {
			check.exact(issue.Key+"'s facts", "none", "one")
			continue
		}
		check.exact(issue.Key+"'s review rounds", row.ReviewRounds, issue.ReviewRounds)
		check.exact(issue.Key+"'s send-back", row.SentBack, issue.SentBack)
		check.hours(issue.Key+"'s implementing hours after the first review", &row.ImplementingHoursAfterFirstReview, issue.ImplementingHoursAfterFirst)
		check.hours(issue.Key+"'s testing hours after the first review", &row.TestingHoursAfterFirstReview, issue.TestingHoursAfterFirst)
	}
	filings := map[string]IssueFiling{}
	for _, filing := range filed {
		if InCut(cut, filing.Root == filing.Key) {
			filings[filing.Key] = filing
		}
	}
	check.exact("filings in the window", len(filings), len(report.Splitting.Filed))
	for _, row := range report.Splitting.Filed {
		filing, held := filings[row.Key]
		if !held {
			check.exact(row.Key+"'s filing", "none", "one")
			continue
		}
		check.moment(row.Key+"'s filing time", &filing.At, &row.At)
		check.exact(row.Key+"'s class", filing.Class, row.Class)
		check.exact(row.Key+"'s filer", filing.Filer, row.Filer)
		check.exact(row.Key+"'s filer issue", filing.FilerIssue, row.FilerIssue)
		check.exact(row.Key+"'s filer phase", filing.FilerPhase, row.FilerPhase)
	}
}
