package graphcensus

import (
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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
