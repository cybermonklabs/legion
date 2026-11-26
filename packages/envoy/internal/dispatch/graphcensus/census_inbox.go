package graphcensus

import (
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The census of the one inbox (R6, R9): every projection the one inbox read uses, held
// to the store the way the kinds of census.go are, with the store on the expected side. Each kind
// is held triple by triple (projection.tuples) but the two attempt tables, whose node carries
// values as well as relations and is held digest against digest. Each reports a row no tuple
// states and a tuple no row projects; each kind of a table that holds a marker counts the rows a
// build of the release before this one wrote and the backfill has not marked yet as a class of
// its own (unmarked), which reads zero once the backfill ends; published_destinations, which holds
// no marker, counts as that class the events an older build's outbox recorded that the queue of
// migration 20261122100122 still holds (events_published_owed), which reads zero once the
// block-index tick has taken them, and user_threads_version, which reads the marked user_threads
// rows alone, counts none; each reports its marked rows as two populations (populations), the rows
// that release wrote and the backfill marked and the rows this release's writers wrote; and each
// counts the tuples its projection does not hold by design as figures of their own (narrowings).
//
// How a kind tells its two populations apart:
//
//   - a table whose rows record their own creation (messages, comments, the two attempt tables,
//     the three block tables and the three link tables, and the event log for
//     events.published_destinations): a row written before this release's first migration was
//     applied (inboxRelease) is the release before's, and any other is this release's. On a
//     deployment a pod of the release before that writes during the roll writes after that moment
//     and is counted in this release's population; c-census writes its first half before this
//     build boots, so there the split is exact.
//   - the five read-state tables other than user_threads, which record no moment of creation:
//     the backfill states a row's mark at the sentinel (graphstore.NoEvent), since no event can be
//     told to have written it, and every route of this release states it under the event the
//     route appends (api/read_marks.go). So a marked row whose mark tuple stands at the sentinel is
//     the release before's.
//   - user_threads: the backfill states a row's read_through before it marks the row, so at or
//     below marked_seq, and the one writer of a thread states the read_through of a row it begins
//     after that row's insert, so above it (inboxwrite.Record's queueKeepRow). A marked row whose
//     person's newest read_through into the thread at or below marked_seq asserts is the release
//     before's.
//
// A population of none refuses only where the caller says the corpus holds both releases
// (StoreReport.RequireBothReleases, `census --store --both-releases`, which c-census passes): a
// database this release was first deployed on holds no row of the release before, and a gate on
// every run that refused it for that would refuse it for good.
//
// comment_mentions is held to the mentions tuples its marked rows project (commentMentionProjection,
// migration 20261120090000), which this release's comment write states beside each row.
var inboxProjections = append(append(append([]projection{
	messageProjection,
	commentPointerProjection,
	commentMentionProjection,
}, itemProjections()...),
	attemptProjection(commentAttemptCensus), attemptProjection(messageAttemptCensus),
	publishedDestinationsProjection,
	userThreadsProjection, userThreadsVersionProjection),
	readStateProjections()...)

// inboxReleaseVersion is this release's first migration (20261116102759, the attributes of a
// delivery attempt), and inboxRelease the moment it was applied to the database: the moment this
// release's schema reached it, which the populations of a table recording its rows' creation are
// split at. storeRelease is the same of the store's own first migration (20261109013400): an event
// the outbox recorded before it was published to destinations no tuple could record yet.
const (
	inboxReleaseVersion = "20261116102759"
	storeReleaseVersion = "20261109013400"
)

var (
	inboxRelease = `(select applied.applied_at from schema_migrations applied where applied.version = ` + inboxReleaseVersion + `)`
	storeRelease = `(select applied.applied_at from schema_migrations applied where applied.version = ` + storeReleaseVersion + `)`
)

// createdPopulations is the populations statement of a table whose rows record their creation in
// created (an SQL expression over the rows from names): the marked rows written before
// inboxRelease, and the others.
func createdPopulations(from, created, marked string) string {
	return `
		select count(*) filter (where ` + created + ` < ` + inboxRelease + `),
		       count(*) filter (where ` + inboxRelease + ` is null or ` + created + ` >= ` + inboxRelease + `)
		from ` + from + `
		where ` + marked
}

// moment renders a time as the census compares it on both sides: UTC, to the microsecond Postgres
// keeps, so a mark's moment on its row and on its tuple read alike and read out in a difference.
func moment(expr string) string {
	return `to_char((` + expr + `) at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`
}

// authorEnd is the node graphstore.AuthorOf reads off the actor column holds (actorAuthor), as the
// kind and the id of the far end of the authored_by tuple graphstore.AuthorFacts states: the
// canonical login of a user, a session, or the canonical login of a personal token's owner; both
// null for an actor that names nobody, and the id null past the store's bound, of which nothing is
// stated.
func authorEnd(column string) (kind, id string) {
	author := `(` + actorAuthor(column) + `)`
	named := author + `[2]`
	return author + `[1]`, `case when ` + graphstore.AddressFitsSQL(named) + ` then ` + named + ` end`
}

// routeEnd is the session or role a route column names as model.ParseRoute reads it
// (graphstore.RouteSQL), as kind and id, both null for a route ParseRoute does not read and the id
// null past the store's bound.
func routeEnd(route string) (kind, id string) {
	role, session := graphstore.RouteSQL(route)
	kind = `case when ` + role + ` is not null then '` + graphstore.KindRole + `' when ` + session + ` is not null then '` +
		graphstore.KindSession + `' end`
	named := `coalesce(` + role + `, ` + session + `)`
	return kind, `case when ` + graphstore.AddressFitsSQL(named) + ` then ` + named + ` end`
}

// liveHeld is the store side of a kind held triple by triple whose holders are the live nodes of
// one kind that holders selects (over n): every relation such a node states now under the tuple
// kinds kinds selects (over t), read as a current relation is (graphstore.CurrentTupleRead), as
// (from_kind, from_id, kind, to_kind, to_id), to_id rendered by render (over held). extra narrows
// the relations (over n and held).
func liveHeld(holders, kinds, render, extra string) string {
	if extra == "" {
		extra = "true"
	}
	return `
		select n.kind, n.id, held.kind, held.to_kind, ` + render + `
		from graph_nodes n
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.at, t.source_event, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held
		where ` + holders + ` and n.state = '` + graphstore.StateLive + `' and held.asserted and ` + extra
}

// --- the messages node ------------------------------------------------------------------------

// agentPage is how both sides of the message kind write the page a direct message hangs from. The
// writer reads that page off the store when it writes the message (graphstore.AgentPage): the
// role's own page, or the page of the role a session claimed then, or the session's own, and no
// row records which. So the part_of of a message on no issue that answers nothing is held by its
// kind, a page, and the page itself by no row.
const agentPage = `'(the page of the agent the message was sent to)'`

var messageProjection = func() projection {
	authorKind, authorID := authorEnd(`m.author`)
	routeKind, routeID := routeEnd(`m.target`)
	placeKind := `case when m.in_reply_to is not null then '` + graphstore.KindMessage + `'
		              when m.replies_to_block is not null then '` + graphstore.KindBlock + `'
		              when m.issue_key is not null then '` + graphstore.KindIssue + `'
		              when m.target is not null then '` + graphstore.KindPage + `' end`
	placeID := `case when m.in_reply_to is not null then m.in_reply_to::text
		            when m.replies_to_block is not null then m.replies_to_block
		            when m.issue_key is not null then case when ` + graphstore.AddressFitsSQL(`m.issue_key`) + ` then m.issue_key end
		            when ` + routeID + ` is not null and ` + graphstore.AddressFitsSQL(`'session/' || `+routeID) + ` then ` + agentPage + ` end`
	return projection{
		name: graphstore.KindMessage, nodeKind: graphstore.KindMessage, table: "messages",
		note: "the messages node: the row side is every messages row naming its node, as the triples of that node and" +
			" replies_to the message in_reply_to and the block replies_to_block name, part_of what it answers or the" +
			" issue it was posted on or the page of the agent it was sent to (held by its kind, since no row records" +
			" which page), addressed_to the session or role its target routes to, authored_by who wrote it and" +
			" broadcast_id its broadcast, against those relations and that value every live message node states now",
		rows: `
		select 'message ' || m.id::text, m.node_kind, m.node_address, stated.kind, stated.to_kind, stated.to_id
		from messages m
		cross join lateral (values
			('` + graphstore.TupleRepliesTo + `', '` + graphstore.KindMessage + `', m.in_reply_to::text),
			('` + graphstore.TupleRepliesTo + `', '` + graphstore.KindBlock + `', m.replies_to_block),
			('` + graphstore.TuplePartOf + `', ` + placeKind + `, ` + placeID + `),
			('` + graphstore.TupleAddressedTo + `', ` + routeKind + `, ` + routeID + `),
			('` + graphstore.TupleAuthoredBy + `', ` + authorKind + `, ` + authorID + `),
			('` + graphstore.TupleBroadcastID + `', '` + graphstore.KindValue + `', m.broadcast_id::text)
		) stated (kind, to_kind, to_id)
		where m.node_kind is not null and stated.to_id is not null`,
		tuples: liveHeld(`n.kind = '`+graphstore.KindMessage+`'`,
			`t.kind in ('`+graphstore.TupleRepliesTo+`', '`+graphstore.TuplePartOf+`', '`+graphstore.TupleAddressedTo+`', '`+graphstore.TupleAuthoredBy+`')`,
			`case when held.kind = '`+graphstore.TuplePartOf+`' and held.to_kind = '`+graphstore.KindPage+`' then `+agentPage+` else held.to_id end`, "") + `
		union all
		select n.kind, n.id, newest.kind, '` + graphstore.KindValue + `', newest.to_id
		from graph_nodes n
		cross join lateral (
			select t.kind, t.to_id, t.asserted from graph_tuples t
			where t.from_kind = n.kind and t.from_id = n.id and t.to_kind = '` + graphstore.KindValue + `'
			  and t.kind = '` + graphstore.TupleBroadcastID + `'
			order by t.seq desc limit 1
		) newest
		where n.kind = '` + graphstore.KindMessage + `' and n.state = '` + graphstore.StateLive + `' and newest.asserted`,
		pastBound: `
		select count(*) from messages m where not ` + graphstore.AddressFitsSQL(`coalesce(m.replies_to_block, '')`),
		unmarked: `
		select count(*) from messages m where m.node_kind is null and ` + graphstore.AddressFitsSQL(`coalesce(m.replies_to_block, '')`),
		populations: createdPopulations(`messages m`, `m.created_at`, `m.node_kind is not null`),
		emptyAgrees: true,
	}
}()

// --- the comment pointers ---------------------------------------------------------------------

var commentPointerProjection = projection{
	name: "comment_pointers", nodeKind: graphstore.KindComment, table: "comments.reply_to, comments.ask_id, comments.answers, comments.replies_to_block",
	note: "the three comment pointers and the block one: the row side is every comments row whose tuples_stored is set," +
		" as the triples of its node and replies_to the comment reply_to names, replies_to the ask ask_id names," +
		" answers the comment answers names, replies_to the block replies_to_block names, and part_of what it" +
		" answers (graphstore.CommentPointerFacts), against those relations every live comment node states now",
	rows: `
		select 'comment ' || c.id::text, c.node_kind, c.node_address, stated.kind, stated.to_kind, stated.to_id
		from comments c
		cross join lateral (values
			('` + graphstore.TupleRepliesTo + `', '` + graphstore.KindComment + `', c.reply_to::text),
			('` + graphstore.TupleRepliesTo + `', '` + graphstore.KindAsk + `', c.ask_id::text),
			('` + graphstore.TupleAnswers + `', '` + graphstore.KindComment + `', c.answers::text),
			('` + graphstore.TupleRepliesTo + `', '` + graphstore.KindBlock + `', c.replies_to_block),
			('` + graphstore.TuplePartOf + `', case when c.answers is not null or c.reply_to is not null then '` + graphstore.KindComment + `'
			                       when c.ask_id is not null then '` + graphstore.KindAsk + `' end,
			         coalesce(c.answers, c.reply_to, c.ask_id)::text)
		) stated (kind, to_kind, to_id)
		where c.tuples_stored and stated.to_id is not null`,
	tuples: liveHeld(`n.kind = '`+graphstore.KindComment+`'`,
		`t.kind in ('`+graphstore.TupleRepliesTo+`', '`+graphstore.TupleAnswers+`', '`+graphstore.TuplePartOf+`')`, `held.to_id`, ""),
	// A comment left out of the store - an anchor or an owner past the bound - and one whose
	// replies_to_block is past it states no pointer by design (graphstore.CommentPointerFacts).
	pastBound: `
		select count(*) from comments c` + refs.CommentStoreJoins + `
		where not (` + refs.CommentInTheStore + `) or not ` + graphstore.AddressFitsSQL(`coalesce(c.replies_to_block, '')`),
	unmarked: `
		select count(*) from comments c` + refs.CommentStoreJoins + `
		where not c.tuples_stored and ` + refs.CommentInTheStore + ` and ` + graphstore.AddressFitsSQL(`coalesce(c.replies_to_block, '')`),
	populations: createdPopulations(`comments c`, `c.created_at`, `c.tuples_stored`),
	emptyAgrees: true,
}

// --- the comment's mentions --------------------------------------------------------------------

// commentMentionProjection is comment_mentions as the projection of what a comment's write states
// of each target it names as a delivery route (api's commentMentionFacts): a mentions tuple out of
// the comment's node to the target, a second to the session it resolved to, and the comment's
// urgency, its delivery value. The store side is those relations of every live comment node stated
// under the event that caused the node, the comment's own write: a session or a role an edit of the
// comment newly names is a mention of the store too (R5), stated under the edit's event, which no
// row of this table projects, since an edit names no delivery route. A person's mention is none of
// these: it is a delivery to that person, which no comment_mentions row holds.
var commentMentionProjection = func() projection {
	routeKind, routeID := routeEnd(`m.target`)
	return projection{
		name: "comment_mentions", nodeKind: graphstore.KindComment, table: "comment_mentions",
		note: "a comment's delivery routes: the row side is every comment_mentions row whose tuples_stored is set, as" +
			" the triples of its comment's node and mentions the session or role its target routes to, mentions the" +
			" session resolved_session_id names and delivery its mode, against those relations every live comment" +
			" node states under the event that caused it",
		rows: `
		select 'comment_mention ' || m.comment_id::text || ' ' || m.target, c.node_kind, c.node_address,
		       stated.kind, stated.to_kind, stated.to_id
		from comment_mentions m
		join comments c on c.id = m.comment_id
		cross join lateral (values
			('` + graphstore.TupleMentions + `', ` + routeKind + `, ` + routeID + `),
			('` + graphstore.TupleMentions + `', '` + graphstore.KindSession + `',
			 case when ` + graphstore.AddressFitsSQL(`m.resolved_session_id`) + ` then m.resolved_session_id end),
			('` + graphstore.TupleDelivery + `', '` + graphstore.KindValue + `', m.delivery)
		) stated (kind, to_kind, to_id)
		where m.tuples_stored and stated.to_id is not null`,
		tuples: liveHeld(`n.kind = '`+graphstore.KindComment+`'`,
			`t.kind = '`+graphstore.TupleMentions+`' and t.to_kind in ('`+graphstore.KindSession+`', '`+graphstore.KindRole+`')`,
			`held.to_id`, `held.source_event = n.source_event`) + `
		union all
		select n.kind, n.id, newest.kind, '` + graphstore.KindValue + `', newest.to_id
		from graph_nodes n
		cross join lateral (
			select t.kind, t.to_id, t.asserted from graph_tuples t
			where t.from_kind = n.kind and t.from_id = n.id and t.to_kind = '` + graphstore.KindValue + `'
			  and t.kind = '` + graphstore.TupleDelivery + `'
			order by t.seq desc limit 1
		) newest
		where n.kind = '` + graphstore.KindComment + `' and n.state = '` + graphstore.StateLive + `' and newest.asserted`,
		// A mention of a comment the store leaves out, and a target or a session past the bound,
		// states no tuple by design (api's mentionStored).
		pastBound: `
		select count(*) from comment_mentions m join comments c on c.id = m.comment_id` + refs.CommentStoreJoins + `
		where not (` + refs.CommentInTheStore + `) or ` + routeID + ` is null or not ` + graphstore.AddressFitsSQL(`coalesce(m.resolved_session_id, '')`),
		unmarked: `
		select count(*) from comment_mentions m join comments c on c.id = m.comment_id` + refs.CommentStoreJoins + `
		where not m.tuples_stored and ` + refs.CommentInTheStore + ` and ` + routeID + ` is not null and ` + graphstore.AddressFitsSQL(`coalesce(m.resolved_session_id, '')`),
		populations: createdPopulations(`comment_mentions m join comments c on c.id = m.comment_id`, `c.created_at`, `m.tuples_stored`),
		emptyAgrees: true,
	}
}()

// --- an item's blocks and their links ----------------------------------------------------------

// itemProjections are the six block and link tables of an item's body (refs' item index): for each
// of a comment, a message and an ask (inboxItemKinds), its blocks, each the node of one block and
// its part_of the item, and its links, each a tuple out of a block's node under the row's own kind.
func itemProjections() []projection {
	var kinds []projection
	for _, item := range inboxItemKinds {
		blocks, links := item.blocks, item.links
		ofItem := `n.kind = '` + graphstore.KindBlock + `' and n.id like '` + item.kind + `/%'`
		kinds = append(kinds, projection{
			name: item.kind + "_block", nodeKind: graphstore.KindBlock, table: blocks,
			note: "the body blocks of each " + item.kind + ": the row side is every " + blocks + " row naming its node at a" +
				" position the item still holds, as the triple of that node, part_of and the " + item.kind + ", against the" +
				" part_of every live block node of each " + item.kind + " states now; a block the item no longer holds is a" +
				" node the store holds gone, counted apart",
			rows: `
		select '` + item.kind + `_block ' || b.node_id, b.node_kind, b.node_address,
		       '` + graphstore.TuplePartOf + `', '` + item.kind + `', b.` + item.column + `::text
		from ` + blocks + ` b
		where b.node_kind is not null and b.position is not null`,
			tuples:      liveHeld(ofItem, `t.kind = '`+graphstore.TuplePartOf+`'`, `held.to_id`, ""),
			pastBound:   `select count(*) from ` + blocks + ` b where not ` + graphstore.AddressFitsSQL(`b.node_id`),
			unmarked:    `select count(*) from ` + blocks + ` b where b.node_kind is null and ` + graphstore.AddressFitsSQL(`b.node_id`),
			populations: createdPopulations(blocks+` b`, `b.created_at`, `b.node_kind is not null`),
			narrowings: []narrowing{{
				name:  "removed_blocks",
				note:  "blocks the item no longer holds, whose rows name nodes the store holds gone and state no part_of a live node holds",
				count: `select count(*) from ` + blocks + ` b where b.node_kind is not null and b.position is null`,
			}},
			emptyAgrees: true,
		}, projection{
			name: item.kind + "_block_ref", nodeKind: graphstore.KindBlock, table: links,
			note: "the links out of the blocks of each " + item.kind + ": the row side is every " + links + " row naming its" +
				" block's node, as the triple of that node, the row's kind and its far end, against every relation but" +
				" part_of a live block node of each " + item.kind + " states now",
			rows: `
		select '` + item.kind + `_block_ref ' || r.from_node, r.node_kind, r.node_address, r.kind, r.to_kind, r.to_id
		from ` + links + ` r
		where r.node_kind is not null`,
			tuples: liveHeld(ofItem, `t.kind <> '`+graphstore.TuplePartOf+`'`, `held.to_id`, ""),
			pastBound: `
		select count(*) from ` + links + ` r where not ` + graphstore.AddressFitsSQL(`r.from_node`) + ` or not ` + graphstore.AddressFitsSQL(`r.to_id`),
			unmarked: `
		select count(*) from ` + links + ` r where r.node_kind is null and ` + graphstore.AddressFitsSQL(`r.from_node`) + ` and ` + graphstore.AddressFitsSQL(`r.to_id`),
			populations: createdPopulations(links+` r`, `r.created_at`, `r.node_kind is not null`),
			emptyAgrees: true,
		})
	}
	return kinds
}

// --- the delivery attempts -----------------------------------------------------------------------

// attemptCensus is one of the two attempt tables as the census reads it: the table as the backfill's
// attempt pass reads it (attemptTable: its name and the node address of a row over d,
// graphstore.AttemptKey.ID), the label of a row, the joins its rows need, the prefix of its nodes'
// addresses, and the facts a row states as graphstore's attemptMoved reads them off the row
// (attempt_rows and attempt_ends), each written kind>to_kind:to_id.
type attemptCensus struct {
	attemptTable
	label, joins, prefix string
	facts                []string
}

// The time values an attempt states: RFC 3339 in UTC as Go writes them, read on both sides as
// moment renders a time, so a value the writer trimmed of its trailing zeros reads alike.
var attemptTimes = []string{graphstore.AttemptClaimedAt, graphstore.AttemptAcceptedAt}

// attemptValue is one value fact of a row: kind>value:<the value>, null where the row holds none.
func attemptValue(kind, value string) string {
	return `'` + kind + `>` + graphstore.KindValue + `:' || ` + value
}

// attemptFacts are the facts every attempt row states alike: its number, its state and whether it
// is a duplicate, always; its envelope and its error where it holds one; its claim's moment; owed_to
// its target while it is pending; and delivered_to its session once the listener took it
// (attemptTaken), which stays stated when the row leaves the taken states, as a session's report
// of an error on an attempt it was sent or queued settles the row failed.
func attemptFacts(target, session string) []string {
	targetKind, targetID := routeEnd(target)
	return []string{
		attemptValue(graphstore.AttemptNumber, `d.attempt::text`),
		attemptValue(graphstore.AttemptState, `d.state`),
		attemptValue(graphstore.AttemptDuplicate, `d.duplicate::text`),
		attemptValue(graphstore.AttemptEnvelopeID, `d.envelope_id`),
		attemptValue(graphstore.AttemptError, `d.error`),
		attemptValue(graphstore.AttemptClaimedAt, moment(`d.claimed_at`)),
		`case when d.state = '` + model.DeliveryStatePending + `' then '` + graphstore.TupleOwedTo + `>' || ` + targetKind + ` || ':' || ` + targetID + ` end`,
		`case when ` + attemptTaken + ` and coalesce(` + session + `, '') <> '' and ` + graphstore.AddressFitsSQL(session) + `
		      then '` + graphstore.TupleDeliveredTo + `>` + graphstore.KindSession + `:' || ` + session + ` end`,
	}
}

// attemptTaken is true of an attempt row d the listener took, now or at any moment the store stated
// it: its state is one the listener took it in (model.DeliveryTakenStatesSQL), or the store's log
// asserts one of those states as a value of its node (graphstore.AttemptState). It is the one rule
// the census holds an attempt's delivered_to to, and it is the writer's own: every writer of an
// attempt's facts - each route through graphstore.QueueAttempted, and the backfill and the
// block-index tick's catch-up through graphstore.AttemptFactsAsWritten - states them in one
// statement (graphstore's attemptMoved), which reads the row once and states the delivered_to with
// the state value whenever the row is in a taken state, and which retracts no delivered_to. So the
// store states a delivered_to exactly where it has stated a taken state, and a log no statement
// deletes from keeps both (TestNoStatementDeletesANodeOrATuple). A row that failed and was never
// taken - a refused send - expects none.
//
// The receipt in the log (a comment.delivery or message.delivery of a taken state) is no exact
// record of it. The backfill states a row a build of the release before wrote as the row stands
// when it runs, so a row that build sent and then settled failed before the backfill took it holds
// a taken receipt and no delivered_to; and the event that stated a delivered_to the backfill wrote
// is its item's event or the sentinel, which tells nothing of the state. The node's state values
// are stated by the same statement as the delivered_to, for every writer alike.
var attemptTaken = `(d.state in ` + model.DeliveryTakenStatesSQL + ` or exists (
		select 1 from graph_tuples taken
		where taken.from_kind = '` + graphstore.KindDeliveryAttempt + `' and taken.from_id = d.node_address
		  and taken.kind = '` + graphstore.AttemptState + `' and taken.to_kind = '` + graphstore.KindValue + `'
		  and taken.to_id in ` + model.DeliveryTakenStatesSQL + ` and taken.asserted))`

var (
	commentAttemptCensus = attemptCensus{
		attemptTable: commentAttempts, prefix: "comment/",
		label: `'comment_delivery ' || d.comment_id::text || ' ' || d.target || ' ' || d.attempt::text`,
		facts: append(attemptFacts(`d.target`, `d.session_id`),
			attemptValue(graphstore.AttemptResolveError, `d.resolve_error`),
			`'`+graphstore.TupleAnsweredBy+`>`+graphstore.KindComment+`:' || d.reply_id::text`),
	}
	messageAttemptCensus = attemptCensus{
		attemptTable: messageAttempts, prefix: "message/",
		label: `'message_delivery ' || d.message_id::text || ' ' || d.attempt::text`,
		joins: ` join messages m on m.id = d.message_id`,
		facts: append(attemptFacts(`m.target`, `nullif(d.session_id, '')`),
			attemptValue(graphstore.AttemptAcceptedAt, moment(`d.accepted_at`)),
			attemptValue(graphstore.AttemptAcceptedAs, `d.accepted_as`),
			`'`+graphstore.TupleAnsweredBy+`>`+graphstore.KindMessage+`:' || d.reply_id::text`),
	}
)

// attemptProjection is one attempt table held digest against digest: a row's digest is every fact
// its row states (attemptCensus.facts), and its node's every fact the node states now - the newest
// value tuple of each value kind while it asserts, and each owed_to, delivered_to and answered_by it
// asserts (graphstore's attemptMoved, as its held_values and held_relations read them) - so a
// value, a relation or a retraction the store lost or kept past its row reads as a row and a node
// that disagree.
func attemptProjection(table attemptCensus) projection {
	values := `'` + strings.Join([]string{graphstore.AttemptNumber, graphstore.AttemptState, graphstore.AttemptEnvelopeID,
		graphstore.AttemptError, graphstore.AttemptResolveError, graphstore.AttemptDuplicate, graphstore.AttemptClaimedAt,
		graphstore.AttemptAcceptedAt, graphstore.AttemptAcceptedAs}, `', '`) + `'`
	relations := `'` + strings.Join([]string{graphstore.TupleOwedTo, graphstore.TupleDeliveredTo, graphstore.TupleAnsweredBy}, `', '`) + `'`
	times := `'` + strings.Join(attemptTimes, `', '`) + `'`
	return projection{
		name: table.table, nodeKind: graphstore.KindDeliveryAttempt, table: table.table,
		note: "one delivery_attempt node per row: the digest is the row's number, state, duplicate flag, envelope, errors" +
			" and moments, owed_to its target while it is pending, delivered_to its session once the listener took it," +
			" which stays once the row leaves the taken states, and answered_by its reply, against the newest value of" +
			" each kind and the relations the node states now",
		rows: `
		select ` + table.label + `, d.node_kind, d.node_address, md5(d.node_address || '|' || ` + statedLinks(table.facts...) + `)
		from ` + table.table + ` d` + table.joins + `
		where d.node_kind is not null`,
		nodes: `
		select n.id, md5(n.id || '|' || (select coalesce(string_agg(held.fact, ',' order by held.fact), '') from (
			select v.kind || '>` + graphstore.KindValue + `:' || case when v.kind in (` + times + `)
			                                  then case when pg_input_is_valid(v.to_id, 'timestamptz') then ` + moment(`v.to_id::timestamptz`) + ` else v.to_id end
			                                  else v.to_id end
			from (
				` + graphstore.CurrentTupleRead(`t.kind`, `t.kind, t.to_id, t.asserted`,
			`t.from_kind = n.kind and t.from_id = n.id and t.to_kind = '`+graphstore.KindValue+`' and t.kind in (`+values+`)`) + `
			) v where v.asserted
			union all
			select r.kind || '>' || r.to_kind || ':' || r.to_id
			from (
				` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
			`t.from_kind = n.kind and t.from_id = n.id and t.to_kind <> '`+graphstore.KindValue+`' and t.kind in (`+relations+`)`) + `
			) r where r.asserted
		) held (fact)))
		from graph_nodes n where n.kind = '` + graphstore.KindDeliveryAttempt + `' and n.id like '` + table.prefix + `%'`,
		pastBound:   `select count(*) from ` + table.table + ` d where not ` + graphstore.AddressFitsSQL(table.address),
		unmarked:    `select count(*) from ` + table.table + ` d where ` + table.owedRow(),
		populations: createdPopulations(table.table+` d`, `d.created_at`, `d.node_kind is not null`),
		emptyAgrees: true,
	}
}

// --- events.published_destinations ------------------------------------------------------------

// The topics of an event's published_destinations that name a node of the store
// (outbox.nodeOfSubject): a session's and a role's.
var (
	agentTopicFrom = strconv.Itoa(len(contracts.AgentTopicPrefix) + 1)
	roleTopicFrom  = strconv.Itoa(len(contracts.RoleTopicPrefix) + 1)
)

// publishedPairs is every (event, topic) of events.published_destinations, with what the outbox's
// record reads of each (outbox.deliveredGraph): the item the event delivered (outbox.deliveredItem,
// the comment or message its payload names, null for an event about anything else), and the
// session or role the topic names (null for an owner's topic, which names no node). Each pair
// answers (event, created_at, topic, item_kind, item_id, reached_kind, reached_id).
var publishedPairs = `
	select e.id as event, e.created_at, topic.name as topic, item.kind as item_kind, item.id as item_id,
	       reached.kind as reached_kind, reached.id as reached_id
	from events e
	cross join lateral (
		select case when e.type like 'comment.%' then '` + graphstore.KindComment + `'
		            when e.type like 'message.%' then '` + graphstore.KindMessage + `' end as kind,
		       case when e.type like 'comment.%' then coalesce(nullif(e.payload->>'id', ''), nullif(e.payload->>'comment_id', ''))
		            when e.type like 'message.%' then coalesce(nullif(e.payload->>'id', ''), nullif(e.payload->>'message_id', '')) end as id
	) item
	cross join lateral unnest(e.published_destinations) topic (name)
	cross join lateral (
		select case when topic.name like '` + contracts.AgentTopicPrefix + `%' then '` + graphstore.KindSession + `'
		            when topic.name like '` + contracts.RoleTopicPrefix + `%' then '` + graphstore.KindRole + `' end as kind,
		       case when topic.name like '` + contracts.AgentTopicPrefix + `%' then substring(topic.name from ` + agentTopicFrom + `)
		            when topic.name like '` + contracts.RoleTopicPrefix + `%' then substring(topic.name from ` + roleTopicFrom + `) end as id
	) reached
	where cardinality(e.published_destinations) > 0`

// publishedOfAStoredItem is true of a pair (over p) whose topic names a node, whose event the
// store existed for and delivered an item of, the item a live node of the store and both ends
// within its bound: the pairs the outbox's record states one delivered_to of.
var publishedOfAStoredItem = `p.reached_kind is not null and coalesce(p.reached_id, '') <> ''
	  and (` + storeRelease + ` is null or p.created_at >= ` + storeRelease + `)
	  and p.item_id is not null and ` + graphstore.AddressFitsSQL(`p.reached_id`) + ` and ` + graphstore.AddressFitsSQL(`p.item_id`) + `
	  and exists (select 1 from graph_nodes subject
	              where subject.kind = p.item_kind and subject.id = p.item_id and subject.state = '` + graphstore.StateLive + `')`

// The deliveries the outbox's record states that no topic of an array names, each projected by a
// row of its own (outbox.deliveredGraph): the delivery of a mention attempt the listener took,
// stated under its comment.delivery receipt (mentionReached), which comment_deliveries' attempt
// projects; and a fallback copy, stated under the comment.created it is a copy of
// (fallbackDelivered), which its comment_fallback_copies row projects. Beside them, the delivery
// of a mention attempt a session's reply flipped to sent, which no receipt follows, so the reply's
// own write states it under the reply's comment.answered (api's queueAnsweredDelivery), and the
// answered attempt's comment_deliveries row, whose reply_id names that reply, projects it. Each is
// a predicate over a delivered_to tuple d and its event e.
var (
	mentionReceiptDelivery = `(e.type = 'comment.delivery' and e.payload->>'state' in ` + model.DeliveryTakenStatesSQL + `
	  and d.from_kind = '` + graphstore.KindComment + `' and d.from_id = e.payload->>'comment_id'
	  and d.to_kind = '` + graphstore.KindSession + `' and d.to_id = btrim(e.payload->>'session_id'))`
	fallbackCopyDelivery = `(e.type = 'comment.created' and d.from_kind = '` + graphstore.KindComment + `' and d.to_kind = '` + graphstore.KindSession + `'
	  and exists (select 1 from comment_fallback_copies f
	              where f.published and f.comment_id::text = d.from_id and f.destination = '` + contracts.AgentTopicPrefix + `' || d.to_id))`
	answeredDelivery = `(e.type = 'comment.answered' and d.from_kind = '` + graphstore.KindComment + `' and d.to_kind = '` + graphstore.KindSession + `'
	  and exists (select 1 from comment_deliveries a
	              where a.comment_id::text = d.from_id and a.reply_id::text = e.payload->>'id'
	                and btrim(a.session_id) = d.to_id and a.state in ` + model.DeliveryTakenStatesSQL + `))`
)

// publishedDeliveries is every delivered_to tuple from a live comment or message node to a session
// or a role, with the event that caused it, as (d, e): the outbox's record of what it reached.
var publishedDeliveries = `
	from graph_tuples d
	join events e on e.id = d.source_event
	where d.kind = '` + graphstore.TupleDeliveredTo + `' and d.to_kind in ('` + graphstore.KindSession + `', '` + graphstore.KindRole + `')
	  and d.from_kind in ('` + graphstore.KindComment + `', '` + graphstore.KindMessage + `') and d.asserted
	  and exists (select 1 from graph_nodes subject
	              where subject.kind = d.from_kind and subject.id = d.from_id and subject.state = '` + graphstore.StateLive + `')`

var publishedDestinationsProjection = projection{
	name: "published_destinations", nodeKind: graphstore.KindComment + ", " + graphstore.KindMessage, table: "events.published_destinations",
	note: "the session half of the outbox's record: the row side is every session and role topic of an event's" +
		" published_destinations whose event delivered a comment or a message the store holds, as the triple of that" +
		" item, delivered_to and the session or role, under that event, against every delivered_to a live comment or" +
		" message node holds into a session or a role, under its event",
	rows: `
		select 'event ' || p.event::text || ' ' || p.topic, p.item_kind, p.item_id,
		       '` + graphstore.TupleDeliveredTo + `', p.reached_kind, p.reached_id || ' under event ' || p.event::text
		from (` + publishedPairs + `) p
		where ` + publishedOfAStoredItem,
	tuples: `
		select d.from_kind, d.from_id, d.kind, d.to_kind, d.to_id || ' under event ' || d.source_event::text
		` + publishedDeliveries + `
		  and (` + storeRelease + ` is null or e.created_at >= ` + storeRelease + `)
		  and not ` + mentionReceiptDelivery + ` and not ` + fallbackCopyDelivery + ` and not ` + answeredDelivery,
	pastBound: `
		select count(*) from (` + publishedPairs + `) p
		where p.reached_kind is not null and (not ` + graphstore.AddressFitsSQL(`coalesce(p.reached_id, '')`) + ` or not ` + graphstore.AddressFitsSQL(`coalesce(p.item_id, '')`) + `)`,
	// An event an older build's outbox recorded since this release's schema reached the database,
	// which the block-index tick has not stated the deliveries of yet, is a row of the queue the
	// tick drains (publishedDeliveriesQueued), which reads zero once the tick has taken it.
	//
	// kept for one release: events-published-owed
	unmarked:    `select count(*) ` + publishedDeliveriesQueued,
	populations: createdPopulations(`(`+publishedPairs+`) p`, `p.created_at`, publishedOfAStoredItem),
	narrowings: []narrowing{{
		name: "owner_topics",
		note: "topics of an issue's, a project's or a document's family, which name no node, so no tuple records who" +
			" subscribed to them",
		count: `select count(*) from (` + publishedPairs + `) p where p.reached_kind is null`,
	}, {
		name: "subjects_without_a_node",
		note: "session and role topics of an event about no comment or message, or about one the store holds no live" +
			" node of, which the outbox records no delivered_to of",
		count: `
		select count(*) from (` + publishedPairs + `) p
		where p.reached_kind is not null and coalesce(p.reached_id, '') <> ''
		  and (` + storeRelease + ` is null or p.created_at >= ` + storeRelease + `)
		  and ` + graphstore.AddressFitsSQL(`p.reached_id`) + ` and ` + graphstore.AddressFitsSQL(`coalesce(p.item_id, '')`) + `
		  and not exists (select 1 from graph_nodes subject
		                  where subject.kind = p.item_kind and subject.id = p.item_id and subject.state = '` + graphstore.StateLive + `')`,
	}, {
		name: "before_the_store",
		note: "session and role topics of events appended before the store's first migration, when no writer stated a" +
			" delivery as a tuple",
		count: `
		select count(*) from (` + publishedPairs + `) p
		where p.reached_kind is not null and p.created_at < ` + storeRelease,
	}, {
		name: "mention_receipts",
		note: "deliveries of a mention attempt the listener took, stated under its comment.delivery receipt, which no" +
			" topic of an array names and the attempt's comment_deliveries row projects",
		count: `select count(*) ` + publishedDeliveries + ` and ` + mentionReceiptDelivery,
	}, {
		name: "fallback_copies",
		note: "fallback copies published to a session's own topic, which the event's array never lists and a" +
			" comment_fallback_copies row projects",
		count: `select count(*) ` + publishedDeliveries + ` and ` + fallbackCopyDelivery,
	}, {
		name: "answered_deliveries",
		note: "deliveries of a mention attempt a session's reply flipped to sent, stated under the reply's" +
			" comment.answered, which no receipt follows, no topic of an array names and the answered attempt's" +
			" comment_deliveries row projects",
		count: `select count(*) ` + publishedDeliveries + ` and ` + answeredDelivery,
	}},
	emptyAgrees: true,
}

// --- user_threads ------------------------------------------------------------------------------

// threadRootKinds are the kinds a thread's root is, which a user_threads row names (0095).
const threadRootKinds = `('` + graphstore.KindAsk + `', '` + graphstore.KindComment + `', '` + graphstore.KindMessage + `')`

// userThreadsMarked is true of a user_threads row u the census holds to the store: marked, of a
// person and a thread the store can key (userThreadsKeyed).
var userThreadsMarked = `u.marked_seq is not null and ` + userThreadsKeyed

// userThreadsKeyed is true of a user_threads row u whose person the store can key, of a thread
// the store does not leave out on purpose: a comment thread whose root is past the store's address
// bound (refs.CommentInTheStore) has no node its marks could point at, and the backfill marks no
// row of it (userThreadsOwed). Such a row is counted with the addresses past the bound, and every
// other row is held or is the unmarked class.
var userThreadsKeyed = graphstore.AddressFitsSQL(`u.login`) + `
	  and not (u.kind = '` + graphstore.KindComment + `' and exists (
		select 1 from comments c` + refs.CommentStoreJoins + `
		where c.id = u.thread_id and not (` + refs.CommentInTheStore + `)))`

// userThreadsRowOf is true where the person n.id holds a user_threads row of the thread held names,
// with more predicates over that row u. The thread's id is cast only where it is a uuid
// (store.CastableUUID), whatever order the planner reads the predicates in.
func userThreadsRowOf(more string) string {
	return `exists (select 1 from user_threads u
		where u.login = n.id and u.thread_id = ` + store.CastableUUID(`held.to_id`) + more + `)`
}

// agentThreadRoot is true of a relation held into a message on no issue, the root of an Agent
// thread, which user_agent_thread_dismissed projects; a dismissed into an issue message's root is a
// Reply thread's, which user_threads projects.
var agentThreadRoot = `exists (select 1 from messages m
	where m.id = ` + store.CastableUUID(`held.to_id`) + ` and m.issue_key is null)`

// userThreadsHeld is the store side of user_threads before its narrowings: every participates_in,
// read_through and dismissed a live person node states now into a thread's root, read as a current
// relation is, a mark written with its moment; less each relation of a row a writer of the release
// before wrote, which the unmarked class counts, and each dismissed of an Agent thread's root - a
// message on no issue - which user_agent_thread_dismissed projects.
func userThreadsHeld(extra string) string {
	render := `case when held.kind = '` + graphstore.TupleParticipatesIn + `' then held.to_id
	               else held.to_id || ' at ' || ` + moment(`held.at`) + ` end`
	return liveHeld(`n.kind = '`+graphstore.KindPerson+`'`,
		`t.kind in ('`+graphstore.TupleParticipatesIn+`', '`+graphstore.TupleReadThrough+`', '`+graphstore.TupleDismissed+`')
		  and t.to_kind in `+threadRootKinds, render, `
		  not `+userThreadsRowOf(` and u.marked_seq is null`)+`
		  and not (held.kind = '`+graphstore.TupleDismissed+`' and held.to_kind = '`+graphstore.KindMessage+`'
		           and `+agentThreadRoot+`)
		  and `+extra)
}

// The narrowings of user_threads: the relations of a person into a thread they hold no row of,
// which no row projects by design. Each is a predicate over n (the person's node) and held (the
// relation), read only where the person holds no row of the thread (userThreadsNarrowed).
//
//   - no_project: participation in a thread whose root's node names no project - an agent
//     conversation, a direct message - which keeps no row (inboxwrite.Write.Project).
//   - addressed_alone: participation in an ask that reached them, the assignee arm of an ask's
//     create or of a move of who it waits on (inboxwrite's waitingOnTuples), and their decline of
//     it, none of which keeps a row; and the participation the arm states with no delivery for
//     a person who moved the ask onto themselves, under that move's event, whose actor is them
//     (inboxwrite.OwnMove). A participation with no delivery under anybody else's event is a
//     difference.
//   - mentioned_alone: participation stated under the event of a mentions tuple into them that
//     asserts the mention, which keeps no row: the writer that resolves the mention states it
//     (inboxwrite.MentionParticipation). A retraction under that event, which an index states of
//     a mention its text no longer names (refs' unnamedMentions), explains no participation; the
//     assertion that caused it stays in the log, so a participation keeps its mention's event as
//     its explanation once the mention is retracted.
//   - row_removed: a relation into a thread whose row the removal of the thread's document deleted,
//     an artifact.removed of that document appended after the relation's own cause: participation
//     outlives the row (R4), and a mark stands only where a build of the release before removed
//     the document, since this release retracts each mark of a row it deletes under the removal's
//     event (api/artifact_removal.go), which leaves no current mark to read.
var userThreadsNarrowings = []struct{ name, note, test string }{{
	name: "no_project",
	note: "participation in a thread whose root names no project, an agent conversation or a direct message, which keeps no row",
	test: `held.kind = '` + graphstore.TupleParticipatesIn + `' and not exists (
		select 1 from graph_nodes root where root.kind = held.to_kind and root.id = held.to_id and root.project_key is not null)`,
}, {
	name: "addressed_alone",
	note: "participation in an ask that reached the person, as its create or a move of who it waits on addressed it, or" +
		" that the person's own move of the ask onto themselves stated, which keeps no row",
	test: `held.kind = '` + graphstore.TupleParticipatesIn + `' and held.to_kind = '` + graphstore.KindAsk + `' and (exists (
		select 1 from graph_tuples d
		where d.from_kind = '` + graphstore.KindAsk + `' and d.from_id = held.to_id and d.kind = '` + graphstore.TupleDeliveredTo + `'
		  and d.to_kind = '` + graphstore.KindPerson + `' and d.to_id = n.id)
		or ` + inboxwrite.OwnMove(`n.id`, `held.to_id`, `held.source_event`) + `)`,
}, {
	name: "mentioned_alone",
	note: "participation stated under the event of a mention of the person, which keeps no row",
	test: `held.kind = '` + graphstore.TupleParticipatesIn + `' and held.source_event <> ` + noEvent + ` and exists (
		select 1 from graph_tuples m
		where m.to_kind = '` + graphstore.KindPerson + `' and m.to_id = n.id and m.kind = '` + graphstore.TupleMentions + `'
		  and m.source_event = held.source_event and m.asserted)`,
}, {
	name: "row_removed",
	note: "relations into a thread whose row the removal of its document deleted: participation outlives the row, and a" +
		" mark stands only where a build of the release before removed it",
	test: `exists (select 1 from events e
		where e.type = 'artifact.removed' and e.id > held.source_event and exists (
			select 1 from graph_tuples placed
			where placed.from_kind = held.to_kind and placed.from_id = held.to_id and placed.to_kind = '` + graphstore.KindArtifact + `'
			  and placed.to_id = e.payload->'artifact'->>'ref_key'))`,
}}

// userThreadsNarrowed is true of a relation one of the narrowings of user_threads holds. The CASE
// reads the narrowings only for a relation of a person who holds no row of the thread, whatever
// order the planner would have read the two in, since one of them reads the event log.
func userThreadsNarrowed(tests ...string) string {
	return `(case when ` + userThreadsRowOf("") + ` then false else (` + strings.Join(tests, `) or (`) + `) end)`
}

var userThreadsProjection = func() projection {
	var all []string
	for _, narrowed := range userThreadsNarrowings {
		all = append(all, narrowed.test)
	}
	kind := projection{
		name: "user_threads", nodeKind: graphstore.KindPerson, table: "user_threads",
		note: "the threads a person takes part in and their marks: the row side is every marked user_threads row as the" +
			" triples of the person's node and participates_in the thread's root, read_through it at the row's read mark" +
			" and dismissed it at dismissed_at where it holds one, against every participates_in, read_through and" +
			" dismissed a live person node states now into a thread's root, each mark at its tuple's own moment; a row of" +
			" a comment thread whose root the store leaves out past its address bound is counted past the bound",
		rows: `
		select 'user_threads ' || u.login || ' ' || u.thread_id::text, '` + graphstore.KindPerson + `', user_threads_login(u.login),
		       stated.kind, u.kind, stated.to_id
		from user_threads u
		cross join lateral (values
			('` + graphstore.TupleParticipatesIn + `', u.thread_id::text),
			('` + graphstore.TupleReadThrough + `', u.thread_id::text || ' at ' || ` + moment(`u.read_through`) + `),
			('` + graphstore.TupleDismissed + `', case when u.dismissed_at is not null
			                      then u.thread_id::text || ' at ' || ` + moment(`u.dismissed_at`) + ` end)
		) stated (kind, to_id)
		where ` + userThreadsMarked + ` and stated.to_id is not null`,
		tuples: userThreadsHeld(`not ` + userThreadsNarrowed(all...)),
		pastBound: `
		select count(*) from user_threads u where not (` + userThreadsKeyed + `)`,
		unmarked: `
		select count(*) from user_threads u where u.marked_seq is null and ` + userThreadsKeyed,
		populations: userThreadsPopulations,
		emptyAgrees: true,
	}
	for _, narrowed := range userThreadsNarrowings {
		kind.narrowings = append(kind.narrowings, narrowing{
			name: narrowed.name, note: narrowed.note,
			count: `select count(*) from (` + userThreadsHeld(userThreadsNarrowed(narrowed.test)) + `) narrowed`,
		})
	}
	kind.narrowings = append(kind.narrowings, narrowing{
		name: "session_authors",
		note: "participation of a session in a thread, which a session keeps no row of: user_threads holds people",
		count: `select count(*) from (` + liveHeld(`n.kind = '`+graphstore.KindSession+`'`,
			`t.kind = '`+graphstore.TupleParticipatesIn+`' and t.to_kind in `+threadRootKinds, `held.to_id`, "") + `) narrowed`,
	})
	return kind
}()

// userThreadsPopulations splits the marked user_threads rows: a row whose person's newest
// read_through into the thread at or below marked_seq asserts is one the backfill marked, which
// stated that read_through before it marked the row; the one writer of a thread states the
// read_through of a row it begins after the row's insert, above marked_seq (inboxwrite's
// queueKeepRow).
var userThreadsPopulations = `
	select count(*) filter (where earlier.marked), count(*) filter (where not coalesce(earlier.marked, false))
	from user_threads u
	left join lateral (
		select t.asserted as marked from graph_tuples t
		where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = user_threads_login(u.login)
		  and t.kind = '` + graphstore.TupleReadThrough + `' and t.to_kind = u.kind and t.to_id = u.thread_id::text
		  and t.seq <= u.marked_seq
		order by t.seq desc limit 1
	) earlier on true
	where ` + userThreadsMarked

// --- user_threads.version, replied_at and last_reply_id ----------------------------------------

// replyWrites are the event types of the writes a reply is: a comment's create and a session's reply
// to a delivery (comment.answered), a message's create and its answer, and an answer to an ask. An
// ask's create (ask.opened) and a move of who an ask waits on are no reply, and neither is an edit.
const replyWrites = `('comment.created', 'comment.answered', 'message.created', 'message.answered', 'ask.answered')`

// replyCaused is true of a delivered_to tuple d into a person that a reply caused, the deliveries
// user_threads.version counts (R6, R9):
//
//   - a tuple whose event is a reply's write (replyWrites). A delivered_to from an ask's create or
//     from a move of who it waits on is caused by that create's or that move's event, and raises no
//     version, as 0096's trigger raises none (it passes p_reply false for an ask's create, and a
//     move fires no user_threads trigger). A delivery appended under an edit's event - the delivery
//     to a person an edit's new mention reaches, which inboxwrite.Record states
//     (Write.ReachesMentioned) - is caused by the edit and raises no version either: an edit fires
//     no user_threads trigger (0095 fires on a comment's insert alone), so that writer moves no row
//     for it.
//   - a tuple with no event (graphstore.NoEvent): the backfill's, where the log holds no event of
//     the write it states, and the seed's, which records no event at all. Such a tuple is judged
//     by its near end and its moment. Only a reply delivers from a comment or a message - no create
//     of an ask, no move and no edit has either for its node - so a delivery from one is a reply's.
//     A delivery from an ask is an answer's where the ask's node states an answered_at at the
//     tuple's own moment (asks.Settle states it with the answer, at the moment Record delivers it),
//     and the create's or a move's otherwise. A backfilled one stands at or below its row's
//     marked_seq (the backfill states it before it marks the row), so it counts in no version and
//     is read only as the reply the mark carried (userThreadsCounted).
var replyCaused = `(case
	when d.source_event <> ` + noEvent + `
		then exists (select 1 from events e where e.id = d.source_event and e.type in ` + replyWrites + `)
	when d.from_kind in ('` + graphstore.KindComment + `', '` + graphstore.KindMessage + `') then true
	else exists (
		select 1 from graph_tuples answered
		where answered.from_kind = d.from_kind and answered.from_id = d.from_id and answered.to_kind = '` + graphstore.KindValue + `'
		  and answered.kind = '` + graphstore.TupleAnsweredAt + `'
		  and case when pg_input_is_valid(answered.to_id, 'timestamptz') then answered.to_id::timestamptz = d.at else false end)
	end)`

// deliveryThread is the root of the thread the near end of a delivered_to tuple d was written in, as
// (kind, id), read off the store's own head pointers as inboxwrite's threadOf reads them: an ask is
// its own thread; a comment's thread is the ask its replies_to names, else the comment its
// replies_to names, else the comment itself; a message's is the root its replies_to chain reaches
// within inboxwrite.MaxThreadDepth steps.
var deliveryThread = `
	select case d.from_kind when '` + graphstore.KindComment + `' then coalesce(head.kind, d.from_kind) else d.from_kind end,
	       case d.from_kind when '` + graphstore.KindComment + `' then coalesce(head.id, d.from_id)
	                        when '` + graphstore.KindMessage + `' then coalesce(top.id, d.from_id)
	                        else d.from_id end
	from (select 1) one
	left join lateral (
		select h.to_kind as kind, h.to_id as id from (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = '`+graphstore.KindComment+`' and t.from_id = d.from_id and t.kind = '`+graphstore.TupleRepliesTo+`'
			  and t.to_kind in ('`+graphstore.KindAsk+`', '`+graphstore.KindComment+`')`) + `
		) h
		where d.from_kind = '` + graphstore.KindComment + `' and h.asserted
		order by h.to_kind = '` + graphstore.KindAsk + `' desc
		limit 1
	) head on true
	left join lateral (
		with recursive climb (id, depth) as (
			select d.from_id, 0
			union all
			select up.to_id, climb.depth + 1
			from climb
			cross join lateral (
				select h.to_id from (
					` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
	`t.from_kind = '`+graphstore.KindMessage+`' and t.from_id = climb.id and t.kind = '`+graphstore.TupleRepliesTo+`'
					  and t.to_kind = '`+graphstore.KindMessage+`'`) + `
				) h where h.asserted
				limit 1
			) up
			where climb.depth < ` + strconv.Itoa(inboxwrite.MaxThreadDepth) + `
		)
		select climb.id from climb where d.from_kind = '` + graphstore.KindMessage + `' order by climb.depth desc limit 1
	) top on true`

// replyDeliveries is every delivered_to into a person that a reply caused (replyCaused), with the
// thread it was delivered from (deliveryThread), as (login, thread_kind, thread_id, seq, at,
// from_id).
var replyDeliveries = `
	select d.to_id as login, thread.kind as thread_kind, thread.id as thread_id, d.seq, d.at, d.from_id
	from graph_tuples d
	cross join lateral (` + deliveryThread + `) thread (kind, id)
	where d.to_kind = '` + graphstore.KindPerson + `' and d.kind = '` + graphstore.TupleDeliveredTo + `' and d.asserted
	  and d.from_kind in ` + threadRootKinds + ` and ` + replyCaused

// versionTriple is how both sides of user_threads_version write what a row says of the deliveries
// it counted: its thread, its version, its replied_at and its last_reply_id.
func versionTriple(thread, version, repliedAt, lastReply string) string {
	return thread + ` || ' version ' || (` + version + `)::text || ' replied_at ' || coalesce(` + moment(repliedAt) + `, 'none')
		|| ' last_reply ' || coalesce(` + lastReply + `, 'none')`
}

// userThreadsVersionProjection proves R6's equation on every marked user_threads row: version is
// marked_version plus the delivered_to tuples into that person from that thread with seq above
// marked_seq that a reply caused (replyCaused), and replied_at and last_reply_id are the moment and
// the near end of the newest of those same tuples, newest by moment and then by seq, as the
// writer's update takes the later of two writes at one moment (inboxwrite's queueMoveRows). A row
// the backfill marked carried its replied_at and last_reply_id across its mark; that reply is held
// to a delivery the backfill stated at or below marked_seq, a reply-caused delivered_to from
// last_reply_id at replied_at, and enters the newest as one more candidate. A row this release's
// writer began carried none: it records replied_at null, and a delivery to its person before it
// began is no reply to its row.
//
// The store's side of a row is computed from that row's marked_version and marked_seq, which are the
// mark the rule is stated from; nothing else of the row is read to answer it but last_reply_id and
// replied_at as the candidate the mark carried, which a delivery the store holds must answer.
var userThreadsVersionProjection = projection{
	name: "user_threads_version", nodeKind: graphstore.KindPerson, table: "user_threads.version, replied_at, last_reply_id",
	note: "R6's equation on every marked user_threads row: the row side is its version, replied_at and last_reply_id," +
		" against marked_version plus the reply-caused delivered_to tuples into the person from the thread above" +
		" marked_seq, and the moment and near end of the newest of them or of the reply the mark carried; a" +
		" delivery from an ask's create or a move, or under an edit, is no reply",
	rows: `
		select 'user_threads ' || u.login || ' ' || u.thread_id::text, '` + graphstore.KindPerson + `', user_threads_login(u.login),
		       'version', u.kind, ` + versionTriple(`u.thread_id::text`, `u.version`, `u.replied_at`, `u.last_reply_id::text`) + `
		from user_threads u
		where ` + userThreadsMarked,
	tuples: `
		with delivered as materialized (` + replyDeliveries + `)
		select '` + graphstore.KindPerson + `', user_threads_login(u.login), 'version', u.kind,
		       ` + versionTriple(`u.thread_id::text`, `u.marked_version + since.counted`, `newest.at`, `newest.from_id`) + `
		from user_threads u
		cross join lateral (
			select count(*) as counted from delivered d
			where d.login = user_threads_login(u.login) and d.thread_kind = u.kind and d.thread_id = u.thread_id::text
			  and d.seq > u.marked_seq
		) since
		left join lateral (
			select d.at, d.from_id from delivered d
			where d.login = user_threads_login(u.login) and d.thread_kind = u.kind and d.thread_id = u.thread_id::text
			  and (d.seq > u.marked_seq or (d.from_id = u.last_reply_id::text and d.at = u.replied_at))
			order by d.at desc, d.seq desc
			limit 1
		) newest on true
		where ` + userThreadsMarked,
	populations: userThreadsPopulations,
	emptyAgrees: true,
}

// --- the five other read-state tables -----------------------------------------------------------

// readStateKind is one of the five read-state tables other than user_threads (migration
// 20261116170000): the mark its row stands for as a tuple from its person (api/read_marks.go) - the
// tuple's kind, its far end's kind and the column naming it, and the moment the tuple carries - the
// event type the route of the release before that deletes such a row appends, empty for a table no
// route deletes from, and a predicate over held narrowing the far ends the table projects.
type readStateKind struct {
	table, kind, toKind, toID, moment, deleter, toFilter, note string
}

var readStateKinds = []readStateKind{
	{table: "user_agent_read", kind: graphstore.TupleReadThrough, toKind: graphstore.KindSession, toID: `r.session_id`,
		moment: `r.read_through`, note: "a person's read mark of a session's conversation"},
	{table: "user_agent_state", kind: graphstore.TupleCleared, toKind: graphstore.KindSession, toID: `r.session_id`,
		moment: `r.cleared_before`, note: "a person's Clear of a session's conversation"},
	// A reply read by its id stores no moment of its own: its marker records the moment of the
	// reading, which is its tuple's.
	{table: "user_agent_reply_read", kind: graphstore.TupleReplyRead, toKind: graphstore.KindMessage, toID: `r.reply_id::text`,
		moment: `r.tuples_stored_at`, deleter: "user_agent_state.updated", note: "a session's reply a person read by its id"},
	// The release before's "Move to Inbox" of an Agent row deletes the row and appends the
	// user_agent_state.updated every write of a person's Agent state appends
	// (dismissInboxAgentThreads at 99e9da45abf1), the type this release's dismissal and restore of
	// one state their marks under too.
	{table: "user_agent_thread_dismissed", kind: graphstore.TupleDismissed, toKind: graphstore.KindMessage, toID: `r.message_id::text`,
		moment: `r.dismissed_at`, deleter: "user_agent_state.updated", note: "a person's dismissal of an Agent thread's root",
		toFilter: agentThreadRoot},
	// A snooze's tuple carries its snoozed_until.
	{table: "user_ask_snooze", kind: graphstore.TupleSnoozed, toKind: graphstore.KindAsk, toID: `r.ask_id::text`,
		moment: `r.snoozed_until`, deleter: "user_state.updated", note: "a person's snooze of an ask"},
}

// readStateProjections are the five tables' kinds.
func readStateProjections() []projection {
	kinds := make([]projection, 0, len(readStateKinds))
	for _, table := range readStateKinds {
		kinds = append(kinds, table.projection())
	}
	return kinds
}

// keyed is true of a row r whose person and far end the store can key (api's markFits).
func (table readStateKind) keyed() string {
	return graphstore.AddressFitsSQL(`user_threads_login(r.login)`) + ` and ` + graphstore.AddressFitsSQL(table.toID)
}

// rowOf is true where the person n.id holds a row of the table naming held's far end, with more
// predicates over that row r. The table keys some rows on a login not yet canonical
// (user_agent_state keys a Clear on the raw actor id), so the row's login is canonicalised as the
// person's node is.
func (table readStateKind) rowOf(more string) string {
	return `exists (select 1 from ` + table.table + ` r
		where user_threads_login(r.login) = n.id and ` + table.toID + ` = held.to_id` + more + `)`
}

// leftByAnOlderDelete is true of a mark whose row is gone that a build of the release before
// deleted, which leaves the tuple standing, while this release retracts the mark under the event
// of the write that deletes its row: the person's log holds an event of the type that release's
// deleting route appends, appended after the event that stated the mark, under which the store
// holds no tuple from the person at all, so no writer of this release appended it. This is the
// evidence the census reads, as the node store's backfill tells a document an older build
// re-indexed by two versions and by no row (markStaleDocuments): a mark with no row and no such
// event stays a difference, a tuple no row projects. No writer can clear such a mark - a writer
// that read the store's mark to retract it would be a reconcile, which R9 rules out - so it is a
// figure of its own, and not a difference that would refuse every census after a roll for good.
// The CASE reads the log only for a mark of a person who holds no row of it.
func (table readStateKind) leftByAnOlderDelete() string {
	if table.deleter == "" {
		return "false"
	}
	return `(case when ` + table.rowOf("") + ` then false else exists (
		select 1 from events e
		where e.type = '` + table.deleter + `' and user_threads_login(e.actor->>'id') = n.id and e.id > held.source_event
		  and not exists (select 1 from graph_tuples written
		                  where written.from_kind = '` + graphstore.KindPerson + `' and written.from_id = n.id
		                    and written.to_kind <> '` + graphstore.KindValue + `' and written.source_event = e.id)) end)`
}

// held is the table's store side before its narrowing: every mark of the table's kind a live person
// node states now into a far end of the table's kind, read as a current relation is, at its own
// moment; less the mark of a row a writer of the release before wrote, which the unmarked class
// counts.
func (table readStateKind) held(extra string) string {
	filter := table.toFilter
	if filter == "" {
		filter = "true"
	}
	return liveHeld(`n.kind = '`+graphstore.KindPerson+`'`,
		`t.kind = '`+table.kind+`' and t.to_kind = '`+table.toKind+`'`,
		`held.to_id || ' at ' || `+moment(`held.at`), filter+`
		  and not `+table.rowOf(` and r.tuples_stored_at is null`)+`
		  and `+extra)
}

func (table readStateKind) projection() projection {
	kind := projection{
		name: table.table, nodeKind: graphstore.KindPerson, table: table.table,
		note: table.note + ": the row side is every " + table.table + " row whose tuples_stored_at is set, as the triple of" +
			" the person's node, " + table.kind + " and the " + table.toKind + " at the mark's moment, against every " +
			table.kind + " a live person node states now into a " + table.toKind + ", at its tuple's own moment",
		rows: `
		select '` + table.table + ` ' || r.login || ' ' || ` + table.toID + `, '` + graphstore.KindPerson + `', user_threads_login(r.login),
		       '` + table.kind + `', '` + table.toKind + `', ` + table.toID + ` || ' at ' || ` + moment(table.moment) + `
		from ` + table.table + ` r
		where r.tuples_stored_at is not null and ` + table.keyed(),
		tuples:    table.held(`not ` + table.leftByAnOlderDelete()),
		pastBound: `select count(*) from ` + table.table + ` r where not (` + table.keyed() + `)`,
		unmarked:  `select count(*) from ` + table.table + ` r where r.tuples_stored_at is null and ` + table.keyed(),
		// The backfill states a row's mark at the sentinel, and every route of this release under
		// the event it appends.
		populations: `
		select count(*) filter (where stated.source_event = ` + noEvent + `),
		       count(*) filter (where stated.source_event is distinct from ` + noEvent + `)
		from ` + table.table + ` r
		left join lateral (
			select t.source_event from graph_tuples t
			where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = user_threads_login(r.login)
			  and t.kind = '` + table.kind + `' and t.to_kind = '` + table.toKind + `' and t.to_id = ` + table.toID + `
			order by t.seq desc limit 1
		) stated on true
		where r.tuples_stored_at is not null and ` + table.keyed(),
		emptyAgrees: true,
	}
	if table.deleter != "" {
		kind.narrowings = []narrowing{{
			name: "left_by_an_older_delete",
			note: "marks whose row a build of the release before deleted, by the " + table.deleter + " it appended after" +
				" the mark with no tuple of this release under it, which this release would have retracted under its event",
			count: `select count(*) from (` + table.held(table.leftByAnOlderDelete()) + `) narrowed`,
		}}
	}
	return kind
}
