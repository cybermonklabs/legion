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
// share. nodes is a select of (id, digest): one row per node of the kind, digesting the same
// fields as the store holds them, of which the census compares those the store holds present
// (presentNodes). The census holds the two against each other in both directions, so a row with
// no node and a node with no row are each a difference.
//
// The ties of decisions, readings and asks are held triple by triple instead (tuples), so a
// relation that reaches the wrong node or the wrong far end reads as a row and a tuple, each
// named, and every other row of that node reads nothing.
//
// A kind with no projection table takes the set of actors the projected tables name as its rows
// (people and agents below), which is the side that exists for it; a role takes the roles the
// event log ever named as well (addressedRoles), and a session the sessions the event log names
// as followers and as reached (recordedSessions).
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
	// present node states now (heldTuples). rows then answers (row_label, node_kind, node_address,
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
	// pastBound counts the addresses this kind's row side names that are past the store's bound
	// (graphstore.MaxAddressBytes), which the writers left unprojected on purpose: it is a full
	// `select count(*)`, reads $1 where names is set, and is empty for a kind no address of which
	// can pass the bound. The census reports the count as a class of its own and excludes those
	// addresses from both comparisons, so a row no store entry answers by design is no difference.
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
	// unstated counts the rows of the kind's table that belong to a document of an issue that has
	// not converted - no issue_trees row holds its issue - which no pass of this build's rule has
	// stated (unconvertedUnstated): a document the release before indexed, whose blocks hold none
	// of what this build's pass states until a pass of its rule reaches them. Those rows, and the
	// nodes of their blocks, are on neither side: the census reports their count as a class of
	// its own and compares none of them. It is a full `select count(*)`, empty for a kind no such
	// row is left out of.
	unstated string
	// owed is a predicate over node.id, a present node of the kind (for a kind held triple by
	// triple, the node holding the tuple), that holds where the node's row is owed by a writer
	// that has not run yet rather than missing: such a node is no node-side difference. Empty for
	// a kind whose every node has its row from the write that stated it.
	owed string
	// rowOwed is the row side's owed predicate, over node, the graph_nodes row a row of the kind
	// names (its columns null where the store holds none), that holds where the row is owed its
	// deletion by a writer that has not run yet rather than standing wrong: such a row is no
	// row-side difference. Empty for a kind every writer of whose rows deletes a row in the write
	// that ends its node.
	rowOwed string
	// emptyAgrees says a kind that holds no row, no present node and no unmarked row is no refusal:
	// one of the tie kinds, a decision's, a reading's and an answered or
	// followed ask's, which a deployment holds none of until an agent writes a decision or a
	// person answers an ask. Every other kind is one every store holds rows of, and checking
	// none of it refuses.
	emptyAgrees bool
	// mayBeEmpty says a deployment can hold no row of this kind - no run of the command bar, no
	// lens - so a census that checked zero rows of it proves nothing of it and refuses nothing for
	// it either (Unpopulated leaves it out). The run and frame kinds and the visibility and state
	// kinds of a run, a lens, a finding, a risk, a constraint and a lens kind are such kinds.
	mayBeEmpty bool
}

// KindReport is one kind's census: how many rows and how many present nodes it read, and what
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
	// CountsUnstated says the kind holds the rows of an unconverted issue's documents that no pass
	// of this build's rule has stated as a class of their own, and Unstated is how many there are,
	// none of them compared and none a difference (projection.unstated).
	CountsUnstated bool
	Unstated       int64
	// CountsOwed says the kind holds its rows that a pass of the backfill still owes a write as a
	// class of its own, and Owed is how many there are, none of them a difference: the Keeps a
	// release before summed over a decision's words with `shaped` (CensusOfKeepSums), which the
	// backfill's Keep pass rewrites, and which read zero once it has run.
	CountsOwed bool
	Owed       int64
	// EmptyAgrees says a kind that read no row, no present node and no unmarked row is no refusal
	// (projection.emptyAgrees).
	EmptyAgrees bool
	// MayBeEmpty is the kind's projection.mayBeEmpty: zero rows of it refuse nothing.
	MayBeEmpty bool
}

// Empty reports whether the kind read no row, no present node, no unmarked row and no unstated
// row, and so holds nothing to compare. A table whose rows all name no node yet holds rows still:
// they are the earlier release's, which the census cannot hold to a node until the backfill marks
// them; and so does one whose rows all wait for a pass of this build's rule.
func (k KindReport) Empty() bool {
	return k.Rows == 0 && k.Nodes == 0 && k.Unmarked == 0 && k.Unstated == 0
}

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
	// Absent is true where the database is no store this build can census (storeAbsence), and
	// AbsentBecause says why: graph_nodes does not exist, or a migration this binary embeds is not
	// applied yet. Every kind's read is this build's, written against the schema its migrations
	// leave, so over such a database the census reads no kind.
	Absent        bool
	AbsentBecause string
	// StoreOnly is, for each tuple kind no table projects (copied_from, moved_to and removed_by),
	// how many the store states now, how many of those reach a far end that does not resolve and
	// how many reach one in a document that still owes its block index (storeOnlyRead). An
	// unresolved far end refuses the census; a count does not, and neither does a far end left out.
	StoreOnly []StoreOnlyReport
	// Dangling is the dangling-citation counter over the restore's work (DanglingCitations), and
	// Collisions the collision census of the same work (CollisionsOf), the block ids the restore
	// must change for each issue: both read the census's snapshot, so both answer before the
	// restore moves anything. Neither refuses the census: the returning set counts before the
	// restore by design, and the moved set is a finding the driver that watches it holds to zero.
	Dangling   DanglingReport
	Collisions []Collision
	// Decisions is what the decisions step's third phase still owes (DecisionsCensus), set by the
	// census command from docs' reader of the decisions documents' sections, nil where no caller
	// read it. It refuses nothing.
	//
	// kept for one release: decisions-documents-fn
	Decisions *DecisionsCensus
	// VersionRenders is the census of the versions version_renders holds (VersionRenderCensus),
	// set by the census command from docs' renderer, nil where no caller read it. A version whose
	// stored markdown is not its render refuses the census.
	VersionRenders *VersionRenderCensus
	// Sizing is what the sizing pass found, or nil when it was not asked for.
	Sizing *SizingReport
	// ReadSeq is the read sequence the census saw: the highest graph_tuples.seq its snapshot
	// holds, which is graph_tuples.seq at the commit the census read, and 0 for a store that
	// holds no tuple or is Absent. R29 of the parent change's specification has a query, a census
	// and an approval each record the read sequence it saw, so storeCensus reads it inside the
	// census's own repeatable-read transaction, in the snapshot every kind is read in. Read on
	// any other snapshot, it could name a tuple a write committed after the census's snapshot was
	// taken, which none of the census's counts saw.
	ReadSeq int64
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
// that may be empty (KindReport.EmptyAgrees) and is: no row, no present node and no unmarked row;
// and a kind a deployment may hold none of (KindReport.MayBeEmpty).
func (r *StoreReport) Unpopulated() []string {
	var empty []string
	for _, kind := range r.Kinds {
		if kind.Rows == 0 && !kind.MayBeEmpty && !(kind.EmptyAgrees && kind.Empty()) {
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
// the sessions. One of the tie kinds is one exception: no row, no node and no
// unmarked row of it is a store nothing of that kind was written to yet, and is no refusal
// (projection.emptyAgrees). A table of the earlier release whose rows no backfill has marked yet
// is not that store: its kind checked no row while rows stand, and it refuses. A kind a
// deployment may hold none of - a run and its frames, and the visibility and state of a run, a
// lens, a finding, a risk, a constraint and a lens kind - is the other: it refuses nothing on zero
// rows alone (projection.mayBeEmpty); a driver over a corpus that holds them still reads the
// count. A version version_renders holds whose stored markdown is not its render refuses too
// (VersionRenders).
//
// A database the store does not exist in yet (Absent) has nothing to census. The sizing is taken
// on just such a database (R4 reads before the first migration), and the sizing refuses nothing
// (SizingReport), so there a census with the sizing passes on the sizing alone, and a census
// asked for without it refuses, since it read nothing.
func (r *StoreReport) Refused() bool {
	if r.Absent {
		return r.Sizing == nil
	}
	return len(r.Unpopulated()) > 0 || r.Differences() > 0 || r.ProjectFindings() > 0 || r.Unresolved() > 0 || r.renderDifferences() > 0
}

// renderDifferences is how many versions version_renders holds store other markdown than their
// render, 0 where the census read none of them.
func (r *StoreReport) renderDifferences() int64 {
	if r.VersionRenders == nil {
		return 0
	}
	return r.VersionRenders.Differences
}

// projections are the kinds the census holds the store to, in the order it reads and reports
// them: each topic's list, one after another. The comment comes first, then the block, the block
// reference and a block's rank and text (blockProjections), then a run of the command bar and its
// frames (runProjections), each a kind a table projects, held to that table; the four after them
// are the people and the agents, which no table projects (actorProjections); the eight after
// those are the ties of decisions, readings and asks that block_refs, document_blocks, asks and
// ask_followers project, and the six after those the same ties as graph_edges answers them, each
// one that view answers (decisionProjections); then every other topic's kinds; and last the
// visibility and the state of each run, lens, finding, risk, constraint and lens kind, held to the
// store's own record of them (visibilityStateProjections). The comment kind is read first:
// TestTheStoreCensusReadsOneSnapshot commits a write after it and before the block kind.
//
// Each topic of the store keeps its kinds in a list in a file of its own, census_<topic>.go, and
// projections is those lists one after another, so a kind of one topic is registered in that
// topic's file alone. A topic whose list is empty projects no kind of its own.
//
// The comment, the block, the block reference and the run each hold a row to the tuples that
// answer it as well as to its node row.
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
//
// The two block kinds hold a block to what refs.IndexBlocks projects of it and to nothing else. A
// block of a lens's finding (lensBlock) is a block node no document holds, so no table projects
// it and neither block kind reads it. And a tuple another writer states out of a block - a
// requirement's constrained_by, a changed_by_run, a finding's authored_by - is no link a
// block_refs row names, so the block_ref kind reads only the kinds that writer projects into
// block_refs (refs.BlockRefKindsSQL, read from the store inside the census's own snapshot) and
// the block kind only a block's placement.
var projections = slices.Concat(
	commentProjections, blockProjections, runProjections, actorProjections, decisionProjections,
	issueProjections, componentProjections, resultProjections, versionProjections, refProjections,
	approvalProjections, searchProjections, citationProjections, queryRoleProjections, anchorProjections,
	visibilityStateProjections,
)

// backfillPasses are the backfill's passes (Backfill), in the order it runs them, each topic's
// taken from its list in census_<topic>.go as projections takes its kinds. The order is what the
// passes rest on. The blocks run before the links out of them (blockBackfills). The runs of the
// command bar run after the comments (runBackfills), and the sessions after both, so that the
// session pass writes only the sessions neither the comment pass nor the run passes could give a
// node (backfillSessions). The retraction of each prevented_by at a check nobody runs runs after
// the runs (preventionBackfills). The project passes run after every pass that writes a
// comment's or a block's node, so that such a node takes its project from the pass that wrote it
// and the fill finds nothing of it to do (projectBackfills). The asks and their followers run
// after the project passes, whose object pass wrote the node of every ask (decisionBackfills).
// Every other topic's passes run after those, once every issue, document, ask and message holds
// its node. The Keep pass, which rewrites the sums of Keeps a release before summed over a
// decision's words with `shaped`, runs after them and before the head fill, which runs last
// (keepBackfills, headBackfills). rule is the marking rule the block passes mark by
// (blockBackfills).
func backfillPasses(rule int) []backfillPass {
	return slices.Concat(
		blockBackfills(rule), commentBackfills, runBackfills, preventionBackfills, actorBackfills, projectBackfills,
		decisionBackfills, issueBackfills, componentBackfills, resultBackfills, versionBackfills, refBackfills,
		approvalBackfills, searchBackfills, citationBackfills, queryRoleBackfills, anchorBackfills, keepBackfills,
		headBackfills,
	)
}

// commentBackfills are the backfill's pass over the comments (backfillComments).
var commentBackfills = []backfillPass{oneKind(backfillComments)}

// preventionBackfills are the backfill's pass over the prevented_by tuples a process of the
// release before left asserted at a check nobody runs (backfillEndedPreventions).
var preventionBackfills = []backfillPass{oneKind(backfillEndedPreventions)}

// actorBackfills are the backfill's passes over the roles and their pages and over the sessions,
// which no projection row names (backfillRoles, backfillSessions).
var actorBackfills = []backfillPass{oneKind(backfillRoles), oneKind(backfillSessions)}

// projectBackfills are the passes of the project of every node, which a read limited to a project
// depends on: the node of each issue, document, ask and message written before its kind's writer
// wrote one, and the project of each comment and block node written before a node named its
// project (backfillObjectNodes, backfillScopes).
var projectBackfills = []backfillPass{backfillObjectNodes, backfillScopes}

// commentProjections are the census's kind of the comments, which the comments table projects.
var commentProjections = []projection{{
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
		authoredLink("c.author"),
	) + `)
		from comments c` + refs.CommentStoreJoins + `
		where ` + refs.CommentInTheStore,
	nodes: `
		select n.id, md5(n.made_at::text || '|' || n.made_by::text || '|' || ` + currentLinks(graphstore.KindComment,
		kindIn("t.kind", commentLinkKinds)) + `)
		from graph_nodes n where n.kind = '` + graphstore.KindComment + `'`,
	pastBound: `
		select count(*) from comments c` + refs.CommentStoreJoins + `
		where not (` + refs.CommentInTheStore + `)
		   or (c.author->>'kind' = '` + graphstore.KindSession + `'
		       and octet_length(coalesce(c.author->>'id', '')) > ` + addressBound + `)`,
}}

// actorProjections are the census's kinds of the people, the sessions, the roles and the roles'
// pages, which no table projects: each row side is the actors the projected tables and the event
// log name.
var actorProjections = []projection{{
	name:     graphstore.KindPerson,
	nodeKind: graphstore.KindPerson,
	table:    "comments.author, going_requests.author, asks.author, asks.answer, block_refs.decided_by, graph_tuples.kept_by, events.payload, graph_nodes.made_by",
	note: "no table projects a person, so the row side is the people the projected tables name as authors," +
		" the owner of each session the server proved that wrote one, the person each ask's answer names and each" +
		" answer the store took ever named, who wrote each decision and reading and their proved owners, each" +
		" person a decision's Keep ever named, the author of each run, and the author of each lens, finding, risk" +
		" and constraint, and of each run whose row is gone, that states its authored_by",
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
	table: "comments.author, going_requests.author, comment_deliveries, message_deliveries, comment_fallback_copies," +
		" asks.author, ask_followers, block_refs.decided_by, events.payload, graph_nodes.made_by",
	note: "no table projects a session, so the row side is the sessions those tables name as authors, as reached," +
		" as askers, as followers and as who wrote a decision or a reading, every session an ask.opened event" +
		" named as the asker or an ask.follower_added event named or a comment.delivery receipt says the listener" +
		" took an attempt to, the author of each run, and the author of each lens, finding, risk and constraint," +
		" and of each run whose row is gone, that states its authored_by",
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
}}

// addressBound is the store's address bound (graphstore.MaxAddressBytes) as a statement spells
// it. It is written into the text rather than bound as a parameter because each side is spliced
// into a statement of its own and one shared constant keeps the two sides reading the same bound.
var addressBound = strconv.Itoa(graphstore.MaxAddressBytes)

// The tuple kinds each projection row states of its node, which the census digests against the
// tuples of those kinds the node states now (currentLinks, placedLinks, heldTuples): a comment's
// anchor, its owner and its author; a block's document and the block or document holding it;
// a run's author; and the ties of a decision that block_refs projects. A block_refs row of a block
// states the kinds blockRefKinds reads out of the store. A tuple of any other kind out of such a
// node is no part of any row.
var (
	commentLinkKinds    = []string{graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy, graphstore.TupleAuthoredBy}
	blockPlacementKinds = []string{refs.TupleInDocument, graphstore.TuplePartOf}
	runLinkKinds        = []string{graphstore.TupleAuthoredBy}
	tieKinds            = []string{refs.TupleShaped, refs.TupleFromAsk, graphstore.TupleDecidedBy}
)

// blockRefKinds is a select of every tuple kind the block_ref kind holds a block's rows and its
// node to: every kind refs.IndexBlocks writes into block_refs (refs.BlockRefKindsSQL, a mention or
// a pointer of a block[] attribute the store's type nodes declare, at any state), but the ties of
// a decision a kind of their own holds triple by triple (tieProjection): a decision's shaped is a
// pointer of its block, and held here as well it would read a correct row of a drifted decision as
// a difference, every row of that block sharing one digest.
var blockRefKinds = `select linked.kind from (` + refs.BlockRefKindsSQL + `) linked (kind)
			where not ` + kindIn("linked.kind", tieKinds)

// kindIn is true of the kind column - a node's, a tuple's or an edge's - whose value is one of
// kinds, a non-empty list of this build's constants (store.QuoteLiterals).
func kindIn(column string, kinds []string) string {
	return column + ` in (` + store.QuoteLiterals(kinds) + `)`
}

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

// authorNode is the far end of the authored_by tuple a row whose author is the jsonb actor states,
// written kind:id: the node graphstore.AuthorOf reads off the actor the row stored - the session
// it names, else the person authorLogin reads, a login canonicalised as commentWriterPeople
// canonicalises one - and null for an actor that names nobody, whose row states no authored_by at
// all.
func authorNode(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
				then '` + graphstore.KindSession + `:' || (` + actor + `->>'id')
			else '` + graphstore.KindPerson + `:' || user_threads_login(` + authorLogin(actor) + `)
		end`
}

// authoredLink is the authored_by link a row whose author is the jsonb actor states, as
// statedLinks takes one: kind>to_kind:to_id, null for an actor naming nobody and for a session
// past the store's bound, of whom the writer states nothing (graphstore.AuthorFacts).
func authoredLink(actor string) string {
	return `case when ` + actor + `->>'kind' <> '` + graphstore.KindSession + `'
		           or octet_length(coalesce(` + actor + `->>'id', '')) <= ` + addressBound + `
		      then '` + graphstore.TupleAuthoredBy + `>' || ` + authorNode(actor) + ` end`
}

// commentWriters are the comments the store holds, each as the actor that wrote it and the time it
// was written: every comments row the store holds (refs.CommentInTheStore). It is who wrote a
// comment, as commentWriterPeople and recordedSessions read it.
var commentWriters = `
	select c.author as actor, c.created_at as at
	from comments c` + refs.CommentStoreJoins + `
	where ` + refs.CommentInTheStore

// commentWriterPeople is each person a comment the store holds or held names as its writer
// (commentWriters), one row for each such comment and person, with the time the comment was
// written and the event that stored it (null for a row). It reads an actor as graphstore.AuthorOf
// does (authorLogin): the login an actor of kind user carries, or the owner a personal token's
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
// read it on a database the store does not exist in yet.
var commentWriterPeople = `
	select user_threads_login(author.login) as login, written.at
	from (` + commentWriters + `) written
	cross join lateral (values (` + authorLogin("written.actor") + `),
		(case when ` + provedSessionOwner("written.actor") + ` then written.actor->>'owner' end)) author (login)
	where author.login is not null and user_threads_login(author.login) <> ''`

// commentPeople are the people the census holds to a person node, each once: who wrote a comment
// the store holds (commentWriterPeople), and the people an ask, a decision or a reading of the
// store names, below.
//
// The backfill gives the people comments name their nodes in its comment pass (writeCommentBatch,
// graphstore.AuthorFacts) and needs no pass of its own for them: a row names its author to that
// pass.
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
// join, so the planner keys the index rather than reading the table whole.
//
// Who wrote each decision, reading and ask the store holds an authored_by or decided_by of names
// people too, and so does every person an ask's answer ever named, and every author of a lens, a
// finding, a risk or a constraint and of a run whose row is gone (storedAuthors, read as
// the relation authors names: censusAuthors where the census binds what it read once,
// inlineAuthors where a statement reads it itself), and so does the owner the server proved of
// each such writer that is a session (storedOwners).
//
// The authors of the runs going_requests holds are read off each row's author by the same two
// rules as a comment's, since the run's writer (graphstore.InteractionFacts) states its author
// through graphstore.AuthorFacts as the comment path does.
func commentPeople(authors string) string {
	return `
	select distinct user_threads_login(author.login) as login
	from (
		select named.login
		from (` + commentWriterPeople + `) named
		union all
		select asker.login
		from asks k
		cross join lateral (values (` + authorLogin("k.author") + `),
			(case when ` + provedSessionOwner("k.author") + ` then k.author->>'owner' end)) asker (login)
		where k.node_kind is not null
		union all
		select k.answer->>'user' from asks k where k.node_kind is not null and k.answer is not null
		union all
		select runner.login
		from going_requests r
		cross join lateral (values (` + authorLogin("r.author") + `),
			(case when ` + provedSessionOwner("r.author") + ` then r.author->>'owner' end)) runner (login)
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
// author is the far end of its authored_by, which no table projects (the tuple table), so the row
// read for it is the tuple itself, the authored_by each reading's node not gone states now, as
// graph_edges' tuple arm would answer it. An ask's author is read the same way, off the
// authored_by every ask node states now: an ask one of whose documents is past the store's bound
// keeps a row that names no node (graphstore.AskPlaces.Fit), which the ask author arms of
// commentPeople leave out, while its writer states the ask's node, its authored_by and its
// author's node all the same. Each names a node the store must hold, so a writer that stated the
// tie and not the node is a difference here. An ask a build before the store wrote states no
// authored_by until the backfill marks its row, so it names no author here.
//
// The authors of artifact_versions name people and sessions too (dispatch://INFRA-1253's T7):
// every author of the version that first held each block whose authors the block_author kind
// holds - an authored node or a block of an issue room, of a document that owes no block index
// (blockVersionAuthors, blockAuthorsHeld) - since the block index states an authored_by to each of
// them through graphstore.AuthorFacts, which writes the author's node.
//
// A node is history and never goes: a decision or a reading deleted or retyped since takes its
// row and its current tie away and leaves its author's node standing, so every person and session
// a block's decided_by or authored_by ever named, asserted or retracted since, is read too, off
// the store's log (writtenHistory), and so is every person a decision's kept_by ever named: a
// person's Keep (api's keepDecisionNode, and the conversion's Keep it carries onto a section's
// decision) writes the keeper's node beside it, and a person whose only act is a Keep is held to
// that node by it alone. So is every person an ask's answered_by or decided_by ever named
// (answeredHistory): a person whose answer a reopen took off the row is still the person that
// answer named.
//
// The census reads it once for its run (readStoredAuthors) and binds what it read into the names
// of the person and session kinds (censusAuthors), so its two reads of the log cost one statement
// a census whatever plan the planner takes for them; the backfill's session pass reads it inside
// its own statement (inlineAuthors).
//
// The authors of the reasoning nodes and of the runs whose row is gone (reasoningAuthored,
// goneRunsAuthored) are read here too, so the person and session row sides read them bound as
// well and read the log for them no more than for the rest.
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
	` + blockVersionAuthors + `
	union all
	` + reasoningAuthored + `
	union all
	` + goneRunsAuthored + `
	union all
	` + writtenHistory + `
	union all
	` + answeredHistory

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

// writtenHistory is every person and session a block's decided_by or authored_by ever named, and
// every person a decision's kept_by ever named, asserted or retracted since, as (kind, id, at)
// (storedAuthors), read as answeredHistory reads its people, from each writer's node. The join to
// a writer of either kind gives the planner no constant to prove graph_tuples_as_of_in's predicate
// from, so the probe binds it itself.
var writtenHistory = `select writer.kind, writer.id, writer.made_at
	from graph_nodes writer
	where writer.kind in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `') and exists (
		select 1 from graph_tuples t
		where t.to_kind = writer.kind and t.to_kind <> '` + graphstore.KindValue + `' and t.to_id = writer.id
		  and t.kind in ('` + graphstore.TupleDecidedBy + `', '` + graphstore.TupleAuthoredBy + `', '` + refs.TupleKeptBy + `')
		  and t.from_kind = '` + refs.BlockNodeKind + `')`

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
// an ask (storedAuthors): the write that names such a session ties it to its owner with an
// alias_of tuple and makes the owner's node with it (graphstore.AuthorFacts). Neither block_refs
// nor any other table records the actor a decision's author was proved by, so the owner is read as
// the store holds that tie now, the far end of each alias_of the session states. authors is the
// relation storedAuthors is read as (censusAuthors or inlineAuthors).
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

// authorLogin is the login of the person graphstore.AuthorOf names of the actor a jsonb column
// holds, uncanonicalised, and null for an actor it names a session of or nobody.
func authorLogin(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'user' and coalesce(` + actor + `->>'id', '') <> '' then ` + actor + `->>'id'
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> '' then null
			when coalesce(` + actor + `->>'owner', '') <> '' then ` + actor + `->>'owner'
		end`
}

// provedSessionOwner is true of a stored actor, the SQL expression actor, that is a session the
// server proved its owner's (graphstore's provedOwner): the actor is the session, its act names
// that same session, and the session is within the store's bound. Its `owner`, where it names one,
// is then the person the session belongs to, whom the write that stored the actor ties to the
// session by an alias_of tuple and makes a node of (graphstore.AuthorFacts). It is
// commentWriterPeople's second arm, spelled once here so that commentWriterPeople and the sizing's
// count of a comment's aliases (sizingWritesWith) read one rule.
func provedSessionOwner(actor string) string {
	return actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
		  and ` + actor + `->'act'->>'session' = ` + actor + `->>'id'
		  and octet_length(` + actor + `->>'id') <= ` + addressBound
}

// reasoningAuthored is the author every lens, finding of one (a block whose id names its lens,
// lensBlock), risk and constraint states now, as (kind, id, at) (storedAuthors): the far end of
// its current authored_by into a person or a session. Their one writer (api's writeReasoningNodes)
// states that tuple to the writer graphstore.AuthorOf names of the node's made_by, and that
// writer's own node beside it (graphstore.AuthorFacts), whatever the credential proved (spec v46
// R12), so the far end is a person or a session the store owes a node, and the owner the server
// proved of a session is read off its alias_of (storedOwners). A node written on the shared
// bearer, or by a pod on a session its body names, names that session, which is so on both sides;
// a node whose made_by names only a workload states none, and that workload is on neither.
var reasoningAuthored = currentAuthors(`(` + kindIn("held.kind", []string{graphstore.KindLens, graphstore.KindRisk, graphstore.KindConstraint}) +
	` or (held.kind = '` + refs.BlockNodeKind + `' and ` + graphstore.FindingIDSQL("held.id") + `))`)

// goneRunsAuthored is the author every run whose row is gone states now - one the sweep stated gone
// and removed the rows of an hour after it was asked (api's sweepGoingRequests), or one a process
// of the release before deleted an hour old during a roll, whose interaction the sweep or the
// backfill stated gone (backfillGoneRuns) - in the shape of reasoningAuthored. The row that named
// the author went with the run, so the store's own record of the writer stands on the row side, as
// it does for a reasoning node: an author's node is history and stays, and without this a person
// whose one write was such a run would read as a node no row projects.
var goneRunsAuthored = currentAuthors(`held.kind = '` + graphstore.KindInteraction + `' and held.state = '` + graphstore.StateGone + `'`)

// recordedSessions are the sessions a row of the store names or an event of the log has named,
// each once, with the first event that named it (null for a session only a row names) and the
// earliest time a row or an event names it. They are every write R14 records a session from: a
// comment a session wrote (graphstore.AuthorOf), as its row names it (commentWriters); every
// session a delivery reached - a comment mention's and a targeted message's attempt alike,
// whether the outbox's record states it (outbox.deliveredGraph) or the attempt's own settle does
// (graphstore.QueueReached), and a fallback copy the outbox published to a session's own topic
// (outbox.fallbackDelivered); an ask a session opened, every session that follows an ask now, and
// every session an
// ask.follower_added event ever named (asks.Follow), so a follower added and removed before the
// store existed is held to a node as well. A node is history and never goes, so a session that
// once followed still owes it. The same holds of a session that asked: every session an
// ask.opened event named as the ask's author is read too, so a session that asked still owes its
// node whatever becomes of the ask's row, and the log keeps addressing it. And every session that
// wrote a decision, a reading or an ask the store names an author of (storedAuthors).
//
// A comment mention's attempt is read twice for the same reason: as its row, and as the
// comment.delivery receipt its settle appends in the transaction that records the session
// (api's completeCommentDelivery), from which the outbox records that session as well
// (outbox.mentionReached). The attempt's row cannot exist without its comment, and the receipt
// keeps its state and its session id in the log whatever becomes of either, so a session only a
// mention reached stays held to that receipt. A fallback copy's row carries no foreign key to its
// comment, and it holds its session as the attempt's receipt does.
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
// The author of a run going_requests holds is a session here where the row's author is one, and
// the author of a reasoning node, or of a run whose row is gone, where its authored_by names one
// (storedAuthors).
//
// It is one definition read by two: the census's session row side and the backfill's session
// pass (backfillSessions), so the sessions the backfill gives a node are exactly the sessions the
// census holds to one, as addressedRoles is for roles. authors is the relation storedAuthors is
// read as: the census binds what it read once (censusAuthors), the backfill reads it in its own
// statement (inlineAuthors).
func recordedSessions(authors string) string {
	return `
	select id, min(event) as event, min(at) as at from (
		select written.actor->>'id' as id, null::bigint as event, written.at
		from (` + commentWriters + `) written
		where written.actor->>'kind' = 'session' and coalesce(written.actor->>'id', '') <> ''
		union all
		select r.author->>'id', null, r.created_at from going_requests r
		where r.author->>'kind' = 'session' and coalesce(r.author->>'id', '') <> ''
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
// gone; a row whose shared fields disagree with its node's; and a node the store holds present
// that no row projects, named with its state.
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
// it runs on a database this build's migrations have not reached, where the census reads no kind
// and says the store is absent (storeAbsence).
//
// declined is the restore's test of which returns it declines (ReturnDeclined), which the
// dangling-citation counter reads its returning set through: nil where the caller has none, and
// then a census whose restore owes a return of the main shape ends in ErrNoReturnDeclined.
func StoreCensus(ctx context.Context, databaseURL string, sizing bool, declined ReturnDeclined) (*StoreReport, error) {
	return storeCensus(ctx, databaseURL, sizing, declined, nil)
}

// storeCensus is StoreCensus with afterKind, which runs once each kind has been read and before
// anything after it is, on the census's own context, and whose error ends the census. Only a
// test passes one: it is how a test commits a write between two of the census's reads and asks
// whether the reads after it still answer the snapshot the first one took. Nil runs nothing.
func storeCensus(ctx context.Context, databaseURL string, sizing bool, declined ReturnDeclined, afterKind func(ctx context.Context, kind string) error) (*StoreReport, error) {
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
	// No statement of the census is JIT-compiled. Each kind is one statement of a few dozen
	// subplans that runs once, and the planner costs one past jit_inline_above_cost and
	// jit_optimize_above_cost as soon as a table's statistics count its rows: a Postgres built
	// with JIT then spends seconds compiling and optimizing each statement before it reads a
	// row, more than the statement itself takes to run.
	if _, err := tx.Exec(ctx, `set local jit = off`); err != nil {
		return nil, fmt.Errorf("turn JIT off for the census snapshot: %w", err)
	}
	report := &StoreReport{}
	// The store is looked up before any kind is read, so a database this build's migrations have
	// not reached answers that rather than failing at the first statement that names a table or a
	// column one of them adds.
	if report.AbsentBecause, err = storeAbsence(ctx, tx); err != nil {
		return nil, err
	}
	report.Absent = report.AbsentBecause != ""
	if !report.Absent {
		// The highest seq is one probe of graph_tuples' primary key from its end, and reading it
		// in this transaction is what makes it the sequence of the snapshot the kinds are read in
		// (ReadSeq).
		if err := tx.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&report.ReadSeq); err != nil {
			return nil, fmt.Errorf("read the census's read sequence: %w", err)
		}
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
		if report.Dangling, err = danglingCitationsOver(ctx, tx, work, declined); err != nil {
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

// storeAbsence says why the database tx reads is no store this build can census, "" where it is
// one. Every read of a kind is written against the schema this build's migrations leave, so the
// census reads the store only over a database that records every migration this binary embeds
// (store.UnappliedMigrations): the release before this one, or its parent's, may hold a store of
// its own - graph_nodes and the rest - without the tables and columns a later migration adds, and
// a census taken there before the rollout migrates it would fail at the first statement that names
// one. graph_nodes missing is said first, since then no store exists at all.
func storeAbsence(ctx context.Context, tx pgx.Tx) (string, error) {
	var missing bool
	if err := tx.QueryRow(ctx, `select to_regclass('graph_nodes') is null`).Scan(&missing); err != nil {
		return "", fmt.Errorf("look up the store: %w", err)
	}
	if missing {
		return "graph_nodes does not exist", nil
	}
	unapplied, err := store.UnappliedMigrations(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("read which of this build's migrations the database has applied: %w", err)
	}
	switch len(unapplied) {
	case 0:
		return "", nil
	case 1:
		return fmt.Sprintf("this build's migration %d is not applied", unapplied[0]), nil
	default:
		return fmt.Sprintf("this build's migration %d is not applied, nor %d after it", unapplied[0], len(unapplied)-1), nil
	}
}

// currentOutOfNodes is two items of a `with recursive` list: name, every relation of the tuple
// kinds kinds out of a node of a kind of nearKinds that the store states now - the newest tuple of
// each, kept while it asserts (graphstore.CurrentTupleRead) - as (from_kind, from_id, kind,
// to_kind, to_id), and name_near, the nodes it reads them out of (relationNodes). It reads each
// such node's tuples of kinds through one probe of graph_tuples_current on (from_kind, from_id,
// kind), stating that index's predicate, which every kind of kinds meets, since each is a relation
// and none is delivered_to.
func currentOutOfNodes(name string, nearKinds, kinds []string) string {
	return relationNodes(name, nearKinds) + `,
	` + name + ` as (
		select held.from_kind, held.from_id, held.kind, held.to_kind, held.to_id
		from ` + name + `_near near
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`,
		`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = near.from_kind and t.from_id = near.from_id
			  and `+kindIn("t.kind", kinds)+` and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held
		where held.asserted
	)`
}

// statedOutOfNodes is currentOutOfNodes' two items for a read of what the log ever stated: name is
// every node of a kind of nearKinds that ever stated a relation of the tuple kinds kinds, asserted
// now or retracted since, once each, as (from_kind, from_id), and name_near the nodes it reads them
// out of (relationNodes). It probes each node once on graph_tuples_current's (from_kind, from_id,
// kind), as currentOutOfNodes does, and keeps the node where any tuple of kinds out of it stands.
// The probe is a lateral read that ends in `limit 1`, which the planner cannot pull up into the
// join: as an `exists` it is planned as a semi-join that reads every tuple of kinds, keyed by the
// kind alone, however few nodes the walk reaches.
func statedOutOfNodes(name string, nearKinds, kinds []string) string {
	return relationNodes(name, nearKinds) + `,
	` + name + ` as (
		select near.from_kind, near.from_id
		from ` + name + `_near near
		cross join lateral (
			select 1 from graph_tuples t
			where t.from_kind = near.from_kind and t.from_id = near.from_id
			  and ` + kindIn("t.kind", kinds) + ` and t.to_kind <> '` + graphstore.KindValue + `'
			limit 1
		) stated
	)`
}

// relationNodes is the item name_near of a `with recursive` list: every node of a kind of
// nearKinds that states a relation, once each, as (from_kind, from_id), which currentOutOfNodes
// and statedOutOfNodes read their tuples out of. No index of the store's set
// (20261109013400_graph_store.up.sql) leads with a tuple's kind, so a read of every tuple of a
// kind starts from the nodes the kind leaves: the walk takes one probe of graph_tuples_current per
// node on its leading (from_kind, from_id) for the next from_id past the last one. It reads the
// nodes off the log rather than off graph_nodes: a writer states these kinds without stating the
// node they leave (docs' writeFacts states a moved_to out of a heading's address and a removed_by
// out of an ask's node and writes neither node), so a tuple whose near end has no node is read
// all the same. The probe states graph_tuples_current's predicate.
func relationNodes(name string, nearKinds []string) string {
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
	)`
}

// storeOnlyRead counts, in one statement, every relation of the three kinds no table projects
// that the store states now - the newest tuple of each (near end, far end), kept while it asserts
// - and those of them whose far end does not resolve: a copied_from (a copy's block to the ask it
// was copied from, graphstore.TupleCopiedFrom) resolves to an ask node that is not gone; a
// moved_to (graphstore.TupleMovedTo) to a block that stands, which is one a document_blocks row
// holds, reached at once or through more moved_to tuples, a cycle reaching none, or to an ask
// node that is not gone, as a copied_from does, which ends a chain (the heading of a section that
// cites an answered ask a person removed from its spec); and a removed_by
// (graphstore.TupleRemovedBy, an ask's node to the person who took its block out) to a person node
// of the store. A far end is followed through one tuple fewer than graphstore.MovedToHops, since
// the tuple that names it is the first of the chain a citation of its near end is read through
// (graphstore.MovedTo), so a far end that resolves here is one every resolver reaches from that
// near end. A moved_to from a document's address - the pre-mortem conversion's, from the document
// it retires to the lens it wrote in its place - resolves to that lens where it is not gone, as
// graphstore.MovedTo reads it, and moves no further.
//
// A moved_to whose chain reaches no block that stands but reaches an address in a document that
// still owes its block index (document_block_versions.version null, as typedNodeOwed reads it) is
// left out, a class of its own and not an unresolved far end: such a document holds no row yet for
// a block its latest version holds - one past the deferral bound, or one a build before wrote
// during the roll - and the block-index reconciliation writes it.
//
// The read takes the three kinds' tuples out of every block, ask and document node the log holds
// (currentOutOfNodes), the near ends their writers state them out of - a copied_from and a
// moved_to out of a block, a moved_to out of a document's address, a removed_by out of an ask's
// node - through graph_tuples_current, one probe per node, and a store that holds none of them
// reads three zero counts. It states that index's to_kind predicate, which leaves out no tuple of
// any of them, since none of their far ends is a value.
var storeOnlyRead = `
	with recursive ` + currentOutOfNodes("stated", []string{refs.BlockNodeKind, graphstore.KindAsk, graphstore.KindArtifact},
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
			  and ask.state <> '` + graphstore.StateGone + `')
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
				  and ask.state <> '` + graphstore.StateGone + `')
			when '` + graphstore.TupleRemovedBy + `' then s.to_kind = '` + graphstore.KindPerson + `' and exists (
				select 1 from graph_nodes person where person.kind = '` + graphstore.KindPerson + `' and person.id = s.to_id)
			else (s.to_kind in ('` + refs.BlockNodeKind + `', '` + graphstore.KindAsk + `')
			  and exists (select 1 from standing where standing.start = s.to_id))
			  or (s.from_kind = '` + graphstore.KindArtifact + `' and s.to_kind = '` + graphstore.KindLens + `' and exists (
				select 1 from graph_nodes lens where lens.kind = '` + graphstore.KindLens + `' and lens.id = s.to_id
				  and lens.state <> '` + graphstore.StateGone + `'))
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
	// The Go readers come after the projections: Keeps, whose decisions' words only Go renders;
	// review carries, whose legacy reasons Go parses; and last the head projection, over every
	// node kind, once every projection of the nodes its rows read has been read.
	for _, read := range []func(context.Context, pgx.Tx) (KindReport, error){CensusOfKeepSums, censusOfReviewCarries, CensusOfHeads} {
		kind, err := read(ctx, tx)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
		if afterKind != nil {
			if err := afterKind(ctx, kind.Kind); err != nil {
				return nil, err
			}
		}
	}
	return kinds, nil
}

// censusOfKind reads one kind: how many rows and present nodes there are, and the differences in
// each direction with the first reportedDifferences of them. args are what the kind's rows bind,
// none but for a kind that binds its names (projection.names).
//
// The four counts are four scalar subqueries of one statement rather than four round trips, so
// a census costs one read a kind here and not four. Each subquery is the statement it was on its
// own, so each is planned and streamed exactly as before.
func censusOfKind(ctx context.Context, tx pgx.Tx, held projection, args []any) (KindReport, error) {
	kind := KindReport{Kind: held.name, NodeKind: held.nodeKind, Table: held.table, Note: held.note,
		Triples: held.tuples != "", CountsUnmarked: held.unmarked != "", CountsUnstated: held.unstated != "",
		EmptyAgrees: held.emptyAgrees, MayBeEmpty: held.mayBeEmpty}
	read := sidesOf(held)
	// pastBound, unmarked and unstated are each a scalar `select count(*)`, so each is read
	// directly rather than wrapped in a count of its own row; a kind without the class reads zero.
	pastBound, unmarked, unstated := held.pastBound, held.unmarked, held.unstated
	if pastBound == "" {
		pastBound = "select 0"
	}
	if unmarked == "" {
		unmarked = "select 0"
	}
	if unstated == "" {
		unstated = "select 0"
	}
	if err := tx.QueryRow(ctx, `select
		(select count(*) from (`+held.rows+`) rows_read),
		(select count(*) from (`+read.nodes+`) nodes_read),
		(select count(*) from (`+read.rowsFound+`) rows_found),
		(select count(*) from (`+read.nodesFound+`) nodes_found),
		(`+pastBound+`),
		(`+unmarked+`),
		(`+unstated+`)`, args...,
	).Scan(&kind.Rows, &kind.Nodes, &kind.RowDifferences, &kind.NodeDifferences, &kind.PastBound, &kind.Unmarked, &kind.Unstated); err != nil {
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
// digest, which nodeDifferences names a node with. Each node's state is one probe of graph_nodes'
// primary key: the lateral read ends in `limit 1`, so the planner cannot make it a join of the
// kind's nodes with themselves, which statistics counting one node of a kind that holds thousands
// plan as a nested loop over every node of the kind for each node.
func presentNodes(held projection) string {
	return `select present.kind, node.id, node.digest, present.state from (` + held.nodes + `) node (id, digest)
		cross join lateral (
			select n.kind, n.state from graph_nodes n
			where n.kind = '` + held.nodeKind + `' and n.id = node.id and n.state <> '` + graphstore.StateGone + `'
			limit 1
		) present`
}

// rowDifferences reads the row side against the store: every row that names no node, names one
// the store does not hold, names one it holds gone, or disagrees with its node's digest, but a row
// whose deletion is owed by a writer that has not run yet (projection.rowOwed). The gone test is
// its own cause rather than a missing node, because a row projecting a node the store has ended is
// a different defect from a row projecting nothing, and it is the one test of a node's state
// here: a row may project a node at any other state.
//
// The two sides meet in one sorted pass, never in a join: both are laid end to end (union all),
// each tagged with its side, and each row reads the digests of the present nodes of its own
// address from a window over (node kind, address), and is read once against each of them, or once
// against none, as a left join of the two reads it. A join of the two is planned from each side's
// estimated rows, and where the statistics count one row of a side that holds thousands - a table
// a bulk write has just filled, before autovacuum reads it again - the planner joins them in a
// nested loop that reads one whole side, with each node's digest, once for each row of the other:
// the census of a document of 2,002 blocks took 108 s in one kind and 18 s in another. The node a
// row names, at any state, is one probe of graph_nodes' primary key per row: the lateral read ends
// in `limit 1`, so the planner cannot pull it up into a join.
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
		from (
			select side.row_side, side.row_label, side.node_kind, side.node_address, side.digest,
			       array_agg(side.node_digest) filter (where not side.row_side)
			           over (partition by side.node_kind, side.node_address) as present_digests
			from (
				select true, row_label, node_kind, node_address, digest, null
				from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
				union all
				select false, null, kind, id, null, digest
				from (` + presentNodes(held) + `) present (kind, id, digest, state)
			) side (row_side, row_label, node_kind, node_address, digest, node_digest)
		) row
		left join lateral unnest(row.present_digests) present (digest) on true
		left join lateral (
			select * from graph_nodes n where n.kind = row.node_kind and n.id = row.node_address limit 1
		) node on true
		where row.row_side
		  and (row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state = '` + graphstore.StateGone + `'
		   or row.digest is distinct from present.digest)` + rowNotOwed(held) + `
		order by 2, 1`
}

// nodeDifferences reads the store against the row side: every node of the kind not gone that no
// row projects, named with the state it is at, so a live one reads as `a live node no row
// projects`, but a node whose row is owed by a writer that has not run yet (projection.owed).
//
// A node is projected where a row of its address stands in its window, the one sorted pass of the
// two sides rowDifferences makes, which answers what `not exists (... where row.node_kind =
// node.kind and row.node_address = node.id)` answers without a join: a present node's kind and id
// are never null, and a row of a null kind or address shares a window with no node, as `=` matches
// no null.
func nodeDifferences(held projection) string {
	return `
		select case when node.state ~ '^[aeiou]' then 'an ' else 'a ' end || node.state
		       || ' node no row projects',
		       node.kind || ' ' || node.id
		from (
			select side.row_side, side.kind, side.id, side.state,
			       bool_or(side.row_side) over (partition by side.kind, side.id) as projected
			from (
				select false, kind, id, state
				from (` + presentNodes(held) + `) present (kind, id, digest, state)
				union all
				select true, node_kind, node_address, null
				from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
			) side (row_side, kind, id, state)
		) node
		where not node.row_side and not node.projected` + notOwed(held) + `
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

// rowNotOwed is the clause a row-side read of held appends to leave out a row whose deletion is
// owed (projection.rowOwed), empty for a kind every writer of whose rows deletes them with their
// node. The predicate reads the row's node, joined as node, whose columns are null where the store
// holds none, so a row it does not hold of is kept.
func rowNotOwed(held projection) string {
	if held.rowOwed == "" {
		return ""
	}
	return `
		  and (` + held.rowOwed + `) is not true`
}

// tripleRowDifferences reads the rows of a kind held triple by triple against the store: every
// row that names no node, names one the store does not hold or holds gone, or states a triple no
// tuple of the kind states now, each at the row and the triple it states, but a row whose deletion
// is owed by a writer that has not run yet (projection.rowOwed). As rowDifferences, it tests no
// state of a node but gone: a row may project a node at any other state.
//
// The rows meet the tuples as rowDifferences meets its two sides, in one sorted pass and never in
// a join: a row is stated where a tuple of its triple, (node kind, address, kind, far end), stands
// in its window. A row with a null anywhere in its triple is stated by no tuple, as `=` matches no
// null, so it is a difference whatever its window holds. The node it names is one primary-key
// probe, as in rowDifferences.
func tripleRowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state = '` + graphstore.StateGone + `'
				then 'the row names a node the store holds ' || node.state
			else 'a row no tuple states'
		end,
		row.row_label || ' ' || row.kind || ' -> ' || row.to_kind || ' ' || row.to_id
		from (
			select side.row_side, side.row_label, side.node_kind, side.node_address, side.kind, side.to_kind, side.to_id,
			       bool_or(not side.row_side) over (
			           partition by side.node_kind, side.node_address, side.kind, side.to_kind, side.to_id) as stated
			from (
				select true, row_label, node_kind, node_address, kind, to_kind, to_id
				from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
				union all
				select false, null, from_kind, from_id, kind, to_kind, to_id
				from (` + held.tuples + `) stated (from_kind, from_id, kind, to_kind, to_id)
			) side (row_side, row_label, node_kind, node_address, kind, to_kind, to_id)
		) row
		left join lateral (
			select * from graph_nodes n where n.kind = row.node_kind and n.id = row.node_address limit 1
		) node on true
		where row.row_side
		  and (row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state = '` + graphstore.StateGone + `'
		   or row.kind is null or row.to_kind is null or row.to_id is null or not row.stated)` + rowNotOwed(held) + `
		order by 2, 1`
}

// tupleDifferences reads the store's tuples of a kind held triple by triple against its rows:
// every tuple no row states the triple of, at from_kind:from_id kind -> to_kind:to_id, but a tuple
// of a node whose row is owed by a writer that has not run yet (projection.owed).
//
// A tuple is projected where a row of its triple stands in its window, the one sorted pass of the
// two sides tripleRowDifferences makes; a tuple with a null anywhere in its triple is projected by
// no row, as `=` matches no null.
func tupleDifferences(held projection) string {
	return `
		select 'a tuple no row projects',
		       node.kind || ':' || node.id || ' ' || node.tie || ' -> ' || node.to_kind || ':' || node.to_id
		from (
			select side.row_side, side.kind, side.id, side.tie, side.to_kind, side.to_id,
			       bool_or(side.row_side) over (
			           partition by side.kind, side.id, side.tie, side.to_kind, side.to_id) as projected
			from (
				select false, kind, id, tie, to_kind, to_id
				from (` + held.tuples + `) node (kind, id, tie, to_kind, to_id)
				union all
				select true, node_kind, node_address, kind, to_kind, to_id
				from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
			) side (row_side, kind, id, tie, to_kind, to_id)
		) node
		where not node.row_side
		  and (node.kind is null or node.id is null or node.tie is null or node.to_kind is null or node.to_id is null
		   or not node.projected)` + notOwed(held) + `
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
		fmt.Fprintf(w, "census: store absent (%s): the database is not migrated to this build, so no kind was read\n", r.AbsentBecause)
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
		if kind.CountsUnstated {
			fmt.Fprintf(w, "census: store kind=%s unstated_rows=%s (rows of a document of an issue that has not converted which no pass of this build's rule has stated, as the release before leaves them: counted, compared with nothing)\n",
				kind.Kind, pgmigrate.Thousands(kind.Unstated))
		}
		if kind.CountsOwed {
			fmt.Fprintf(w, "census: store kind=%s owed_rows=%s (rows a pass of the backfill still owes a write, which it makes on its next run: counted, no difference)\n",
				kind.Kind, pgmigrate.Thousands(kind.Owed))
		}
		switch {
		case kind.EmptyAgrees && kind.Empty():
			fmt.Fprintf(w, "census: store kind=%s holds no row and no %s: nothing of it is written yet, which is no refusal\n", kind.Kind, side)
		case kind.Rows == 0 && kind.MayBeEmpty:
			fmt.Fprintf(w, "census: store kind=%s checked no row, so the census proves nothing of it; a deployment may hold none, so it refuses nothing\n", kind.Kind)
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
		// kept for one release: decisions-documents-fn
		if r.Decisions != nil {
			writeDecisions(w, *r.Decisions)
		}
		if r.VersionRenders != nil {
			writeVersionRenders(w, *r.VersionRenders)
		}
	}
	writeProjectCensus(w, r.Projects)
	for _, row := range r.ProjectIncomplete {
		fmt.Fprintf(w, "census: project incomplete %s\n", row)
	}
	if !r.Absent {
		// read_seq is printed whole, without Thousands' separators: it is no count but a position
		// in graph_tuples, and a reader holds it against graph_tuples.seq as a statement writes it.
		fmt.Fprintf(w, "census: store rows_checked=%s differences=%s read_seq=%d\n",
			pgmigrate.Thousands(r.Rows()), pgmigrate.Thousands(r.Differences()), r.ReadSeq)
	}
	if r.Sizing != nil {
		r.Sizing.Write(w)
	}
	empty := r.Unpopulated()
	switch {
	case r.Absent && r.Sizing == nil:
		fmt.Fprintln(w, "census: store REFUSED (the database holds no store of this build, so the census read nothing)")
	case r.Absent:
		fmt.Fprintln(w, "census: store absent, sizing ok")
	case r.Differences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s difference(s))\n", pgmigrate.Thousands(r.Differences()))
	case r.renderDifferences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s rendered version(s) store other markdown than their render)\n",
			pgmigrate.Thousands(r.renderDifferences()))
	case r.ProjectFindings() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s row(s) whose node names no project or the wrong one, or node(s) no row stands behind;"+
			" run envoy-dispatch backfill-graph-store for the missing, gone and unscoped ones but those naming a party the store"+
			" refuses, and compare each differs row with its node; an orphaned node has no in-band cure, unless an older build's removal made it during a two-build roll,"+
			" which this build's block-index tick then states gone)\n",
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
