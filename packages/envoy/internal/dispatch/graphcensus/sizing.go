package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// sizingWindowDays is how far back the sizing reads a rate. Four weeks covers every weekly
// rhythm a working week has, and is short enough that a database's distant past does not hold
// its current rate down.
const sizingWindowDays = 28

// sizingYearDays is how far ahead the sizing projects: one year, the horizon the limit is
// judged at.
const sizingYearDays = 365

// measurableRows is the fewest rows a table may hold for its own bytes-a-row to be worth
// reading. Below it a table is mostly its indexes' empty pages and its own catalog overhead,
// and the figure that comes out of dividing its size by its rows is tens of times the real one.
const measurableRows = 10_000

// measuredBytesARow is the bytes a row measured for dispatch://INFRA-1149 on this pod's
// PostgreSQL 18.6, used where a store table is too small to measure its own. measuredFrom is
// where it came from, printed beside every figure that uses it so nobody reads it as this
// database's own measurement.
const (
	measuredBytesARow int64 = 783
	measuredFrom            = "1,050,010 tuples at pg_total_relation_size 822,673,408 bytes" +
		" on PostgreSQL 18.6, table, seven indexes and TOAST"
)

// sizingWindow is the window every rate is read over, spliced into each statement from
// sizingWindowDays so the SQL and the divisor the Go side applies cannot read two windows.
var sizingWindow = fmt.Sprintf("now() - interval '%d days'", sizingWindowDays)

// eventRateQuery is the write rate by kind: what the database recorded over the sizing window,
// a day at a time. It is read as one statement over events and printed row by row.
var eventRateQuery = fmt.Sprintf(`select type, count(*) / %d.0 as per_day
	from events where created_at > %s group by type order by 2 desc`, sizingWindowDays, sizingWindow)

// blockIndexBacklogQuery is what the block-index reconciliation still owes documents: how many
// hold no indexed version (document_block_versions.version null), and when the oldest of those
// debts was incurred. The server's own tick reads the same two figures and logs them
// (docs.ReconcileBlockIndex); this is the read for somebody holding the database and not the
// log.
//
// Nothing dates a debt, so the rows are dated by the write that owed them, which is what
// refs.OwedBlockIndexDebts derives; the tick's own read of the same backlog derives it from
// that one text too, so the two figures cannot drift.
var blockIndexBacklogQuery = `
	select count(*), min(owed.owed_at) from (` + refs.OwedBlockIndexDebts + `
	) owed`

// backlogClockQuery is the newest write the database recorded: the created_at of its highest
// event id, read through events_pkey. It is the clock the oldest debt's age is measured
// against, in place of now(), because this report must print the same bytes twice over an
// unchanged database and an age against now() would move between the two runs. The same clock
// dates every write rate above it, and a document version write appends an event of its own,
// so the gap between the two is the reconciliation's own lag behind the writes it serves. A
// database that recorded no event at all has no clock, and the age prints as zero against
// `none`.
const backlogClockQuery = `select created_at from events order by id desc limit 1`

// BlockIndexBacklog is what the block-index reconciliation owes documents in this snapshot:
// how many are owed an index, when the oldest of those debts was incurred, and the clock the
// age is measured against.
type BlockIndexBacklog struct {
	Documents    int64
	OldestOwedAt time.Time
	ClockAt      time.Time
}

// Age is how long the oldest owed document has waited: from the write that owed it the index
// to the newest write the database recorded. Zero where nothing is owed, where the database
// recorded no write, and where the debt is newer than that write, which is the write that owed
// it arriving after the last event the clock read.
func (b BlockIndexBacklog) Age() time.Duration {
	if b.OldestOwedAt.IsZero() || b.ClockAt.IsZero() || !b.ClockAt.After(b.OldestOwedAt) {
		return 0
	}
	return b.ClockAt.Sub(b.OldestOwedAt)
}

// branchTables are the tables the migrations of this branch touch, each with whether the
// migration alters a relation that already held rows. graph_nodes, graph_tuples,
// graph_store_cutover and graph_erasures are created by 20261109013400 and written by
// 20261109051232, so no existing row is rewritten for them; document_blocks, block_refs and
// comments are altered by 20261109022248, and a table that large is a migration that needs
// another shape, which is what the limit refuses.
var branchTables = []struct {
	name    string
	altered bool
}{
	{"graph_nodes", false},
	{"graph_tuples", false},
	{"graph_store_cutover", false},
	{"graph_erasures", false},
	{"block_refs", true},
	{"comments", true},
	{"document_blocks", true},
}

// StoreKind is one kind of row this tree writes beside the store, with how many nodes and how
// many tuples one such row states and the rate its rows arrive at.
//
// The tuple counts are this tree's, read off its writers, and each counts the two base
// attributes graphstore.Write states for a node its insert makes beside the relations the
// writer states itself:
//   - a comment states its visibility and its state, which that call states for its node, the
//     artifact it is anchored to and the one that owns it (api's commentStored), and the
//     authored_by tuple of whoever wrote it (graphstore.Authored): five on a comment that has
//     both relations and an author the store can name, which is the upper bound this
//     projection takes;
//   - a block states its visibility and its state the same way, the block or document that
//     holds it and its membership of that document (refs.IndexBlocks): four; a block that
//     moves states a retraction and an assertion, which is the same two relations again;
//   - a block reference states the pair's own tuple, and its node is its block's, so it makes
//     no node and no base attribute of its own;
//   - a delivery states one delivered_to tuple (outbox.deliveredGraph). The role, session and
//     page nodes under it, and each role's held_by, are written only where they change, so
//     they are no part of a steady rate.
type StoreKind struct {
	Name, Driver  string
	Nodes, Tuples int
	// RowsADay is what the database was written at over the window, TuplesADay and
	// NodeRowsADay what that puts in graph_tuples and in graph_nodes a day.
	RowsADay, TuplesADay, NodeRowsADay float64
	// rateQuery counts the kind's own rows inside the window.
	rateQuery string
}

// storeKinds are the four, each with the rate read from the rows the projection itself holds
// rather than from an event type: a row of the projection is exactly one write of its kind, and
// its own created_at is when that write happened.
func storeKinds() []StoreKind {
	return []StoreKind{{
		Name: "comment", Driver: "comments.created_at", Nodes: 1, Tuples: 5,
		rateQuery: fmt.Sprintf(`select count(*) from comments where created_at > %s`, sizingWindow),
	}, {
		Name: "block", Driver: "document_blocks.created_at", Nodes: 1, Tuples: 4,
		rateQuery: fmt.Sprintf(`select count(*) from document_blocks where created_at > %s`, sizingWindow),
	}, {
		Name: "block_ref", Driver: "block_refs.created_at", Nodes: 0, Tuples: 1,
		rateQuery: fmt.Sprintf(`select count(*) from block_refs where created_at > %s`, sizingWindow),
	}, {
		Name: "delivery", Driver: "comment_deliveries and message_deliveries, sent", Nodes: 0, Tuples: 1,
		rateQuery: fmt.Sprintf(`select
			(select count(*) from comment_deliveries where state = 'sent' and created_at > %[1]s)
			+ (select count(*) from message_deliveries where state = 'sent' and created_at > %[1]s)`, sizingWindow),
	}}
}

// EventRate is one event type's rate over the sizing window.
type EventRate struct {
	Type   string
	PerDay float64
}

// TableSize is one table of this branch as the database holds it now.
type TableSize struct {
	Name    string
	Altered bool
	Rows    int64
	Bytes   int64
	// Held is false for a table the database does not have, which is a database older than
	// this branch's migrations.
	Held bool
}

// StoreTableGrowth is one of the two store tables projected forward: what it holds now, how
// many rows a day the kinds above put in it, the bytes a row it is measured at and where that
// figure came from, and the size it reaches in a year.
type StoreTableGrowth struct {
	Name            string
	Bytes, Rows     int64
	RowsADay        float64
	BytesARow       int64
	BytesARowSource string
	YearBytes       int64
	Past            bool
}

// SizingReport is what the sizing pass read. Every figure in it comes from a count or from a
// timestamp the database already holds, so two runs over one unchanged database print the same
// report.
type SizingReport struct {
	Limit    int64
	Events   []EventRate
	Tables   []TableSize
	Kinds    []StoreKind
	Growth   []StoreTableGrowth
	Backlog  BlockIndexBacklog
	Refusals []string
}

func (s *SizingReport) Refused() bool { return len(s.Refusals) > 0 }

// readSizing reads the growth pass in the census's own snapshot: the write rate by kind over
// the last 28 days of events, the size of every table this branch's migrations touch, the
// tuples and nodes a write of each kind states, where graph_tuples and graph_nodes reach in a
// year at the rate the database has been written at, and what the block-index reconciliation
// still owes documents.
func readSizing(ctx context.Context, tx pgx.Tx) (*SizingReport, error) {
	report := &SizingReport{Limit: pgmigrate.CensusTableLimit}
	events, err := readEventRates(ctx, tx)
	if err != nil {
		return nil, err
	}
	report.Events = events
	// Each branch table is measured once, and the growth projection below reads the two store
	// tables out of this map rather than measuring them again: readTableSize is a
	// pg_total_relation_size and a full count(*), and the two it would repeat are the largest
	// tables of the branch. The snapshot is the one repeatable-read transaction's, so the
	// figures the two passes print are the same figures.
	sizes := make(map[string]TableSize, len(branchTables))
	for _, table := range branchTables {
		size, err := readTableSize(ctx, tx, table.name)
		if err != nil {
			return nil, err
		}
		size.Altered = table.altered
		sizes[table.name] = size
		report.Tables = append(report.Tables, size)
		if table.altered && size.Held && size.Bytes >= report.Limit {
			report.Refusals = append(report.Refusals, fmt.Sprintf(
				"%s is %s, at or above the census limit of %s, and this branch's migrations alter it",
				size.Name, bytesText(size.Bytes), bytesText(report.Limit)))
		}
	}
	kinds := storeKinds()
	for index := range kinds {
		var window int64
		if err := tx.QueryRow(ctx, kinds[index].rateQuery).Scan(&window); err != nil {
			return nil, fmt.Errorf("read the write rate of %s: %w", kinds[index].Name, err)
		}
		kinds[index].RowsADay = float64(window) / sizingWindowDays
		kinds[index].TuplesADay = kinds[index].RowsADay * float64(kinds[index].Tuples)
		kinds[index].NodeRowsADay = kinds[index].RowsADay * float64(kinds[index].Nodes)
	}
	report.Kinds = kinds
	var tuplesADay, nodesADay float64
	for _, kind := range kinds {
		tuplesADay += kind.TuplesADay
		nodesADay += kind.NodeRowsADay
	}
	for _, growth := range []struct {
		name     string
		rowsADay float64
	}{{"graph_tuples", tuplesADay}, {"graph_nodes", nodesADay}} {
		size, measured := sizes[growth.name]
		if !measured {
			return nil, fmt.Errorf("the sizing projects %s, which branchTables does not name", growth.name)
		}
		projected := StoreTableGrowth{
			Name: growth.name, Bytes: size.Bytes, Rows: size.Rows, RowsADay: growth.rowsADay,
		}
		projected.BytesARow, projected.BytesARowSource = bytesARow(size)
		projected.YearBytes = size.Bytes + int64(growth.rowsADay*sizingYearDays)*projected.BytesARow
		projected.Past = projected.YearBytes >= report.Limit
		if projected.Past {
			report.Refusals = append(report.Refusals, fmt.Sprintf(
				"%s reaches %s within a year at %.2f rows a day, past the census limit of %s",
				projected.Name, bytesText(projected.YearBytes), projected.RowsADay, bytesText(report.Limit)))
		}
		report.Growth = append(report.Growth, projected)
	}
	backlog, err := readBlockIndexBacklog(ctx, tx)
	if err != nil {
		return nil, err
	}
	report.Backlog = backlog
	return report, nil
}

// readBlockIndexBacklog reads what the block-index reconciliation owes documents and the clock
// its age is measured against. It refuses nothing: a backlog is a fact for the reader of the
// report and for the driver that watches it grow, and what counts as too large depends on the
// rate the database is written at, which the lines above this one carry.
func readBlockIndexBacklog(ctx context.Context, tx pgx.Tx) (BlockIndexBacklog, error) {
	var backlog BlockIndexBacklog
	var oldest *time.Time
	if err := tx.QueryRow(ctx, blockIndexBacklogQuery).Scan(&backlog.Documents, &oldest); err != nil {
		return BlockIndexBacklog{}, fmt.Errorf("read the documents owed a block index: %w", err)
	}
	if oldest != nil {
		backlog.OldestOwedAt = *oldest
	}
	var clock *time.Time
	if err := tx.QueryRow(ctx, backlogClockQuery).Scan(&clock); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BlockIndexBacklog{}, fmt.Errorf("read the newest write the database recorded: %w", err)
	}
	if clock != nil {
		backlog.ClockAt = *clock
	}
	return backlog, nil
}

// bytesARow is the bytes one row of a store table costs, table, indexes and TOAST: measured
// from the table itself where it holds enough rows for that to mean anything, and otherwise the
// figure measured for this work, which the report names as borrowed. graph_nodes carries two
// indexes where graph_tuples carries seven, so the borrowed figure over-states a node, which is
// the safe direction for a size gate.
func bytesARow(size TableSize) (int64, string) {
	if size.Held && size.Rows >= measurableRows {
		return size.Bytes / size.Rows, "measured from this database"
	}
	return measuredBytesARow, "measured for dispatch://INFRA-1149 (" + measuredFrom +
		"), the table here holding too few rows to measure"
}

func readEventRates(ctx context.Context, tx pgx.Tx) ([]EventRate, error) {
	rows, err := tx.Query(ctx, eventRateQuery)
	if err != nil {
		return nil, fmt.Errorf("read the write rate by kind: %w", err)
	}
	defer rows.Close()
	var rates []EventRate
	for rows.Next() {
		var rate EventRate
		if err := rows.Scan(&rate.Type, &rate.PerDay); err != nil {
			return nil, fmt.Errorf("read a write rate: %w", err)
		}
		rates = append(rates, rate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the write rate by kind: %w", err)
	}
	// The statement orders by rate alone, so two types written the same number of times come
	// back in whatever order the plan produced them. The report orders the ties by name, so
	// two runs over one unchanged database print one output.
	sort.SliceStable(rates, func(i, j int) bool {
		if rates[i].PerDay != rates[j].PerDay {
			return rates[i].PerDay > rates[j].PerDay
		}
		return rates[i].Type < rates[j].Type
	})
	return rates, nil
}

// readTableSize reads one table's rows and its total relation size. A table the database does
// not have answers nothing held rather than failing: a census of a database older than this
// branch's migrations says which tables are missing.
func readTableSize(ctx context.Context, tx pgx.Tx, name string) (TableSize, error) {
	size := TableSize{Name: name}
	var oid *uint32
	if err := tx.QueryRow(ctx, `select to_regclass($1)::oid`, name).Scan(&oid); err != nil {
		return TableSize{}, fmt.Errorf("look up %s: %w", name, err)
	}
	if oid == nil || *oid == 0 {
		return size, nil
	}
	size.Held = true
	if err := tx.QueryRow(ctx, `select pg_total_relation_size($1)`, name).Scan(&size.Bytes); err != nil {
		return TableSize{}, fmt.Errorf("read the size of %s: %w", name, err)
	}
	if err := tx.QueryRow(ctx, `select count(*) from `+name).Scan(&size.Rows); err != nil {
		return TableSize{}, fmt.Errorf("count the rows of %s: %w", name, err)
	}
	return size, nil
}

// Write prints the sizing, one `census: sizing ` line per fact, so a driver can look for the
// line it wants by its leading fields.
func (s *SizingReport) Write(w io.Writer) {
	fmt.Fprintf(w, "census: sizing window_days=%d year_days=%d limit_bytes=%d\n",
		sizingWindowDays, sizingYearDays, s.Limit)
	for _, rate := range s.Events {
		fmt.Fprintf(w, "census: sizing write-rate type=%s per_day=%.2f\n", rate.Type, rate.PerDay)
	}
	if len(s.Events) == 0 {
		fmt.Fprintln(w, "census: sizing write-rate none (no event in the window)")
	}
	for _, table := range s.Tables {
		if !table.Held {
			fmt.Fprintf(w, "census: sizing table name=%s held=false\n", table.Name)
			continue
		}
		verdict := "inside"
		if table.Bytes >= s.Limit {
			verdict = "at-or-above"
		}
		fmt.Fprintf(w, "census: sizing table name=%s altered=%t rows=%s bytes=%d limit_bytes=%d verdict=%s\n",
			table.Name, table.Altered, pgmigrate.Thousands(table.Rows), table.Bytes, s.Limit, verdict)
	}
	for _, kind := range s.Kinds {
		fmt.Fprintf(w, "census: sizing tuples-a-write kind=%s tuples=%d nodes=%d rows_a_day=%.2f"+
			" tuples_a_day=%.2f nodes_a_day=%.2f driver=%s\n",
			kind.Name, kind.Tuples, kind.Nodes, kind.RowsADay, kind.TuplesADay, kind.NodeRowsADay, kind.Driver)
	}
	for _, growth := range s.Growth {
		fmt.Fprintf(w, "census: sizing rows-a-day table=%s rows_a_day=%.2f\n", growth.Name, growth.RowsADay)
		fmt.Fprintf(w, "census: sizing bytes-a-row table=%s bytes_a_row=%d source=%s\n",
			growth.Name, growth.BytesARow, growth.BytesARowSource)
		verdict := "inside"
		if growth.Past {
			verdict = "past"
		}
		fmt.Fprintf(w, "census: sizing year table=%s bytes_now=%d bytes_at_a_year=%d limit_bytes=%d verdict=%s\n",
			growth.Name, growth.Bytes, growth.YearBytes, s.Limit, verdict)
	}
	// The backlog of the reconciliation that defers a large document's block index: a loop
	// that falls behind owes more documents and owes them for longer, and nothing else in this
	// report would say so. Two lines, so a driver can read the count without parsing the age.
	fmt.Fprintf(w, "census: sizing backlog owed_documents=%d dated_by=%s\n",
		s.Backlog.Documents, backlogDatedBy)
	fmt.Fprintf(w, "census: sizing backlog oldest_owed_at=%s oldest_owed_age_seconds=%.2f against=%s\n",
		backlogTime(s.Backlog.OldestOwedAt), s.Backlog.Age().Seconds(), backlogTime(s.Backlog.ClockAt))
	for _, refusal := range s.Refusals {
		fmt.Fprintf(w, "census: sizing refusal: %s\n", refusal)
	}
	if len(s.Refusals) == 0 {
		fmt.Fprintln(w, "census: sizing ok")
	} else {
		fmt.Fprintf(w, "census: sizing REFUSED (%d reason(s))\n", len(s.Refusals))
	}
}

// bytesText writes a size as the report prints it: the bytes, and the GiB a human compares.
func bytesText(n int64) string {
	return fmt.Sprintf("%d bytes (%.2f GiB)", n, float64(n)/float64(1<<30))
}

// backlogDatedBy says on the line itself what dates a document's debt, because no column of
// the schema does and a reader of this report would otherwise have to read the source to know
// what the age is measured from (blockIndexBacklogQuery).
const backlogDatedBy = "the created_at of the latest markdown version each owed document holds," +
	" the write that owed it the index, or the document's own created_at where it holds no version"

// backlogTime writes a timestamp as the backlog lines print it: UTC to the nanosecond, so two
// runs over one unchanged database print one byte sequence, and `none` where there is no such
// time, which a zero value would otherwise print as a date in the year 1.
func backlogTime(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.UTC().Format(time.RFC3339Nano)
}
