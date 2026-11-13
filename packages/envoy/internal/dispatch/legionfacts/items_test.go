package legionfacts

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

// TestTheItemsRowsCarryTheStoredFactsOfEachIssue: the rows an eval's issue list serves are one
// per stored row, each keyed by its issue, of kind `issue`, carrying the stored facts as columns
// with the row's own measured time, the computation as the source and the issue's own address as
// the link (R5). The columns are compared field by field against the row they came from, so a
// column that carried another issue's number, or the moment of the call in place of the moment
// the facts were computed as of, fails here.
func TestTheItemsRowsCarryTheStoredFactsOfEachIssue(t *testing.T) {
	seeded := seed(t, 4)
	project := seeded.fixture.Project

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	items := ItemRows(rows)

	if len(items) != len(rows) {
		t.Fatalf("%d rows made %d items, want one item per row", len(rows), len(items))
	}
	withHours, withoutHours := 0, 0
	for index, item := range items {
		row := rows[index]
		if item.RowKey != row.Issue || item.RowKind != ItemRowKind || item.Project != row.Project {
			t.Errorf("row %s answered key %q, kind %q, project %q, want %q, %q, %q",
				row.Issue, item.RowKey, item.RowKind, item.Project, row.Issue, ItemRowKind, row.Project)
		}
		if !item.MeasuredAt.Equal(row.UpdatedAt) {
			t.Errorf("%s was measured at %s, want the row's own %s", row.Issue, item.MeasuredAt, row.UpdatedAt)
		}
		if item.Source != ItemSource {
			t.Errorf("%s names the source %q, want %q", row.Issue, item.Source, ItemSource)
		}
		if want := "/issues/" + row.Issue; item.Href != want {
			t.Errorf("%s links to %q, want the issue's own address %q", row.Issue, item.Href, want)
		}
		if len(row.HoursByPhase) == 0 {
			withoutHours++
		} else {
			withHours++
		}
		compareItemColumns(t, row, item.Columns)
	}
	// Both shapes are in the answer, so the columns above were read over an issue whose work
	// took measured time and over one whose work took none.
	if withHours == 0 || withoutHours == 0 {
		t.Fatalf("%d rows carry phase hours and %d carry none, want both shapes in the answer",
			withHours, withoutHours)
	}
	t.Logf("%d items rows built for %d stored rows; scope project %s, window %s to %s; %d rows carry phase hours, %d carry none",
		len(items), len(rows), project, seeded.fixture.From.Format(time.RFC3339),
		seeded.fixture.To.Format(time.RFC3339), withHours, withoutHours)
}

// compareItemColumns reads each column of one row against the stored value it comes from: the
// twenty every row carries, one `hours_<phase>` column for each phase the issue has hours in and
// no other, six `bar_<role>` columns for each of the seven bars, and one `idle_<role>` for each
// of the six roles.
func compareItemColumns(t *testing.T, row Row, columns map[string]any) {
	t.Helper()
	want := map[string]any{
		ColumnOpenedAt:                     row.OpenedAt,
		ColumnClosedAt:                     row.ClosedAt,
		ColumnWorkHours:                    row.WorkHours,
		ColumnReviewRounds:                 row.ReviewRounds,
		ColumnRootKey:                      row.RootKey,
		ColumnTreeDepth:                    row.TreeDepth,
		ColumnSlotHours:                    row.SlotHours,
		ColumnWaitingReason:                row.WaitingReason,
		ColumnWorkFirstAt:                  row.WorkFirstAt,
		ColumnWorkLastAt:                   row.WorkLastAt,
		ColumnParkedHours:                  row.ParkedHours,
		ColumnQueuedHours:                  row.QueuedHours,
		ColumnStartedAt:                    row.StartedAt,
		ColumnMergedAt:                     row.MergedAt,
		ColumnSlotWaitHours:                row.SlotWaitHours,
		ColumnSentBack:                     row.SentBack,
		ColumnImplementingAfterFirstReview: row.ImplementingAfterFirstReview,
		ColumnTestingAfterFirstReview:      row.TestingAfterFirstReview,
		ColumnChildren:                     row.Children,
		ColumnRepo:                         row.Repo,
	}
	for phase, hours := range row.HoursByPhase {
		want[ColumnHoursPrefix+phase] = hours
	}
	if len(row.Bars) != 7 || len(row.IdleByRole) != 6 {
		t.Errorf("%s holds %d bars and %d roles' idle hours, want all 7 and all 6", row.Issue, len(row.Bars), len(row.IdleByRole))
	}
	for role, bar := range row.Bars {
		want[ColumnBarPrefix+role] = bar.Hours
		want[ColumnBarPrefix+role+"_waiting_on_person"] = bar.Segments.WaitingOnPerson
		want[ColumnBarPrefix+role+"_in_a_turn"] = bar.Segments.InATurn
		want[ColumnBarPrefix+role+"_ci_wait"] = bar.Segments.CIWait
		want[ColumnBarPrefix+role+"_waiting_on_children"] = bar.Segments.WaitingOnChildren
		want[ColumnBarPrefix+role+"_idle"] = bar.Segments.Idle
	}
	for role, hours := range row.IdleByRole {
		want[ColumnIdlePrefix+role] = hours
	}
	if names := slices.Sorted(maps.Keys(columns)); !slices.Equal(names, slices.Sorted(maps.Keys(want))) {
		t.Errorf("%s carries the columns %v, want %v", row.Issue, names, slices.Sorted(maps.Keys(want)))
		return
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !sameColumn(columns[name], want[name]) {
			t.Errorf("%s carries %s = %s, want the stored %s", row.Issue, name,
				showColumn(columns[name]), showColumn(want[name]))
		}
	}
}

// sameColumn is whether a column holds the stored value, each of the two shapes a column carries
// compared as itself: a moment by its instant, an optional moment by its instant or its absence,
// and every other value by equality.
func sameColumn(got, want any) bool {
	switch value := want.(type) {
	case time.Time:
		at, is := got.(time.Time)
		return is && at.Equal(value)
	case *time.Time:
		at, is := got.(*time.Time)
		return is && sameTime(at, value)
	default:
		return got == want
	}
}

// showColumn is one column's value for a failure message, a moment in UTC so two zones read
// alike.
func showColumn(value any) string {
	switch at := value.(type) {
	case time.Time:
		return at.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		return fmt.Sprintf("%v", utc(at))
	default:
		return fmt.Sprintf("%v", value)
	}
}
