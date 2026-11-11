package legionfacts

import "time"

// items.go turns the stored rows into the rows an eval's issue list serves (dispatch://INFRA-2915,
// R5). dispatch://INFRA-2916 mounts `GET /api/v1/projects/{key}/evals/{slug}/items`; this file
// holds the row that route answers for an issue and the function that shapes it, so the route
// reads stored rows and computes nothing, as every other reader of the facts does.

// ItemRowKind is the row kind an issue's row carries. A row kind is a registered name rather
// than a fixed list (dispatch://INFRA-979/spec#b-r18), and `issue` is the name of this one.
const ItemRowKind = "issue"

// ItemSource names where a row's columns came from: this package, which computes each row
// (Compute and ComputeProject) and writes it to issue_facts. One name stands for every row,
// because a row written for one issue and a row written by a sweep are the one computation.
const ItemSource = "legionfacts"

// The columns of an issue's row that every row carries, whatever the issue. Each is the
// issue_facts column of the same name, so a reader of a column and a reader of the table read one
// word for one number.
const (
	ColumnOpenedAt      = "opened_at"
	ColumnClosedAt      = "closed_at"
	ColumnWorkHours     = "work_hours"
	ColumnReviewRounds  = "review_rounds"
	ColumnRootKey       = "root_key"
	ColumnTreeDepth     = "tree_depth"
	ColumnSlotHours     = "slot_hours"
	ColumnWaitingReason = "waiting_reason"
	ColumnWorkFirstAt   = "work_first_at"
	ColumnWorkLastAt    = "work_last_at"
	// ColumnHoursPrefix starts the column of one phase's hours: `hours_implementing`,
	// `hours_awaiting_person`. The stored row holds the hours as a map of the board's column
	// ids, and an eval names one column and reads one number, so each phase's hours is a column
	// of its own rather than a map under one name. A phase with no hours has no column, as the
	// stored row holds no entry for it.
	ColumnHoursPrefix = "hours_"
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
// issue, in the order the rows were given, which is the key order Compute and ComputeProject
// answer in.
//
// The measured time is the row's own UpdatedAt, the moment its facts were computed as of, and
// never the moment of this call: a row read an hour after it was written was measured an hour
// ago, and an eval that charts it reads it at the moment it was true.
func ItemRows(rows []Row) []ItemRow {
	items := make([]ItemRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, ItemRow{
			Project:    row.Project,
			RowKey:     row.Issue,
			RowKind:    ItemRowKind,
			Columns:    itemColumns(row),
			MeasuredAt: row.UpdatedAt,
			Source:     ItemSource,
			Href:       "/issues/" + row.Issue,
		})
	}
	return items
}

// itemColumns is one row's facts by column name: the ten columns every issue's row carries, and
// one column for each phase the issue has hours in.
func itemColumns(row Row) map[string]any {
	columns := map[string]any{
		ColumnOpenedAt:      row.OpenedAt,
		ColumnClosedAt:      row.ClosedAt,
		ColumnWorkHours:     row.WorkHours,
		ColumnReviewRounds:  row.ReviewRounds,
		ColumnRootKey:       row.RootKey,
		ColumnTreeDepth:     row.TreeDepth,
		ColumnSlotHours:     row.SlotHours,
		ColumnWaitingReason: row.WaitingReason,
		ColumnWorkFirstAt:   row.WorkFirstAt,
		ColumnWorkLastAt:    row.WorkLastAt,
	}
	for phase, hours := range row.HoursByPhase {
		columns[ColumnHoursPrefix+phase] = hours
	}
	return columns
}
