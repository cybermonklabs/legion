package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
// migration alters a relation that already held rows. graph_nodes, graph_tuples and
// graph_erasures are created by 20261109013400 and written by 20261109051232, so no existing
// row is rewritten for them; document_blocks, block_refs and comments are altered by
// 20261109022248, and server_build_announcements by 20261115051200, and a table that large is a
// migration that needs another shape, which is what the limit refuses.
var branchTables = []struct {
	name    string
	altered bool
}{
	{"graph_nodes", false},
	{"graph_tuples", false},
	{"graph_erasures", false},
	{"block_refs", true},
	{"comments", true},
	{"document_blocks", true},
	{"server_build_announcements", true},
}

// The children of dispatch://INFRA-1098 whose writes the sizing sums (R4): this one, the one that
// moves the inbox, the messages and the read state, and the one that moves a block's text and rank.
const (
	movedHere       = "dispatch://INFRA-1149"
	movedWithInbox  = "dispatch://INFRA-1152"
	movedWithBlocks = "dispatch://INFRA-1253"
)

// StoreKind is one term of the projection: one kind of write the five children of
// dispatch://INFRA-1098 move, the child that moves it, how many nodes and how many tuples one such
// write appends, and how many such writes the database's own history holds inside the window.
// Rows a day is the sum over the terms of writes a day times tuples a write (R4).
//
// Every term reads tables that exist before this branch's first migration, so the sizing reads
// the same history on a database the store does not exist in yet, which is the database R4
// takes its readings from. Where a writer's tuples vary from one write to the next, each fact is
// a term of its own, counted over the writes that state it: a comment states its node and two
// base attributes always, and an authored_by, an anchored_to, an owned_by and its author's
// aliases only where its row names each. The role, session, person and page nodes a write makes,
// and each role's held_by, are written once for each thing and never again, so they are no part
// of a steady rate.
type StoreKind struct {
	Name, MovedBy, Driver string
	Nodes, Tuples         int
	// Writes is how many writes of the kind the window holds.
	Writes int64
	// RowsADay is what the database was written at over the window, TuplesADay and
	// NodeRowsADay what that puts in graph_tuples and in graph_nodes a day, and Share the
	// kind's part of every term's tuples a day.
	RowsADay, TuplesADay, NodeRowsADay, Share float64
	// Replayed and Unreadable are the version rows the moved-parent term parsed and how many
	// of them did not parse (replayParents), zero for every other term.
	Replayed, Unreadable int64
	// count is the scalar SQL that counts the kind's writes inside the window, over the rows
	// sizingWritesWith names, and empty for the term replayParents counts.
	count string
}

// movedParents is the name of the term no statement can count: a block's parent is the nesting
// of its rendered version, which no table written before the store holds (replayParents).
const movedParents = "block_parent_moved"

// storeKinds are the terms R4 states, each beside the writer whose tuples it counts:
//   - a comment's node and its visibility and state (graphstore.Write), the authored_by and the
//     aliases of whoever wrote it (graphstore.AuthorFacts), and the artifacts it is anchored to
//     and owned by (api's commentStored);
//   - a delivery to a comment's mention, one delivered_to (outbox.deliveredGraph);
//   - a version's blocks as refs.IndexBlocks states them: a block it inserts afresh, its node, its
//     two base attributes, its in_document and its part_of; a block it restores, its state and
//     its in_document, with at most the retraction and the assertion of a parent the restore
//     moved; a block it drops, its state gone; a block whose parent moved, the retraction of the
//     old parent and the assertion of the new; and a block reference, the pair's assertion and,
//     at most, the retraction that ends it, which no table records once the pair is gone;
//   - dispatch://INFRA-1253's text value and rank value of each block whose text a version wrote;
//   - dispatch://INFRA-1152's delivered_to of a message, and one tuple for each read mark, clear,
//     dismissal and snooze. Each of the tables those four are written to keeps the newest mark of
//     a key and no earlier one, so its term counts every key marked inside the window once.
func storeKinds() []StoreKind {
	return []StoreKind{{
		Name: "comment", MovedBy: movedHere, Nodes: 1, Tuples: 2,
		Driver: "comments.created_at: the comment's node and its visibility and state",
		count:  `(select count(*) from sized_comments)`,
	}, {
		Name: graphstore.TupleAuthoredBy, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each naming an author the store can key",
		count:  `(select count(*) from sized_comments where authored)`,
	}, {
		Name: graphstore.TupleAnchoredTo, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each anchored to a document the store can key",
		count:  `(select count(*) from sized_comments where anchored)`,
	}, {
		Name: graphstore.TupleOwnedBy, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each owned by a document the store can key",
		count:  `(select count(*) from sized_comments where owned)`,
	}, {
		Name: "author_alias", MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at: each alias_of and recorded_as the credential that wrote one proved",
		count:  `(select coalesce(sum(aliases), 0) from sized_comments)`,
	}, {
		Name: "delivery", MovedBy: movedHere, Tuples: 1,
		Driver: "comment_deliveries.created_at, sent: a delivered_to for each attempt that reached its destination",
		count:  `(select count(*) from comment_deliveries where state = '` + graphstore.DeliverySent + `' and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "block", MovedBy: movedHere, Nodes: 1, Tuples: 4,
		Driver: "document_block_texts of the versions written in the window, a block's first row: its node, its visibility" +
			" and state, its in_document and its part_of",
		count: `(select count(*) from sized_texts where written and earlier is null)`,
	}, {
		Name: "block_restored", MovedBy: movedHere, Tuples: 4,
		Driver: "document_block_texts of the versions written in the window, a text after a row that deleted the block:" +
			" its state and its in_document, and at most the retraction and the assertion of a parent the restore moved",
		count: `(select count(*) from sized_texts where written and not earlier)`,
	}, {
		Name: "block_gone", MovedBy: movedHere, Tuples: 1,
		Driver: "document_block_texts of the versions written in the window, a row that deleted the block: its state gone",
		count:  `(select count(*) from sized_texts where not written)`,
	}, {
		Name: movedParents, MovedBy: movedHere, Tuples: 2,
		Driver: "each version the block index indexed in the window against the one it indexed before, parsed and placed" +
			" as refs.PlaceBlocks places them, with blocks matched by their text, those of one document with equal text" +
			" in document order, so the term is exact except there: the old parent's retraction and the new one's assertion",
	}, {
		Name: "block_ref", MovedBy: movedHere, Tuples: 2,
		Driver: "block_refs.created_at: the pair's assertion and, at most, the retraction that ends it",
		count:  `(select count(*) from block_refs where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "block_text", MovedBy: movedWithBlocks, Tuples: 2,
		Driver: "document_block_texts of the versions written in the window, each row with a text: a text value and a rank value",
		count:  `(select count(*) from sized_texts where written)`,
	}, {
		Name: "message_delivery", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "message_deliveries.created_at, sent: a delivered_to for each attempt that reached its session",
		count:  `(select count(*) from message_deliveries where state = '` + graphstore.DeliverySent + `' and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "read_mark", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_threads.read_through and user_agent_read.read_through in the window, and user_agent_reply_read" +
			" of a reply written in the window: a read_through or reply_read",
		count: `((select count(*) from user_threads where read_through > ` + sizingWindow + `)
			+ (select count(*) from user_agent_read where read_through > ` + sizingWindow + `)
			+ (select count(*) from user_agent_reply_read r join messages m on m.id = r.reply_id
			   where m.created_at > ` + sizingWindow + `))`,
	}, {
		Name: "clear", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_agent_state.cleared_before in the window: a cleared",
		count:  `(select count(*) from user_agent_state where cleared_before > ` + sizingWindow + `)`,
	}, {
		Name: "dismissal", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_agent_thread_dismissed.dismissed_at and user_threads.dismissed_at in the window: a dismissed",
		count: `((select count(*) from user_agent_thread_dismissed where dismissed_at > ` + sizingWindow + `)
			+ (select count(*) from user_threads where dismissed_at > ` + sizingWindow + `))`,
	}, {
		Name: "snooze", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_ask_snooze.snoozed_until after the window opened, which every snooze written in it is: a snoozed",
		count:  `(select count(*) from user_ask_snooze where snoozed_until > ` + sizingWindow + `)`,
	}}
}

// sizingWritesWith is the two sets of rows several terms count over, each read once for every
// term that counts it: the comments written inside the window with the facts each one's write
// states beside its node, and the document_block_texts rows of the versions written inside it,
// each with whether the block's row before it held a text (null for the block's first row).
//
// A comment's facts are read off its row as their writers read them: its author as commentAuthor
// does (graphstore.AuthorOf), a document it is anchored to or owned by only where the document's
// address fits the store's bound, and its aliases as graphstore's aliases states them - the owner a
// session's credential proved, the session a person's credential proved, and the raw id a person
// was recorded under where that is not their canonical login.
var sizingWritesWith = `
	with sized_comments as (
		select (case when c.author->>'kind' <> '` + graphstore.KindSession + `'
		                  or octet_length(coalesce(c.author->>'id', '')) <= ` + addressBound + `
		             then ` + commentAuthor + ` end) is not null as authored,
		       coalesce(octet_length(anchor.ref_key) <= ` + addressBound + `, false) as anchored,
		       coalesce(octet_length(owner.ref_key) <= ` + addressBound + `, false) as owned,
		       (case when c.author->>'kind' = '` + graphstore.KindSession + `' and coalesce(c.author->>'id', '') <> ''
		                  and octet_length(c.author->>'id') <= ` + addressBound + `
		                  and c.author->'act'->>'session' = c.author->>'id'
		                  and user_threads_login(coalesce(c.author->>'owner', '')) <> '' then 1 else 0 end)
		       + (case when person.login <> '' and coalesce(c.author->'act'->>'session', '') <> ''
		                    and coalesce(c.author->'act'->>'token_id', '') = ''
		                    and octet_length(c.author->'act'->>'session') <= ` + addressBound + ` then 1 else 0 end)
		       + (case when person.login <> '' and coalesce(c.author->>'id', '') <> ''
		                    and c.author->>'id' <> person.login then 1 else 0 end) as aliases
		from comments c
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id
		cross join lateral (select case
			when c.author->>'kind' = 'user' and coalesce(c.author->>'id', '') <> ''
				then user_threads_login(c.author->>'id')
			when c.author->>'kind' = '` + graphstore.KindSession + `' and coalesce(c.author->>'id', '') <> '' then null
			when coalesce(c.author->>'owner', '') <> '' then user_threads_login(c.author->>'owner')
		end) person (login)
		where c.created_at > ` + sizingWindow + `
	),
	sized_texts as (
		select t.text is not null as written, earlier.held as earlier
		from artifact_versions v
		join document_block_texts t on t.artifact_id = v.artifact_id and t.version = v.number
		left join lateral (
			select p.text is not null as held from document_block_texts p
			where p.artifact_id = t.artifact_id and p.block_id = t.block_id and p.version < t.version
			order by p.version desc limit 1
		) earlier on true
		where v.created_at > ` + sizingWindow + `
	)`

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
	kinds, err := readStoreKinds(ctx, tx)
	if err != nil {
		return nil, err
	}
	report.Kinds = kinds
	var tuplesADay, nodesADay float64
	for _, kind := range kinds {
		tuplesADay += kind.TuplesADay
		nodesADay += kind.NodeRowsADay
	}
	for index := range report.Kinds {
		if tuplesADay > 0 {
			report.Kinds[index].Share = report.Kinds[index].TuplesADay / tuplesADay
		}
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

// readStoreKinds reads how many writes of each term the window holds: every term a statement
// counts in one statement over sizingWritesWith, so each set of rows it names is read once, and
// the moved parents by replayParents.
func readStoreKinds(ctx context.Context, tx pgx.Tx) ([]StoreKind, error) {
	kinds := storeKinds()
	counts := make([]string, 0, len(kinds))
	targets := make([]any, 0, len(kinds))
	for index := range kinds {
		if kinds[index].count == "" {
			continue
		}
		counts = append(counts, kinds[index].count)
		targets = append(targets, &kinds[index].Writes)
	}
	if err := tx.QueryRow(ctx, sizingWritesWith+"\n\tselect "+strings.Join(counts, ",\n\t\t")).Scan(targets...); err != nil {
		return nil, fmt.Errorf("read the writes of each kind inside the window: %w", err)
	}
	for index := range kinds {
		kind := &kinds[index]
		if kind.Name == movedParents {
			replayed, err := replayParents(ctx, tx)
			if err != nil {
				return nil, err
			}
			kind.Writes, kind.Replayed, kind.Unreadable = replayed.moved, replayed.read, replayed.unreadable
		}
		kind.RowsADay = float64(kind.Writes) / sizingWindowDays
		kind.TuplesADay = kind.RowsADay * float64(kind.Tuples)
		kind.NodeRowsADay = kind.RowsADay * float64(kind.Nodes)
	}
	return kinds, nil
}

// replayVersionsQuery is every version the block index indexed whose row was written inside the
// window, with the version that document's index indexed before it, each with its markdown and
// its document's ref_key. Ahead of each windowed version come its document_block_texts rows, each
// with the text the block's row before it held. The rows come in document and version order, a
// version's texts before the version itself, so the replay holds one document's last version and
// one version's texts at a time.
//
// A version's row is dated once, when it is written, and a document's versions are numbered in
// the order they are written, so the versions inside the window are each document's last ones
// and the one before the first of them is the one whose next version lies inside it.
var replayVersionsQuery = `
	with indexed as (
		select i.artifact_id, i.version, v.created_at > ` + sizingWindow + ` as inside,
		       coalesce(lead(v.created_at > ` + sizingWindow + `)
		                over (partition by i.artifact_id order by i.version), false) as followed_inside
		from document_indexed_versions i
		join artifact_versions v on v.artifact_id = i.artifact_id and v.number = i.version
	),
	replayed as (select artifact_id, version, inside from indexed where inside or followed_inside)
	select r.artifact_id::text, r.version, 1, a.ref_key, v.markdown, null::text, null::text, null::text
	from replayed r
	join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.version
	join artifacts a on a.id = r.artifact_id
	union all
	select r.artifact_id::text, r.version, 0, null, null, t.block_id, t.text, earlier.text
	from replayed r
	join document_block_texts t on t.artifact_id = r.artifact_id and t.version = r.version
	left join lateral (
		select p.text from document_block_texts p
		where p.artifact_id = t.artifact_id and p.block_id = t.block_id and p.version < t.version
		order by p.version desc limit 1
	) earlier on true
	where r.inside
	order by 1, 2, 3, 6`

// replayedParents is what replayParents read: how many blocks the windowed versions moved to
// another parent, over how many version rows, and how many of those rows did not parse.
type replayedParents struct{ moved, read, unreadable int64 }

// replayParents counts the blocks each version the block index indexed inside the window placed
// under another parent than the version it indexed before did, which is how many part_of moves
// that index stated (refs' reconcileBlockParents). No table written before the store holds a
// block's parent, so each version's markdown is parsed and its blocks placed by the walk the
// index places them by (refs.PlaceBlocks).
//
// A version's markdown names no paragraph's id - a parse mints a fresh one for every block that
// writes none - so the two versions' blocks are matched by their text, which is the body the index
// sums (markdown[From:To]): a block the later version left alone holds one text in both, and a
// block it wrote or dropped is the one its document_block_texts row names, under its earlier text
// in the version before and its new text in the later one (blockIdentities). Blocks of one
// document with equal text are matched in document order, so the count is exact except there. A
// version that does not parse is counted as read and unreadable, and neither it nor the version
// after it is compared, as the index wrote no blocks for it.
func replayParents(ctx context.Context, tx pgx.Tx) (replayedParents, error) {
	rows, err := tx.Query(ctx, replayVersionsQuery)
	if err != nil {
		return replayedParents{}, fmt.Errorf("read the indexed versions of the window: %w", err)
	}
	defer rows.Close()
	var done replayedParents
	var document string
	var earlier *placedVersion
	var changes []blockTextChange
	for rows.Next() {
		var artifactID string
		var version, part int
		var refKey, markdown, blockID, text, before *string
		if err := rows.Scan(&artifactID, &version, &part, &refKey, &markdown, &blockID, &text, &before); err != nil {
			return replayedParents{}, fmt.Errorf("read an indexed version of the window: %w", err)
		}
		if artifactID != document {
			document, earlier, changes = artifactID, nil, changes[:0]
		}
		if part == 0 {
			changes = append(changes, blockTextChange{id: *blockID, text: text, earlier: before})
			continue
		}
		done.read++
		later := placeVersion(*refKey, markdown)
		if later == nil {
			done.unreadable++
		}
		if earlier != nil && later != nil {
			done.moved += movedBlocks(earlier, later, changes)
		}
		earlier, changes = later, changes[:0]
	}
	if err := rows.Err(); err != nil {
		return replayedParents{}, fmt.Errorf("read the indexed versions of the window: %w", err)
	}
	return done, nil
}

// blockTextChange is one document_block_texts row of a version: the block it names, the text the
// version gave it (nil where the version dropped it), and the text its row before held (nil where
// there was none, or where that row recorded the block deleted).
type blockTextChange struct {
	id            string
	text, earlier *string
}

// placedVersion is one version as the replay reads it: its canonical markdown, its unique
// blocks, the index among them of each block the index places, in document order, and beside
// each of those the index of the block holding it, -1 for the document (refs.PlaceBlocks).
type placedVersion struct {
	markdown        string
	blocks          []model.ArtifactBlock
	placed, parents []int
}

// placeVersion parses one version's markdown and places its blocks, and answers nil for a
// version with no markdown or one that does not parse. The blocks are those of the render, as
// the index takes them from it, so their ranges index into the render's markdown.
func placeVersion(refKey string, markdown *string) *placedVersion {
	if markdown == nil {
		return nil
	}
	tree, err := pmdoc.ParseRendering(*markdown)
	if err != nil {
		return nil
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return nil
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	nodes, placed, parents := refs.PlaceBlocks(refKey, blocks)
	return &placedVersion{markdown: rendered, blocks: nodes, placed: placed, parents: parents}
}

// blockIdentity is a block as the replay matches it across two versions: the id a
// document_block_texts row names it by, or, for a block the later version left alone, its text
// and its place among the blocks of one version holding that text.
type blockIdentity struct {
	id         string
	text       string
	occurrence int
}

// blockPlacement is where a block sits: inside the block parent names, or, where held is false,
// in the document itself.
type blockPlacement struct {
	parent blockIdentity
	held   bool
}

// movedBlocks counts the blocks both versions hold whose placement the later one moved. changes
// are the later version's document_block_texts rows, in block id order.
func movedBlocks(earlier, later *placedVersion, changes []blockTextChange) int64 {
	wasNamed, nowNamed := map[string][]string{}, map[string][]string{}
	for _, change := range changes {
		if change.earlier != nil {
			wasNamed[*change.earlier] = append(wasNamed[*change.earlier], change.id)
		}
		if change.text != nil {
			nowNamed[*change.text] = append(nowNamed[*change.text], change.id)
		}
	}
	was := blockIdentities(earlier, wasNamed)
	placedBefore := make(map[blockIdentity]blockPlacement, len(earlier.placed))
	for index, block := range earlier.placed {
		placedBefore[was[block]] = placementOf(was, earlier.parents[index])
	}
	now := blockIdentities(later, nowNamed)
	var moved int64
	for index, block := range later.placed {
		before, both := placedBefore[now[block]]
		if both && placementOf(now, later.parents[index]) != before {
			moved++
		}
	}
	return moved
}

// blockIdentities names each placed block of one version as movedBlocks matches it with the
// other version, at the block's index among the version's blocks. named maps a text to the ids
// of the document_block_texts rows that name a block holding it in this version; the blocks of
// one text take those ids in document order, and every block left over, which the later version
// left alone, is its text and its place among the blocks of this version holding that text. The
// text is a slice of the version's markdown, so no block's text is copied.
func blockIdentities(version *placedVersion, named map[string][]string) []blockIdentity {
	identities := make([]blockIdentity, len(version.blocks))
	taken := map[string]int{}
	left := map[string]int{}
	for _, index := range version.placed {
		block := &version.blocks[index]
		text := version.markdown[block.From:block.To]
		if ids := named[text]; taken[text] < len(ids) {
			identities[index] = blockIdentity{id: ids[taken[text]]}
			taken[text]++
			continue
		}
		identities[index] = blockIdentity{text: text, occurrence: left[text]}
		left[text]++
	}
	return identities
}

// placementOf is the placement of a block whose parent is the block at parent among the
// version's blocks, -1 for the document.
func placementOf(identities []blockIdentity, parent int) blockPlacement {
	if parent < 0 {
		return blockPlacement{}
	}
	return blockPlacement{parent: identities[parent], held: true}
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
		// The share has a line of its own, so the line above keeps the fields a driver reads it by.
		replay := ""
		if kind.Name == movedParents {
			replay = fmt.Sprintf(" versions_read=%d versions_unreadable=%d", kind.Replayed, kind.Unreadable)
		}
		fmt.Fprintf(w, "census: sizing share kind=%s moved_by=%s writes_in_window=%d share_of_tuples=%.4f%s\n",
			kind.Name, kind.MovedBy, kind.Writes, kind.Share, replay)
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
		refs.OwedTimeText(s.Backlog.OldestOwedAt), s.Backlog.Age().Seconds(), refs.OwedTimeText(s.Backlog.ClockAt))
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
