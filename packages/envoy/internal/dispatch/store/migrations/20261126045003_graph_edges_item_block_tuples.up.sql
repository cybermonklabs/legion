-- 20261126045003_graph_edges_item_block_tuples.up.sql
--
-- The body blocks of a comment, a message and an ask are block nodes now, with their links and the
-- item's own citations as tuples, and the view reads them from the store. Seven arms move.
--
-- The three arms over an item's blocks - a block's part_of its comment, message or ask
-- (comment_blocks, message_blocks, ask_blocks) - and the three over the links those blocks hold
-- (comment_block_refs, message_block_refs, ask_block_refs) each answer only the rows whose
-- node_kind is null (migration 20261116140000). The store answers a block's part_of while the
-- block's row names the block's node and stands at a position, and a link while the link's row
-- names that node and its block stands at a position, which is the old arms' own position filter
-- carried over. The refs arm answers only the rows whose tuples_stored is false (migration
-- 20261116135959): every row of a document, which another child moves, and every row of an item
-- the store states no citation for. The store answers an item's citation, its `mentions` tuple out
-- of the item's node, while the refs row of that pair stands and holds tuples_stored. So a row this
-- release writes goes to the store's arms, a row the release before it wrote - during a roll, or
-- after a rollback, an item of this release's re-indexed by that build among them - goes to its old
-- arm, and a row either release deleted goes to neither: a build of the release before deletes a
-- row and retracts nothing, and the probes below answer no tuple whose row is gone.
--
-- Every writer of this release states those facts in the transaction that writes the rows, and
-- names the node in the insert or update that writes each row (refs.IndexItem, refs.PlanAskParts
-- and the item writer of refs rows, refs.PlanItemReplace): a block's node, kind `block`
-- (graphstore.KindBlock), at the address its row's node_id already holds,
-- `<item kind>/<item uuid>#<block id>`, the address a reply to a block names since 20261126045001
-- and 20261126045002; its part_of to the item; each link it holds, under that link's own kind and
-- to that link's own far end, as a document block's links are (refs.PlanBlocks); and the
-- item's citations, under `mentions`, to each far end its refs rows name, each row holding
-- tuples_stored from its own insert. An item the store holds no node of has no block node and no
-- citation either, and its rows name none and hold false; a block whose address, or a link or a
-- citation whose far end, is past the store's address bound is no tuple, and its row names no node
-- or holds false. Each of those rows is answered by its old arm for as long as that is so.
--
-- An item's block is rendered under the address form the old arms answered it by, `comment_block`,
-- `message_block` or `ask_block`, so every reader keyed on those forms (refs.Edges' arm over an
-- item's blocks, refs.BacklinkCounts) reads one relation whichever arm answers it, and graph_edges
-- never answers an item's block under `block`. One arm of its own over the tuples, the last, reads
-- every item's blocks: its own predicate fixes the store's kind, `from_kind = 'block'`, and keeps
-- the addresses an item kind's prefix names (`comment/`, `message/`, `ask/`), which is the one
-- thing that tells an item's block from a document's, whose address starts with its document's
-- ref_key, a project key in capitals. It renders from_kind by a case over that prefix. The case
-- hides no index column: the arm's `distinct on` is the tuple arm's over the four columns its
-- predicate leaves free, so a read keyed on the far end binds (to_kind, to_id) and walks the block
-- tuples into that node once, and a read keyed on the near end binds from_id beside the arm's own
-- from_kind of `block` in graph_tuples_current; the rule of 20261126045001 against a case is the
-- tuple arm's, whose kind columns no predicate fixes. The arm states `kind <> 'delivered_to'`, which
-- leaves out no tuple, since no block states a delivery, so a plan of a read keyed on the near end
-- proves graph_tuples_current's partial predicate where that index leaves that kind out, as no
-- kind column can. The tuple arm leaves out every tuple out of an address the item block arm
-- keeps, by the same filter negated, so the two split the store's block tuples: none is answered
-- by both, and none by neither.
--
-- Two rules of 20261109215947 hold through this. A `part_of` whose from_kind is `block` is still
-- left out of the tuple arm (its `projects` case names none), because a document's block states a
-- `part_of` to its parent - its document, or the block that holds it - beside the `in_document`
-- the view renders as its part_of, and rendering both would answer a top-level block twice. An
-- item's block states no `in_document` and one `part_of`, to its item, which the item block arm
-- answers once and only where the far end is the item kind its prefix names; the tuple arm reads
-- no tuple out of an item's block at all, so neither its block nor its block_ref case meets one.
--
-- The tuple arm renders an item's citation under its own kinds, which are the tuple's columns: its
-- `projects` case names a comment's, a message's and an ask's `mentions`, and each probe binds the
-- refs row of the pair by the four columns of its primary key (from_kind, from_id, to_kind, to_id)
-- and tests its tuples_stored. The marker is the row's and not its source's for the reason
-- 20261116135959 gives: a source marker would leave a row an older build writes for an item of
-- this release's answered by neither arm. The refs arm's own test is a column of the row it has
-- already read, so it adds no table, index or subquery to that arm.
--
-- Each new `exists` ends in `offset 0`, as the ones before it do, for the reason 20261109215947
-- gives: PostgreSQL would otherwise be free to plan the correlated probe as a read of the whole of
-- its table. The item block arm's probes read the six tables' (node_kind, node_address) indexes
-- (20261116140002 to 20261116140007), the two of the item kind its prefix names, once for each edge
-- the arm would answer.
--
-- Measured on this pod's PostgreSQL 18.6 over the seed at scale 0.01 (130,240 tuples across 14
-- far-end kinds, 60,644 of them out of an item's block), on an earlier set of read indexes that
-- also held graph_tuples_inbound, beside 20261126045002's view over the same rows, in shared
-- buffers read by the whole view, this one's against that one's (under the four read indexes, over
-- 700,000 synthetic tuples out of nodes the store does not hold, so that no arm answers a row, the
-- whole view reads 30 against 24 out of one comment block, 43 against 40 into one comment, 43
-- against 38 out of one comment and 63 against 55 into one issue):
--
--   out of one comment block (2 rows)       47 against 29
--   into one comment (2 rows)               56 against 30
--   out of one comment, its citations (2)   60 against 50
--   into one ask (4 rows)                   65 against 29
--   into one issue (15 tuples, 8 of them out of blocks; 20 rows)   219 against 102
--
-- Every arm reads graph_tuples in one index search. Out of the comment block, the item block arm
-- binds (block, the address) of graph_tuples_current and reads 22 buffers, and the tuple arm binds
-- (comment_block, the address) and finds none in 9. Into the comment, the ask and the issue, the
-- item block arm binds (to_kind, to_id) of graph_tuples_as_of_in and reads 26, 38 and 83 buffers,
-- and into the issue the tuple arm reads 101, its six citation probes among them; at this scale
-- the planner reads a citation probe through refs_to (to_kind, to_id) rather than the primary key.
--
-- With 650,000 tuples added beside the seed that no node holds (780,240 in all, 510,644 out of an
-- item's block; 1,765 into the issue read, 1,258 of those out of blocks, 756 of them an item's),
-- the read into that issue reads 10,771 buffers against 4,380. Those figures were read on an
-- earlier set of read indexes that also held graph_tuples_inbound (to_kind, to_id, kind,
-- from_kind, from_id, seq desc): the item block arm bound (to_kind, to_id, from_kind) of it in a
-- bitmap scan of 3 index searches, over `kind`, and walked the 1,258 block tuples into the issue
-- once to keep the 756 an item prefix names (1,298 buffers; a `like` prefix binds no btree column
-- under the database's ICU collation), 2,857 with its link probes. The same read through one
-- constant arm per item kind read those block tuples three times, 14,875 buffers in all. The tuple
-- arm reads 7,879: the 1,765 tuples into the issue, leaving the 756 out, the block_refs probes of
-- the 502 document blocks' links (1,006), and 5,045 in 506 citation probes of tuples no refs row
-- holds, which the view before leaves to no probe. Under the four read indexes (20261109013400)
-- the item block arm binds (to_kind, to_id) of graph_tuples_as_of_in in one index search and
-- filters `from_kind = 'block'` as well, so it walks every tuple into the node: over 700,000
-- synthetic tuples, into an issue 5,000 tuples reach, 1,250 of them out of blocks and none an
-- item's, it reads 170 buffers to keep none, where graph_tuples_inbound's 9 index searches over
-- the same rows read 145.
--
-- refs.TupleKinds, the list the store's arms render and the announcer writes, gains `part_of`, an
-- item block's placement; `mentions` is there since 20261109215947.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short transaction,
-- so a reference read waits behind it. The arm list re-stated is 20261126045002's, with the
-- `not tuples_stored` test of the refs arm, the six `node_kind is null` tests over an item's blocks
-- and their links, the citation case and the item block addresses left out of the tuple arm, and
-- the item block arm added; the three reply arms keep their constant kind, `replies_to`, so a read
-- of any other kind drops them from its plan (20261126045001). It is the arm list a later
-- migration re-states from. Nothing here refuses or rewrites a row.

create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from refs r
  where not r.tuples_stored
  union all
  select 'issue', key, 'child_of', 'issue', parent_key, created_at, null
  from issues where parent_key is not null
  union all
  select 'issue', issue_key, 'blocked_by', 'issue', target_key, created_at, null
  from issue_links where kind = 'blocked_by'
  union all
  select 'artifact', id::text, 'attached_to', 'issue', issue_key, created_at, null
  from artifacts where issue_key is not null
  union all
  select 'ask', k.id::text, 'anchored_to', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
  where k.node_kind is null
  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  where c.node_kind is null
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  where k.node_kind is null
  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  where c.node_kind is null
  union all
  select 'comment', id::text, 'replies_to', 'comment', reply_to::text, created_at, null
  from comments where reply_to is not null and not tuples_stored
  union all
  select 'comment', id::text, 'answers', 'comment', answers::text, created_at, null
  from comments where answers is not null and not tuples_stored
  union all
  select 'comment', id::text, 'replies_to', 'ask', ask_id::text, created_at, null
  from comments where ask_id is not null and not tuples_stored
  union all
  select 'message', id::text, 'replies_to', 'message', in_reply_to::text, created_at, null
  from messages where in_reply_to is not null and node_kind is null
  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null
  from ask_followers
  where node_kind is null
  union all
  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent, s.imported_at, null
  from components c join architecture_snapshots s on s.id = c.snapshot_id
  where c.parent is not null
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id, s.imported_at, null
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, null
  from issue_component_members m join issue_components c using (issue_key)
  union all
  select 'block', from_node, kind, to_kind, to_id, created_at, source_seq
  from block_refs
  where (to_types is null or block_ref_listed(to_types, to_id))
    and node_kind is null
  union all
  select 'block', d.node_id, 'part_of', 'artifact', a.ref_key, d.created_at, null
  from document_blocks d join artifacts a on a.id = d.artifact_id
  where d.node_kind is null
  union all
  select 'comment_block', b.node_id, 'part_of', 'comment', b.comment_id::text, b.created_at, null
  from comment_blocks b where b.position is not null and b.node_kind is null
  union all
  select 'message_block', b.node_id, 'part_of', 'message', b.message_id::text, b.created_at, null
  from message_blocks b where b.position is not null and b.node_kind is null
  union all
  select 'ask_block', b.node_id, 'part_of', 'ask', b.ask_id::text, b.created_at, null
  from ask_blocks b where b.position is not null and b.node_kind is null
  union all
  select 'comment_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from comment_block_refs r
  join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
  where b.position is not null and r.node_kind is null
  union all
  select 'message_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from message_block_refs r
  join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
  where b.position is not null and r.node_kind is null
  union all
  select 'ask_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from ask_block_refs r
  join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
  where b.position is not null and r.node_kind is null
  union all
  select 'comment', c.id::text, 'replies_to', 'comment_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block not like 'ask/%' and not c.tuples_stored
  union all
  select 'comment', c.id::text, 'replies_to', 'ask_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block like 'ask/%' and not c.tuples_stored
  union all
  select 'message', m.id::text, 'replies_to', 'message_block', m.replies_to_block, m.created_at, null
  from messages m where m.replies_to_block is not null and m.node_kind is null
  union all
  select edge.from_kind, edge.from_id, edge.kind, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           case when tuple.kind = 'in_document' then 'part_of' else tuple.kind end as kind,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.kind as stated,
           tuple.asserted,
           case
             when tuple.from_kind = 'block' and tuple.kind = 'in_document' then 'block'
             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'comment' and tuple.kind = 'replies_to' and tuple.to_kind in ('comment', 'ask') then 'comment_pointer'
             when tuple.from_kind = 'comment' and tuple.kind = 'answers' and tuple.to_kind = 'comment' then 'comment_pointer'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
             when tuple.from_kind = 'message' and tuple.kind = 'replies_to' and tuple.to_kind = 'message' then 'message'
             when tuple.from_kind in ('comment', 'message', 'ask') and tuple.kind = 'mentions' then 'citation'
           end as projects
    from graph_tuples tuple
    where tuple.to_kind <> 'value'
      and not (tuple.from_kind = 'block'
               and (tuple.from_id like 'comment/%' or tuple.from_id like 'message/%' or tuple.from_id like 'ask/%'))
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone' and edge.projects is not null
    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
    and case edge.projects
          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'block_ref' then exists (
            select 1 from block_refs projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.kind = edge.stated and projected.to_kind = edge.to_kind
              and projected.to_id = edge.to_id
            offset 0)
          when 'comment' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'comment_pointer' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.tuples_stored
            offset 0)
          when 'ask' then exists (
            select 1 from asks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'ask_follower' then exists (
            select 1 from ask_followers projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.session_id = edge.to_id
            offset 0)
          when 'message' then exists (
            select 1 from messages projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'citation' then exists (
            select 1 from refs projected
            where projected.from_kind = edge.from_kind and projected.from_id = edge.from_id
              and projected.to_kind = edge.to_kind and projected.to_id = edge.to_id
              and projected.kind = edge.stated and projected.tuples_stored
            offset 0)
        end
  union all
  select edge.from_kind, edge.from_id, 'replies_to'::text, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           'message_block'::text as to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.to_kind = 'block' and tuple.kind = 'replies_to' and tuple.from_kind = 'message'
    order by tuple.to_id, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and exists (
      select 1 from messages projected
      where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
      offset 0)
  union all
  select edge.from_kind, edge.from_id, 'replies_to'::text, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           'comment_block'::text as to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.to_kind = 'block' and tuple.kind = 'replies_to' and tuple.from_kind = 'comment'
      and tuple.to_id not like 'ask/%'
    order by tuple.to_id, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and exists (
      select 1 from comments projected
      where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
        and projected.tuples_stored
      offset 0)
  union all
  select edge.from_kind, edge.from_id, 'replies_to'::text, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           'ask_block'::text as to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.to_kind = 'block' and tuple.kind = 'replies_to' and tuple.from_kind = 'comment'
      and tuple.to_id like 'ask/%'
    order by tuple.to_id, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and exists (
      select 1 from comments projected
      where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
        and projected.tuples_stored
      offset 0)
  union all
  select edge.from_kind, edge.from_id, edge.kind, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_id)
           case
             when tuple.from_id like 'comment/%' then 'comment_block'
             when tuple.from_id like 'message/%' then 'message_block'
             when tuple.from_id like 'ask/%' then 'ask_block'
           end as from_kind,
           tuple.from_id,
           tuple.kind,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.from_kind = 'block' and tuple.to_kind <> 'value'
      and (tuple.from_id like 'comment/%' or tuple.from_id like 'message/%' or tuple.from_id like 'ask/%')
      -- No block states a delivery: the literal proves graph_tuples_current's partial predicate.
      and tuple.kind <> 'delivered_to'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = 'block' and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and case edge.from_kind
          when 'comment_block' then case
            when edge.kind = 'part_of' then edge.to_kind = 'comment' and exists (
              select 1 from comment_blocks projected
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.position is not null
              offset 0)
            else exists (
              select 1 from comment_block_refs projected
              join comment_blocks held on held.comment_id = projected.comment_id and held.block_id = projected.block_id
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.kind = edge.kind and projected.to_kind = edge.to_kind
                and projected.to_id = edge.to_id and held.position is not null
              offset 0)
          end
          when 'message_block' then case
            when edge.kind = 'part_of' then edge.to_kind = 'message' and exists (
              select 1 from message_blocks projected
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.position is not null
              offset 0)
            else exists (
              select 1 from message_block_refs projected
              join message_blocks held on held.message_id = projected.message_id and held.block_id = projected.block_id
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.kind = edge.kind and projected.to_kind = edge.to_kind
                and projected.to_id = edge.to_id and held.position is not null
              offset 0)
          end
          when 'ask_block' then case
            when edge.kind = 'part_of' then edge.to_kind = 'ask' and exists (
              select 1 from ask_blocks projected
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.position is not null
              offset 0)
            else exists (
              select 1 from ask_block_refs projected
              join ask_blocks held on held.ask_id = projected.ask_id and held.block_id = projected.block_id
              where projected.node_kind = 'block' and projected.node_address = edge.from_id
                and projected.kind = edge.kind and projected.to_kind = edge.to_kind
                and projected.to_id = edge.to_id and held.position is not null
              offset 0)
          end
        end;
