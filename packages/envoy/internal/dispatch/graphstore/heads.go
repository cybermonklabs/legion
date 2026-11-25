package graphstore

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The head projection of each issue's tree (migration 20261123180100_issue_tree_head):
// issue_head_nodes holds one row per node of an issue's tree with what the tree read answers of
// it at the head, and issue_head_marks names each issue whose rows may be read. Every writer of a
// node, and of a value or a relation out of a node, of an issue's tree writes the node's rows in
// its own transaction, after its own statements, through QueueHeads or WriteHeads, which read the
// rows again from what the store holds there, and so does every writer of a comment's row, of its
// blocks' rows or of their texts (comments, comment_blocks, comment_block_texts), whose columns the
// rows of a comment and of its blocks hold; graphstore's TestEveryWriterOfAHeadFactWritesTheHeadRow
// and TestEveryWriterOfACommentWritesItsHeadRows hold every such writer to it.
//
// A write the head rows are not kept for takes the marks of the issues whose trees hold the node it
// writes away, in its own transaction: the trigger of graph_tuples, graph_nodes and document_blocks
// (issue_head_marks_drop) does, for every row a statement inserts, deletes or updates (on
// document_blocks only the columns the head rows read), and so do the triggers of the three
// conversation tables (issue_head_items_drop), for every row a statement inserts, deletes or
// updates in a column the head rows read, each inside the statement that writes the row, unless the
// transaction has set headRowsSetting by then, which every statement of this build that writes one
// of those six tables sets as it runs (KeptHeadRows). A build that keeps no head rows - the release
// before, during a roll or after a rollback - sets no such thing, so the head read walks the tuples
// of every issue such a build wrote, until the backfill fills the issue again (graphcensus'
// backfillIssueHeadNodes).

// headRowsSetting is the transaction's run-time setting that says its writes of graph_tuples,
// graph_nodes and document_blocks, and of the conversation tables, are a build's that keeps the head
// rows, and headRowsKept its value. The triggers of those tables read it (current_setting with
// missing_ok), and a transaction that never set it reads null.
const (
	headRowsSetting = "dispatch.head_rows"
	headRowsKept    = "kept"
)

// KeptHeadRows is a condition true of every row, which sets headRowsSetting for the rest of the
// transaction before the statement it is a part of writes its first row: an uncorrelated scalar
// subquery is an InitPlan, which Postgres runs once, as the first row is read, and an AFTER ROW
// trigger reads its WHEN as each row goes in, so the setting stands before the first trigger
// reads it. Every statement of this package that inserts into or updates graph_tuples or
// graph_nodes carries it in the WHERE of the rows it writes, and so does every statement of this
// build that inserts or deletes document_blocks rows or updates a column of them the head rows read
// (refs' queueRows), whose writer writes the head rows of the blocks it wrote last in its write phase,
// every statement of this build that writes a comment's row, its blocks' rows or their texts, whose
// writer writes their head rows after it in the same transaction
// (TestEveryWriterOfACommentWritesItsHeadRows), and the head write in the WHERE of the nodes it
// names, of which it always names one.
const KeptHeadRows = `(select set_config('` + headRowsSetting + `', '` + headRowsKept + `', true)) is not null`

// NodeKey is one node of the store, by its kind and its id.
type NodeKey struct{ Kind, ID string }

// HeadsOf is the nodes a write of nodes and tuples names, whose head rows the write moves: each
// node it writes, and the near end of each tuple it appends, each once, in the order the write
// names them.
func HeadsOf(nodes []Node, tuples []Tuple) []NodeKey {
	keys := make([]NodeKey, 0, len(nodes)+len(tuples))
	for _, node := range nodes {
		keys = append(keys, NodeKey{Kind: node.Kind, ID: node.ID})
	}
	for _, tuple := range tuples {
		keys = append(keys, NodeKey{Kind: tuple.FromKind, ID: tuple.FromID})
	}
	return distinctKeys(keys)
}

// HeadsOfClaims is the nodes claims state single-valued relations out of (Claim), each once.
func HeadsOfClaims(claims []Claim) []NodeKey {
	keys := make([]NodeKey, 0, len(claims))
	for _, claim := range claims {
		keys = append(keys, NodeKey{Kind: claim.FromKind, ID: claim.FromID})
	}
	return distinctKeys(keys)
}

// HeldHeads is the nodes a held write (Uncaused) names, as HeadsOf names those of a write: each
// node its node write wrote and the near end of each fact it holds for its cause.
func HeldHeads(write Uncaused) []NodeKey {
	keys := make([]NodeKey, 0, len(write.Nodes)+len(write.Tuples))
	for _, node := range write.Nodes {
		keys = append(keys, NodeKey{Kind: node.Kind, ID: node.ID})
	}
	for _, held := range write.Tuples {
		keys = append(keys, NodeKey{Kind: held.FromKind, ID: held.FromID})
	}
	for _, change := range write.changes {
		keys = append(keys, NodeKey{Kind: change.kind, ID: change.id})
	}
	return distinctKeys(keys)
}

// distinctKeys is keys with each key once, in the order of its first place.
func distinctKeys(keys []NodeKey) []NodeKey {
	seen := make(map[NodeKey]struct{}, len(keys))
	return slices.DeleteFunc(keys, func(key NodeKey) bool {
		if _, held := seen[key]; held {
			return true
		}
		seen[key] = struct{}{}
		return false
	})
}

// QueueHeads queues on b the head write of nodes: each one's rows of issue_head_nodes read again
// from what the store holds when the statement runs (headWriteStatement), after every statement
// queued before it, and the rows of every node under one of them whose issue its parent gives, so
// a node that moved takes its subtree with it. A node no issue's tree holds is left with no row. A
// writer queues it after the last of its own statements that writes the nodes, their tuples or the
// rows they read (document_blocks, issue_room_blocks, document_block_texts, a conversation item's
// row), in the same transaction. Nodes that name nothing queue nothing.
func QueueHeads(b *store.WriteBatch, nodes []NodeKey) {
	if len(nodes) == 0 {
		return
	}
	kinds, ids := nodeKeyColumns(nodes)
	b.Queue("write the head rows of the nodes written", headWriteStatement, kinds, ids)
}

// WriteHeads is QueueHeads run at once, as a batch of its own built for ctx, for a writer with no
// write phase.
func WriteHeads(ctx context.Context, tx pgx.Tx, nodes []NodeKey) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueHeads(b, nodes)
		return nil
	})
}

// QueueHeadMark queues on b the mark of the issue key (issue_head_marks), for the write that makes
// an issue's first node: every write of the issue's tree from then on is a writer's that keeps its
// head rows, so they are whole from the issue's birth and its head read reads them. A mark that
// stands is kept as it is.
func QueueHeadMark(b *store.WriteBatch, key string) {
	b.Queue("mark the head rows of a new issue whole", `
		insert into issue_head_marks (issue_key, filled_at) values ($1, clock_timestamp())
		on conflict (issue_key) do nothing`, key)
}

// nodeKeyColumns is nodes as the two arrays a statement binds them as: their kinds and their ids.
func nodeKeyColumns(nodes []NodeKey) (kinds, ids []string) {
	kinds, ids = make([]string, len(nodes)), make([]string, len(nodes))
	for index, node := range nodes {
		kinds[index], ids[index] = storedKindOf(node.Kind), node.ID
	}
	return kinds, ids
}

// HeadColumns are the columns of issue_head_nodes but its key, in the order HeadRowsSQL answers
// them after (issue_key, node_kind, node_address) and the head write writes them.
const HeadColumns = `parent_kind, parent_address, rank, text, supertag, state, visibility, made_at, made_by,
	authors, first_authors, first_at, document_id, block_type, text_sha256, changed_version,
	value_kinds, value_texts, relation_kinds, relation_to_kinds, relation_to_ids,
	item_issue_key, item_author, item_text, item_made_at, item_changed_at, item_reply_to, item_ask_id,
	item_artifact_id, item_document_id, item_version, item_block_id, item_excerpt, item_position,
	item_edited_at, item_deleted_at, item_deleted_by, item_answers`

// KindCommentBlock is the store kind of a block of a comment, addressed
// `comment/<comment uuid>#<block id>` (refs.ItemComment.BlockKind), which the store holds no
// graph_nodes row of: its head row is its comment_blocks row's.
const KindCommentBlock = "comment_block"

// headAnchorVersionSQL is the version a comment's anchor names, where it spells one, as the comment
// loader reads it (refs' anchorVersion), over the comment c.
const headAnchorVersionSQL = `case when c.anchor->>'version' ~ '^[1-9][0-9]{0,8}$' then (c.anchor->>'version')::int end`

// headRankAttribute is the attribute a node's rank is stated under (refs.AttributeRank), which
// the head row holds in a column of its own.
const headRankAttribute = "rank"

// headDepth bounds the walks of the head write: up from a node through the parents its issue
// follows, and down from a moved node through the nodes whose issue follows it. No writer leaves a
// cycle of part_of, and a tree deeper than this is none the server writes.
const headDepth = "32"

// headFixedSQL is true of a node whose own address names the issue that holds it, which its parent
// never moves: an issue's own node, and a document's, a version's or a document block's, whose
// address begins with its issue's key; not a finding of a lens, which is a block whose address
// names its lens (FindingIDSQL). Every other node follows its parent.
func headFixedSQL(kind, id string) string {
	return `(` + kind + ` = '` + KindIssue + `' or (` + kind + ` in ('` + KindArtifact + `', '` + KindVersion + `', '` +
		blockKind + `') and not ` + FindingIDSQL(id) + `))`
}

// headUUIDSQL is the SQL text expr as a uuid where it spells one, and null otherwise, so a probe of
// an item's primary key never fails on an id that names none.
func headUUIDSQL(expr string) string {
	return `(case when ` + expr + ` ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (` + expr + `)::uuid end)`
}

// headOwnIssueSQL is the issue that holds the node of its own, or null: an issue's own node its key,
// a node whose address names its issue (headFixedSQL) that issue where it stands, an ask, a comment
// and a message its row's issue, and a block of one (`<kind>/<uuid>#<block id>`) its item's.
func headOwnIssueSQL(kind, id string) string {
	item := `split_part(split_part(` + id + `, '#', 1), '/', 2)`
	itemIssue := func(table, uuid string) string {
		return `(select own_item.issue_key from ` + table + ` own_item where own_item.id = ` + headUUIDSQL(uuid) + `)`
	}
	return `(case
		when ` + kind + ` = '` + KindIssue + `' then (select own_issue.key from issues own_issue where own_issue.key = ` + id + `)
		when ` + headFixedSQL(kind, id) + ` then (select own_issue.key from issues own_issue where own_issue.key = split_part(` + id + `, '/', 1))
		when ` + kind + ` = '` + KindAsk + `' then ` + itemIssue("asks", id) + `
		when ` + kind + ` = '` + KindComment + `' then ` + itemIssue("comments", id) + `
		when ` + kind + ` = '` + KindMessage + `' then ` + itemIssue("messages", id) + `
		when ` + kind + ` = 'ask_block' then ` + itemIssue("asks", item) + `
		when ` + kind + ` = 'comment_block' then ` + itemIssue("comments", item) + `
		when ` + kind + ` = 'message_block' then ` + itemIssue("messages", item) + `
	end)`
}

// headPlacementSQL is a lateral's body answering the far end of the standing part_of of the node
// whose kind and id the SQL kind and id name, as up.to_kind and up.to_id: of its part_of tuples, the
// newest of each far end kept while it asserts, and of those the newest, by time and then seq, as
// the walk of the tree reads it (refs' asOfPlacement at the head).
func headPlacementSQL(kind, id string) string {
	return `select held.to_kind, held.to_id from (
			select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
			from graph_tuples t
			where t.from_kind = ` + kind + ` and t.from_id = ` + id + ` and t.kind = '` + TuplePartOf + `'
			  and t.to_kind <> '` + KindValue + `'
			order by t.to_kind, t.to_id, t.at desc, t.seq desc
		) held
		where held.asserted
		order by held.at desc, held.seq desc
		limit 1`
}

// headFactFarEnd tells two tuples of one kind out of a node apart: the far end of a relation, and
// nothing of a value, whose newest tuple of its attribute is its value, compared byte for byte (refs'
// treeFactFarEnd).
const headFactFarEnd = `t.to_kind collate "C", (case when t.to_kind = '` + KindValue + `' then '' else t.to_id end) collate "C"`

// HeadRowsSQL is a select of the head rows of the nodes the SQL relation nodes answers as (kind,
// id), each of its rows a node: (issue_key, node_kind, node_address) and HeadColumns, one row for
// each issue whose tree holds the node, read from what the store holds as the statement runs. It
// is the one reading of the rows: the head write writes what it answers, the census compares what
// the table holds with it, and the backfill fills an issue with it.
//
// Which issues hold a node: the issue that holds it of its own (headOwnIssueSQL), and the issue that
// holds each node above it up its standing part_of through nodes that follow their parent, up to
// and with the first node whose address names its issue (headFixedSQL). So a lens, a finding, a
// risk or a result is held where its parent is, an ask of one issue placed under a block of another
// is held under both, and a block of one issue placed under a node of another is held under the
// other too, as a node that issue's walk answers and does not enter (refs.TreeOwns).
//
// What a row holds: the node's standing part_of (headPlacementSQL); its rank and text, and every
// other value and every relation it states now - the newest tuple of each attribute and of each far
// end of a relation, by time and then seq, kept while it asserts (refs' treeFactsStatement at the
// head), relations by kind, far end kind and far end id in the database's collation; its
// graph_nodes row's supertag, state, visibility, made_at and made_by; the who of each authored_by
// it states now in seq order, as the block loader reads them; and for a document block its
// document_blocks row's document (its issue room's document for a block of a room), type, text sum
// and changed version, with the authors and time of the version that first held it where Dispatch
// knows the block was made there (refs' blockNodesStatement); for a comment, the columns of its row
// the comment loader reads (refs' loadCommentNodes: its issue, author, body, times, the comment it
// replies to (reply_to), the comment it answers (answers) and the ask it answers, its document and
// the document and version it is written on), and
// for a current block of a comment, its comment_blocks row, its text at its newest revision and its
// comment's issue, author and document, as the comment block loader and the block texts read read
// them (refs' itemBlockNodes, BlockTexts); and of both, the comment's marks, when its author last
// edited it and when and by whom it was deleted, as both loaders read them. A node the store holds no row of, that states nothing
// and that is no current block of a comment has no row.
func HeadRowsSQL(nodes string) string {
	return `
	with recursive head_targets (kind, id) as (
		select distinct named.kind, named.id from (` + nodes + `) named (kind, id)
	), head_computed as (
		select target.kind, target.id,
		       facts.parent_kind, facts.parent_address, facts.rank, facts.text,
		       node.supertag, node.state, node.visibility, node.made_at, node.made_by,
		       authored.authors,
		       case when known.made then first_version.authors end as first_authors,
		       case when known.made then first_version.created_at end as first_at,
		       block.owner_id as document_id, block.type as block_type, block.text_sha256, block.changed_version,
		       coalesce(facts.value_kinds, '{}') as value_kinds, coalesce(facts.value_texts, '{}') as value_texts,
		       coalesce(facts.relation_kinds, '{}') as relation_kinds, coalesce(facts.relation_to_kinds, '{}') as relation_to_kinds,
		       coalesce(facts.relation_to_ids, '{}') as relation_to_ids,
		       coalesce(comment.issue_key, item_block.issue_key) as item_issue_key,
		       coalesce(comment.author, item_block.author) as item_author,
		       coalesce(comment.body, item_block.text) as item_text,
		       coalesce(comment.created_at, item_block.made_at) as item_made_at,
		       coalesce(comment.edited_at, item_block.changed_at) as item_changed_at,
		       comment.reply_to as item_reply_to, comment.ask_id as item_ask_id,
		       coalesce(comment.artifact_id, item_block.artifact_id) as item_artifact_id, comment.document_id as item_document_id,
		       coalesce(comment.version, item_block.changed_revision) as item_version,
		       item_block.block_id as item_block_id, item_block.excerpt as item_excerpt, item_block.position as item_position,
		       coalesce(comment.edited_at, item_block.post_edited_at) as item_edited_at,
		       coalesce(comment.deleted_at, item_block.deleted_at) as item_deleted_at,
		       coalesce(comment.deleted_by, item_block.deleted_by) as item_deleted_by,
		       comment.answers as item_answers
		from head_targets target
		left join graph_nodes node on node.kind = target.kind and node.id = target.id
		left join lateral (
			select max(f.to_id) filter (where f.valued and f.kind = '` + headRankAttribute + `') as rank,
			       max(f.to_id) filter (where f.valued and f.kind = '` + AttributeText + `') as text,
			       array_agg(f.kind order by f.kind) filter (where f.valued and f.kind not in ('` + headRankAttribute + `', '` + AttributeText + `')) as value_kinds,
			       array_agg(f.to_id order by f.kind) filter (where f.valued and f.kind not in ('` + headRankAttribute + `', '` + AttributeText + `')) as value_texts,
			       array_agg(f.kind order by f.kind, f.to_kind, f.to_id) filter (where not f.valued) as relation_kinds,
			       array_agg(f.to_kind order by f.kind, f.to_kind, f.to_id) filter (where not f.valued) as relation_to_kinds,
			       array_agg(f.to_id order by f.kind, f.to_kind, f.to_id) filter (where not f.valued) as relation_to_ids,
			       (array_agg(f.to_kind order by f.at desc, f.seq desc) filter (where not f.valued and f.kind = '` + TuplePartOf + `'))[1] as parent_kind,
			       (array_agg(f.to_id order by f.at desc, f.seq desc) filter (where not f.valued and f.kind = '` + TuplePartOf + `'))[1] as parent_address,
			       count(*) as stated
			from (
				select distinct on (t.kind, ` + headFactFarEnd + `) t.kind, t.to_kind, t.to_id, t.asserted, t.at, t.seq,
				       t.to_kind = '` + KindValue + `' as valued
				from graph_tuples t
				where t.from_kind = target.kind and t.from_id = target.id
				order by t.kind, ` + headFactFarEnd + `, t.at desc, t.seq desc
			) f
			where f.asserted
		) facts on true
		left join lateral (
			select jsonb_agg(held.who order by held.seq) as authors
			from (
				select distinct on (t.to_kind, t.to_id) t.who, t.seq, t.asserted
				from graph_tuples t
				where t.from_kind = target.kind and t.from_id = target.id and t.kind = '` + TupleAuthoredBy + `'
				  and t.to_kind <> '` + KindValue + `'
				order by t.to_kind, t.to_id, t.seq desc
			) held
			where held.asserted
		) authored on true
		left join lateral (
			select d.artifact_id, d.block_id, d.type, d.text_sha256, d.changed_version,
			       coalesce(room_block.document_id, d.artifact_id) as owner_id
			from document_blocks d
			left join issue_room_blocks room_block on room_block.tree_artifact_id = d.artifact_id
			  and block_id_key(room_block.block_id) = block_id_key(d.block_id) and room_block.block_id = d.block_id
			where target.kind = '` + blockKind + `' and d.node_id = target.id
			limit 1
		) block on true
		left join lateral (
			select true as moved
			from artifacts moved_tree
			join graph_tuples stood on stood.from_kind = '` + blockKind + `' and stood.from_id = moved_tree.ref_key || '#' || block.block_id
			  and stood.kind = 'in_document' and stood.asserted
			where moved_tree.id = block.artifact_id and moved_tree.kind = 'tree'
			having count(distinct stood.to_id) > 1
		) moved on true
		left join lateral (
			select first.version from document_block_texts first
			where first.artifact_id = block.artifact_id and first.block_id = block.block_id and first.text is not null
			  and moved.moved is null
			order by first.version limit 1
		) first_text on true
		left join artifact_versions first_version on first_version.artifact_id = block.owner_id and first_version.number = first_text.version
		left join lateral (
			select first_text.version = 1 or exists (
				select 1 from document_indexed_versions indexed where indexed.artifact_id = block.owner_id and indexed.version = first_text.version - 1
			) as made
		) known on true
		left join lateral (
			select c.issue_key, c.author, c.body, c.created_at, c.edited_at, c.deleted_at, c.deleted_by,
			       c.reply_to::text as reply_to, c.answers::text as answers,
			       c.ask_id::text as ask_id, c.artifact_id, coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id) as document_id,
			       case when c.anchor->>'artifact_id' is not null then ` + headAnchorVersionSQL + ` else c.written_version end as version
			from comments c
			where target.kind = '` + KindComment + `' and c.id = ` + headUUIDSQL(`target.id`) + `
		) comment on true
		left join lateral (
			select b.node_id, b.block_id, b.excerpt, b.made_at, b.changed_revision, b.changed_at, b.position,
			       c.issue_key, c.author, c.artifact_id, c.edited_at as post_edited_at, c.deleted_at, c.deleted_by,
			       (select t.text from comment_block_texts t
			        where t.comment_id = b.comment_id and t.block_id = b.block_id
			        order by t.revision desc limit 1) as text
			from comment_blocks b
			join comments c on c.id = b.comment_id
			where target.kind = '` + KindCommentBlock + `' and b.node_id = target.id and b.position is not null
		) item_block on true
		where node.kind is not null or facts.stated > 0 or item_block.node_id is not null
	), head_chain (kind, id, at_kind, at_id, depth) as (
		select computed.kind, computed.id, computed.kind, computed.id, 0 from head_computed computed
		union all
		select computed.kind, computed.id, computed.parent_kind, computed.parent_address, 1
		from head_computed computed where computed.parent_kind is not null
		union all
		select chain.kind, chain.id, up.to_kind, up.to_id, chain.depth + 1
		from head_chain chain
		cross join lateral (` + headPlacementSQL(`chain.at_kind`, `chain.at_id`) + `) up
		where chain.depth >= 1 and chain.depth < ` + headDepth + ` and not ` + headFixedSQL(`chain.at_kind`, `chain.at_id`) + `
	), head_held (issue_key, kind, id) as (
		select distinct own.issue_key, chain.kind, chain.id
		from head_chain chain
		cross join lateral (select ` + headOwnIssueSQL(`chain.at_kind`, `chain.at_id`) + ` as issue_key) own
		where own.issue_key is not null
	)
	select held.issue_key, computed.kind, computed.id, ` + prefixed("computed.", HeadColumns) + `
	from head_held held
	join head_computed computed on computed.kind = held.kind and computed.id = held.id`
}

// prefixed is the comma-separated columns, each with prefix before it.
func prefixed(prefix, columns string) string {
	names := strings.Split(columns, ",")
	for index, name := range names {
		names[index] = prefix + strings.TrimSpace(name)
	}
	return strings.Join(names, ", ")
}

// headMovedSQL is a select of the nodes the head write reads again for the nodes $1 and $2 name,
// kinds and ids: each of them, and under each that follows its parent (headFixedSQL), every node a
// part_of into it asserts, down through those that follow theirs, so a node whose issue moved with
// its parent is read again with it. Its first row sets headRowsSetting (KeptHeadRows) for the rest
// of the transaction, as every statement of this build that writes a table with a head mark trigger
// sets it.
var headMovedSQL = `
	with recursive head_named (kind, id, depth) as (
		select named.kind, named.id, 0 from unnest($1::text[], $2::text[]) named (kind, id) where ` + KeptHeadRows + `
		union
		select child.from_kind, child.from_id, named.depth + 1
		from head_named named
		cross join lateral (
			select under.from_kind, under.from_id from (
				select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
				from graph_tuples t
				where t.to_kind = named.kind and t.to_id = named.id and t.kind = '` + TuplePartOf + `' and t.to_kind <> '` + KindValue + `'
				order by t.from_kind, t.from_id, t.at desc, t.seq desc
			) under
			where under.asserted
		) child
		where named.depth < ` + headDepth + ` and not ` + headFixedSQL(`named.kind`, `named.id`) + `
	)
	select distinct kind, id from head_named`

// headWriteStatement is the head write of the nodes $1 and $2 name (QueueHeads): the rows
// HeadRowsSQL reads of them and of the nodes under them whose issue moved with them
// (headMovedSQL), upserted on their key, and every row the table held of one of those nodes under
// an issue that holds it no more, deleted. The two touch no row in common, so one statement makes
// both.
var headWriteStatement = `
	with head_nodes as materialized (` + headMovedSQL + `
	), head_rows as materialized (` + HeadRowsSQL(`select kind, id from head_nodes`) + `
	), head_dropped as (
		delete from issue_head_nodes stale
		using head_nodes target
		where stale.node_kind = target.kind and stale.node_address = target.id
		  and not exists (
			select 1 from head_rows kept
			where kept.issue_key = stale.issue_key and kept.kind = stale.node_kind and kept.id = stale.node_address
		  )
	)
	insert into issue_head_nodes (issue_key, node_kind, node_address, ` + HeadColumns + `)
	select * from head_rows
	on conflict (issue_key, node_kind, node_address) do update set (` + HeadColumns + `) = (` + prefixed("excluded.", HeadColumns) + `)`
