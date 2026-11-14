package legionfacts

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

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// goldenPartsFile is the per-issue parts the base head's window aggregation (metrics.Compute,
// since deleted) took from each finished issue, recorded before the parts were split out of it
// (legion/metrics/golden_record_test.go, deleted with it: once the split landed it would have
// recorded the new code's answer and stopped being the oracle).
var goldenPartsFile = filepath.Join("..", "legion", "metrics", "testdata", "golden_issue_parts.json")

// goldenParts is that file: the history it was recorded over and each finished issue's parts, in
// the shapes the base head's report wrote them.
type goldenParts struct {
	Head string `json:"head"`
	Spec struct {
		Project string    `json:"project"`
		Trees   int       `json:"trees"`
		Now     time.Time `json:"now"`
	} `json:"spec"`
	Issues map[string]struct {
		WorkTime *goldenWorkTime `json:"work_time"`
		Bars     []goldenBar     `json:"bars"`
		Rework   goldenRework    `json:"rework"`
		Children int             `json:"children"`
		TreeRoot string          `json:"tree_root"`
		Root     bool            `json:"root"`
	} `json:"issues"`
}

// goldenWorkTime is one issue's work-time row as the golden file holds it.
type goldenWorkTime struct {
	Root        bool       `json:"root"`
	StartedAt   time.Time  `json:"started_at"`
	ClosedAt    time.Time  `json:"closed_at"`
	MergedAt    *time.Time `json:"merged_at"`
	Hours       float64    `json:"hours"`
	ParkedHours float64    `json:"parked_hours"`
	QueuedHours float64    `json:"queued_hours"`
}

// goldenBar is one bar of one issue as the golden file holds it: the bar of a population of that
// issue alone, so its median per issue is the issue's own hours on it and its issues are one or
// none.
type goldenBar struct {
	Role                string           `json:"role"`
	Hours               float64          `json:"hours"`
	Issues              int              `json:"issues"`
	MedianHoursPerIssue float64          `json:"median_hours_per_issue"`
	Segments            metrics.Segments `json:"segments"`
}

// goldenRework is one issue's rework row as the golden file holds it.
type goldenRework struct {
	ReviewRounds                int     `json:"review_rounds"`
	SentBack                    bool    `json:"sent_back"`
	ImplementingHoursAfterFirst float64 `json:"implementing_hours_after_first_review"`
	TestingHoursAfterFirst      float64 `json:"testing_hours_after_first_review"`
}

// goldenHoursTolerance is how far a stored hour figure may stand from the recorded one: the
// spec's 0.1 hour. Every count, flag, key and moment is compared exactly.
const goldenHoursTolerance = 0.1

// TestEveryStoredPartEqualsTheWindowAggregationsPartAtTheBaseHead: for the history the golden
// file was recorded over, every per-issue value the facts store, written by WriteProject and read
// back by Read, equals the part the base head's window aggregation took from that issue: the
// work-time row, each of the seven bars with its five segments, the rework row, the children and
// the tree. A part that moves when the code that computes it is split out of the aggregation
// fails here, whichever of the stored columns it lands in, wherever the recorded history holds a
// value other than 0 for it: a column the history holds 0 for in every issue (as it does for
// several segments) shows only that the stored value is 0 there too.
func TestEveryStoredPartEqualsTheWindowAggregationsPartAtTheBaseHead(t *testing.T) {
	raw, err := os.ReadFile(goldenPartsFile)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPartsFile, err)
	}
	var golden goldenParts
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", goldenPartsFile, err)
	}
	if len(golden.Issues) < 200 {
		t.Fatalf("%s holds the parts of %d issues, want the at least 200 it was recorded with", goldenPartsFile, len(golden.Issues))
	}

	database := storetest.Open(t)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{
		Project: golden.Spec.Project, Trees: golden.Spec.Trees, Now: golden.Spec.Now,
	})
	rows, _, err := ComputeProject(t.Context(), database.Pool, fixture.Project, golden.Spec.Now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", fixture.Project, err)
	}
	if _, err := WriteProject(t.Context(), database.Pool, ProjectWrite{Project: fixture.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", fixture.Project, err)
	}
	stored, err := Read(t.Context(), database.Pool, fixture.Project)
	if err != nil {
		t.Fatalf("read the facts of %s: %v", fixture.Project, err)
	}

	check := &goldenCheck{t: t}
	for _, key := range slices.Sorted(maps.Keys(golden.Issues)) {
		want := golden.Issues[key]
		row, has := stored[key]
		if !has {
			t.Errorf("%s finished with Legion phase work at the base head and has no stored row", key)
			continue
		}
		check.issue = key
		if want.WorkTime == nil {
			t.Errorf("%s: the golden file records no work time, which every finished issue had", key)
			continue
		}
		work := want.WorkTime
		check.hours("work_hours", row.WorkHours, work.Hours)
		check.hours("parked_hours", row.ParkedHours, work.ParkedHours)
		check.hours("queued_hours", row.QueuedHours, work.QueuedHours)
		check.moment("started_at", row.StartedAt, &work.StartedAt)
		check.moment("closed_at", row.ClosedAt, &work.ClosedAt)
		check.moment("merged_at", row.MergedAt, work.MergedAt)
		check.exact("root_key", row.RootKey, want.TreeRoot)
		check.exact("root", row.RootKey == key, want.Root)
		check.exact("the work-time row's root", row.RootKey == key, work.Root)
		check.exact("children", row.Children, want.Children)
		check.exact("review_rounds", row.ReviewRounds, want.Rework.ReviewRounds)
		check.exact("sent_back", row.SentBack, want.Rework.SentBack)
		check.hours("implementing_after_first_review", row.ImplementingAfterFirstReview, want.Rework.ImplementingHoursAfterFirst)
		check.hours("testing_after_first_review", row.TestingAfterFirstReview, want.Rework.TestingHoursAfterFirst)
		if len(row.Bars) != len(want.Bars) {
			t.Errorf("%s stores %d bars, want the %d the aggregation drew", key, len(row.Bars), len(want.Bars))
		}
		for _, bar := range want.Bars {
			part, has := row.Bars[bar.Role]
			if !has {
				t.Errorf("%s stores no %s bar", key, bar.Role)
				continue
			}
			check.hours("bars."+bar.Role+".hours", part.Hours, bar.Hours)
			check.hours("bars."+bar.Role+" as the median of one issue", part.Hours, bar.MedianHoursPerIssue)
			check.exact("bars."+bar.Role+".issues", goldenIssueCount(part), bar.Issues)
			check.hours("bars."+bar.Role+".waiting_on_person", part.Segments.WaitingOnPerson, bar.Segments.WaitingOnPerson)
			check.hours("bars."+bar.Role+".in_a_turn", part.Segments.InATurn, bar.Segments.InATurn)
			check.hours("bars."+bar.Role+".ci_wait", part.Segments.CIWait, bar.Segments.CIWait)
			check.hours("bars."+bar.Role+".waiting_on_children", part.Segments.WaitingOnChildren, bar.Segments.WaitingOnChildren)
			check.hours("bars."+bar.Role+".idle", part.Segments.Idle, bar.Segments.Idle)
		}
		check.issues++
	}
	if check.issues != len(golden.Issues) {
		t.Errorf("compared %d issues of the %d the golden file holds", check.issues, len(golden.Issues))
	}
	t.Logf("history %s, %d trees, as of %s (recorded at %s): %d issues compared, %d values, %d of them differ; the largest hour difference is %g",
		fixture.Project, golden.Spec.Trees, golden.Spec.Now.Format(time.RFC3339), golden.Head,
		check.issues, check.values, check.differ, check.widest)
}

// goldenIssueCount is the bar's issue count for one issue: one where any of its bar spans fall
// on the bar, which is what a bar's issue count counts for an issue alone.
func goldenIssueCount(part metrics.BarPart) int {
	if part.HoldsHours {
		return 1
	}
	return 0
}

// goldenCheck compares one issue's stored values with its recorded parts, counting what it
// compared, what differed and the largest hour difference it saw.
type goldenCheck struct {
	t      *testing.T
	issue  string
	issues int
	values int
	differ int
	widest float64
}

func (c *goldenCheck) fail(name string, got, want any) {
	c.t.Helper()
	c.differ++
	c.t.Errorf("%s: stored %s is %v, want the base head's %v", c.issue, name, got, want)
}

func (c *goldenCheck) hours(name string, got, want float64) {
	c.t.Helper()
	c.values++
	gap := math.Abs(got - want)
	c.widest = max(c.widest, gap)
	if gap > goldenHoursTolerance {
		c.fail(name, got, want)
	}
}

func (c *goldenCheck) exact(name string, got, want any) {
	c.t.Helper()
	c.values++
	if got != want {
		c.fail(name, got, want)
	}
}

func (c *goldenCheck) moment(name string, got, want *time.Time) {
	c.t.Helper()
	c.values++
	if !sameTime(got, want) {
		c.fail(name, fmt.Sprint(utc(got)), fmt.Sprint(utc(want)))
	}
}
