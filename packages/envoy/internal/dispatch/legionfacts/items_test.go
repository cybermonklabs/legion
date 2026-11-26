package legionfacts

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

// TestTheItemsRowsCarryTheStoredFactsOfEachIssue: the rows an eval's issue list serves are built
// from what the items route reads of what the facts pass stored: the rows of the issues closed in
// the range (ReadClosedIn) and the filings made in it (ReadFilingsIn). A closed issue's row is
// keyed by its issue, of kind `issue`, carries the stored facts as columns and is measured at the
// issue's close (R24); a filing's row is keyed by the filed issue, carries the filing's columns
// and is measured when it was filed; each names the computation as its source and the issue's own
// address as its link. Both are compared field by field against what the computation answered
// before the write, so a column that carried another issue's number, the moment of the call in
// place of the moment the facts were measured, or a fact the write or the read lost, fails here.
// The repo column is the repository the caller resolved for the issue, not the stored row's: each
// issue is handed one no stored row holds, so a column read from the row fails.
func TestTheItemsRowsCarryTheStoredFactsOfEachIssue(t *testing.T) {
	seeded := seed(t, 4)
	ctx, pool, project := t.Context(), seeded.database.Pool, seeded.fixture.Project

	rows, filings, err := ComputeProject(ctx, pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := WriteProject(ctx, pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	if err := WriteFilings(ctx, pool, FilingsWrite{Project: project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings: %v", err)
	}
	// The range holds the whole history, read an hour after its end, as the route reads a range's
	// rows as of the request (Row.At).
	from, to := seeded.fixture.From, seeded.now.Add(time.Hour)
	readAt := to
	closedRows, err := ReadClosedIn(ctx, pool, project, from, to, readAt)
	if err != nil {
		t.Fatalf("read the closed issues' facts: %v", err)
	}
	storedFilings, err := ReadFilingsIn(ctx, pool, project, from, to)
	if err != nil {
		t.Fatalf("read the filings: %v", err)
	}

	closed := map[string]Row{}
	repos := map[string]string{}
	for _, row := range rows {
		if row.ClosedAt != nil {
			closed[row.Issue] = row.At(readAt)
		}
		repos[row.Issue] = "example/resolved-" + row.Issue
	}
	items := ItemRows(closedRows, repos)
	if len(items) != len(closed) {
		t.Fatalf("the %d closed issues the computation answered read as %d items, want one item each", len(closed), len(items))
	}
	withHours, withoutHours := 0, 0
	for _, item := range items {
		row, computed := closed[item.RowKey]
		if !computed {
			t.Errorf("an item is keyed %q, which the computation answered no closed row for", item.RowKey)
			continue
		}
		if item.RowKind != ItemRowKind || item.Project != row.Project {
			t.Errorf("%s answered kind %q, project %q, want %q, %q", row.Issue, item.RowKind, item.Project, ItemRowKind, row.Project)
		}
		if !item.MeasuredAt.Equal(*row.ClosedAt) {
			t.Errorf("%s was measured at %s, want its close %s", row.Issue, item.MeasuredAt, row.ClosedAt)
		}
		compareItemLink(t, item)
		if len(row.PhaseHours) == 0 {
			withoutHours++
		} else {
			withHours++
		}
		compareItemColumns(t, row, repos[row.Issue], item.Columns)
	}
	// Both shapes are in the answer, so the columns above were read over an issue whose work
	// took measured time and over one whose work took none.
	if withHours == 0 || withoutHours == 0 {
		t.Fatalf("%d closed rows carry phase hours and %d carry none, want both shapes in the answer", withHours, withoutHours)
	}

	computedFilings := map[string]Filing{}
	for _, filing := range filings {
		computedFilings[filing.Key] = filing
	}
	filed := FilingItemRows(storedFilings)
	if len(filed) == 0 || len(filed) != len(computedFilings) {
		t.Fatalf("the %d filings the computation answered read as %d items, want one item each and at least one", len(computedFilings), len(filed))
	}
	for _, item := range filed {
		filing, computed := computedFilings[item.RowKey]
		if !computed {
			t.Errorf("a filing's item is keyed %q, which the computation answered no filing for", item.RowKey)
			continue
		}
		if item.RowKind != ItemRowKind || item.Project != project {
			t.Errorf("the filing of %s answered kind %q, project %q, want %q, %q", filing.Key, item.RowKind, item.Project, ItemRowKind, project)
		}
		if !item.MeasuredAt.Equal(filing.At) {
			t.Errorf("the filing of %s was measured at %s, want its filing %s", filing.Key, item.MeasuredAt, filing.At)
		}
		compareItemLink(t, item)
		want := map[string]any{
			ColumnFiledAt: filing.At, ColumnFilerRole: filing.Filer, ColumnFilerIssue: filing.FilerIssue,
			ColumnFilerPhase: filing.FilerPhase, ColumnClass: filing.Class, ColumnRootKey: filing.Root,
		}
		if names := slices.Sorted(maps.Keys(item.Columns)); !slices.Equal(names, slices.Sorted(maps.Keys(want))) {
			t.Errorf("the filing of %s carries the columns %v, want %v", filing.Key, names, slices.Sorted(maps.Keys(want)))
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if !sameColumn(item.Columns[name], want[name]) {
				t.Errorf("the filing of %s carries %s = %s, want the computed %s", filing.Key, name,
					showColumn(item.Columns[name]), showColumn(want[name]))
			}
		}
	}
	t.Logf("%d items rows built for the %d stored rows of issues closed from %s to %s (%d carry phase hours, %d carry none), and %d for the filings made then",
		len(items), len(closedRows), from.Format(time.RFC3339), to.Format(time.RFC3339), withHours, withoutHours, len(filed))
}

// compareItemLink reads an item's source and link: the computation, and the issue's own address.
func compareItemLink(t *testing.T, item ItemRow) {
	t.Helper()
	if item.Source != ItemSource {
		t.Errorf("%s names the source %q, want %q", item.RowKey, item.Source, ItemSource)
	}
	if want := "/issues/" + item.RowKey; item.Href != want {
		t.Errorf("%s links to %q, want the issue's own address %q", item.RowKey, item.Href, want)
	}
}

// compareItemColumns reads each column of one row against the stored value it comes from, and
// the repo column against repo, the repository the issue resolves to: the twenty every row
// carries, one `hours_<phase>` column for each phase the issue has hours in and no other, six
// `bar_<role>` columns for each of the seven bars, and one `idle_<role>` for each of the six
// roles.
func compareItemColumns(t *testing.T, row Row, repo string, columns map[string]any) {
	t.Helper()
	want := map[string]any{
		ColumnOpenedAt:                     row.OpenedAt,
		ColumnClosedAt:                     row.ClosedAt,
		ColumnWorkHours:                    row.WorkHours,
		ColumnReviewRounds:                 row.ReviewRounds,
		ColumnRootKey:                      row.Tree,
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
		ColumnImplementingAfterFirstReview: row.ImplementingHoursAfterFirstReview,
		ColumnTestingAfterFirstReview:      row.TestingHoursAfterFirstReview,
		ColumnChildren:                     row.Children,
		ColumnRepo:                         repo,
	}
	for phase, hours := range row.PhaseHours {
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
