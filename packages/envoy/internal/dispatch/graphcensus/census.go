package graphcensus

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// reportedDifferences is how many differences of one direction of one kind the report prints by
// name. The count beside them is every one there is, so a store that drifted by a million rows
// prints a million in its count and twenty rows to start from.
const reportedDifferences = 20

// projection is one kind of the store held against what projects it.
//
// rows is a select of (row_label, node_kind, node_address, digest): one row per projection row,
// naming the node that row says it projects and digesting the fields the row and its node
// share. nodes is a select of (id, digest): one row per live node of the kind, digesting the
// same fields as the store holds them. The census holds the two against each other in both
// directions, so a row with no node and a node with no row are each a difference.
//
// A kind with no projection table takes the set of actors the projected tables name as its rows
// (people and agents below), which is the side that exists for it.
type projection struct {
	// name is the kind as graph_store_cutover and the report name it, nodeKind the kind of
	// graph_nodes row it projects and table the relation its rows come from, for the report.
	name     string
	nodeKind string
	table    string
	// note says what this kind's two sides are where that is not the plain one row per node,
	// so the report says what it checked rather than only what it found.
	note string
	rows string
	// nodes is the live nodes of the kind. It names no state: liveNodes wraps it.
	nodes string
}

// KindReport is one kind's census: how many rows and how many live nodes it read, and what the
// two said about each other.
type KindReport struct {
	Kind, NodeKind, Table, Note string
	Rows, Nodes                 int64
	// RowDifferences and NodeDifferences are how many there are in each direction, and the
	// Reported slices the first reportedDifferences of them, each as its cause and its row.
	RowDifferences, NodeDifferences int64
	ReportedRows, ReportedNodes     []Difference
}

// Difference is one disagreement: why it is one, and the row or node it was found at.
type Difference struct{ Cause, At string }

// CutoverState is one row of graph_store_cutover: whether the store has taken that kind over.
type CutoverState struct {
	Kind     string
	Complete bool
}

// StoreReport is what the store census found. It is a report and not an error: a census that
// reads a difference has done its job, and the deployment reading it decides.
type StoreReport struct {
	Kinds   []KindReport
	Cutover []CutoverState
	// Sizing is what the sizing pass found, or nil when it was not asked for.
	Sizing *SizingReport
}

// Rows is how many projection rows the census checked over every kind.
func (r *StoreReport) Rows() int64 {
	var rows int64
	for _, kind := range r.Kinds {
		rows += kind.Rows
	}
	return rows
}

// Differences is how many disagreements the census read over every kind and both directions.
func (r *StoreReport) Differences() int64 {
	var found int64
	for _, kind := range r.Kinds {
		found += kind.RowDifferences + kind.NodeDifferences
	}
	return found
}

// Refused reports whether the census refuses. It passes only on a census that read more than
// zero rows and found no difference in either direction: a census over a database holding
// nothing proves nothing, and passing it would make the gate read as green wherever the
// projections are empty - a fresh database, or a read that reached the wrong one.
func (r *StoreReport) Refused() bool {
	if r.Rows() == 0 || r.Differences() > 0 {
		return true
	}
	return r.Sizing != nil && r.Sizing.Refused()
}

// The kinds the census holds the store to. The first three are the kinds graph_store_cutover
// names, each with a table that projects it; the four after them are the people and the agents,
// which no table projects.
//
// A block's address is its document's ref_key, '#' and its block id, and a block whose address
// is past refs.MaxBlockNodeBytes is deliberately no node of the store: its row carries no node
// address and nothing is stated about it. Both block sides leave those rows out, so the census
// reports no difference the writers made on purpose.
var projections = []projection{{
	name:     "comment",
	nodeKind: "comment",
	table:    "comments",
	note:     "one node per row; the digest is the row's created_at and author against the node's made_at and made_by",
	rows: `
		select 'comment ' || c.id::text, c.node_kind, c.node_address,
		       md5(c.created_at::text || '|' || c.author::text)
		from comments c`,
	nodes: `
		select n.id, md5(n.made_at::text || '|' || n.made_by::text)
		from graph_nodes n where n.kind = 'comment'`,
}, {
	name:     "block",
	nodeKind: refs.BlockNodeKind,
	table:    "document_blocks",
	note: "one node per row, blocks past the address bound aside; the digest is the row's parent_block_id" +
		" against the parent the node's part_of tuple states, a block with no such tuple telling itself" +
		" from one the document holds directly",
	rows: `
		select 'block ' || d.node_id, d.node_kind, d.node_address,
		       md5(d.node_id || '|t|' || coalesce(d.parent_block_id, ''))
		from document_blocks d
		where octet_length(d.node_id) <= ` + blockBound,
	nodes: `
		select n.id, md5(n.id || '|' || ` + currentParent + `)
		from graph_nodes n where n.kind = '` + refs.BlockNodeKind + `'`,
}, {
	name:     "block_ref",
	nodeKind: refs.BlockNodeKind,
	table:    "block_refs",
	note: "a row is one link out of a block and its node is that block's, so several rows share one node:" +
		" the node side is the live block nodes the rows name, and the digest is the row's from_node against it",
	rows: `
		select 'block_ref ' || r.from_node || ' ' || r.kind || ' -> ' || r.to_kind || ' ' || r.to_id,
		       r.node_kind, r.node_address, md5(r.from_node)
		from block_refs r
		where octet_length(r.from_node) <= ` + blockBound,
	nodes: `
		select n.id, md5(n.id)
		from graph_nodes n
		where n.kind = '` + refs.BlockNodeKind + `'
		  and exists (select 1 from block_refs r where r.node_kind = n.kind and r.node_address = n.id)`,
}, {
	name:     graphstore.KindPerson,
	nodeKind: graphstore.KindPerson,
	table:    "comments.author",
	note:     "no table projects a person, so the row side is the people the projected tables name as authors",
	rows: `
		select 'person ' || p.login, '` + graphstore.KindPerson + `', p.login, ''
		from (` + commentPeople + `) p`,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindPerson + `'`,
}, {
	name:     graphstore.KindSession,
	nodeKind: graphstore.KindSession,
	table:    "comments.author, comment_deliveries, message_deliveries",
	note:     "no table projects a session, so the row side is the sessions those tables name as authors and as reached",
	rows: `
		select 'session ' || s.id, '` + graphstore.KindSession + `', s.id, ''
		from (` + recordedSessions + `) s`,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindSession + `'`,
}, {
	name:     graphstore.KindRole,
	nodeKind: graphstore.KindRole,
	table:    "comment_deliveries.target, messages.target, issues.route",
	note:     "no table projects a role, so the row side is the roles those tables address",
	rows: `
		select 'role ' || r.name, '` + graphstore.KindRole + `', r.name, ''
		from (` + addressedRoles + `) r`,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindRole + `'`,
}, {
	name:     graphstore.KindPage,
	nodeKind: graphstore.KindPage,
	table:    "comment_deliveries.target, messages.target, issues.route",
	note: "a role's page, whose id is its role's: the row side is the pages of the roles those tables address," +
		" and a session's own page is no part of this census",
	rows: `
		select 'page ' || '` + graphstore.KindRole + `/' || r.name, '` + graphstore.KindPage + `',
		       '` + graphstore.KindRole + `/' || r.name, ''
		from (` + addressedRoles + `) r`,
	nodes: `
		select n.id, '' from graph_nodes n
		where n.kind = '` + graphstore.KindPage + `' and n.id like '` + graphstore.KindRole + `/%'`,
}}

// blockBound is refs.MaxBlockNodeBytes as a statement spells it. It is written into the text
// rather than bound as a parameter because each side is spliced into a statement of its own and
// one shared constant keeps the two sides reading the same bound.
var blockBound = strconv.Itoa(refs.MaxBlockNodeBytes)

// currentParent is the parent a block node states now, as a statement reads it: the newest
// tuple per far end, kept while that newest one asserts (graphstore.CurrentOut), with the
// parent's own block id read out of its address. A ref_key holds no '#' (migration 0121), so
// the first one parts the document from the block.
//
// It answers `t|<block id>` for a block another block holds, `t|` for one the document holds
// directly, which document_blocks spells as a null parent_block_id, and `f|` for a node that
// states no parent at all. The last two are different facts - a block the document holds, and
// a block the store says nothing about - and a digest that wrote both as the empty string
// would read a node with no parent as agreeing with every top-level row. Two asserted parents
// are an impossible state the digests must still tell from one, so they are joined rather than
// reduced to either of them.
const currentParent = `
	(select coalesce(string_agg('t|' || held.parent, ',' order by held.parent), 'f|')
	 from (
		select distinct on (t.to_kind, t.to_id)
		       case when t.to_kind = '` + refs.BlockNodeKind + `'
		            then substr(t.to_id, position('#' in t.to_id) + 1) else '' end as parent,
		       t.asserted
		from graph_tuples t
		where t.from_kind = '` + refs.BlockNodeKind + `' and t.from_id = n.id
		  and t.kind = '` + graphstore.TuplePartOf + `' and t.to_kind <> '` + graphstore.KindValue + `'
		order by t.to_kind, t.to_id, t.seq desc
	 ) held where held.asserted)`

// commentPeople are the people the projected tables name as the author of a row, as
// graphstore.AuthorOf reads an actor: the login an actor of kind user carries, or the owner a
// personal token's actor carries, each canonicalised the way user_threads_login canonicalises a
// login (migration 0095, which is model.CanonicalLogin in SQL).
const commentPeople = `
	select distinct user_threads_login(author.login) as login
	from (
		select case
			when c.author->>'kind' = 'user' and coalesce(c.author->>'id', '') <> '' then c.author->>'id'
			when c.author->>'kind' = 'session' and coalesce(c.author->>'id', '') <> '' then null
			when coalesce(c.author->>'owner', '') <> '' then c.author->>'owner'
		end as login
		from comments c
	) author
	where author.login is not null and user_threads_login(author.login) <> ''`

// recordedSessions are the sessions the projected tables and the delivery records name: a
// comment a session wrote (graphstore.AuthorOf), and every session a delivery reached - a
// comment mention's and a targeted message's alike, whether the outbox's record states it
// (outbox.deliveredGraph) or the attempt's own settle does (graphstore.Reached).
//
// R14 is exact about which attempt that is: "a sent attempt is a delivery; a pending or failed
// one is not". So this binds `state = 'sent'`, the test the writer binds too: an attempt still
// pending has reached nobody yet and a failed one never will, and counting either here would
// hold the store to a node no writer is meant to have written.
const recordedSessions = `
	select distinct id from (
		select case when c.author->>'kind' = 'session' and coalesce(c.author->>'id', '') <> ''
		            then c.author->>'id' end as id
		from comments c
		union
		select d.session_id from comment_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state = '` + graphstore.DeliverySent + `'
		union
		select d.session_id from message_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state = '` + graphstore.DeliverySent + `'
	) sessions
	where id is not null and id <> ''`

// addressedRoles are the roles the records a delivery is routed by name: a comment mention's
// target, a message's target and an issue's route. The publisher makes a role's node when a
// batch resolves a destination that names it, and those are the three places a destination can
// name one.
const addressedRoles = `
	select distinct name from (
		select substring(d.target from 6) as name from comment_deliveries d where d.target like 'role:%'
		union
		select substring(m.target from 6) from messages m where m.target like 'role:%'
		union
		select substring(i.route from 6) from issues i where i.route like 'role:%'
	) roles
	where name <> ''`

// StoreCensus reads the node and tuple store against the tables that project it, in one
// read-only repeatable-read transaction on a connection of its own. It writes nothing: the
// transaction is read only, and every statement it runs is a select.
//
// A difference is reported by its cause and by the row it was found at, in both directions: a
// projection row naming no node, naming a node the store does not hold or naming one it holds
// gone; a row whose shared fields disagree with its node's; and a live node no row projects.
//
// Two writes can state one fact twice by design and nothing here reads wrong for it: a tuple at
// the source_event sentinel is outside both repeat keys, and the backfill writes every fact it
// cannot date from a row's own event at that sentinel, so a second run appends a second tuple
// stating what the store already holds. The current read takes the newest asserting tuple per
// far end, which is that same fact, and this census counts nodes and projection rows rather
// than tuples.
//
// sizing adds the growth pass (SizingReport), which reads the same snapshot.
func StoreCensus(ctx context.Context, databaseURL string, sizing bool) (*StoreReport, error) {
	conn, err := connect(ctx, databaseURL, applicationName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("open the census snapshot: %w", err)
	}
	defer tx.Rollback(ctx)
	report := &StoreReport{}
	for _, held := range projections {
		kind, err := censusOfKind(ctx, tx, held)
		if err != nil {
			return nil, err
		}
		report.Kinds = append(report.Kinds, kind)
	}
	cutover, err := readCutover(ctx, tx)
	if err != nil {
		return nil, err
	}
	report.Cutover = cutover
	if sizing {
		sized, err := readSizing(ctx, tx)
		if err != nil {
			return nil, err
		}
		report.Sizing = sized
	}
	return report, nil
}

// censusOfKind reads one kind: how many rows and live nodes there are, and the differences in
// each direction with the first reportedDifferences of them.
//
// The four counts are four scalar subqueries of one statement rather than four round trips, so
// a census of seven kinds costs seven reads here and not twenty-eight. Each subquery is the
// statement it was on its own, so each is planned and streamed exactly as before.
func censusOfKind(ctx context.Context, tx pgx.Tx, held projection) (KindReport, error) {
	kind := KindReport{Kind: held.name, NodeKind: held.nodeKind, Table: held.table, Note: held.note}
	if err := tx.QueryRow(ctx, `select
		(select count(*) from (`+held.rows+`) rows (row_label, node_kind, node_address, digest)),
		(select count(*) from (`+liveNodes(held)+`) nodes (id, digest)),
		(select count(*) from (`+rowDifferences(held)+`) rows_found),
		(select count(*) from (`+nodeDifferences(held)+`) nodes_found)`,
	).Scan(&kind.Rows, &kind.Nodes, &kind.RowDifferences, &kind.NodeDifferences); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", held.name, err)
	}
	var err error
	if kind.RowDifferences > 0 {
		kind.ReportedRows, err = readDifferences(ctx, tx, held.name, rowDifferences(held))
		if err != nil {
			return KindReport{}, err
		}
	}
	if kind.NodeDifferences > 0 {
		kind.ReportedNodes, err = readDifferences(ctx, tx, held.name, nodeDifferences(held))
		if err != nil {
			return KindReport{}, err
		}
	}
	return kind, nil
}

// liveNodes is the kind's node side bound to the live state: a node the store holds gone is no
// node a row may project, and the row side holds it to that separately (goneNodeCause).
func liveNodes(held projection) string {
	return `select id, digest from (` + held.nodes + `) node (id, digest)
		where exists (select 1 from graph_nodes live
		              where live.kind = '` + held.nodeKind + `' and live.id = node.id
		                and live.state = '` + graphstore.StateLive + `')`
}

// rowDifferences reads the row side against the store: every row that names no node, names one
// the store does not hold, names one it holds gone, or disagrees with its node's digest. The
// gone test is its own cause rather than a missing node, because a row projecting a node the
// store has ended is a different defect from a row projecting nothing.
func rowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state <> '` + graphstore.StateLive + `'
				then 'the row names a node the store holds ' || node.state
			else 'the row and its node disagree: the row digests ' || row.digest
			     || ' and the node ' || coalesce(live.digest, '(none)')
		end,
		row.row_label
		from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
		left join graph_nodes node on node.kind = row.node_kind and node.id = row.node_address
		left join (` + liveNodes(held) + `) live (id, digest) on live.id = row.node_address
		where row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state <> '` + graphstore.StateLive + `'
		   or row.digest is distinct from live.digest
		order by 2, 1`
}

// nodeDifferences reads the store against the row side: every live node of the kind no row
// projects.
func nodeDifferences(held projection) string {
	return `
		select 'a live node no row projects', '` + held.nodeKind + ` ' || node.id
		from (` + liveNodes(held) + `) node (id, digest)
		where not exists (
			select 1 from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
			where row.node_kind = '` + held.nodeKind + `' and row.node_address = node.id)
		order by 2`
}

func readDifferences(ctx context.Context, tx pgx.Tx, kind, sql string) ([]Difference, error) {
	rows, err := tx.Query(ctx, sql+` limit `+strconv.Itoa(reportedDifferences))
	if err != nil {
		return nil, fmt.Errorf("read the differences of %s: %w", kind, err)
	}
	defer rows.Close()
	var found []Difference
	for rows.Next() {
		var difference Difference
		if err := rows.Scan(&difference.Cause, &difference.At); err != nil {
			return nil, fmt.Errorf("read a difference of %s: %w", kind, err)
		}
		found = append(found, difference)
	}
	return found, rows.Err()
}

func readCutover(ctx context.Context, tx pgx.Tx) ([]CutoverState, error) {
	rows, err := tx.Query(ctx, `select kind, complete from graph_store_cutover order by kind`)
	if err != nil {
		return nil, fmt.Errorf("read the cutover: %w", err)
	}
	defer rows.Close()
	var states []CutoverState
	for rows.Next() {
		var state CutoverState
		if err := rows.Scan(&state.Kind, &state.Complete); err != nil {
			return nil, fmt.Errorf("read a cutover row: %w", err)
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// Write prints the report, one `census: store ` line per fact, every difference under its kind,
// and the sizing after it, ending with the verdict. It prints counts, digests, kinds and the
// addresses of rows and nodes; no body, no actor and no value of an attribute leaves the
// database.
func (r *StoreReport) Write(w io.Writer) {
	for _, kind := range r.Kinds {
		fmt.Fprintf(w, "census: store kind=%s nodes=%s rows=%s table=%s rows_without_a_node=%s nodes_without_a_row=%s\n",
			kind.Kind, pgmigrate.Thousands(kind.Nodes), pgmigrate.Thousands(kind.Rows), kind.Table,
			pgmigrate.Thousands(kind.RowDifferences), pgmigrate.Thousands(kind.NodeDifferences))
		fmt.Fprintf(w, "census: store kind=%s checks %s\n", kind.Kind, kind.Note)
		writeDifferences(w, kind.Kind, kind.RowDifferences, kind.ReportedRows)
		writeDifferences(w, kind.Kind, kind.NodeDifferences, kind.ReportedNodes)
	}
	for _, state := range r.Cutover {
		fmt.Fprintf(w, "census: store cutover kind=%s complete=%t\n", state.Kind, state.Complete)
	}
	fmt.Fprintf(w, "census: store rows_checked=%s differences=%s\n",
		pgmigrate.Thousands(r.Rows()), pgmigrate.Thousands(r.Differences()))
	if r.Sizing != nil {
		r.Sizing.Write(w)
	}
	switch {
	case r.Rows() == 0:
		fmt.Fprintln(w, "census: store REFUSED (the census read no projection row at all, so it proves nothing)")
	case r.Differences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s difference(s))\n", pgmigrate.Thousands(r.Differences()))
	case r.Sizing != nil && r.Sizing.Refused():
		fmt.Fprintf(w, "census: store ok, sizing REFUSED (%d reason(s))\n", len(r.Sizing.Refusals))
	default:
		fmt.Fprintln(w, "census: store ok")
	}
}

func writeDifferences(w io.Writer, kind string, total int64, found []Difference) {
	for _, difference := range found {
		fmt.Fprintf(w, "census: store kind=%s difference at %s: %s\n", kind, difference.At, difference.Cause)
	}
	if total > int64(len(found)) {
		fmt.Fprintf(w, "census: store kind=%s and %s more difference(s) not printed\n",
			kind, pgmigrate.Thousands(total-int64(len(found))))
	}
}
