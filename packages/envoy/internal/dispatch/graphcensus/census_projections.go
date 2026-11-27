package graphcensus

import (
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

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
	owed:    typedNodeOwed,
	rowOwed: typedRowOwed,
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
	rowOwed: typedRowOwed,
}, {
	name:     graphstore.KindPerson,
	nodeKind: graphstore.KindPerson,
	table:    "comments.author, asks.author, asks.answer, block_refs.decided_by, graph_tuples.kept_by, events.payload",
	note: "no table projects a person, so the row side is the people the projected tables name as authors," +
		" the owner of each session the server proved that wrote one, the person each ask's answer names and each" +
		" answer the store took ever named, who wrote each decision and reading and their proved owners, and" +
		" each person a decision's Keep ever named",
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
	table:    "comments.author, comment_deliveries, message_deliveries, comment_fallback_copies, asks.author, ask_followers, block_refs.decided_by, events.payload",
	note: "no table projects a session, so the row side is the sessions those tables name as authors, as reached," +
		" as askers, as followers and as who wrote a decision or a reading, and every session an ask.opened" +
		" event named as the asker, an ask.follower_added event named or a comment.delivery receipt says the" +
		" listener took an attempt to",
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
		rowOwed:     typedRowOwed,
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
			" a live block or ask node states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(refs.TupleShaped), refs.BlockNodeKind, graphstore.KindAsk),
	edgeProjection(refs.TupleFromAsk+edgeSuffix, refs.TupleFromAsk,
		"graph_edges' from_ask: the row side is every from_ask edge graph_edges answers out of a block node not"+
			" gone, read node by node, as the triple of that node, from_ask and its ask, against every from_ask a"+
			" live block node states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(refs.TupleFromAsk), refs.BlockNodeKind),
	edgeProjection(graphstore.TupleDecidedBy+edgeSuffix, graphstore.TupleDecidedBy,
		"graph_edges' decided_by: the row side is every decided_by edge graph_edges answers out of a block node"+
			" not gone, read node by node, as the triple of that node, decided_by and its person or session, against"+
			" every decided_by a live block node states now, an edge the old arm answers from a row past the bound aside",
		tieEdgePastBound, tiePastBound(graphstore.TupleDecidedBy), refs.BlockNodeKind),
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
