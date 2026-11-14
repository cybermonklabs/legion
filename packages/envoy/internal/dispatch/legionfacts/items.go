package legionfacts

import (
	"context"
	"fmt"
	"time"
)

// items.go turns the stored rows into the rows an eval's issue list serves (dispatch://INFRA-2915,
// R5). dispatch://INFRA-2916 mounts `GET /api/v1/projects/{key}/evals/{slug}/items`; this file
// holds the row that route answers for an issue and the function that shapes it, so the route
// reads stored rows and computes nothing, as every other reader of the facts does.

// ItemRowKind is the row kind an issue's row carries. A row kind is a registered name rather
// than a fixed list (dispatch://INFRA-979/spec#b-r18), and `issue` is the name of this one.
const ItemRowKind = "issue"

// ItemSource names where a row's columns came from: this package, which computes each row
// (computeKeys and ComputeProject) and writes it to issue_facts. One name stands for every row,
// because a row written for one issue and a row written by a sweep are the one computation.
const ItemSource = "legionfacts"

// The columns of an issue's row that every row carries, whatever the issue. Each is the
// issue_facts column of the same name, so a reader of a column and a reader of the table read one
// word for one number.
const (
	ColumnOpenedAt                     = "opened_at"
	ColumnClosedAt                     = "closed_at"
	ColumnWorkHours                    = "work_hours"
	ColumnReviewRounds                 = "review_rounds"
	ColumnRootKey                      = "root_key"
	ColumnTreeDepth                    = "tree_depth"
	ColumnSlotHours                    = "slot_hours"
	ColumnWaitingReason                = "waiting_reason"
	ColumnWorkFirstAt                  = "work_first_at"
	ColumnWorkLastAt                   = "work_last_at"
	ColumnParkedHours                  = "parked_hours"
	ColumnQueuedHours                  = "queued_hours"
	ColumnStartedAt                    = "started_at"
	ColumnMergedAt                     = "merged_at"
	ColumnSlotWaitHours                = "slot_wait_hours"
	ColumnSentBack                     = "sent_back"
	ColumnImplementingAfterFirstReview = "implementing_after_first_review"
	ColumnTestingAfterFirstReview      = "testing_after_first_review"
	ColumnChildren                     = "children"
	ColumnRepo                         = "repo"
	// ColumnHoursPrefix starts the column of one phase's hours: `hours_implementing`,
	// `hours_awaiting_person`. The stored row holds the hours as a map of the board's column
	// ids, and an eval names one column and reads one number, so each phase's hours is a column
	// of its own rather than a map under one name. A phase with no hours has no column, as the
	// stored row holds no entry for it.
	ColumnHoursPrefix = "hours_"
	// ColumnBarPrefix starts the columns of one bar of phase time, for the same reason:
	// `bar_implementer` is the bar's hours and `bar_implementer_in_a_turn` one segment of them,
	// one of each for every bar the stored row holds, which is all seven.
	ColumnBarPrefix = "bar_"
	// ColumnIdlePrefix starts the column of one role's idle hours, `idle_reviewer`, one for each
	// of the six roles the stored row holds.
	ColumnIdlePrefix = "idle_"
)

// ItemRow is one row of an eval's items answer: the issue, its stored Legion facts as columns,
// when they were measured, what computed them and where a reader of the row goes to see the
// issue. The field set is dispatch://INFRA-979/spec#b-r18's, which every row kind of that route
// carries: a row key, a row kind, the columns, the measured time, the source and a link.
//
// Project is the issue's own project, which is the project the row counts for: the stored row
// names it, and no caller passes one of its own.
type ItemRow struct {
	Project string `json:"project"`
	RowKey  string `json:"row_key"`
	RowKind string `json:"row_kind"`
	// Columns is the facts, by column name. A later eval names the columns it wants, so the
	// values are the stored ones and this adds nothing to any of them: a count is a count, an
	// hour figure is the stored float, a moment is the stored moment and an open issue's
	// `closed_at` is null.
	Columns    map[string]any `json:"columns"`
	MeasuredAt time.Time      `json:"measured_at"`
	Source     string         `json:"source"`
	// Href is the issue's own address, `/issues/<key>`, which opens the issue: the one spelling
	// of it this repository writes (api/search.go's searchHref, api/duplicates.go).
	Href string `json:"href"`
}

// ItemRows turns stored facts rows into the items rows an eval's issue list serves: one row per
// issue, in the order the rows were given, which is the key order computeKeys and ComputeProject
// answer in.
//
// The measured time is when the facts were measured: a closed issue's close, the moment its
// values stopped moving and the moment its Legion evals count it on, whenever its row was last
// recomputed; and an open issue's row's own UpdatedAt, the moment its still-moving facts were
// computed as of. It is never the moment of this call: a row read an hour after it was written was
// measured an hour ago, and an eval that charts it reads it at the moment it was true.
func ItemRows(rows []Row) []ItemRow {
	items := make([]ItemRow, 0, len(rows))
	for _, row := range rows {
		measured := row.UpdatedAt
		if row.ClosedAt != nil {
			measured = *row.ClosedAt
		}
		items = append(items, ItemRow{
			Project:    row.Project,
			RowKey:     row.Issue,
			RowKind:    ItemRowKind,
			Columns:    itemColumns(row),
			MeasuredAt: measured,
			Source:     ItemSource,
			Href:       "/issues/" + row.Issue,
		})
	}
	return items
}

// itemColumns is one row's facts by column name: the twenty columns every issue's row carries,
// one column for each phase the issue has hours in, six for each of the seven bars (its hours and
// its five segments) and one for each role's idle hours. A bar's holds_hours is no column: it is
// what a fold counts a bar's issues by, and no issue list shows it.
func itemColumns(row Row) map[string]any {
	columns := map[string]any{
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
		columns[ColumnHoursPrefix+phase] = hours
	}
	// A bar's columns: its hours under the bar's own, and each of its five segments under
	// `_<segment>`, by the segment's own name (metrics.Segment).
	for role, bar := range row.Bars {
		columns[ColumnBarPrefix+role] = bar.Hours
		for segment, hours := range bar.Segments.ByName() {
			columns[ColumnBarPrefix+role+"_"+string(segment)] = hours
		}
	}
	for role, hours := range row.IdleByRole {
		columns[ColumnIdlePrefix+role] = hours
	}
	return columns
}

// The columns an items row reads of the issue itself, which no stored row holds: its title and
// its priority (null for an issue with none). WithIssues adds them.
const (
	ColumnTitle    = "title"
	ColumnPriority = "priority"
)

// The columns of a filing's row (FilingItemRows): the issue_filings column of each name.
const (
	ColumnFiledAt    = "filed_at"
	ColumnFilerRole  = "filer_role"
	ColumnFilerIssue = "filer_issue"
	ColumnFilerPhase = "filer_phase"
	ColumnClass      = "class"
)

// ItemIssue is what an items row reads of its issue in issues: the title, and the priority, nil
// where the issue has none.
type ItemIssue struct {
	Title    string
	Priority *int
}

// ReadItemIssues reads the title and priority of each of keys that an issue holds, by key: the
// join of an items page's rows with issues, one read of the primary key per page.
func ReadItemIssues(ctx context.Context, q Querier, keys []string) (map[string]ItemIssue, error) {
	issues := make(map[string]ItemIssue, len(keys))
	if len(keys) == 0 {
		return issues, nil
	}
	rows, err := q.Query(ctx, `select key, title, priority from issues where key = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the issues of the items rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var issue ItemIssue
		if err := rows.Scan(&key, &issue.Title, &issue.Priority); err != nil {
			return nil, fmt.Errorf("read the issues of the items rows: %w", err)
		}
		issues[key] = issue
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the issues of the items rows: %w", err)
	}
	return issues, nil
}

// WithIssues is items with each row's issue's title and priority among its columns, as issues
// (ReadItemIssues) holds them: an issue it does not hold reads an empty title and no priority, so
// every row carries the same columns.
func WithIssues(items []ItemRow, issues map[string]ItemIssue) []ItemRow {
	for _, item := range items {
		issue := issues[item.RowKey]
		item.Columns[ColumnTitle], item.Columns[ColumnPriority] = issue.Title, issue.Priority
	}
	return items
}

// FilingItemRows turns stored filings into the items rows legion-splitting's filings serve: one
// `issue` row per filed issue, in the order the filings were given, whose columns are the
// filing's (when, by which role, issue and phase, its class and the filed issue's tree root),
// measured when it was filed.
func FilingItemRows(filings []Filing) []ItemRow {
	items := make([]ItemRow, 0, len(filings))
	for _, filing := range filings {
		items = append(items, ItemRow{
			Project: filing.Project,
			RowKey:  filing.Issue,
			RowKind: ItemRowKind,
			Columns: map[string]any{
				ColumnFiledAt:    filing.FiledAt,
				ColumnFilerRole:  filing.FilerRole,
				ColumnFilerIssue: filing.FilerIssue,
				ColumnFilerPhase: filing.FilerPhase,
				ColumnClass:      filing.Class,
				ColumnRootKey:    filing.RootKey,
			},
			MeasuredAt: filing.FiledAt,
			Source:     ItemSource,
			Href:       "/issues/" + filing.Issue,
		})
	}
	return items
}
