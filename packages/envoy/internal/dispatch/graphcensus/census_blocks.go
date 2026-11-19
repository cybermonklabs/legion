package graphcensus

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockProjections are the census's kinds of the document blocks (projections): a block against
// its document_blocks row, the links out of a block against its block_refs rows, and the rank
// and the text every block holds (dispatch://INFRA-1253/spec#b-r-one-tree), each against the
// block's row, and the size every plan node holds against the opening line of its block's text,
// the document each block of an issue room is in against its issue_room_blocks row (room_block),
// and who wrote each block whose node records its authors against the version that first held it
// (block_author).
//
// A row of an issue room is keyed by the room's tree artifact, and the document the block is in
// is the one its issue_room_blocks row names (refs.BlockDocumentSQL), whose in_document the block
// states, whose band its rank stands in and under whose top a top-level block hangs; every other
// row's document is its own artifact. A row of a room any artifact of which owes its block index
// (roomOwed) is counted owed, on neither side of the block, rank and text kinds, since the
// reconciliation indexes the room whole (T12a of dispatch://INFRA-1253's plan).
var blockProjections = []projection{{
	name:     "block",
	nodeKind: refs.BlockNodeKind,
	table:    "document_blocks",
	note: "one node per row, blocks past the address bound and the blocks of a lens's finding aside; the digest is" +
		" the row's document and the block its parent_block_id names, or the document's top where it names none - the" +
		" issue for a block of an issue's spec, Verification or plan document, the document itself for any other -" +
		" against the in_document and part_of tuples the node states now, a block with no part_of telling itself from" +
		" one at its document's top and a part_of to an ask's node read as the ask's block in the node's own document;" +
		" an authored node's row with no parent_block_id states a part_of to no block of its artifact, and the node's" +
		" part_of to anything but a block of its own artifact - its issue, a conversation node, another artifact's" +
		" block - reads as that; a row of an issue room's tree artifact reads its document from its issue_room_blocks" +
		" row, and one of a room that owes its block index is owed",
	rows: `
		select 'block ' || d.node_id, d.node_kind, d.node_address,
		       md5(d.node_id || '|' || ` + statedLinks(
		`'`+refs.TupleInDocument+`>artifact:' || document.ref_key`,
		`'`+graphstore.TuplePartOf+`>' || case when d.parent_block_id is not null then '`+refs.BlockNodeKind+`:' || a.ref_key || '#' || d.parent_block_id
		      when d.type in `+refs.AuthoredTypesSQL+` then '`+outsideArtifact+`' || a.ref_key
		      else `+refs.DocumentTopSQL("document")+` end`,
	) + `)
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		join artifacts document on document.id = ` + refs.BlockDocumentSQL("d") + `
		where octet_length(d.node_id) <= ` + addressBound + ` and not (a.kind = 'tree' and ` + roomOwed("d.artifact_id") + `)`,
	nodes: `
		select n.id, md5(n.id || '|' || ` + blockPlacedLinks(kindIn("t.kind", blockPlacementKinds)) + `)
		from graph_nodes n where n.kind = '` + refs.BlockNodeKind + `' and not ` + lensBlock("n"),
	pastBound: `
		select count(*) from document_blocks d where octet_length(d.node_id) > ` + addressBound,
	owed: `(` + typedNodeOwed + ` or ` + roomNodeOwed + `)`,
}, {
	name:     "block_ref",
	nodeKind: refs.BlockNodeKind,
	table:    "block_refs",
	note: "a row is one link out of a block and its node is that block's, so several rows share one node:" +
		" the node side is every block node not gone, a lens's finding aside, that states a link now of a kind" +
		" refs.IndexBlocks writes into block_refs (a mention, or a pointer of a block[] attribute the store's" +
		" type nodes declare, at any state), whether a row names it or not, and the digest is every link the" +
		" rows of that block name against every such link its node states now; a row an ask's node holds, and" +
		" a decision's shaped, from_ask and decided_by, are no part of this kind",
	// The rows are the block_refs rows of those kinds alone (blockRefKinds). A row marked with an
	// ask's node is the ask's shaped, which hangs on that node and not on the block
	// (refs.TypedFacts.pairs), and a decision's shaped, from_ask and decided_by are ties held triple
	// by triple by kinds of their own (decisionProjections): none of them is a link of the block
	// this kind holds rows to.
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
			  and b.kind in (` + blockRefKinds + `) and b.node_kind is distinct from '` + graphstore.KindAsk + `'
			group by b.from_node
		) named on named.from_node = r.from_node
		where octet_length(r.from_node) <= ` + addressBound + ` and octet_length(r.to_id) <= ` + addressBound + `
		  and r.kind in (` + blockRefKinds + `) and r.node_kind is distinct from '` + graphstore.KindAsk + `'`,
	// Every block node is read, and not only those a row names: a mention or a pointer the store
	// states out of a block no block_refs row names is a link graph_edges answers, and a node
	// side that began at the rows could never find it. A block whose only links are its
	// placement, its shaped, its from_ask, its decided_by, or tuples no block_refs row projects -
	// its authored_by among them, which the block index states through graphstore.AuthorFacts -
	// states no link here and is no node of this kind; the block kind holds its placement.
	nodes: `
		select held.id, md5(held.id || '|' || held.links)
		from (
			select n.id, ` + currentLinks(refs.BlockNodeKind, `t.kind in (`+blockRefKinds+`)`) + ` as links
			from graph_nodes n where n.kind = '` + refs.BlockNodeKind + `' and not ` + lensBlock("n") + `
		) held
		where held.links <> ''`,
	pastBound: `
		select count(*) from block_refs r
		where (octet_length(r.from_node) > ` + addressBound + ` or octet_length(r.to_id) > ` + addressBound + `)
		  and r.kind in (` + blockRefKinds + `) and r.node_kind is distinct from '` + graphstore.KindAsk + `'`,
}, {
	name:     "block_rank",
	nodeKind: refs.BlockNodeKind,
	table:    "document_blocks",
	note: "the rank of every block of a document that owes no block index: the row side is each row that names its" +
		" node, and the digest is the band of the row's document (refs.DocumentBandSQL; for a row of an issue room," +
		" the document its issue_room_blocks row names) against the first character of the rank value its node holds" +
		" now, so a block with no rank, or with one of another document's band, is a difference; a block of a document" +
		" still owed its index, or of an issue room any artifact of which is, is on neither side, since the" +
		" reconciliation writes its rank, and a census of documents that all owe theirs reads none of this kind",
	rows: `
		select 'block_rank ' || d.node_id, d.node_kind, d.node_address, md5(d.node_id || '|' || ` + refs.DocumentBandSQL("document") + `)
		from document_blocks d
		join artifacts document on document.id = ` + refs.BlockDocumentSQL("d") + `
		where d.node_kind is not null and octet_length(d.node_id) <= ` + addressBound + ` and not ` + roomOwed("d.artifact_id"),
	nodes: `
		select n.id, md5(n.id || '|' || coalesce(left(` + currentValue(refs.AttributeRank) + `, 1), ''))
		from graph_nodes n
		where n.kind = '` + refs.BlockNodeKind + `' and not ` + lensBlock("n") + ` and not ` + blockDocumentOwed("n"),
	pastBound: `
		select count(*) from document_blocks d where octet_length(d.node_id) > ` + addressBound,
	emptyAgrees: true,
}, {
	name:     "block_text",
	nodeKind: refs.BlockNodeKind,
	table:    "document_block_texts",
	note: "the text of every block of a document that owes no block index: the row side is each document_blocks row" +
		" that names its node, and the digest is the sha256 of the newest text its document_block_texts rows hold, or" +
		" the row's text_sha256 where none holds one, against the sha256 of the text value its node holds now, so a" +
		" block with no text, or with another text than the one its rows hold, is a difference, and an erase that" +
		" rewrites both alike reads none; a block of a document still owed its index, or of an issue room any" +
		" artifact of which is, is on neither side, since the reconciliation writes its text, and a census of" +
		" documents that all owe theirs reads none of this kind",
	// The projection of a text value is the newest document_block_texts row of its block (R15 of
	// dispatch://INFRA-1253), and text_sha256 only for a block no row of that table records the
	// text of: the erase rewrites the quote of an erased block in both a holding block's rows and
	// its text values (grapherase's redactBlocks) and leaves text_sha256 as the version stated it.
	rows: `
		select 'block_text ' || d.node_id, d.node_kind, d.node_address,
		       md5(d.node_id || '|' || coalesce((
		           select encode(sha256(convert_to(t.text, 'UTF8')), 'hex') from document_block_texts t
		           where t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.text is not null
		           order by t.version desc limit 1), d.text_sha256))
		from document_blocks d
		where d.node_kind is not null and octet_length(d.node_id) <= ` + addressBound + ` and not ` + roomOwed("d.artifact_id"),
	nodes: `
		select n.id, md5(n.id || '|' || coalesce(encode(sha256(convert_to(` + currentValue(graphstore.AttributeText) + `, 'UTF8')), 'hex'), ''))
		from graph_nodes n
		where n.kind = '` + refs.BlockNodeKind + `' and not ` + lensBlock("n") + ` and not ` + blockDocumentOwed("n"),
	pastBound: `
		select count(*) from document_blocks d where octet_length(d.node_id) > ` + addressBound,
	emptyAgrees: true,
}, {
	name:     "block_size",
	nodeKind: refs.BlockNodeKind,
	table:    "document_block_texts",
	note: "the size of every plan node of a document that owes no block index, a value no table projects: the row" +
		" side is each document_blocks row that names its node, and the digest is the size the first line of its" +
		" newest document_block_texts text writes (refs.WrittenSizeSQL, a plan_task's size attribute), or none," +
		" against the size value its node holds now, so a plan node whose line writes a size its node does not" +
		" hold, and a node holding a size its line does not write, are each a difference; a block of a document" +
		" still owed its index, or of an issue room any artifact of which is, is on neither side, since the" +
		" reconciliation writes its size, and a census of documents that all owe theirs reads none of this kind",
	rows: `
		select 'block_size ' || d.node_id, d.node_kind, d.node_address, md5(d.node_id || '|' || coalesce(` + refs.WrittenSizeSQL("d") + `, ''))
		from document_blocks d
		where d.node_kind is not null and octet_length(d.node_id) <= ` + addressBound + ` and not ` + roomOwed("d.artifact_id"),
	nodes: `
		select n.id, md5(n.id || '|' || coalesce(` + currentValue(refs.AttributePlanSize) + `, ''))
		from graph_nodes n
		where n.kind = '` + refs.BlockNodeKind + `' and not ` + lensBlock("n") + ` and not ` + blockDocumentOwed("n"),
	pastBound: `
		select count(*) from document_blocks d where octet_length(d.node_id) > ` + addressBound,
	emptyAgrees: true,
}, {
	name:     "room_block",
	nodeKind: refs.BlockNodeKind,
	table:    "issue_room_blocks",
	note: "the document each block of an issue room is in, held triple by triple: each issue_room_blocks row is the" +
		" in_document of the block its tree artifact and id name, to the node of the document it names, against every" +
		" in_document a present block node of a tree artifact states now, so a row whose block states another document," +
		" or none, and a block of a room no row names, are each a difference; a row of a block whose id is past the" +
		" block id bound, which the room records and which is no node, is past the bound; a room that owes its block" +
		" index is on neither side, since the typed-node pass above the deferral bound records the document of each" +
		" decision and reading it places before the reconciliation states its in_document; a store that holds no tree" +
		" reads none",
	// The rows are issue_room_blocks' alone, each with the node of the document_blocks row it
	// projects beside, so a row whose block no document_blocks row holds names no node, and a
	// document_blocks row of a tree no issue_room_blocks row names leaves its block's in_document a
	// tuple no row projects. The tuple side reads the block nodes whose id starts with a tree
	// artifact's ref_key, the part of a block node's id before its first '#'. A row of a block
	// whose id is past text.MaxBlockIDBytes, which the room's index records with no document_blocks
	// row (refs' reconcileRoomBlocks), projects no node, so it is counted past the bound with a row
	// whose address is.
	rows: `
		select 'room_block ' || tree.ref_key || '#' || r.block_id, d.node_kind, d.node_address,
		       '` + refs.TupleInDocument + `', '` + graphstore.KindArtifact + `', document.ref_key
		from issue_room_blocks r
		join artifacts tree on tree.id = r.tree_artifact_id
		join artifacts document on document.id = r.document_id
		left join document_blocks d on d.artifact_id = r.tree_artifact_id and d.block_id = r.block_id
		where octet_length(r.block_id) <= ` + blockIDBound + ` and octet_length(tree.ref_key || '#' || r.block_id) <= ` + addressBound + `
		  and not ` + roomOwed("r.tree_artifact_id"),
	tuples: `
		select n.kind, n.id, held.kind, held.to_kind, held.to_id
		from artifacts tree
		join graph_nodes n on n.kind = '` + refs.BlockNodeKind + `' and split_part(n.id, '#', 1) = tree.ref_key
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id and t.kind = '`+refs.TupleInDocument+`'`) + `
		) held
		where tree.kind = 'tree'
		  and n.state <> '` + graphstore.StateGone + `' and held.asserted`,
	pastBound: `
		select count(*) from issue_room_blocks r
		join artifacts tree on tree.id = r.tree_artifact_id
		where octet_length(r.block_id) > ` + blockIDBound + ` or octet_length(tree.ref_key || '#' || r.block_id) > ` + addressBound,
	owed:        roomNodeOwed,
	emptyAgrees: true,
}, blockAuthorProjection}

// blockIDBound is the bound past which a block id is no node of the store (text.MaxBlockIDBytes)
// as a statement spells it, written into the text as addressBound is.
var blockIDBound = strconv.Itoa(text.MaxBlockIDBytes)

// outsideArtifact starts the part_of link the block kind reads an authored node's place by where
// that place is no block of the node's own artifact, followed by that artifact's ref_key: the
// rule document_blocks.parent_block_id projects an authored node's part_of by (refs'
// placeAuthored) holds the parent's block id where the parent is a block of the same indexed
// artifact and nothing where it is the issue, a conversation node or a block of another artifact,
// so a null column tells only that the parent is none of the artifact's blocks.
const outsideArtifact = "outside:"

// blockPlacedLinks is placedLinks of a block node n, but where n is an authored node - its
// placement value refs.PlacementAuthored - a part_of whose far end is no block of n's own
// artifact is written outside:<that artifact's ref_key>, which is what its row's null
// parent_block_id states (outsideArtifact), and a part_of to an ask's node is read as that far
// end too, never as the ask's block, which only a reading's row names. A ref_key holds no '#', so
// n's artifact is the part of its id before the first one.
func blockPlacedLinks(kinds string) string {
	return `(select coalesce(string_agg(placed.link, ',' order by placed.link), '')
	 from (
		select case when held.kind = '` + graphstore.TuplePartOf + `' and authored.held
		            then case when held.to_kind = '` + refs.BlockNodeKind + `' and split_part(held.to_id, '#', 1) = split_part(n.id, '#', 1)
		                      then held.kind || '>' || held.to_kind || ':' || held.to_id
		                      else held.kind || '>` + outsideArtifact + `' || split_part(n.id, '#', 1) end
		            when held.kind = '` + graphstore.TuplePartOf + `' and held.to_kind = '` + graphstore.KindAsk + `'
		            then coalesce(held.kind || '>` + refs.BlockNodeKind + `:' || split_part(n.id, '#', 1) || '#' || ` +
		askBlockID(`held.to_id`) + `, held.kind || '>' || held.to_kind || ':' || held.to_id)
		            else held.kind || '>' || held.to_kind || ':' || held.to_id end as link
		from (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = n.id
		  and t.to_kind <> '`+graphstore.KindValue+`' and `+kinds) + `
		) held
		cross join lateral (select coalesce(` + currentValue(refs.AttributePlacement) + ` = '` + refs.PlacementAuthored + `', false) as held) authored
		where held.asserted
	 ) placed)`
}

// blockAuthorsHeld is true of each document_blocks row d, with its artifacts row a, whose block's
// authors the block_author kind holds, the one test both its sides read a block by: a row naming
// its node whose type records its authors (refs.RecordsAuthorsSQL: an authored node, or a block of
// an issue's tree artifact but a decision and a reading), of a document that owes no block index
// (roomOwed), which is no block of an issue room that has stood in more than one fragment
// (refs.MovedRoomBlockSQL).
//
// The test reads the block's type alone, never its node's supertag, so a decision and a reading
// are left out of both sides alike: a reading in an ask no one has answered is a node of supertag
// block whose type is reading, and a reading retyped away keeps the authored_by the typed-node
// pass gave it, which names the one actor who introduced it (refs.TypedSources.Authors), a record
// no row holds. A block retyped away from a type that records its authors keeps the authors its
// first version gave it too, and is out of both sides from then on. A block of a room that moved
// between fragments has text rows recording versions of more than one document, none of which says
// which, so no row tells the version that first held it.
func blockAuthorsHeld(d, a string) string {
	return `(` + d + `.node_kind = '` + refs.BlockNodeKind + `' and ` + refs.RecordsAuthorsSQL(d, a) + `
		and not ` + refs.MovedRoomBlockSQL(d+".artifact_id", d+".block_id") + ` and not ` + roomOwed(d+".artifact_id") + `)`
}

// blockFirstAuthors is the FROM and WHERE text over each document_blocks row d whose block's
// authors the census holds (blockAuthorsHeld), with each author of the version v that first held
// its block, as graphstore.AuthorOf names an actor (author.kind, author.id; an actor naming nobody
// is left out). The version is the lowest one document_block_texts records a text of the block in,
// read against the block's own document (refs.BlockDocumentSQL), the one refs.IndexBlocks takes
// the block's authors from: the block index states one authored_by per author of it through
// graphstore.AuthorFacts, which makes the author's person or session node, for every such block of
// every document it indexes, a block an earlier build wrote included, so once a document owes
// nothing each of its blocks names those authors (blockAuthorProjection) and each author is a node
// the store holds (storedAuthors). A document still owed is on neither side, since the pass that
// drains it writes them.
var blockFirstAuthors = `
	from document_blocks d
	join artifacts a on a.id = d.artifact_id
	cross join lateral (
		select t.version from document_block_texts t
		where t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.text is not null
		order by t.version limit 1
	) first
	join artifact_versions v on v.artifact_id = ` + refs.BlockDocumentSQL("d") + ` and v.number = first.version
	cross join lateral jsonb_array_elements(case when jsonb_typeof(v.authors) = 'array' then v.authors else '[]'::jsonb end) written (actor)
	cross join lateral (select ` + authorNodeKind("written.actor") + `, ` + authorNodeID("written.actor") + `) author (kind, id)
	where author.kind is not null and ` + blockAuthorsHeld("d", "a")

// blockVersionAuthors are those authors as (kind, id, at) (storedAuthors), at the version's time.
var blockVersionAuthors = `select author.kind, author.id, v.created_at` + blockFirstAuthors

// blockAuthorProjection is the census's kind of each block's authors (T7 of
// dispatch://INFRA-1253's plan), held triple by triple over the blocks blockAuthorsHeld reads: each
// row is an authored_by of such a block's node to one author of the version that first held the
// block (blockFirstAuthors), against every authored_by such a block's node states now while it is
// present. So an authored node of a document that owes nothing and states no authored_by is a row
// no tuple states, an author it names that its version does not is a tuple no row projects, and a
// block either side leaves out - a decision, a reading, a block of a document still owed, a block
// of a room that moved between fragments - the other leaves out too.
var blockAuthorProjection = projection{
	name:     "block_author",
	nodeKind: refs.BlockNodeKind,
	table:    "document_blocks, artifact_versions.authors",
	note: "who wrote each block whose node records its authors, held triple by triple over one set of document_blocks" +
		" rows on both sides: each row is an authored_by of the node of a row of an authored node, or of a block of an" +
		" issue's tree artifact but a decision and a reading, by the row's type, to each author of the version that" +
		" first held the block, against every authored_by the present node of such a row states now; a block of a" +
		" document that owes its block index, and a block of an issue room that has stood in more than one fragment," +
		" whose text rows name no document, are on neither side",
	rows: `
		select distinct 'block_author ' || d.node_id || ' ' || author.kind || ':' || author.id, d.node_kind, d.node_address,
		       '` + graphstore.TupleAuthoredBy + `', author.kind, author.id` + blockFirstAuthors + `
		  and octet_length(d.node_id) <= ` + addressBound + ` and octet_length(author.id) <= ` + addressBound,
	tuples: `
		select n.kind, n.id, held.kind, held.to_kind, held.to_id
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		join graph_nodes n on n.kind = d.node_kind and n.id = d.node_address
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = n.kind and t.from_id = n.id and t.kind = '`+graphstore.TupleAuthoredBy+`'
			  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held
		where ` + blockAuthorsHeld("d", "a") + ` and n.state <> '` + graphstore.StateGone + `' and held.asserted`,
	emptyAgrees: true,
}

// authorNodeKind and authorNodeID are the kind and the id of the node graphstore.AuthorOf names of
// the actor a jsonb holds, as authorNode writes them together: a person by the login its actor
// carries, or by the owner a personal token's carries, canonicalised as commentWriterPeople does, and a
// session by its id; null for an actor that names nobody.
func authorNodeKind(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'user' and coalesce(` + actor + `->>'id', '') <> '' then '` + graphstore.KindPerson + `'
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> '' then '` + graphstore.KindSession + `'
			when coalesce(` + actor + `->>'owner', '') <> '' then '` + graphstore.KindPerson + `'
		end`
}

func authorNodeID(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'user' and coalesce(` + actor + `->>'id', '') <> '' then user_threads_login(` + actor + `->>'id')
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> '' then ` + actor + `->>'id'
			when coalesce(` + actor + `->>'owner', '') <> '' then user_threads_login(` + actor + `->>'owner')
		end`
}

// documentOwed is true of the document whose id the SQL expression artifactID names while it owes
// its block index, by either mark the block-index reconciliation lists a document by
// (refs.OwedBlockIndexDebts, docs' owedDocuments): its version null, or this release's own mark,
// store_owed_at, which the backfill and its drain set (writeBlockBatch, drainBlockIndexes) and a
// release before this one neither reads nor clears. The reconciliation has yet to write the ranks
// and the texts of its blocks.
func documentOwed(artifactID string) string {
	return `exists (select 1 from document_block_versions owed where owed.artifact_id = ` + artifactID +
		` and (owed.version is null or owed.store_owed_at is not null))`
}

// blockDocumentOwed is roomOwed of the document of the block node alias, named by the part of its
// id before the first '#', since a ref_key holds none: for a block of an issue room, its tree
// artifact's room.
func blockDocumentOwed(alias string) string {
	return `exists (
			select 1 from artifacts document
			where document.ref_key = split_part(` + alias + `.id, '#', 1) and ` + roomOwed("document.id") + `)`
}

// roomOwed is documentOwed of the artifact whose id the SQL expression artifactID names, or of
// any document whose fragment of that artifact's issue room its document_rooms row names: a
// block of an issue room is indexed by a reconciliation of the whole room on a mark of any of its
// artifacts (docs' indexOwedRoom), and owes that index while any of them is owed. A document that
// is its own room has no document_rooms row naming it, and is documentOwed alone.
func roomOwed(artifactID string) string {
	return `(` + documentOwed(artifactID) + ` or exists (
			select 1 from document_rooms held where held.room = ` + artifactID + ` and ` + documentOwed("held.artifact_id") + `))`
}

// roomNodeOwed is the owed predicate (projection.owed) of a block node of an issue room, one
// whose id starts with a tree artifact's ref_key, while any artifact of its room owes its block
// index (roomOwed): the room's reconciliation has yet to state what its rows project.
var roomNodeOwed = `exists (
		select 1 from artifacts tree
		where tree.kind = 'tree' and tree.ref_key = split_part(node.id, '#', 1) and ` + roomOwed("tree.id") + `)`

// blockBackfills are the backfill's passes over the document blocks (backfillPasses): the blocks,
// then the links out of them, since a link's node is its block's and the pointer pass stamps a
// link only where its block's node stands (blockNodeStands), then the one drain of every stored
// document's block index (drainBlockIndexes).
var blockBackfills = []backfillPass{oneKind(backfillBlocks), oneKind(backfillBlockRefs), oneKind(drainBlockIndexes)}

// blockDrainKind is the kind the backfill's report names the drain by.
const blockDrainKind = "block_index_drain"

// drainBlockIndexes marks every stored document owed its block index once, with this release's
// own mark, document_block_versions.store_owed_at (migration 20261116221539), as the block pass
// marks each document whose rows it names (writeBlockBatch), so the block-index reconciliation
// (docs.Service.RunBlockIndexReconciliation) indexes each again, under the fence it already takes,
// and gives every block an earlier build indexed what that build did not write: its rank, its
// text, its placement and, for a plan node whose opening line writes one, its size, and, for a
// top-level block of an issue's spec, Verification or plan document, its part_of to the issue
// (refs.IndexBlocks). That index writes no document_blocks row for a block whose text is
// unchanged, so no block's changed_version and no document_block_texts row moves for it.
//
// The mark is not the null version refs.RebuildAll sets. A pod of the release before this one,
// during a roll or after a rollback, reads a null version as its own debt: it indexes the
// document, writes its rows and states none of these values, and the document would be owed
// nothing again, while the drain, done, never marks it twice. That release neither reads nor
// clears store_owed_at, so this release's reconciliation, which lists a document holding it (docs'
// owedDocuments) and whose fence wins at the version the document already stands at while it
// holds it (refs' standsAtVersion), still reaches the document, and its pass clears the mark.
//
// issue_tree_block_drain records how far the drain has got, and the backfill runs at every boot,
// so it marks the documents once: a run that finds the drain done marks nothing, and a run stopped
// part way resumes after the last document its last committed batch marked. It reports the
// documents it marked as the kind's Owed, and the kind as complete once the drain is done.
func drainBlockIndexes(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: blockDrainKind}
	if _, err := database.Pool.Exec(ctx, `
		insert into issue_tree_block_drain default values on conflict (singleton) do nothing
	`); err != nil {
		return done, fmt.Errorf("record the block index drain: %w", err)
	}
	for {
		marked, finished, err := drainBlockIndexBatch(ctx, database)
		done.Owed += marked
		if err != nil {
			return done, err
		}
		if finished {
			break
		}
	}
	done.Complete = true
	return done, nil
}

// drainBlockIndexBatch marks one batch of documents owed, in one transaction, and answers how many
// it marked and whether the drain is done. It takes the drain's row first, so two runs take turns,
// then the batch's owner rows and their document_block_versions rows in the store's order
// (lockBlockDocuments), the rows a version write holds while it writes, so a write in flight
// finishes before its document is marked and the mark is never lost under it. A document already
// holding the mark is left as it is; one whose version is null is marked too, since a pod of the
// release before may drain that version first.
//
// The cursor starts at the nil uuid, below every document's id, as the object pass's does
// (zeroUUID), so the batch's read binds one bound of the keyset and no test of whether there is
// one: under a generic plan that test would read from the first document every batch.
func drainBlockIndexBatch(ctx context.Context, database *store.Store) (int64, bool, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin a block index drain batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var after string
	var finished bool
	if err := tx.QueryRow(ctx, `
		select coalesce(after, $1::uuid)::text, done from issue_tree_block_drain for update
	`, zeroUUID).Scan(&after, &finished); err != nil {
		return 0, false, fmt.Errorf("read the block index drain: %w", err)
	}
	if finished {
		return 0, true, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `
		select b.artifact_id::text
		from document_block_versions b
		join artifacts a on a.id = b.artifact_id
		where a.kind = 'doc' and b.artifact_id > $1::uuid
		order by b.artifact_id
		limit $2
	`, after, backfillBatch)
	if err != nil {
		return 0, false, fmt.Errorf("list the documents a block index drain batch marks: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, false, fmt.Errorf("list the documents a block index drain batch marks: %w", err)
	}
	var marked int64
	if len(documents) > 0 {
		if err := lockBlockDocuments(ctx, tx, documents); err != nil {
			return 0, false, err
		}
		tag, err := tx.Exec(ctx, `
			update document_block_versions set store_owed_at = now()
			where artifact_id = any($1::uuid[]) and store_owed_at is null
		`, documents)
		if err != nil {
			return 0, false, fmt.Errorf("mark a batch of documents owed this release's block index: %w", err)
		}
		marked = tag.RowsAffected()
		after = documents[len(documents)-1]
	}
	finished = len(documents) < backfillBatch
	if _, err := tx.Exec(ctx, `
		update issue_tree_block_drain set after = $1::uuid, done = $2
	`, after, finished); err != nil {
		return 0, false, fmt.Errorf("record how far the block index drain has got: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit a block index drain batch: %w", err)
	}
	return marked, finished, nil
}
