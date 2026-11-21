package graphcensus

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// share. nodes is a select of (id, digest): one row per node of the kind, at any state,
// digesting the same fields as the store holds them; the census reads those not gone
// (presentNodes). The census holds the two against each other in both directions, so a row with
// no node and a node with no row are each a difference.
//
// The ties of decisions, readings and asks are held triple by triple instead (tuples), so a
// relation that reaches the wrong node or the wrong far end reads as a row and a tuple, each
// named, and every other row of that node reads nothing.
//
// A kind with no projection table takes the set of actors the projected tables name as its rows
// (people and agents below), which is the side that exists for it; a role takes the roles the
// event log ever named as well (addressedRoles), and a person and a session the authors and the
// sessions reached that the events of a comment the erase took still name (commentWriters,
// recordedSessions).
type projection struct {
	// name is the kind as the report names it, nodeKind the kind of graph_nodes row it
	// projects and table the relation its rows come from, for the report.
	name     string
	nodeKind string
	table    string
	// note says what this kind's two sides are where that is not the plain one row per node,
	// so the report says what it checked rather than only what it found.
	note string
	rows string
	// nodes is the kind's nodes at any state. It names no state: presentNodes wraps it.
	nodes string
	// tuples, where it is set, holds the kind triple by triple, over the triple of holding node,
	// tuple kind and far end, and nodes is unused. It is a
	// select of (from_kind, from_id, kind, to_kind, to_id): one row per relation of the kind a
	// live node states now (heldTuples). rows then answers (row_label, node_kind, node_address,
	// kind, to_kind, to_id): one row per relation a projection row states, of the node it names.
	// A row no tuple states is a difference named at the row, and a tuple no row projects one
	// named at the tuple, so a tuple moved to the wrong far end reads as the row it left and the
	// tuple it became, and the other rows of that node read nothing.
	tuples string
	// names is a select of one text column, the kind's row side, which the census reads once for
	// the run (readNames) and which rows and pastBound read as $1, rather than reading it again in
	// every statement that reads the rows: a row side that reads the event log scans it, and no
	// index narrows that scan. Two kinds with one names read it once between them, as a role and
	// its page do. It is empty for a kind whose rows read their own table.
	names string
	// authored says names reads as $1, $2 and $3 the kinds, ids and times of storedAuthors
	// (censusAuthors), which the census reads once for the run (readStoredAuthors), as it reads
	// each kind's names once: its arms read the store's log, the largest table, and the person and
	// session kinds would otherwise each read them in their own names.
	authored bool
	// linked says rows and nodes read as $1 the tuple kinds a block's link is held under: the
	// pointer attributes the installed block schema declares and `mentions` (blockLinkKinds).
	linked bool
	// pastBound counts the addresses this kind's row side names that are past the store's bound
	// (graphstore.MaxAddressBytes), which the writers left unprojected on purpose: it is a full
	// `select count(*)`, reads $1 where names is set or linked, and is empty for a kind no
	// address of which can pass the bound. The census reports the count as a class of its own and
	// excludes those addresses from both comparisons, so a row no store entry answers by design
	// is no difference.
	pastBound string
	// unmarked counts the rows of the kind's table that name no node and are no difference: an
	// asks or ask_followers row a process of the release before wrote, which graph_edges' old
	// arms answer until the decisions step's backfill or catch-up marks it. It is a full
	// `select count(*)` of the rows of the kind's own population - an ask's
	// anchor counts the unmarked asks that name a document to anchor to, and its answer those
	// that hold one - so a kind whose every row is unmarked is no empty kind (KindReport.Empty).
	// The row side reads marked rows alone, and the census reports the count as a class of its
	// own, which the handoff's list for the root names as the test of the three old arms. Empty
	// for a kind whose row names no node only by a defect.
	unmarked string
	// owed is a predicate over node.id, a live node of the kind (for a kind held triple by
	// triple, the node holding the tuple), that holds where the node's row is owed by a writer
	// that has not run yet rather than missing: such a node is no node-side difference. Empty for
	// a kind whose every node has its row from the write that stated it.
	owed string
	// emptyAgrees says a kind that holds no row, no live node and no unmarked row is no refusal:
	// one of the tie kinds, a decision's, a reading's and an answered or
	// followed ask's, which a deployment holds none of until an agent writes a decision or a
	// person answers an ask. Every other kind is one every store holds rows of, and checking
	// none of it refuses.
	emptyAgrees bool
}

// KindReport is one kind's census: how many rows and how many nodes not gone it read, and what
// the two said about each other.
type KindReport struct {
	Kind, NodeKind, Table, Note string
	// Rows and Nodes are how many of each side the census read. Of a kind held triple by triple
	// (Triples), Rows counts the relations its rows state and Nodes the relations the store states.
	Rows, Nodes int64
	// Triples says the kind is held triple by triple (projection.tuples): Nodes counts tuples, a
	// row difference is a row no tuple states and a node difference a tuple no row projects.
	Triples bool
	// RowDifferences and NodeDifferences are how many there are in each direction, and the
	// Reported slices the first reportedDifferences of them, each as its cause and its row.
	RowDifferences, NodeDifferences int64
	ReportedRows, ReportedNodes     []Difference
	// PastBound is how many addresses this kind's row side named that are past the store's bound,
	// each a class of its own and no difference (projection.pastBound).
	PastBound int64
	// CountsUnmarked says the kind holds its table's rows that name no node as a class of their
	// own, and Unmarked is how many there are, none of them a difference (projection.unmarked).
	CountsUnmarked bool
	Unmarked       int64
	// EmptyAgrees says a kind that read no row, no live node and no unmarked row is no refusal
	// (projection.emptyAgrees).
	EmptyAgrees bool
}

// Empty reports whether the kind read no row, no live node and no unmarked row, and so holds
// nothing to compare. A table whose rows all name no node yet holds rows still: they are the
// earlier release's, which the census cannot hold to a node until the backfill marks them.
func (k KindReport) Empty() bool { return k.Rows == 0 && k.Nodes == 0 && k.Unmarked == 0 }

// Difference is one disagreement: why it is one, and the row or node it was found at.
type Difference struct{ Cause, At string }

// StoreReport is what the store census found. It is a report and not an error: a census that
// reads a difference has done its job, and the deployment reading it decides.
type StoreReport struct {
	Kinds []KindReport
	// Projects is the project census (ProjectCensus): for each kind whose nodes carry a project
	// or parties, its rows with no node, with a node naming neither, and with a node naming
	// another than the row's. Every count of it refuses the census. ProjectIncomplete names the
	// first of those rows (ProjectComplete), as many as the census names differences of a kind.
	Projects          []ProjectKindReport
	ProjectIncomplete []string
	// Absent is true where the database holds no store at all: graph_nodes is created by the
	// first migration of this branch (20261109013400), so a database it has not reached has no
	// kind to read, and the census reads none.
	Absent bool
	// StoreOnly is, for each tuple kind no table projects (copied_from, moved_to and removed_by),
	// how many the store states now, how many of those reach a far end that does not resolve and
	// how many reach one in a document that still owes its block index (storeOnlyRead). An
	// unresolved far end refuses the census; a count does not, and neither does a far end left out.
	StoreOnly []StoreOnlyReport
	// Dangling is the dangling-citation counter over the restore's work (DanglingCitations), and
	// Collisions the collision census of the same work (CollisionsOf), the block ids the restore
	// must change for each issue: both read the census's snapshot, so both answer before the
	// restore moves anything. Neither refuses the census. The returning
	// set counts before the restore by design, and the moved set is a finding the driver that
	// watches it holds to zero: a decisions document may hold a citation of a section an edit
	// deleted before this release, which no move made and no deploy should wait on.
	Dangling   DanglingReport
	Collisions []Collision
	// Decisions is what the decisions step's third phase still owes (DecisionsCensus), set by the
	// census command from docs' reader of the decisions documents' sections, nil where no caller
	// read it. It refuses nothing.
	Decisions *DecisionsCensus
	// Sizing is what the sizing pass found, or nil when it was not asked for.
	Sizing *SizingReport
}

// StoreOnlyReport is one tuple kind no table projects: how many relations of it the store states
// now, how many of those reach a far end that does not resolve, and how many the census leaves
// out, because the far end they reach is in a document that still owes its block index, which
// holds no row yet for a block its latest version holds.
type StoreOnlyReport struct {
	Kind                        string
	Tuples, Unresolved, LeftOut int64
}

// Unresolved is how many far ends of the store-only kinds do not resolve, over every such kind.
func (r *StoreReport) Unresolved() int64 {
	var unresolved int64
	for _, kind := range r.StoreOnly {
		unresolved += kind.Unresolved
	}
	return unresolved
}

// ProjectFindings is how many rows and nodes the project census found incomplete over every kind.
func (r *StoreReport) ProjectFindings() int64 {
	var found int64
	for _, kind := range r.Projects {
		found += kind.Findings()
	}
	return found
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

// Unpopulated is every kind the census checked no row of, in the order it read them, but a kind
// that may be empty (KindReport.EmptyAgrees) and is: no row, no live node and no unmarked row.
func (r *StoreReport) Unpopulated() []string {
	var empty []string
	for _, kind := range r.Kinds {
		if kind.Rows == 0 && !(kind.EmptyAgrees && kind.Empty()) {
			empty = append(empty, kind.Kind)
		}
	}
	return empty
}

// Refused reports whether the census refuses. It passes only where it checked more than zero
// rows of every kind it covers and found no difference in either direction (R19), and where every
// far end of a kind no table projects resolves. A total over the kinds is no such proof: two
// empty sides of one kind agree, and one kind's rows say nothing about another's, so a census
// whose comments carried every row would pass while it proved nothing of the blocks, the roles or
// the sessions. One of the tie kinds is the one exception: no row, no node and no
// unmarked row of it is a store nothing of that kind was written to yet, and is no refusal
// (projection.emptyAgrees). A table of the earlier release whose rows no backfill has marked yet
// is not that store: its kind checked no row while rows stand, and it refuses.
//
// A database the store does not exist in yet (Absent) has nothing to census. The sizing is taken
// on just such a database (R4 reads before the first migration), and the sizing refuses nothing
// (SizingReport), so there a census with the sizing passes on the sizing alone, and a census
// asked for without it refuses, since it read nothing.
func (r *StoreReport) Refused() bool {
	if r.Absent {
		return r.Sizing == nil
	}
	return len(r.Unpopulated()) > 0 || r.Differences() > 0 || r.ProjectFindings() > 0 || r.Unresolved() > 0
}

// The kinds the census holds the store to. The first three are the kinds a table projects, each
// with that table; the four after them are the people and the agents, which no table projects;
// the eight after those are the ties of decisions, readings and asks
// that block_refs, document_blocks, asks and ask_followers project; and the six after those are
// the same ties as graph_edges answers them (edgeProjection), each one that view answers.
//
// Each of the first three holds a row to the tuples that answer it as well as to its node row.
// A row that names its node is answered from the tuples by graph_edges, refs.Edges and
// refs.BacklinkCounts (migration 20261109215947), so a link whose tuple is missing or retracted
// is gone from every read while the node row still stands, and a census of the node rows alone
// reads zero differences over a store that has lost edges. A row's digest therefore carries
// every link the row states (statedLinks), and its node's digest carries every link the store
// states now under the same tuple kinds (currentLinks): the two agree exactly when the store
// states each link the row names and no other.
//
// An address past the store's bound (graphstore.MaxAddressBytes) is deliberately no part of the
// store: a block whose address is longer carries no node, a role name of 1,400 bytes is no node
// and no page, a comment anchored to a document whose ref_key is longer states no such tuple, and
// a session named past it is no node either. Each row side leaves those addresses out and counts
// them as a class of its own (pastBound), so the census reports no difference the writers made on
// purpose, in both directions.
var projections = []projection{{
	name:     graphstore.KindComment,
	nodeKind: graphstore.KindComment,
	table:    "comments",
	note: "one node per row; the digest is the row's created_at and author, the artifacts its anchor and its" +
		" artifact_id name and the author graphstore.AuthorOf reads off it, against the node's made_at and" +
		" made_by and the anchored_to, owned_by and authored_by tuples it states now",
	rows: `
		select 'comment ' || c.id::text, c.node_kind, c.node_address,
		       md5(c.created_at::text || '|' || c.author::text || '|' || ` + statedLinks(
		`'`+graphstore.TupleAnchoredTo+`>artifact:' || anchor.ref_key`,
		`'`+graphstore.TupleOwnedBy+`>artifact:' || owner.ref_key`,
		`case when c.author->>'kind' <> '`+graphstore.KindSession+`'
		           or octet_length(coalesce(c.author->>'id', '')) <= `+addressBound+`
		      then '`+graphstore.TupleAuthoredBy+`>' || `+commentAuthor+` end`,
	) + `)
		from comments c` + refs.CommentStoreJoins + `
		where ` + refs.CommentInTheStore,
	nodes: `
		select n.id, md5(n.made_at::text || '|' || n.made_by::text || '|' || ` + currentLinks(graphstore.KindComment,
		`t.kind in ('`+graphstore.TupleAnchoredTo+`', '`+graphstore.TupleOwnedBy+`', '`+graphstore.TupleAuthoredBy+`')`) + `)
		from graph_nodes n where n.kind = '` + graphstore.KindComment + `'`,
	pastBound: `
		select count(*) from comments c` + refs.CommentStoreJoins + `
		where not (` + refs.CommentInTheStore + `)
		   or (c.author->>'kind' = '` + graphstore.KindSession + `'
		       and octet_length(coalesce(c.author->>'id', '')) > ` + addressBound + `)`,
}, {
	name:     "block",
	nodeKind: refs.BlockNodeKind,
	table:    "document_blocks",
	note: "one node per row, blocks past the address bound aside; the digest is the row's document and the block" +
		" its parent_block_id names, or the document where it names none, against the in_document and part_of" +
		" tuples the node states now, a block with no part_of telling itself from one the document holds directly" +
		" and a part_of to an ask's node read as the ask's block in the node's own document",
	rows: `
		select 'block ' || d.node_id, d.node_kind, d.node_address,
		       md5(d.node_id || '|' || ` + statedLinks(
		`'`+refs.TupleInDocument+`>artifact:' || a.ref_key`,
		`'`+graphstore.TuplePartOf+`>' || case when d.parent_block_id is null then 'artifact:' || a.ref_key
		      else '`+refs.BlockNodeKind+`:' || a.ref_key || '#' || d.parent_block_id end`,
	) + `)
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where octet_length(d.node_id) <= ` + addressBound,
	nodes: `
		select n.id, md5(n.id || '|' || ` + placedLinks(`t.kind in ('`+refs.TupleInDocument+`', '`+graphstore.TuplePartOf+`')`) + `)
		from graph_nodes n where n.kind = '` + refs.BlockNodeKind + `'`,
	pastBound: `
		select count(*) from document_blocks d where octet_length(d.node_id) > ` + addressBound,
	owed: typedNodeOwed,
}, {
	name:     "block_ref",
	nodeKind: refs.BlockNodeKind,
	table:    "block_refs",
	note: "a row is one link out of a block and its node is that block's, so several rows share one node:" +
		" the node side is every live block node that states a pointer or a mention now, whether a row" +
		" names it or not, and the digest is every pointer and mention the rows of that block name against" +
		" every such link its node states now; a row an ask's node holds, and a decision's shaped, from_ask" +
		" and decided_by, are no part of this kind",
	// The kinds are the pointer attributes the installed block schema declares and `mentions`
	// (blockLinkKinds), bound as $1 rather than spelled, since the schema a census reads under is
	// the store's and not only the one this binary embeds. A row marked with an ask's node is the
	// ask's shaped, which hangs on that node and not on the block (refs.TypedFacts.pairs), and a
	// decision's shaped, from_ask and decided_by are ties held triple by triple by kinds of their
	// own: none of them is a link of the block this kind holds rows to.
	linked: true,
	rows: `
		select 'block_ref ' || r.from_node || ' ' || r.kind || ' -> ' || r.to_kind || ' ' || r.to_id,
		       r.node_kind, r.node_address, md5(r.from_node || '|' || named.links)
		from block_refs r
		join (
			select b.from_node,
			       string_agg(b.kind || '>' || b.to_kind || ':' || b.to_id, ','
			                  order by b.kind || '>' || b.to_kind || ':' || b.to_id) as links
			from block_refs b
			where octet_length(b.from_node) <= ` + addressBound + ` and octet_length(b.to_id) <= ` + addressBound + `
			  and b.kind = any($1::text[]) and b.node_kind is distinct from '` + graphstore.KindAsk + `'
			group by b.from_node
		) named on named.from_node = r.from_node
		where octet_length(r.from_node) <= ` + addressBound + ` and octet_length(r.to_id) <= ` + addressBound + `
		  and r.kind = any($1::text[]) and r.node_kind is distinct from '` + graphstore.KindAsk + `'`,
	// Every block node is read, and not only those a row names: a mention or a pointer the store
	// states out of a block no block_refs row names is a link graph_edges answers, and a node
	// side that began at the rows could never find it. A block whose only links are of other
	// kinds - its placement, its from_ask, its decided_by - states no link here and is no node
	// of this kind; the block kind holds its placement.
	nodes: `
		select held.id, md5(held.id || '|' || held.links)
		from (
			select n.id, ` + currentLinks(refs.BlockNodeKind, `t.kind = any($1::text[])`) + ` as links
			from graph_nodes n where n.kind = '` + refs.BlockNodeKind + `'
		) held
		where held.links <> ''`,
	pastBound: `
		select count(*) from block_refs r
		where (octet_length(r.from_node) > ` + addressBound + ` or octet_length(r.to_id) > ` + addressBound + `)
		  and r.kind = any($1::text[]) and r.node_kind is distinct from '` + graphstore.KindAsk + `'`,
}, {
	name:     graphstore.KindPerson,
	nodeKind: graphstore.KindPerson,
	table:    "comments.author, asks.author, asks.answer, block_refs.decided_by, events.actor, events.payload",
	note: "no table projects a person, so the row side is the people the projected tables name as authors," +
		" the owner of each session the server proved that wrote one, the same of every comment the erase took," +
		" as the actor of the event that stored it names them, the person each ask's answer names and each" +
		" answer the store took ever named, and who wrote each decision and reading and their proved owners",
	names:    `select p.login from (` + commentPeople(censusAuthors) + `) p`,
	authored: true,
	rows: `
		select 'person ' || p.login, '` + graphstore.KindPerson + `', p.login, ''
		from unnest($1::text[]) p (login)`,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindPerson + `'`,
}, {
	name:     graphstore.KindSession,
	nodeKind: graphstore.KindSession,
	table:    "comments.author, comment_deliveries, message_deliveries, comment_fallback_copies, asks.author, ask_followers, block_refs.decided_by, events.actor, events.payload",
	note: "no table projects a session, so the row side is the sessions those tables name as authors, as reached," +
		" as askers, as followers and as who wrote a decision or a reading, every session an ask.opened event" +
		" named as the asker, an ask.follower_added event named or a comment.delivery receipt says the listener" +
		" took an attempt to, and the author of every comment the erase took, as the actor of the event that" +
		" stored it names it",
	names:    `select s.id from (` + recordedSessions(censusAuthors) + `) s`,
	authored: true,
	rows: `
		select 'session ' || s.id, '` + graphstore.KindSession + `', s.id, ''
		from unnest($1::text[]) s (id) where octet_length(s.id) <= ` + addressBound,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindSession + `'`,
	pastBound: `
		select count(*) from unnest($1::text[]) s (id) where octet_length(s.id) > ` + addressBound,
}, {
	name:     graphstore.KindRole,
	nodeKind: graphstore.KindRole,
	table:    "comment_deliveries.target, messages.target, issues.route, events.payload",
	note: "no table projects a role, so the row side is the roles those tables address now and every role" +
		" an issue event's route, a message event's target or a new comment's mention ever named",
	rows: `
		select 'role ' || r.name, '` + graphstore.KindRole + `', r.name, ''
		from unnest($1::text[]) r (name) where octet_length(r.name) <= ` + addressBound,
	names: roleNames,
	nodes: `
		select n.id, '' from graph_nodes n where n.kind = '` + graphstore.KindRole + `'`,
	pastBound: `
		select count(*) from unnest($1::text[]) r (name) where octet_length(r.name) > ` + addressBound,
}, {
	name:     graphstore.KindPage,
	nodeKind: graphstore.KindPage,
	table:    "comment_deliveries.target, messages.target, issues.route, events.payload",
	note: "a role's page, whose id is its role's: the row side is the pages of the roles those tables address" +
		" now and the event log ever named, and a session's own page is no part of this census",
	rows: `
		select 'page ' || '` + graphstore.KindRole + `/' || r.name, '` + graphstore.KindPage + `',
		       '` + graphstore.KindRole + `/' || r.name, ''
		from unnest($1::text[]) r (name)
		where octet_length('` + graphstore.KindRole + `/' || r.name) <= ` + addressBound,
	names: roleNames,
	nodes: `
		select n.id, '' from graph_nodes n
		where n.kind = '` + graphstore.KindPage + `' and n.id like '` + graphstore.KindRole + `/%'`,
	pastBound: `
		select count(*) from unnest($1::text[]) r (name)
		where octet_length('` + graphstore.KindRole + `/' || r.name) > ` + addressBound,
},
	tieProjection(refs.TupleShaped,
		"a decision's shaped, and an ask's, which hangs on the ask's node: the row side is every shaped row of"+
			" block_refs as the triple of the node it names, shaped and its block, against every shaped tuple a"+
			" live block or ask node states now", refs.BlockNodeKind, graphstore.KindAsk),
	tieProjection(refs.TupleFromAsk,
		"a decision's from_ask: the row side is every from_ask row of block_refs as the triple of the"+
			" decision's node, from_ask and its ask, against every from_ask a live block node states now", refs.BlockNodeKind),
	tieProjection(graphstore.TupleDecidedBy,
		"a decision's decided_by: the row side is every decided_by row of block_refs as the triple of the"+
			" decision's node, decided_by and its person or session, against every decided_by a live block node"+
			" states now", refs.BlockNodeKind),
	{
		name:     graphstore.TupleFollowedBy,
		nodeKind: graphstore.KindAsk,
		table:    "ask_followers",
		note: "an ask's followers: the row side is every marked ask_followers row as the triple of the ask's node" +
			" it names, followed_by and its session, against every followed_by a live ask node states now",
		rows: `
		select 'ask_follower ' || f.ask_id::text || ' ' || f.session_id, f.node_kind, f.node_address,
		       '` + graphstore.TupleFollowedBy + `', '` + graphstore.KindSession + `', f.session_id
		from ask_followers f
		where f.node_kind is not null and octet_length(f.session_id) <= ` + addressBound,
		tuples: heldTuples(`t.kind = '`+graphstore.TupleFollowedBy+`'`, graphstore.KindAsk),
		pastBound: `
		select count(*) from ask_followers f where octet_length(f.session_id) > ` + addressBound,
		unmarked: `
		select count(*) from ask_followers f where f.node_kind is null and octet_length(f.session_id) <= ` + addressBound,
		emptyAgrees: true,
	}, {
		name:     "reading_parent",
		nodeKind: refs.BlockNodeKind,
		table:    "document_blocks.parent_block_id",
		note: "the parent of each reading: the row side is the document_blocks row of every reading's node as the" +
			" triple of that node, part_of and the block its parent_block_id names, against the part_of every live" +
			" reading's node states now, a part_of to an ask's node read as the ask's block in the node's own document",
		rows: `
		select 'reading ' || d.node_id, d.node_kind, d.node_address, '` + graphstore.TuplePartOf + `',
		       case when d.parent_block_id is null then 'artifact' else '` + refs.BlockNodeKind + `' end,
		       case when d.parent_block_id is null then a.ref_key else a.ref_key || '#' || d.parent_block_id end
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		join graph_nodes reading on reading.kind = '` + refs.BlockNodeKind + `' and reading.id = d.node_id
		  and reading.supertag = '` + refs.ReadingSupertag + `'`,
		tuples:      readingParents,
		owed:        typedNodeOwed,
		emptyAgrees: true,
	},
	askProjection("ask_parent", "asks.block_artifact_id, asks.artifact_id",
		"an ask's placement: the row side is every marked asks row as the triples of its node, part_of and the"+
			" document holding its block, and owned_by and the document owning it, against the part_of and owned_by"+
			" every live ask node states now",
		`(document.id is not null or owner.id is not null)`,
		[]string{`'` + graphstore.TuplePartOf + `', 'artifact', document.ref_key`, `'` + graphstore.TupleOwnedBy + `', 'artifact', owner.ref_key`},
		`t.kind in ('`+graphstore.TuplePartOf+`', '`+graphstore.TupleOwnedBy+`')`),
	askProjection("ask_anchor", "asks.block_artifact_id, asks.anchor",
		"an ask's anchor: the row side is every marked asks row whose block's document or anchor names a"+
			" document, as the triple of its node, anchored_to and that document, against the anchored_to every"+
			" live ask node states now",
		`anchor.id is not null`,
		[]string{`'` + graphstore.TupleAnchoredTo + `', 'artifact', anchor.ref_key`},
		`t.kind = '`+graphstore.TupleAnchoredTo+`'`),
	{
		name:     "ask_answer",
		nodeKind: graphstore.KindAsk,
		table:    "asks.answer",
		note: "a person's answer: the row side is every marked asks row holding an answer, and the digest is its" +
			" words, its selection, its time and its person, character for character, against the answer, selected" +
			" and answered_at values and the answered_by and decided_by its node holds now",
		rows: `
		select 'ask ' || k.id::text, k.node_kind, k.node_address, md5(k.id::text || '|' || ` + statedLinks(
			`'`+graphstore.TupleAnswer+`>value:' || (k.answer->>'text')`,
			`'`+graphstore.TupleSelected+`>value:' || coalesce(case when jsonb_typeof(k.answer->'selected') = 'array'
			      then k.answer->'selected' end, '[]'::jsonb)::text`,
			`'`+graphstore.TupleAnsweredAt+`>value:' || (k.answer->>'at')`,
			`case when `+answerer+` <> '' and octet_length(`+answerer+`) <= `+addressBound+`
			      then '`+graphstore.TupleAnsweredBy+`>`+graphstore.KindPerson+`:' || `+answerer+` end`,
			`case when `+answerer+` <> '' and octet_length(`+answerer+`) <= `+addressBound+`
			      then '`+graphstore.TupleDecidedBy+`>`+graphstore.KindPerson+`:' || `+answerer+` end`) + `)
		from asks k
		where k.node_kind is not null and k.answer is not null`,
		// The values are read as a current value is, the newest tuple of each kind while it asserts,
		// and the two relations as a current relation is; selected is read as the JSON it holds,
		// as the row's is, so the labels are compared character for character whatever spacing
		// either encoding put between them. A value that is no JSON is compared as it is written,
		// and so differs.
		nodes: `
		select held.id, md5(held.id || '|' || held.facts)
		from (
			select n.id, (select coalesce(string_agg(answered.fact, ',' order by answered.fact), '') from (
				select v.kind || '>value:' || case when v.kind = '` + graphstore.TupleSelected + `' and pg_input_is_valid(v.to_id, 'jsonb')
				                                   then v.to_id::jsonb::text else v.to_id end
				from (
					select distinct on (t.kind) t.kind, t.to_id, t.asserted
					from graph_tuples t
					where t.from_kind = '` + graphstore.KindAsk + `' and t.from_id = n.id and t.to_kind = '` + graphstore.KindValue + `'
					  and t.kind in ('` + graphstore.TupleAnswer + `', '` + graphstore.TupleSelected + `', '` + graphstore.TupleAnsweredAt + `')
					order by t.kind, t.seq desc
				) v where v.asserted
				union all
				select r.kind || '>' || r.to_kind || ':' || r.to_id from (
					` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = n.id and t.to_kind <> '`+graphstore.KindValue+`'
					  and t.kind in ('`+graphstore.TupleAnsweredBy+`', '`+graphstore.TupleDecidedBy+`')`) + `
				) r where r.asserted
			) answered (fact)) as facts
			from graph_nodes n where n.kind = '` + graphstore.KindAsk + `'
		) held
		where held.facts <> ''`,
		unmarked:    askUnmarked(`k.answer is not null`),
		pastBound:   askPastBound,
		emptyAgrees: true,
	},
	edgeProjection(refs.TupleShaped+edgeSuffix, refs.TupleShaped,
		"graph_edges' shaped: the row side is every shaped edge graph_edges answers out of a block or ask node not"+
			" gone, read node by node, as the triple of that node, shaped and its far end, against every shaped tuple"+
			" a live block or ask node states now", "", "", refs.BlockNodeKind, graphstore.KindAsk),
	edgeProjection(refs.TupleFromAsk+edgeSuffix, refs.TupleFromAsk,
		"graph_edges' from_ask: the row side is every from_ask edge graph_edges answers out of a block node not"+
			" gone, read node by node, as the triple of that node, from_ask and its ask, against every from_ask a"+
			" live block node states now", "", "", refs.BlockNodeKind),
	edgeProjection(graphstore.TupleDecidedBy+edgeSuffix, graphstore.TupleDecidedBy,
		"graph_edges' decided_by: the row side is every decided_by edge graph_edges answers out of a block node"+
			" not gone, read node by node, as the triple of that node, decided_by and its person or session, against"+
			" every decided_by a live block node states now", "", "", refs.BlockNodeKind),
	edgeProjection(graphstore.TupleFollowedBy+edgeSuffix, graphstore.TupleFollowedBy,
		"graph_edges' followed_by: the row side is every followed_by edge graph_edges answers out of an ask node"+
			" not gone, read node by node, as the triple of that node, followed_by and its session, against every"+
			" followed_by a live ask node states now, an edge the old arm answers from an unmarked row aside",
		followerRowUnmarked, `
		select count(*) from ask_followers f where octet_length(f.session_id) > `+addressBound, graphstore.KindAsk),
	edgeProjection("ask_parent"+edgeSuffix, graphstore.TupleOwnedBy,
		"graph_edges' half of an ask's placement, its owned_by: the row side is every owned_by edge graph_edges"+
			" answers out of an ask node not gone, read node by node, as the triple of that node, owned_by and the"+
			" document owning it, against every owned_by a live ask node states now; no arm of graph_edges answers"+
			" an ask's part_of, which ask_parent holds against asks alone; an edge the old arm answers from an unmarked"+
			" row aside", askRowUnmarked, askPastBound, graphstore.KindAsk),
	edgeProjection("ask_anchor"+edgeSuffix, graphstore.TupleAnchoredTo,
		"graph_edges' anchor of an ask: the row side is every anchored_to edge graph_edges answers out of an ask"+
			" node not gone, read node by node, as the triple of that node, anchored_to and its document, against"+
			" every anchored_to a live ask node states now, an edge the old arm answers from an unmarked row aside",
		askRowUnmarked, askPastBound, graphstore.KindAsk),
}

// addressBound is the store's address bound (graphstore.MaxAddressBytes) as a statement spells
// it. It is written into the text rather than bound as a parameter because each side is spliced
// into a statement of its own and one shared constant keeps the two sides reading the same bound.
var addressBound = strconv.Itoa(graphstore.MaxAddressBytes)

// statedLinks is the links a projection row states, as one value its digest can carry: links are
// SQL expressions over the row, each answering one link written kind>to_kind:to_id, or null where
// the row states no such link. They are joined in order as currentLinks joins the store's, and
// a row that states none reads the empty string.
func statedLinks(links ...string) string {
	return `(select coalesce(string_agg(stated.link, ',' order by stated.link), '')
		from (values (` + strings.Join(links, `), (`) + `)) stated (link))`
}

// currentLinks is every link the node n of kind fromKind states now under the tuple kinds the
// predicate kinds selects (written over t.kind), read as a statement reads a current relation:
// the newest tuple per (kind, far end), kept while that newest one asserts
// (graphstore.CurrentTupleRead). Each is written kind>to_kind:to_id, as statedLinks writes a
// row's, and they are joined in order, so a node that states none reads the empty string. A link
// whose newest tuple is a retraction is absent here exactly as a tuple never written is. The read
// walks graph_tuples_current where kinds lists the kinds it reads, which proves that index's
// predicate, and graph_tuples_as_of_out where kinds lists the kinds it leaves out, as a block's
// links do: graph_tuples_current leaves delivered_to out, and such a list does not.
//
// A block's part_of is one of these links, so a block that states no parent at all and a block
// the document holds directly read differently: the first has no part_of link and the second
// has the one to its document, which is the link its row's null parent_block_id states.
func currentLinks(fromKind, kinds string) string {
	return `(select coalesce(string_agg(held.link, ',' order by held.link), '')
	 from (
		` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`,
		`t.kind || '>' || t.to_kind || ':' || t.to_id as link, t.asserted`,
		`t.from_kind = '`+fromKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
	 ) held where held.asserted)`
}

// heldTuples is the store side of a kind held triple by triple (projection.tuples): every
// relation a live node of one of holders states now under the tuple kinds the predicate kinds
// selects (written over t.kind), read as a statement reads a current relation, the newest tuple
// per (kind, far end) kept while it asserts (graphstore.CurrentTupleRead), each as (from_kind,
// from_id, kind, to_kind, to_id). Each node's relations are read through graph_tuples_current,
// one probe per node, as currentLinks reads them.
func heldTuples(kinds string, holders ...string) string {
	return `
		select n.kind, n.id, held.kind, held.to_kind, held.to_id
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held
		where n.kind in ('` + strings.Join(holders, `', '`) + `') and n.state = '` + graphstore.StateLive + `'
		  and held.asserted`
}

// askBlockID is the block id the marked asks row of the ask node at address (an SQL expression)
// names (asks.block_id), null where no marked row names that node or its row names no block. It
// is the block placedLinks and readingParents read a part_of to that ask's node as.
func askBlockID(address string) string {
	return `(select k.block_id from asks k
		where k.node_kind = '` + graphstore.KindAsk + `' and k.node_address = ` + address + ` and k.block_id is not null)`
}

// placedLinks is currentLinks of a block node n under kinds, but a part_of whose far end is an
// ask's node is written as the block of n's own document that holds that ask: the block of the
// id the ask's marked row names (askBlockID), which is the ask's own block in the document
// that row names and a copy's block in any other document of its owner, since a copy keeps its
// source's block id (docs' copiedAskSources). A reading in an answered ask hangs on the ask's
// node, while its row's parent_block_id keeps the id of the ask block holding it, which is that
// tuple's projection (refs' reconcileBlockParents), so both read as one link to that block. An
// ask no marked row names, or whose row names no block, keeps its own address, which no row
// states. A ref_key holds no '#', so n's document is the part of its id before the first one.
func placedLinks(kinds string) string {
	return `(select coalesce(string_agg(placed.link, ',' order by placed.link), '')
	 from (
		select case when held.kind = '` + graphstore.TuplePartOf + `' and held.to_kind = '` + graphstore.KindAsk + `'
		            then coalesce(held.kind || '>` + refs.BlockNodeKind + `:' || split_part(n.id, '#', 1) || '#' || ` +
		askBlockID(`held.to_id`) + `, held.kind || '>' || held.to_kind || ':' || held.to_id)
		            else held.kind || '>' || held.to_kind || ':' || held.to_id end as link
		from (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held where held.asserted
	 ) placed)`
}

// readingParents is reading_parent's store side (heldTuples' shape): the part_of every live
// reading's node states now, a part_of to an ask's node read as placedLinks reads it, as the
// block of the reading's own document that holds the ask.
var readingParents = `
		select n.kind, n.id, held.kind,
		       case when placed.block_id is null then held.to_kind else '` + refs.BlockNodeKind + `' end,
		       coalesce(split_part(n.id, '#', 1) || '#' || placed.block_id, held.to_id)
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and t.kind = '`+graphstore.TuplePartOf+`'`) + `
		) held
		cross join lateral (
			select case when held.to_kind = '` + graphstore.KindAsk + `' then ` + askBlockID(`held.to_id`) + ` end as block_id
		) placed
		where n.kind = '` + refs.BlockNodeKind + `' and n.supertag = '` + refs.ReadingSupertag + `'
		  and n.state = '` + graphstore.StateLive + `' and held.asserted`

// typedNodeOwed is the owed predicate (projection.owed) of a kind whose node may be a decision's
// or a reading's. A document that still owes its block index (document_block_versions.version
// null) holds no row yet for a block its latest version added, while that version's typed-node
// pass already wrote the block's node: a decision or a reading written above the deferral bound
// (refs.PlanDecisionNodes), the one writer of a block node that writes no row beside it. The
// block-index reconciliation writes the row, so such a node is owed its row and no difference
// until then; a node of any other supertag in such a document is a difference still, since no
// writer leaves one without its row. A ref_key holds no '#', so a block node's document is the
// part of its id before the first one.
var typedNodeOwed = `exists (
		select 1 from graph_nodes typed
		join artifacts a on a.ref_key = split_part(typed.id, '#', 1)
		join document_block_versions owed on owed.artifact_id = a.id
		where typed.kind = '` + refs.BlockNodeKind + `' and typed.id = node.id
		  and typed.supertag in ('` + refs.DecisionSupertag + `', '` + refs.ReadingSupertag + `')
		  and owed.version is null)`

// tieProjection is the kind of one tie that block_refs projects - a
// decision's shaped, from_ask or decided_by, and an ask's shaped - held triple by triple
// (projection.tuples): each row is the triple of the node that states its tie (node_kind,
// node_address: the decision's block node, or for an ask block's shaped the ask's node,
// refs.TypedFacts.pairs), kind and its far end, against every tuple of kind a live node of one of
// holders states now. So a tuple that reaches the wrong node is a row no tuple states and a tuple
// no row projects, and not a count that still matches, and a row of the same node whose tuple
// stands is no difference. A row that names no node is a difference, as a block_ref row's is:
// every writer of these kinds names it, and a row past the store's bound is counted apart.
func tieProjection(kind, note string, holders ...string) projection {
	inBound := `octet_length(r.from_node) <= ` + addressBound + ` and octet_length(r.to_id) <= ` + addressBound
	return projection{
		name: kind, nodeKind: strings.Join(holders, ", "), table: "block_refs", note: note, emptyAgrees: true,
		rows: `
		select 'block_ref ' || r.from_node, r.node_kind, r.node_address, r.kind, r.to_kind, r.to_id
		from block_refs r
		where r.kind = '` + kind + `' and ` + inBound,
		tuples: heldTuples(`t.kind = '`+kind+`'`, holders...),
		pastBound: `
		select count(*) from block_refs r
		where r.kind = '` + kind + `' and not (` + inBound + `)`,
	}
}

// edgeSuffix ends the name of a kind edgeProjection holds, after the name of the kind of the same
// tie that a table projects: shaped_edge beside shaped.
const edgeSuffix = "_edge"

// edgeProjection is one tie of decisions, readings and asks as graph_edges answers it, held
// triple by triple (projection.tuples) as tieProjection holds block_refs: each row is an edge the
// view answers out of a node of one of holders (graphEdgesOut), as the triple of that node, kind
// and its far end, against every tuple of kind a live node of one of holders states now. So an
// edge the view answers that no tuple states is a row no tuple states, a tuple the view answers no
// edge for is a tuple no row projects, and an edge that reaches the wrong node reads as both.
//
// The view's newest definition (store/migrations/20261121034242_graph_edges_asks_and_tuple_arm_filter.up.sql,
// 20261116094101's arms with 20261120122700's filter of the tuple arm) answers
// each of these ties from one arm, which the projection row decides. For a row that names its
// node, the tuple arm answers the newest tuple of the triple while it asserts, out of a node not
// gone: shaped, from_ask and decided_by out of a block node, and shaped out of an ask's node, while
// a block_refs row of that node, kind and far end stands (a block's pointer only while its target
// is of a type the attribute lists, and shaped, from_ask and decided_by list none); anchored_to
// and owned_by out of an ask's node while an asks row names that node; and followed_by while the
// ask_followers row of that ask and that session names it. For a row that names no node, which a
// process of the release before wrote, an old arm answers the row alone: the block_refs arm its
// link, the two asks arms its anchored_to and owned_by, and the ask_followers arm its
// followed_by. No arm answers a reading's part_of or an ask's part_of (the tuple arm answers a
// block's in_document, as its part_of to its document), so the parent of a reading has no kind
// here, and an ask's placement is held by its owned_by alone (ask_parent_edge).
//
// unmarked is a predicate over the node n and its edge that holds where an old arm answers the
// edge from a row that names no node - an asks or ask_followers row a process of the release
// before wrote, or one the ask's writer leaves unmarked on purpose because a document it names is
// past the store's bound, while it writes the ask's node all the same - and is empty for a kind no
// old arm answers that way. Such an edge is no row here, as such a row is no row of the ask kinds
// (unmarked_rows, addresses_past_bound); pastBound is the class the ask kinds count beside it.
func edgeProjection(name, kind, note, unmarked, pastBound string, holders ...string) projection {
	return projection{
		name: name, nodeKind: strings.Join(holders, ", "), table: "graph_edges", note: note, emptyAgrees: true,
		rows:      graphEdgesOut(kind, unmarked, holders...),
		tuples:    heldTuples(`t.kind = '`+kind+`'`, holders...),
		pastBound: pastBound,
	}
}

// askRowUnmarked and followerRowUnmarked are edgeProjection's unmarked predicates of the ask's
// own relations and of its followers: the asks row of the node, or the ask_followers row of the
// node and the edge's session, names no node, so the view answers the edge from its old arm. Each
// probes the index graph_edges' old arms are probed through (asks_id_text, ask_followers_ask_id_text).
const (
	askRowUnmarked      = `exists (select 1 from asks k where k.id::text = n.id and k.node_kind is null)`
	followerRowUnmarked = `exists (select 1 from ask_followers f
		where f.ask_id::text = n.id and f.session_id = edge.to_id and f.node_kind is null)`
)

// graphEdgesOut is the row side of an edgeProjection kind: every edge of kind graph_edges answers
// out of a node of one of holders that is not gone - the nodes its tuple arm answers out of -
// read node by node, the view probed with the node's kind and id, as refs.Edges probes it with
// both bound, but an edge unmarked (a predicate over n and edge, empty for none) says an old arm
// answers from a row that names no node. The probe ends in `offset 0`, the fence the view's own
// probes take: without it the planner pulls the probe up into the join, and an arm that is a
// subquery - the tuple arm's newest tuple of each triple - takes the node's kind and id as a
// filter over every tuple of that kind rather than as the keys of its index, read again for each
// node. Fenced, each arm keys its own index with the node's kind and id. An edge an old arm
// answers out of a row whose ask the store holds no node of yet is no row of this read either.
func graphEdgesOut(kind, unmarked string, holders ...string) string {
	read := `
		select 'graph_edges ' || n.kind || ':' || n.id, n.kind, n.id, edge.kind, edge.to_kind, edge.to_id
		from graph_nodes n
		cross join lateral (
			select e.kind, e.to_kind, e.to_id from graph_edges e
			where e.from_kind = n.kind and e.from_id = n.id and e.kind = '` + kind + `'
			offset 0
		) edge
		where n.kind in ('` + strings.Join(holders, `', '`) + `') and n.state <> '` + graphstore.StateGone + `'`
	if unmarked != "" {
		read += `
		  and not ` + unmarked
	}
	return read
}

// askDocuments are the three documents an asks row k names, joined as the ask's writer reads
// them (graphstore.ReadAskPlaces) and the backfill's asks pass joins them (askRowsOwed): anchor,
// the document holding its block or else the one its anchor quotes; owner, the document an ask
// on no issue belongs to; document, the one holding its block. It follows `from asks k`.
const askDocuments = `
		left join artifacts anchor on anchor.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
		left join artifacts owner on owner.id = k.artifact_id
		left join artifacts document on document.id = k.block_artifact_id`

// askFits is true of an asks row none of whose documents' addresses is past the store's bound,
// the one kind of row the ask's writer marks (graphstore.AskPlaces.Fit) and the backfill owes
// (askRowsOwed). It reads askDocuments.
var askFits = `octet_length(coalesce(anchor.ref_key, '')) <= ` + addressBound + `
		  and octet_length(coalesce(owner.ref_key, '')) <= ` + addressBound + `
		  and octet_length(coalesce(document.ref_key, '')) <= ` + addressBound

// askPastBound counts the asks rows some document of which is past the store's bound: the
// writer states none of such an ask's relations and leaves its row unmarked on purpose, and the
// census counts them apart.
var askPastBound = `
		select count(*) from asks k` + askDocuments + `
		where not (` + askFits + `)`

// askUnmarked counts every other asks row that names no node and that member, a predicate over
// the row and askDocuments, makes one of a kind's own population: a row a process of the release
// before wrote and the decisions step's backfill owes, which graph_edges' old arms answer, a class
// of its own and no difference (projection.unmarked). So an ask kind whose every row is unmarked
// holds rows still, and is no kind nothing of which is written yet (KindReport.Empty).
func askUnmarked(member string) string {
	return `
		select count(*) from asks k` + askDocuments + `
		where k.node_kind is null and ` + askFits + ` and ` + member
}

// askProjection is a kind of the relations an ask's node holds that its asks row projects, held
// triple by triple (projection.tuples): links are the relations a marked row states, each written
// `kind, to_kind, to_id` as SQL over the row and askDocuments, a null to_id where the row states
// none, member the predicate that makes a row one of the kind, and kinds the tuple kinds its node
// states them under. A marked row is held by its node, the ask's own, and an unmarked one is
// counted apart.
func askProjection(name, table, note, member string, links []string, kinds string) projection {
	return projection{
		name: name, nodeKind: graphstore.KindAsk, table: table, note: note, emptyAgrees: true,
		rows: `
		select 'ask ' || k.id::text, k.node_kind, k.node_address, stated.kind, stated.to_kind, stated.to_id
		from asks k` + askDocuments + `
		cross join lateral (values (` + strings.Join(links, `), (`) + `)) stated (kind, to_kind, to_id)
		where k.node_kind is not null and stated.to_id is not null`,
		tuples:    heldTuples(kinds, graphstore.KindAsk),
		pastBound: askPastBound,
		unmarked:  askUnmarked(member),
	}
}

// answerer is the person node an asks row's answer names, written as asks.AnswererOf names it:
// the canonical login of the answer's user (user_threads_login is model.CanonicalLogin in SQL),
// empty where it names nobody. It reads `asks k`.
const answerer = `coalesce(user_threads_login(k.answer->>'user'), '')`

// actorAuthor is the node graphstore.AuthorOf reads off a stored actor, the SQL expression actor,
// as one value: an array of two, the node's kind and its id, or null for an actor that names
// nobody. The login an actor of kind user carries is a person, and so is the owner a personal
// token's actor carries, each canonicalised the way user_threads_login canonicalises a login
// (migration 0095, which is model.CanonicalLogin in SQL); a session's id is the session. It is
// spelled once here for the readings of who wrote a comment: the far end of the authored_by a row
// states (commentAuthor), the people a comment names (commentWriterPeople) and the person whose
// aliases the sizing counts (sizingWritesWith).
func actorAuthor(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'user' and coalesce(` + actor + `->>'id', '') <> ''
				then array['` + graphstore.KindPerson + `', user_threads_login(` + actor + `->>'id')]
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
				then array['` + graphstore.KindSession + `', ` + actor + `->>'id']
			when coalesce(` + actor + `->>'owner', '') <> ''
				then array['` + graphstore.KindPerson + `', user_threads_login(` + actor + `->>'owner')]
		end`
}

// commentAuthor is the far end of the authored_by tuple a comment row states, written kind:id:
// the node graphstore.AuthorOf reads off the actor the row stored (actorAuthor), and null for an
// actor that names nobody, whose comment states no authored_by at all.
var commentAuthor = `array_to_string(` + actorAuthor("c.author") + `, ':')`

// commentWriters are the comments the store holds or held, each as the actor that wrote it, the
// time it was written and the event that stored it, null for a row: every comments row the store
// holds (refs.CommentInTheStore), and the comment.created or comment.answered event of every
// comment the comments table no longer holds. It is who wrote a comment, as commentWriterPeople and
// recordedSessions read it.
//
// The events are there for the erase. It deletes a comment's row and keeps the comment's events,
// and of an event it rewrites only the payload's values that carry what a person wrote: never a
// structural value such as an id, a state or a time (grapherase's structuralKey), and never the
// actor column (grapherase's redactEvents). That actor is the one the row stored as its author
// and the one graphstore.QueueAuthored wrote the author's node from: api's createComment and
// replyComment pass all three the one actor. The erase leaves the author's node - it never takes
// a session, and takes a person only when asked to - so without the event that node would be a
// difference no erase can close. No route deletes a comment, and the erase takes only a comment
// the store holds a node of, whose author's node the write that stored it made with it, so an
// event whose comment is gone names an author a writer wrote a node for. An event of a comment the
// table still holds adds nothing: the row names the same author, and only the row says whether the
// store holds the comment at all (refs.CommentInTheStore). An event whose payload names no comment
// says nothing of one and is not read; the filler events of a seeded corpus are such events.
//
// The comment an event stored is matched by the payload's id, which the redaction never rewrites,
// compared as text so that an id no row could hold reads as no row rather than failing a cast.
var commentWriters = `
	select c.author as actor, c.created_at as at, null::bigint as event
	from comments c` + refs.CommentStoreJoins + `
	where ` + refs.CommentInTheStore + `
	union all
	select e.actor, e.created_at, e.id
	from events e
	where e.type in ('comment.created', 'comment.answered') and coalesce(e.payload->>'id', '') <> ''
	  and not exists (select 1 from comments c where c.id::text = e.payload->>'id')`

// erasedSince is the one rule by which the census holds no erased person to a record the erase
// left behind, a predicate over three SQL expressions: kind and id the node the record names, and
// at the time the record was written. It holds where an erase of that node ran at or after at
// (graph_erasures.ran_at). Two records outlive what they record and still name who wrote it: the
// events of a comment the erase took, which keep the actor that wrote it (commentWriters), and an
// erase's own record of who wrote or answered the ask, decision or reading it took
// (erasedWriters). The erase takes a person once nothing of the store names them, and both records
// name them after it, so counting them would hold the store to the node the operator removed, for
// good. A person who writes again after it is held by what they wrote then, which is newer. The
// erase dates itself by the clock of the host it ran on and an event is dated by the database's,
// but every record this leaves out was written no later than the erase of the node it records,
// which committed before the erase of its writer could begin: that erase refuses while anything
// of the store still names the person. No erase takes a session, so every record of one stands.
func erasedSince(kind, id, at string) string {
	return `exists (
		select 1 from graph_erasures erased
		where erased.node_kind = ` + kind + ` and erased.node_id = ` + id + ` and erased.ran_at >= ` + at + `)`
}

// commentWriterPeople is each person a comment the store holds or held names as its writer
// (commentWriters), one row for each such comment and person, with the time the comment was
// written and the event that stored it (null for a row). It reads an actor as graphstore.AuthorOf
// does (actorAuthor): the login an actor of kind user carries, or the owner a personal token's
// actor carries, each canonicalised the way user_threads_login canonicalises a login (migration
// 0095, which is model.CanonicalLogin in SQL). Beside them are the owners of the sessions the
// server proved that wrote a comment: the comment's author is the session, whose act names that
// same session and whose owner is the person it belongs to, and the write that ties the two with
// an alias_of tuple makes the owner's node with it (graphstore.AuthorFacts), except for a session
// past the store's bound, of which nothing is written.
//
// Only the comments the store holds name anybody here (refs.CommentInTheStore): a comment
// anchored to or owned by a document whose address is past the bound is left whole out of the
// store, so it states no authored_by and makes no author node, and a person whose only comment is
// one of those is no node of the store and no difference of this census.
//
// It is the one reading of who a comment names, read by two: the census's person row side
// (commentPeople) and the sizing's people (sizingWritesWith), so the sizing counts the people
// the census holds to a node for a comment. It reads no table of the store, so the sizing can
// read it on a database the store does not exist in yet. Its proved-session arm
// (provedSessionOwner) is read by the erase as well (ProvedSessionComments).
var commentWriterPeople = `
	select user_threads_login(author.login) as login, written.at, written.event
	from (` + commentWriters + `) written
	cross join lateral (
		select authored.node[2] from (select ` + actorAuthor("written.actor") + `) authored (node)
		where authored.node[1] = '` + graphstore.KindPerson + `'
		union all
		select written.actor->>'owner'
		where ` + provedSessionOwner("written.actor") + `
	) author (login)
	where author.login is not null and user_threads_login(author.login) <> ''`

// provedSessionOwner is true of a stored actor, the SQL expression actor, that is a session the
// server proved: the actor is the session, its act names that same session, and the session is
// within the store's bound. Its `owner`, where it names one, is then the person the session
// belongs to, whom the write that stored the actor ties to the session by an alias_of tuple and
// makes a node of (graphstore.AuthorFacts). It is commentWriterPeople's second arm, spelled once
// here so that the census and the erase (ProvedSessionComments) read one rule.
func provedSessionOwner(actor string) string {
	return actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
		  and ` + actor + `->'act'->>'session' = ` + actor + `->>'id'
		  and octet_length(` + actor + `->>'id') <= ` + addressBound
}

// ProvedSessionComments is the select of the id of every comments row that names the person
// bound at $1 as the owner of the session that wrote it, in id order: a row of a comment the
// store holds (refs.CommentInTheStore) whose author is a session the server proved
// (provedSessionOwner) and whose `owner` is that person's login as user_threads_login reads it.
// Each is a row the census names that person by (commentWriterPeople), though nothing the row
// projects states a tuple into her: the comment's authored_by names the session, and only the
// session's alias_of names her. The erase refuses a person while such a row stands (grapherase's
// readBlockers), since the census would go on naming her from it once her node was gone.
var ProvedSessionComments = `
	select c.id::text from comments c` + refs.CommentStoreJoins + `
	where ` + refs.CommentInTheStore + `
	  and ` + provedSessionOwner("c.author") + `
	  and user_threads_login(c.author->>'owner') = $1
	order by c.id`

// commentPeople are the people the census holds to a person node, each once: who wrote a comment
// the store holds or held (commentWriterPeople), and the people an ask, a decision or a reading of
// the store names, below.
//
// A person the erase took is held to no event that came before that erase (erasedSince): the
// events of their erased comments name them as the author after it.
//
// The backfill gives the people comments name their nodes in its comment pass (writeCommentBatch,
// graphstore.AuthorFacts) and needs no pass of its own for them: a row names its author to that
// pass, and an event of a comment the erase took names an author whose node was written when the
// comment was stored.
//
// An ask whose row names its node names people too: its author, read as a
// comment's is (graphstore.AskFacts states its authored_by), and the person whose answer it holds,
// whose node its answered_by and decided_by point at (asks.Settle). A node is history and never
// goes, as a session that once followed an ask still owes its node (recordedSessions), so a person
// whose answer a reopen took off the row is still the person that answer named: every person an
// ask's answered_by or decided_by ever named, asserted or retracted since, is read here off the
// store's own log, which holds that answer still. Only an answer the store took names anybody so,
// and an answer a build before the store wrote and took off again names nobody, since the
// backfill states a row's answer as it stands (graphcensus' asks pass). The log is read from the
// person's node through graph_tuples_as_of_in, whose partial predicate the arm states beside its
// join, so the planner keys the index rather than reading the table whole. An erase of the ask
// takes those tuples with it and records the people they named (erasedWriters), which holds them
// still, until an erase of their own runs.
//
// Who wrote each decision, reading and ask the store holds an authored_by or decided_by of names
// people too, and so does every person an ask's answer ever named (storedAuthors, read as the
// relation authors names: censusAuthors where the census binds what it read once, inlineAuthors
// where a statement reads it itself), and so does the owner the server proved of each such writer
// that is a session (storedOwners).
func commentPeople(authors string) string {
	return `
	select distinct user_threads_login(author.login) as login
	from (
		select named.login
		from (` + commentWriterPeople + `) named
		where named.event is null
		   or not ` + erasedSince(`'`+graphstore.KindPerson+`'`, `named.login`, `named.at`) + `
		union all
		select case
			when k.author->>'kind' = 'user' and coalesce(k.author->>'id', '') <> '' then k.author->>'id'
			when k.author->>'kind' = 'session' and coalesce(k.author->>'id', '') <> '' then null
			when coalesce(k.author->>'owner', '') <> '' then k.author->>'owner'
		end
		from asks k where k.node_kind is not null
		union all
		select k.author->>'owner'
		from asks k
		where k.node_kind is not null and k.author->>'kind' = 'session' and coalesce(k.author->>'id', '') <> ''
		  and k.author->'act'->>'session' = k.author->>'id'
		  and octet_length(k.author->>'id') <= ` + addressBound + `
		union all
		select k.answer->>'user' from asks k where k.node_kind is not null and k.answer is not null
		union all
		select stored.id from ` + authors + ` where stored.kind = '` + graphstore.KindPerson + `'
		union all
		select owner.login from (` + storedOwners(authors) + `) owner
	) author (login)
	where author.login is not null and user_threads_login(author.login) <> ''`
}

// storedAuthors are who wrote the decisions and the readings of every document, and every ask the
// store names an author of, as (kind, id, at): each a person or a session, whose node the writer
// writes beside the tie that names it (refs.PlanBlocks, refs.PlanDecisionNodes and
// graphstore.AskFacts, through graphstore.AuthorFacts). A decision's author is the far end of its
// decided_by, which its block_refs row projects, and that row is what is read for it. A reading's
// author is the far end
// of its authored_by, which no table projects (the tuple table), so the
// row read for it is the tuple itself, the authored_by each live reading's node states now, as
// graph_edges' tuple arm would answer it. An ask's author is read the same way, off the
// authored_by every ask node states now: an ask one of whose documents is past the store's bound
// keeps a row that names no node (graphstore.AskPlaces.Fit), which the ask author arms of
// commentPeople leave out, while its writer states the ask's node, its authored_by and its
// author's node all the same. Each names a node the store must hold, so a writer that stated the
// tie and not the node is a difference here. An ask a build before the store wrote states no
// authored_by until the backfill marks its row, so it names no author here.
//
// A node is history and never goes: a decision or a reading deleted or retyped since takes its
// row and its current tie away and leaves its author's node standing, so every person and session
// a block's decided_by or authored_by ever named, asserted or retracted since, is read too, off
// the store's log (writtenHistory), and so is every person an ask's answered_by or decided_by ever
// named (answeredHistory): a person whose answer a reopen took off the row is still the person
// that answer named. An erase of the decision, the reading or the ask takes the tuples that name
// its writers and records those writers (erasedWriters), which are read too, so a session no erase
// removes and the person it belongs to are still held to their nodes.
//
// The census reads it once for its run (readStoredAuthors) and binds what it read into the names
// of the person and session kinds (censusAuthors), so its two reads of the log cost one statement
// a census whatever plan the planner takes for them; the backfill's session pass reads it inside
// its own statement (inlineAuthors).
var storedAuthors = `
	select r.to_kind as kind, r.to_id as id, r.created_at as at
	from block_refs r
	where r.kind = '` + graphstore.TupleDecidedBy + `'
	  and r.to_kind in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `')
	union all
	` + currentAuthors(`held.kind = '`+refs.BlockNodeKind+`' and held.supertag = '`+refs.ReadingSupertag+`'`) + `
	union all
	` + currentAuthors(`held.kind = '`+graphstore.KindAsk+`'`) + `
	union all
	` + writtenHistory + `
	union all
	` + answeredHistory + `
	union all
	select erased.kind, erased.id, erased.at from (` + erasedWriters + `) erased`

// censusAuthors is storedAuthors as the census binds it into the names of the person and session
// kinds, the kinds, ids and times readStoredAuthors read as $1, $2 and $3 (projection.authored),
// and inlineAuthors is storedAuthors read in the statement that names it. Each is a FROM item
// named stored, of (kind, id, at).
const censusAuthors = `unnest($1::text[], $2::text[], $3::timestamptz[]) stored (kind, id, at)`

var inlineAuthors = `(` + storedAuthors + `) stored`

// answeredHistory is every person an ask's answered_by or decided_by ever named, and every person
// an ask's removed_by ever named (the person who took the ask's block out of its document, whose
// node that write makes), asserted or retracted since, as (kind, id, at) (storedAuthors), read
// from each person's node: the probe of graph_tuples binds graph_tuples_as_of_in's partial
// predicate (to_kind <> 'value') beside its join, as graphstore.CurrentIn binds it, so that index
// is one the probe can take.
var answeredHistory = `select answered.kind, answered.id, answered.made_at from graph_nodes answered
		where answered.kind = '` + graphstore.KindPerson + `' and exists (
			select 1 from graph_tuples t
			where t.to_kind = answered.kind and t.to_kind <> '` + graphstore.KindValue + `' and t.to_id = answered.id
			  and t.kind in ('` + graphstore.TupleAnsweredBy + `', '` + graphstore.TupleDecidedBy + `', '` + graphstore.TupleRemovedBy + `')
			  and t.from_kind = '` + graphstore.KindAsk + `')`

// writtenHistory is every person and session a block's decided_by or authored_by ever named,
// asserted or retracted since, as (kind, id, at) (storedAuthors), read as answeredHistory reads
// its people, from each writer's node. The join to a writer of either kind gives the planner no
// constant to prove graph_tuples_as_of_in's predicate from, so the probe binds it itself.
var writtenHistory = `select writer.kind, writer.id, writer.made_at
	from graph_nodes writer
	where writer.kind in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `') and exists (
		select 1 from graph_tuples t
		where t.to_kind = writer.kind and t.to_kind <> '` + graphstore.KindValue + `' and t.to_id = writer.id
		  and t.kind in ('` + graphstore.TupleDecidedBy + `', '` + graphstore.TupleAuthoredBy + `')
		  and t.from_kind = '` + refs.BlockNodeKind + `')`

// erasedWriters are the people and sessions an erase of an ask, a decision or a reading took the
// authored_by, decided_by or answered_by of, as (kind, id, at): the writers `envoy-dispatch erase`
// records in its graph_erasures row (grapherase's Erasure.Writers), each dated by that erase. The
// erase deletes the node's tuples in both directions, and nothing else names who wrote or answered
// it once its rows are gone, while that writer's node stands: a session's always, since no erase
// removes one, and a person's until an erase of that person. So a writer is read here unless an
// erase of that same node ran after the one that recorded it (erasedSince): an erased person is
// held by no record an erase before theirs wrote.
var erasedWriters = `
	select written->>'kind' as kind, written->>'id' as id, erasure.ran_at as at
	from graph_erasures erasure
	cross join lateral jsonb_array_elements(case when jsonb_typeof(erasure.removed->'writers') = 'array'
	                                             then erasure.removed->'writers' else '[]'::jsonb end) written
	where written->>'kind' in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `')
	  and coalesce(written->>'id', '') <> ''
	  and not ` + erasedSince(`written->>'kind'`, `written->>'id'`, `erasure.ran_at`)

// currentAuthors is the author every node held of graph_nodes that holders selects states now:
// the far end of its current authored_by into a person or a session, as (kind, id, at), read
// through graph_tuples_current, one probe per node.
func currentAuthors(holders string) string {
	return `select written.to_kind, written.to_id, written.at
	from graph_nodes held
	cross join lateral (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.at, t.asserted`,
		`t.from_kind = held.kind and t.from_id = held.id and t.kind = '`+graphstore.TupleAuthoredBy+`'
		  and t.to_kind <> '`+graphstore.KindValue+`' and t.to_kind in ('`+graphstore.KindPerson+`', '`+graphstore.KindSession+`')`) + `
	) written
	where ` + holders + ` and written.asserted`
}

// storedOwners are the people the server proved own a session that wrote a decision, a reading or
// an ask (storedAuthors), an erased one's too: the write that names such a session ties it to its
// owner with an alias_of tuple and makes the owner's node with it (graphstore.AuthorFacts).
// Neither block_refs nor any other table records the actor a decision's author was proved by, so
// the owner is read as the store holds that tie now, the far end of each alias_of the session
// states, which an erase of that person deletes with the person's node. authors is the relation
// storedAuthors is read as (censusAuthors or inlineAuthors).
//
// The probe is keyed by the session and names no far end: an alias_of points at a person and at
// nothing else, since graphstore's aliases is its one writer, so it states graph_tuples_current's
// predicate (to_kind <> 'value', and kind <> 'delivered_to', which the kind it binds proves) where
// it would bind to_kind = 'person'. Only an index led by from_kind and from_id keys the probe:
// graph_tuples_as_of_in, led by to_kind, holds no from_id to key it by.
func storedOwners(authors string) string {
	return `
	select owner.to_id as login
	from (select distinct stored.id from ` + authors + ` where stored.kind = '` + graphstore.KindSession + `') writer
	cross join lateral (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindSession+`' and t.from_id = writer.id and t.kind = '`+graphstore.TupleAliasOf+`'
		  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) owner
	where owner.asserted`
}

// recordedSessions are the sessions a row of the store names or an event of the log has named,
// each once, with the first event that named it (null for a session only a row names) and the
// earliest time a row or an event names it. They are every write R14 records a session from: a
// comment a session wrote (graphstore.AuthorOf), as its row names it or, for a comment the erase
// took, the event that stored it (commentWriters); every session a delivery reached - a comment
// mention's and a targeted message's attempt alike, whether the outbox's record states it
// (outbox.deliveredGraph) or the attempt's own settle does (graphstore.QueueReached), and a
// fallback copy the outbox published to a session's own topic (outbox.fallbackDelivered); an ask
// a session opened, every session that follows an ask now, and every session an
// ask.follower_added event ever named (asks.Follow), so a follower added and removed before the
// store existed is held to a node as well. A node is history and never goes, so a session that
// once followed still owes it. The same holds of a session that asked: every session an
// ask.opened event named as the ask's author is read too, so a session whose ask an erase took
// away, asks row, follower rows and node with it, still owes its node, which no erase removes
// (grapherase refuses a session) and the log keeps addressing; the erase redacts no ask event. An
// ask.opened names a session here and never a person, and no erase removes a session, so no
// erase can run after that event that leaves it naming a node the store no longer holds. And
// every session that wrote a decision, a reading or an ask the store names an author of, or that
// an erase of one recorded as its writer (storedAuthors).
//
// A comment mention's attempt is read twice for the same reason: as its row, and as the
// comment.delivery receipt its settle appends in the transaction that records the session
// (api's completeCommentDelivery), from which the outbox records that session as well
// (outbox.mentionReached). The erase deletes the attempt's row with its comment, since the row
// cannot exist without it, and keeps the receipt with its state and its session id, which it never
// rewrites (grapherase's structuralKey), so a session only an erased comment's mention reached
// stays held to that receipt. A fallback copy's row carries no foreign key to its comment, so the
// erase leaves that row, and it holds its session as it did.
//
// R14 is exact about which attempt is a delivery: an attempt the listener took is one, and a
// pending or failed one is not. So this binds the two states that mean it
// (model.DeliveryTakenStatesSQL), on an attempt's row and on its receipt alike, the test the
// writers bind too (graphstore.QueueReached, outbox.mentionReached): an attempt still pending has
// reached nobody yet and a failed one never will, and counting either here would hold the store to
// a node no writer is meant to have written. A queued attempt later answered is read by the same
// test before and after, so the flip to sent owes no node the settle did not already write.
//
// Only the comments the store holds name a session here (commentWriters), for the reason
// commentWriterPeople binds the same: a comment left out of the store records no author at all.
//
// It is one definition read by two: the census's session row side and the backfill's session
// pass (backfillSessions), so the sessions the backfill gives a node are exactly the sessions the
// census holds to one, as addressedRoles is for roles. authors is the relation storedAuthors is
// read as: the census binds what it read once (censusAuthors), the backfill reads it in its own
// statement (inlineAuthors).
func recordedSessions(authors string) string {
	return `
	select id, min(event) as event, min(at) as at from (
		select written.actor->>'id' as id, written.event, written.at
		from (` + commentWriters + `) written
		where written.actor->>'kind' = 'session' and coalesce(written.actor->>'id', '') <> ''
		union all
		select d.session_id, null, d.created_at from comment_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state in ` + model.DeliveryTakenStatesSQL + `
		union all
		select d.session_id, null, d.created_at from message_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state in ` + model.DeliveryTakenStatesSQL + `
		union all
		select substring(f.destination from ` + strconv.Itoa(len(contracts.AgentTopicPrefix)+1) + `), null, f.created_at
		from comment_fallback_copies f
		where f.published and f.destination like '` + contracts.AgentTopicPrefix + `%'
		union all
		select a.author->>'id', null, a.created_at from asks a
		where a.author->>'kind' = 'session' and coalesce(a.author->>'id', '') <> ''
		union all
		select f.session_id, null, f.since from ask_followers f
		union all
		select case when e.type in ('ask.follower_added', 'comment.delivery') then e.payload->>'session_id'
		            when e.payload->'author'->>'kind' = 'session' then e.payload->'author'->>'id' end,
		       e.id, e.created_at
		from events e
		where e.type in ('ask.follower_added', 'ask.opened')
		   or (e.type = 'comment.delivery' and e.payload->>'state' in ` + model.DeliveryTakenStatesSQL + `)
		union all
		select stored.id, null, stored.at from ` + authors + ` where stored.kind = '` + graphstore.KindSession + `'
	) sessions
	where id is not null and id <> ''
	group by id`
}

// addressedRoles are the roles a row of the store addresses now or an event of the log has ever
// addressed (R19), each once, with the first event that named it, null for a role only a row
// names. The rows are a comment mention's target, a message's target and an issue's route as they
// stand now. The events are the route every issue event recorded the issue under, the target
// every message event recorded, and the target of each mention a new comment's event named. A
// node is history and never goes, so an issue routed to one role and then re-routed to another
// still owes the first its node and its page: issues.route names only the second, and the
// issue's own events name both.
//
// It is one definition read by two: the census's role and page row side (roleNames)
// and the backfill's role pass (backfillRoles), so the roles the backfill gives a node are
// exactly the roles the census holds to one. The publisher writes a role's node and its page from
// those same fields of those same events (outbox.addressedRoles), whether or not it routed
// anything to the role. Every role a row names now was named by the event its write appended, so
// the two sides agree once the outbox has read the log; a role named by an event the outbox has
// not read yet is a row with no node until it does.
//
// It reads every event of six types, which no index narrows, so a census reads it once and binds
// the names into every statement that reads the role and page rows (projection.names).
const addressedRoles = `
	select name, min(event) as event from (
		select substring(d.target from 6) as name, null::bigint as event
		from comment_deliveries d where d.target like 'role:%'
		union all
		select substring(m.target from 6), null from messages m where m.target like 'role:%'
		union all
		select substring(i.route from 6), null from issues i where i.route like 'role:%'
		union all
		select substring(addressed.target from 6), e.id
		from events e
		cross join lateral (
			select e.payload->>'route' where e.type in ('issue.created', 'issue.updated', 'issue.closed')
			union all
			select e.payload->>'target' where e.type in ('message.created', 'message.answered')
			union all
			select mention->>'target'
			from jsonb_array_elements(case when e.type = 'comment.created'
			                                and jsonb_typeof(e.payload->'mentions') = 'array'
			                               then e.payload->'mentions' end) mention
		) addressed (target)
		where e.type in ('issue.created', 'issue.updated', 'issue.closed',
		                 'message.created', 'message.answered', 'comment.created')
		  and addressed.target like 'role:%'
	) roles
	where name <> ''
	group by name`

// roleNames is the role row side as a kind binds it (projection.names): the name of every role
// addressedRoles names, which the role kind and the page kind both read.
const roleNames = `select addressed.name from (` + addressedRoles + `) addressed`

// readNames reads the row side a kind binds (projection.names) once for a run of the census, in
// one statement, so the event log is scanned once however many statements then read the kind's
// rows; kind names the kind in an error, and args are what names binds, the stored authors for a
// kind whose names read them (projection.authored). It runs inside the census's snapshot, so every
// statement after it reads the names that snapshot holds.
func readNames(ctx context.Context, tx pgx.Tx, kind, names string, args []any) ([]string, error) {
	rows, err := tx.Query(ctx, names, args...)
	if err != nil {
		return nil, fmt.Errorf("read the %s row side: %w", kind, err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read a name of the %s row side: %w", kind, err)
		}
		read = append(read, name)
	}
	return read, rows.Err()
}

// storedWriters is storedAuthors as the census read it, column by column, for censusAuthors.
type storedWriters struct {
	kinds, ids []string
	ats        []time.Time
}

// readStoredAuthors reads storedAuthors once for a run of the census, in one statement, so the
// store's log its history arms read is read once however many statements then read the person and
// the session row sides, whatever plan the planner takes for those arms. It runs inside the
// census's snapshot, as readNames does, so every statement after it reads what that snapshot holds.
func readStoredAuthors(ctx context.Context, tx pgx.Tx) (*storedWriters, error) {
	rows, err := tx.Query(ctx, `select stored.kind, stored.id, stored.at from `+inlineAuthors)
	if err != nil {
		return nil, fmt.Errorf("read who wrote the decisions, readings and asks the store holds: %w", err)
	}
	defer rows.Close()
	authors := &storedWriters{kinds: []string{}, ids: []string{}, ats: []time.Time{}}
	for rows.Next() {
		var kind, id string
		var at time.Time
		if err := rows.Scan(&kind, &id, &at); err != nil {
			return nil, fmt.Errorf("read one who wrote a decision, a reading or an ask: %w", err)
		}
		authors.kinds, authors.ids, authors.ats = append(authors.kinds, kind), append(authors.ids, id), append(authors.ats, at)
	}
	return authors, rows.Err()
}

// StoreCensus reads the node and tuple store against the tables that project it, in one
// read-only repeatable-read transaction on a connection of its own. It writes nothing: the
// transaction is read only, and every statement it runs is a select, but for the savepoint the
// growth pass reads the store's type nodes in (installStoreBlockSchema).
//
// A difference is reported by its cause and by the row it was found at, in both directions: a
// projection row naming no node, naming a node the store does not hold or naming one it holds
// gone; a row whose shared fields disagree with its node's; and a node not gone no row projects.
//
// Two writes can state one fact twice by design and nothing here reads wrong for it: a tuple at
// the source_event sentinel is outside both repeat keys, and the backfill writes every fact it
// cannot date from a row's own event at that sentinel, so two runs at once that both read a row
// before either names its node each append a tuple stating the same fact. The current read takes
// the newest asserting tuple per far end, which is that same fact, and this census counts nodes
// and projection rows rather than tuples.
//
// sizing adds the growth pass (SizingReport), which reads the same snapshot. The growth pass
// reads no table of the store but its type nodes, and those only where the store holds them, so
// it runs on a database this branch's migrations have not reached, where the census reads no kind
// and says the store is absent.
func StoreCensus(ctx context.Context, databaseURL string, sizing bool) (*StoreReport, error) {
	return storeCensus(ctx, databaseURL, sizing, nil)
}

// storeCensus is StoreCensus with afterKind, which runs once each kind has been read and before
// anything after it is, on the census's own context, and whose error ends the census. Only a
// test passes one: it is how a test commits a write between two of the census's reads and asks
// whether the reads after it still answer the snapshot the first one took. Nil runs nothing.
func storeCensus(ctx context.Context, databaseURL string, sizing bool, afterKind func(ctx context.Context, kind string) error) (*StoreReport, error) {
	conn, err := store.Connect(ctx, databaseURL, applicationName)
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
	// The store is looked up before any kind is read, so a database the store does not exist in
	// answers that rather than failing at the first statement that names graph_nodes.
	if err := tx.QueryRow(ctx, `select to_regclass('graph_nodes') is null`).Scan(&report.Absent); err != nil {
		return nil, fmt.Errorf("look up the store: %w", err)
	}
	if !report.Absent {
		if report.Kinds, err = censusOfKinds(ctx, tx, afterKind); err != nil {
			return nil, err
		}
		if report.StoreOnly, err = readStoreOnly(ctx, tx); err != nil {
			return nil, err
		}
		work, err := ReadRestoreWork(ctx, tx)
		if err != nil {
			return nil, err
		}
		report.Collisions = CollisionsOf(work)
		if report.Dangling, err = danglingCitationsOver(ctx, tx, work); err != nil {
			return nil, err
		}
		if report.Projects, err = ProjectCensus(ctx, tx); err != nil {
			return nil, err
		}
		if report.ProjectFindings() > 0 {
			limit := reportedDifferences
			if report.ProjectIncomplete, err = projectIncomplete(ctx, tx, &limit); err != nil {
				return nil, err
			}
		}
	}
	if sizing {
		sized, err := readSizing(ctx, tx)
		if err != nil {
			return nil, err
		}
		report.Sizing = sized
	}
	return report, nil
}

// currentOutOfNodes is two items of a `with recursive` list: name, every relation of the tuple
// kinds kinds out of a node of a kind of nearKinds that the store states now - the newest tuple of
// each, kept while it asserts (graphstore.CurrentTupleRead) - as (from_kind, from_id, kind,
// to_kind, to_id), and name_near, the nodes it reads them out of. No index of the store's set
// (20261109013400_graph_store.up.sql) leads with a tuple's kind, so a read of every tuple of a
// kind starts from the nodes the kind leaves: name_near walks every node of nearKinds that states
// a relation, once each, one probe of graph_tuples_current per node on its leading (from_kind,
// from_id) for the next from_id past the last one, and name reads each such node's tuples of
// kinds through one probe of the same index on (from_kind, from_id, kind). The walk reads the
// nodes off the log rather than off graph_nodes: a writer states these kinds without stating the
// node they leave (docs' writeFacts states a moved_to out of a heading's address and a removed_by
// out of an ask's node and writes neither node), so a tuple whose near end has no node is read
// all the same. Both probes state graph_tuples_current's predicate, which every kind of kinds
// meets, since each is a relation and none is delivered_to.
func currentOutOfNodes(name string, nearKinds, kinds []string) string {
	relations := `t.to_kind <> '` + graphstore.KindValue + `' and t.kind <> '` + graphstore.TupleDeliveredTo + `'`
	return name + `_near (from_kind, from_id) as (
		select first.from_kind, first.from_id
		from (values ('` + strings.Join(nearKinds, `'), ('`) + `')) near_kind (from_kind)
		cross join lateral (
			select t.from_kind, t.from_id from graph_tuples t
			where t.from_kind = near_kind.from_kind and ` + relations + `
			order by t.from_id limit 1
		) first
		union all
		select next.from_kind, next.from_id
		from ` + name + `_near near
		cross join lateral (
			select t.from_kind, t.from_id from graph_tuples t
			where t.from_kind = near.from_kind and t.from_id > near.from_id and ` + relations + `
			order by t.from_id limit 1
		) next
	),
	` + name + ` as (
		select held.from_kind, held.from_id, held.kind, held.to_kind, held.to_id
		from ` + name + `_near near
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`,
		`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = near.from_kind and t.from_id = near.from_id
			  and t.kind in ('`+strings.Join(kinds, `', '`)+`') and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held
		where held.asserted
	)`
}

// storeOnlyRead counts, in one statement, every relation of the three kinds no table projects
// that the store states now - the newest tuple of each (near end, far end), kept while it asserts
// - and those of them whose far end does not resolve: a copied_from (a copy's block to the ask it
// was copied from, graphstore.TupleCopiedFrom) resolves to a live ask node; a moved_to
// (graphstore.TupleMovedTo) to a block that stands, which is one a document_blocks row holds,
// reached at once or through more moved_to tuples, a cycle reaching none, or to a live ask node,
// as a copied_from does, which ends a chain (the heading of a section that cites an answered ask
// a person removed from its spec); and a removed_by (graphstore.TupleRemovedBy, an ask's node to
// the person who took its block out) to a person node of the store. A far end is followed through
// one tuple fewer than graphstore.MovedToHops, since the tuple that names it is the first of the
// chain a citation of its near end is read through (graphstore.MovedTo), so a far end that
// resolves here is one every resolver reaches from that near end.
//
// A moved_to whose chain reaches no block that stands but reaches an address in a document that
// still owes its block index (document_block_versions.version null, as typedNodeOwed reads it) is
// left out, a class of its own and not an unresolved far end: such a document holds no row yet for
// a block its latest version holds - one past the deferral bound, or one a build before wrote
// during the roll - and the block-index reconciliation writes it.
//
// The read takes the three kinds' tuples out of every block and ask node the log holds
// (currentOutOfNodes), the near ends their writers state them out of - a copied_from and a
// moved_to out of a block, a removed_by out of an ask's node - through graph_tuples_current, one
// probe per node, and a store that holds none of them reads three zero counts. It states that
// index's to_kind predicate, which leaves out no tuple of any of them, since none of their far
// ends is a value.
var storeOnlyRead = `
	with recursive ` + currentOutOfNodes("stated", []string{refs.BlockNodeKind, graphstore.KindAsk},
	[]string{graphstore.TupleCopiedFrom, graphstore.TupleMovedTo, graphstore.TupleRemovedBy}) + `,
	followed (start, kind, address, hops, path) as (
		select far.to_id, far.to_kind, far.to_id, 0, array[far.to_id]
		from (select distinct s.to_kind, s.to_id from stated s where s.kind = '` + graphstore.TupleMovedTo + `') far
		union all
		select f.start, m.to_kind, m.to_id, f.hops + 1, f.path || m.to_id
		from followed f
		join stated m on m.kind = '` + graphstore.TupleMovedTo + `' and m.from_kind = f.kind and m.from_id = f.address
		where f.kind = '` + refs.BlockNodeKind + `' and f.hops < ` + strconv.Itoa(graphstore.MovedToHops-1) + ` and m.to_id <> all(f.path)
		  and not exists (select 1 from document_blocks d where d.node_id = f.address)
	),
	standing as (
		select distinct f.start from followed f
		where f.kind = '` + refs.BlockNodeKind + `' and exists (select 1 from document_blocks d where d.node_id = f.address)
		   or f.kind = '` + graphstore.KindAsk + `' and exists (
			select 1 from graph_nodes ask where ask.kind = '` + graphstore.KindAsk + `' and ask.id = f.address
			  and ask.state = '` + graphstore.StateLive + `')
	),
	owing as (
		select distinct f.start from followed f
		where f.kind = '` + refs.BlockNodeKind + `' and exists (
			select 1 from artifacts a
			join document_block_versions owed on owed.artifact_id = a.id
			where a.ref_key = split_part(f.address, '#', 1) and owed.version is null)
	),
	resolved as (
		select s.kind, case s.kind
			when '` + graphstore.TupleCopiedFrom + `' then s.to_kind = '` + graphstore.KindAsk + `' and exists (
				select 1 from graph_nodes ask where ask.kind = '` + graphstore.KindAsk + `' and ask.id = s.to_id
				  and ask.state = '` + graphstore.StateLive + `')
			when '` + graphstore.TupleRemovedBy + `' then s.to_kind = '` + graphstore.KindPerson + `' and exists (
				select 1 from graph_nodes person where person.kind = '` + graphstore.KindPerson + `' and person.id = s.to_id)
			else s.to_kind in ('` + refs.BlockNodeKind + `', '` + graphstore.KindAsk + `')
			  and exists (select 1 from standing where standing.start = s.to_id)
		end as resolves,
		s.kind = '` + graphstore.TupleMovedTo + `' and s.to_kind = '` + refs.BlockNodeKind + `'
		  and exists (select 1 from owing where owing.start = s.to_id) as left_out
		from stated s
	)
	select listed.kind, count(r.kind),
	       count(r.kind) filter (where not r.resolves and not r.left_out),
	       count(r.kind) filter (where not r.resolves and r.left_out)
	from (values (1, '` + graphstore.TupleCopiedFrom + `'), (2, '` + graphstore.TupleMovedTo + `'), (3, '` + graphstore.TupleRemovedBy + `')) listed (position, kind)
	left join resolved r on r.kind = listed.kind
	group by listed.position, listed.kind
	order by listed.position`

// readStoreOnly reads the kinds no table projects in the census's snapshot (storeOnlyRead), one
// report for each whether or not the store holds any of it, copied_from first.
func readStoreOnly(ctx context.Context, tx pgx.Tx) ([]StoreOnlyReport, error) {
	rows, err := tx.Query(ctx, storeOnlyRead)
	if err != nil {
		return nil, fmt.Errorf("read the kinds no table projects: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoreOnlyReport, error) {
		var kind StoreOnlyReport
		err := row.Scan(&kind.Kind, &kind.Tuples, &kind.Unresolved, &kind.LeftOut)
		return kind, err
	})
}

// censusOfKinds reads every kind of projections in order, running afterKind after each.
func censusOfKinds(ctx context.Context, tx pgx.Tx, afterKind func(ctx context.Context, kind string) error) ([]KindReport, error) {
	var kinds []KindReport
	// A kind's names are read before the first kind that binds them and kept for every kind after
	// it that binds the same ones, as the page kind binds the role kind's. The stored authors are
	// read before the first kind whose names read them and kept for every kind after it.
	read := map[string][]string{}
	var authors *storedWriters
	for _, held := range projections {
		var args []any
		if held.names != "" {
			names, done := read[held.names]
			if !done {
				var bound []any
				if held.authored {
					if authors == nil {
						var err error
						if authors, err = readStoredAuthors(ctx, tx); err != nil {
							return nil, err
						}
					}
					bound = []any{authors.kinds, authors.ids, authors.ats}
				}
				var err error
				if names, err = readNames(ctx, tx, held.name, held.names, bound); err != nil {
					return nil, err
				}
				read[held.names] = names
			}
			args = []any{names}
		}
		if held.linked {
			args = []any{blockLinkKinds()}
		}
		kind, err := censusOfKind(ctx, tx, held, args)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
		if afterKind != nil {
			if err := afterKind(ctx, held.name); err != nil {
				return nil, err
			}
		}
	}
	return kinds, nil
}

// blockLinkKinds is the tuple kinds a block's link is held under, which the block_ref kind holds
// its rows and its nodes to: each pointer attribute the installed block schema declares
// (pmdoc.LinkKinds) and `mentions`, but the ties of a decision a kind of their own holds triple
// by triple (tieProjection): a decision's shaped is a pointer of its block, and held here as well
// it would read a correct row of a drifted decision as a difference, every row of that block
// sharing one digest.
func blockLinkKinds() []string {
	return slices.DeleteFunc(append(pmdoc.LinkKinds(), "mentions"), func(kind string) bool {
		return kind == refs.TupleShaped || kind == refs.TupleFromAsk || kind == graphstore.TupleDecidedBy
	})
}

// censusOfKind reads one kind: how many rows and nodes not gone there are, and the differences in
// each direction with the first reportedDifferences of them. args are what the kind's rows bind,
// none but for a kind that binds its names (projection.names) or its link kinds
// (projection.linked).
//
// The four counts are four scalar subqueries of one statement rather than four round trips, so
// a census of seven kinds costs seven reads here and not twenty-eight. Each subquery is the
// statement it was on its own, so each is planned and streamed exactly as before.
func censusOfKind(ctx context.Context, tx pgx.Tx, held projection, args []any) (KindReport, error) {
	kind := KindReport{Kind: held.name, NodeKind: held.nodeKind, Table: held.table, Note: held.note,
		Triples: held.tuples != "", CountsUnmarked: held.unmarked != "", EmptyAgrees: held.emptyAgrees}
	read := sidesOf(held)
	// pastBound and unmarked are each a scalar `select count(*)`, so each is read directly rather
	// than wrapped in a count of its own row; a kind without the class reads zero.
	pastBound, unmarked := held.pastBound, held.unmarked
	if pastBound == "" {
		pastBound = "select 0"
	}
	if unmarked == "" {
		unmarked = "select 0"
	}
	if err := tx.QueryRow(ctx, `select
		(select count(*) from (`+held.rows+`) rows_read),
		(select count(*) from (`+read.nodes+`) nodes_read),
		(select count(*) from (`+read.rowsFound+`) rows_found),
		(select count(*) from (`+read.nodesFound+`) nodes_found),
		(`+pastBound+`),
		(`+unmarked+`)`, args...,
	).Scan(&kind.Rows, &kind.Nodes, &kind.RowDifferences, &kind.NodeDifferences, &kind.PastBound, &kind.Unmarked); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", held.name, err)
	}
	var err error
	if kind.RowDifferences > 0 {
		kind.ReportedRows, err = readDifferences(ctx, tx, held.name, read.rowsFound, args)
		if err != nil {
			return KindReport{}, err
		}
	}
	if kind.NodeDifferences > 0 {
		kind.ReportedNodes, err = readDifferences(ctx, tx, held.name, read.nodesFound, args)
		if err != nil {
			return KindReport{}, err
		}
	}
	return kind, nil
}

// kindSides are the reads censusOfKind counts of one kind besides its rows: its node side, and the
// differences in each direction, each of those two a select of (cause, at).
type kindSides struct{ nodes, rowsFound, nodesFound string }

// sidesOf is held's reads: of a kind held triple by triple its tuples and the rows and tuples
// the other side does not hold, and of any other its nodes not gone and the rows and nodes that
// disagree.
func sidesOf(held projection) kindSides {
	if held.tuples != "" {
		return kindSides{nodes: held.tuples, rowsFound: tripleRowDifferences(held), nodesFound: tupleDifferences(held)}
	}
	return kindSides{nodes: presentNodes(held), rowsFound: rowDifferences(held), nodesFound: nodeDifferences(held)}
}

// presentNodes is the kind's node side bound to every state but gone: a node the store holds gone
// is no node a row may project, and the row side holds it to that separately. Any other state is
// a node a row projects, so a state a later release adds, such as proposed, settled and
// overtaken (R8 of this change), reads here as live does. It answers (kind, id, digest, state):
// the kind's nodes answer (id, digest), each is given the kind here, and its state beside its
// digest, which nodeDifferences names a node with.
func presentNodes(held projection) string {
	return `select present.kind, node.id, node.digest, present.state from (` + held.nodes + `) node (id, digest)
		join graph_nodes present on present.kind = '` + held.nodeKind + `' and present.id = node.id
		                        and present.state <> '` + graphstore.StateGone + `'`
}

// rowDifferences reads the row side against the store: every row that names no node, names one
// the store does not hold, names one it holds gone, or disagrees with its node's digest. The
// gone test is its own cause rather than a missing node, because a row projecting a node the
// store has ended is a different defect from a row projecting nothing, and it is the one test
// of a node's state here: a row may project a node at any other state.
func rowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state = '` + graphstore.StateGone + `'
				then 'the row names a node the store holds ' || node.state
			else 'the row and its node disagree: the row digests ' || row.digest
			     || ' and the node ' || coalesce(present.digest, '(none)')
		end,
		row.row_label
		from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
		left join graph_nodes node on node.kind = row.node_kind and node.id = row.node_address
		left join (` + presentNodes(held) + `) present (kind, id, digest, state)
			on present.kind = row.node_kind and present.id = row.node_address
		where row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state = '` + graphstore.StateGone + `'
		   or row.digest is distinct from present.digest
		order by 2, 1`
}

// nodeDifferences reads the store against the row side: every node of the kind not gone that no
// row projects, named with the state it is at, so a live one reads as `a live node no row
// projects`, but a node whose row is owed by a writer that has not run yet (projection.owed).
func nodeDifferences(held projection) string {
	return `
		select case when node.state ~ '^[aeiou]' then 'an ' else 'a ' end || node.state
		       || ' node no row projects',
		       node.kind || ' ' || node.id
		from (` + presentNodes(held) + `) node (kind, id, digest, state)
		where not exists (
			select 1 from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
			where row.node_kind = node.kind and row.node_address = node.id)` + notOwed(held) + `
		order by 2`
}

// notOwed is the clause a node-side read of held appends to leave out a node whose row is owed
// (projection.owed), empty for a kind whose every node has its row.
func notOwed(held projection) string {
	if held.owed == "" {
		return ""
	}
	return `
		  and not ` + held.owed
}

// tripleRowDifferences reads the rows of a kind held triple by triple against the store: every
// row that names no node, names one the store does not hold or holds gone, or states a triple no
// tuple of the kind states now, each at the row and the triple it states.
func tripleRowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state <> '` + graphstore.StateLive + `'
				then 'the row names a node the store holds ' || node.state
			else 'a row no tuple states'
		end,
		row.row_label || ' ' || row.kind || ' -> ' || row.to_kind || ' ' || row.to_id
		from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
		left join graph_nodes node on node.kind = row.node_kind and node.id = row.node_address
		where row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state <> '` + graphstore.StateLive + `'
		   or not exists (
			select 1 from (` + held.tuples + `) stated (from_kind, from_id, kind, to_kind, to_id)
			where stated.from_kind = row.node_kind and stated.from_id = row.node_address
			  and stated.kind = row.kind and stated.to_kind = row.to_kind and stated.to_id = row.to_id)
		order by 2, 1`
}

// tupleDifferences reads the store's tuples of a kind held triple by triple against its rows:
// every tuple no row states the triple of, at from_kind:from_id kind -> to_kind:to_id, but a tuple
// of a node whose row is owed by a writer that has not run yet (projection.owed).
func tupleDifferences(held projection) string {
	return `
		select 'a tuple no row projects',
		       node.kind || ':' || node.id || ' ' || node.tie || ' -> ' || node.to_kind || ':' || node.to_id
		from (` + held.tuples + `) node (kind, id, tie, to_kind, to_id)
		where not exists (
			select 1 from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
			where row.node_kind = node.kind and row.node_address = node.id
			  and row.kind = node.tie and row.to_kind = node.to_kind and row.to_id = node.to_id)` + notOwed(held) + `
		order by 2`
}

func readDifferences(ctx context.Context, tx pgx.Tx, kind, sql string, args []any) ([]Difference, error) {
	rows, err := tx.Query(ctx, sql+` limit `+strconv.Itoa(reportedDifferences), args...)
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

// Write prints the report, one `census: store ` line per fact, every difference under its kind,
// the store-only kinds, the dangling-citation counter and the collision census, and the sizing
// after them, ending with the verdict. A kind held triple by triple prints the store's tuple count
// and its rows' triple count, and the differences as the rows no tuple states and the tuples no row
// projects; a store-only kind prints the far ends it leaves out as left_out. It prints counts,
// digests, kinds and the addresses of rows and nodes; no body, no actor and no value of an
// attribute leaves the database.
func (r *StoreReport) Write(w io.Writer) { r.write(w, false) }

// WriteVerbose is Write with one line for each citation the dangling-citation counter counted,
// naming its set, the node that cites and the address it cites, under the counter's line.
func (r *StoreReport) WriteVerbose(w io.Writer) { r.write(w, true) }

func (r *StoreReport) write(w io.Writer, verbose bool) {
	if r.Absent {
		fmt.Fprintln(w, "census: store absent (the database holds no graph_nodes: this branch's migrations have not run, so no kind was read)")
	}
	for _, kind := range r.Kinds {
		// A kind held triple by triple counts tuples where every other kind counts nodes.
		side := "node"
		if kind.Triples {
			side = "tuple"
		}
		fmt.Fprintf(w, "census: store kind=%s %ss=%s rows=%s table=%s rows_without_a_%s=%s %ss_without_a_row=%s addresses_past_bound=%s\n",
			kind.Kind, side, pgmigrate.Thousands(kind.Nodes), pgmigrate.Thousands(kind.Rows), kind.Table,
			side, pgmigrate.Thousands(kind.RowDifferences), side, pgmigrate.Thousands(kind.NodeDifferences),
			pgmigrate.Thousands(kind.PastBound))
		fmt.Fprintf(w, "census: store kind=%s checks %s\n", kind.Kind, kind.Note)
		if kind.CountsUnmarked {
			fmt.Fprintf(w, "census: store kind=%s unmarked_rows=%s (rows a writer of the release before wrote, which name no node yet and graph_edges' old arms answer)\n",
				kind.Kind, pgmigrate.Thousands(kind.Unmarked))
		}
		switch {
		case kind.EmptyAgrees && kind.Empty():
			fmt.Fprintf(w, "census: store kind=%s holds no row and no %s: nothing of it is written yet, which is no refusal\n", kind.Kind, side)
		case kind.Rows == 0:
			fmt.Fprintf(w, "census: store kind=%s checked no row, so the census proves nothing of it\n", kind.Kind)
		}
		writeDifferences(w, kind.Kind, kind.RowDifferences, kind.ReportedRows)
		writeDifferences(w, kind.Kind, kind.NodeDifferences, kind.ReportedNodes)
	}
	for _, kind := range r.StoreOnly {
		fmt.Fprintf(w, "census: store-only kind=%s tuples=%s unresolved=%s left_out=%s\n",
			kind.Kind, pgmigrate.Thousands(kind.Tuples), pgmigrate.Thousands(kind.Unresolved), pgmigrate.Thousands(kind.LeftOut))
	}
	if !r.Absent {
		writeMoves(w, r.Dangling, r.Collisions, verbose)
		if r.Decisions != nil {
			writeDecisions(w, *r.Decisions)
		}
	}
	writeProjectCensus(w, r.Projects)
	for _, row := range r.ProjectIncomplete {
		fmt.Fprintf(w, "census: project incomplete %s\n", row)
	}
	if !r.Absent {
		fmt.Fprintf(w, "census: store rows_checked=%s differences=%s\n",
			pgmigrate.Thousands(r.Rows()), pgmigrate.Thousands(r.Differences()))
	}
	if r.Sizing != nil {
		r.Sizing.Write(w)
	}
	empty := r.Unpopulated()
	switch {
	case r.Absent && r.Sizing == nil:
		fmt.Fprintln(w, "census: store REFUSED (the database holds no store, so the census read nothing)")
	case r.Absent:
		fmt.Fprintln(w, "census: store absent, sizing ok")
	case r.Differences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s difference(s))\n", pgmigrate.Thousands(r.Differences()))
	case r.ProjectFindings() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s row(s) whose node names no project or the wrong one, or node(s) no row stands behind;"+
			" run envoy-dispatch backfill-graph-store for the missing, gone and unscoped ones but those naming a party the store"+
			" refuses, run the erase each orphaned line names, and compare each differs row with its node)\n",
			pgmigrate.Thousands(r.ProjectFindings()))
	case r.Unresolved() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s far end(s) of a kind no table projects do not resolve)\n",
			pgmigrate.Thousands(r.Unresolved()))
	case len(empty) > 0:
		fmt.Fprintf(w, "census: store REFUSED (kind(s) %s checked no row, so the census proves nothing of them)\n",
			strings.Join(empty, ", "))
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
