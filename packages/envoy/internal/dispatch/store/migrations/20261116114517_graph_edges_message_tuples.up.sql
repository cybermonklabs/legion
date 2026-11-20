-- 20261116114517_graph_edges_message_tuples.up.sql
--
-- A message's pointers are tuples now, and the view reads its replies from them. The two arms over
-- messages - a reply's `replies_to` the message it answers (in_reply_to) and its `replies_to` the
-- block of that message it names (replies_to_block) - each answer only the rows of messages whose
-- `node_kind` is null, and the store answers a message's `replies_to` only while the messages row
-- of that message names its node. That is the split 20261109215947 gave a comment's `anchored_to`
-- and `owned_by` over the comments row, made over the messages row.
--
-- The messages row's node pair is the marker (migration 20261116114515). Every writer of this
-- release names the message's node in the insert that writes the row, and states the message's
-- pointers in the same transaction, under the event that stored it (api's writeMessageNode,
-- graphstore.MessageFacts, and the seed). So a row this release writes goes to the store's arms, a
-- row the release before it wrote - during a roll, or after a rollback - goes to its old arm, and a
-- row either release deleted goes to neither: a build of the release before deletes a row and
-- retracts nothing, and the joins below answer no tuple whose row is gone. The split is per row
-- and not per kind for the reasons 20261109215947 gives, and one more of this kind's own: a
-- message whose replies_to end is past the store's address bound states none of its pointers and
-- names no node (graphstore.MessageFacts), so its old arms answer it for good.
--
-- What a message points at is the node the store holds of it, so the tuple of a reply to a block
-- names the block's own node: kind `block` (graphstore.KindBlock), at the address the reply's
-- replies_to_block holds, `message/<message uuid>#<block id>`, which is the node id
-- message_blocks.node_id holds for that block. That is the address the writer of an item's block
-- nodes gives a message's block, and that writer keeps it: a message block's node is
-- (block, message/<message uuid>#<block id>), never a node of a kind of its own, since the closed
-- supertag list holds no message_block. The view renders that one relation - a message's
-- `replies_to` whose far end is a `block` - under the address form the old arm answered,
-- `message_block`, so every reader keyed on that form (refs.Edges' arm over an item's blocks,
-- refs.BacklinkCounts) reads one relation whichever arm answers it, and graph_edges never answers
-- an item's block under `block`.
--
-- That relation has an arm of its own, the last, and the tuple arm leaves it out (its `projects`
-- case for a message is a reply to a message alone). Each of the two then renders its far end's
-- kind as a column of the tuple or as a constant, never as an expression over the tuple's kind,
-- so a read keyed on the far end binds the leading columns of graph_tuples_as_of_in, the store's
-- one read index led by the far end (20261109013400), in both: the tuple arm binds (to_kind,
-- to_id) as before, and the reply arm binds the fixed (block, the far end's id, replies_to),
-- filters the reply's kind, message, out of the rows that range holds, and answers its rows under
-- `message_block`. Its `distinct on` keys the two columns its predicate leaves free, the far end's
-- id and the reply's, which that index holds by time and not by the reply, so the arm sorts the
-- replies it read of the one far end: four replies to a block are a quicksort of 25 kB. The arm
-- renders its kind as the constant `replies_to`, outside the `distinct on`: a read of any other
-- kind then compares two constants against this arm and PostgreSQL drops the arm from its plan,
-- where a kind the arm read out of the tuple would be a column of the `distinct on` subquery that
-- the read's kind is never pushed into, and the arm would probe graph_tuples for every such read.
--
-- Measured on this pod's PostgreSQL 18.6 over 640,000 tuples across 11 far-end kinds, 20,000 of
-- them replies to a block, on an earlier set of read indexes that also held graph_tuples_inbound
-- (to_kind, to_id, kind, from_kind, from_id, seq desc), under which the reply arm bound the reply's
-- kind as well and sorted nothing: a read of the edges into one issue reads graph_tuples in 5
-- buffers and one index search in the tuple arm, as the view before this one does, and in 3
-- buffers and one search in the reply arm, which answers nothing; a read of the edges into one
-- message block reads 3 buffers and one search in the tuple arm, which answers nothing, and 7
-- buffers and one search in the reply arm for its four replies. Under the four read indexes, over
-- 700,000 synthetic tuples across 15 far-end kinds with the same 20,000 replies, the reply arm
-- reads those four replies in the same 7 buffers and one search, and sorts them. Computing the
-- kind inside the tuple arm instead cost every keyed read that leading column: 88 buffers in 23
-- index searches against 5 in one, a skip scan over every far-end kind, measured on that earlier
-- set; graph_tuples_as_of_in is led by the same two columns.
--
-- The two arms render `replies_to` and nothing else of a message. A message's `part_of`, its
-- `addressed_to`, its `broadcast_id` (a value, which no arm reads) and its `authored_by` are no
-- arm of this view, as a page's `part_of` and a comment's `authored_by` are not: none was an edge
-- of the view before, and refs.TupleKinds, the list the store's arms render and the announcer
-- writes, gains `replies_to` alone.
--
-- Each new `exists` ends in `offset 0`, as the ones before them do, for the reason
-- 20261109215947 gives: PostgreSQL would otherwise be free to plan the correlated probe as a read
-- of the whole of messages. Each probes messages' (node_kind, node_address) index (migration
-- 20261116114516) once for each edge its arm would answer.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short
-- transaction, so a reference read waits behind it. The arm list re-stated is 20261116094101's,
-- which moved an ask's anchored_to, owned_by and followed_by into the store, with the two
-- `node_kind is null` tests over messages, the messages case of the tuple arm and the reply arm
-- added, and it is the arm list a later migration re-states from. Nothing here refuses or
-- rewrites a row.

create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from refs
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
  from comments where reply_to is not null
  union all
  select 'comment', id::text, 'answers', 'comment', answers::text, created_at, null
  from comments where answers is not null
  union all
  select 'comment', id::text, 'replies_to', 'ask', ask_id::text, created_at, null
  from comments where ask_id is not null
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
  from comment_blocks b where b.position is not null
  union all
  select 'message_block', b.node_id, 'part_of', 'message', b.message_id::text, b.created_at, null
  from message_blocks b where b.position is not null
  union all
  select 'ask_block', b.node_id, 'part_of', 'ask', b.ask_id::text, b.created_at, null
  from ask_blocks b where b.position is not null
  union all
  select 'comment_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from comment_block_refs r
  join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'message_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from message_block_refs r
  join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'ask_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from ask_block_refs r
  join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'comment', c.id::text, 'replies_to', 'comment_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block not like 'ask/%'
  union all
  select 'comment', c.id::text, 'replies_to', 'ask_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block like 'ask/%'
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
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
             when tuple.from_kind = 'message' and tuple.kind = 'replies_to' and tuple.to_kind = 'message' then 'message'
           end as projects
    from graph_tuples tuple
    where tuple.to_kind <> 'value'
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
      offset 0);
