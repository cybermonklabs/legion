-- 20261126045002_graph_edges_comment_tuples.up.sql
--
-- A comment's pointers are tuples now, and the view reads a reply's edges from them. The five arms
-- over what a comment answers - its `replies_to` the comment reply_to names, its `answers` the
-- comment answers names, its `replies_to` the ask ask_id names, and its `replies_to` the comment
-- block or the ask part replies_to_block names - each answer only the rows of comments whose
-- `tuples_stored` is false, and the store answers those relations only while the comments row of
-- the comment that states them names its node and holds `tuples_stored` true. That is the split
-- 20261126045001 gave a message's replies over the messages row, made over the comments row with a
-- marker of the comment's own (migration 20261116124447).
--
-- The marker is no node column. The node store names the node on every comment its writers and its
-- backfill write, pointers or none, so a test on node_kind would answer an older reply from
-- neither arm. Every writer of this release names the node and sets tuples_stored in the insert
-- that writes the row, and states the comment's pointers in the same transaction, under the event
-- that stored it (api's commentStoreWrite, graphstore.CommentPointerFacts, and the seed). So a row
-- this release writes goes to the store's arms, a row the release before it wrote - during a roll,
-- or after a rollback - goes to its old arms, and a row either release deleted goes to neither: a
-- build of the release before deletes a row and retracts nothing, and the probes below answer no
-- tuple whose row is gone. A comment whose pointer is past the store's address bound states none
-- of them and keeps tuples_stored false (graphstore.CommentPointerFacts), so its old arms answer
-- it for good.
--
-- What a reply to a block points at is the node the store holds of that block, as 20261126045001
-- gave a message's: kind `block` (graphstore.KindBlock), at the address the reply's
-- replies_to_block holds, `comment/<comment uuid>#<block id>` for a comment's block and
-- `ask/<ask uuid>#<part id>` for an ask's part, which is the node id comment_blocks.node_id and
-- ask_blocks.node_id hold for it. The view renders that relation under the address form the old
-- arm answered, `comment_block` or `ask_block`, told apart by the address's `ask/` prefix as the
-- old arms tell them apart, so every reader keyed on those forms (refs.Edges' arm over an item's
-- blocks, refs.BacklinkCounts) reads one relation whichever arm answers it.
--
-- Each of the two forms has an arm of its own over the tuples, the last two, as the message block
-- form has: each renders its far end's kind as a constant, never as an expression over the
-- tuple's kind, so a read keyed on the far end binds the leading columns of graph_tuples_as_of_in,
-- the store's one read index led by the far end (20261109013400): (block, the address,
-- replies_to), the reply's kind, comment, a filter of that range, which a read into a block that
-- also holds a message's replies therefore walks past as well, and each arm sorts what it keeps by
-- the reply, which that index does not hold it by. A read keyed on the near end binds
-- graph_tuples_current's, whose partial predicate the arm's literal kind proves. Each arm, the
-- message block form's among them, renders its kind as the constant `replies_to` outside its
-- `distinct on`, so a read of any other kind compares two constants against it and PostgreSQL
-- drops it from the plan (20261126045001). The tuple arm
-- renders a comment's `replies_to` to a comment or an ask and its `answers` to a comment under
-- their own kinds, which are the tuple's columns, and leaves out the `replies_to` whose far end is
-- a block (its `projects` case names `comment` and `ask` far ends alone).
--
-- Measured on this pod's PostgreSQL 18.6 over 708,845 tuples across 16 far-end kinds, 20,555 of
-- them a comment's replies to a block, beside the seed at scale 0.01, on an earlier set of read
-- indexes that also held graph_tuples_inbound (to_kind, to_id, kind, from_kind, from_id, seq desc),
-- which bound the reply's kind too: every keyed read walks graph_tuples in one index search per
-- arm. A read of the edges into one comment (5 rows) reads 4 buffers in the tuple arm, its Index
-- Cond (to_kind, to_id), and 3 in each comment block arm, which answers nothing; into one ask (11
-- rows), 18 buffers in the tuple arm and 3 in each block arm; into one comment block (1 row), 4
-- buffers in the comment block arm, its Index Cond then (block, the address, replies_to, comment),
-- 4 in the ask part arm, whose `like` keeps none of them, and 3 in the tuple arm, which answers
-- nothing; into one ask part (5 rows), 8 buffers in the ask part arm and 8 in the comment block
-- arm, whose `not like` keeps none; into one issue (86 rows), 4 buffers in the tuple arm and 3 in
-- each block arm, as the message block arm reads. A read of the edges out of one comment reads
-- graph_tuples_current in one search per arm, binding (from_kind, from_id) in the tuple arm and
-- (comment, the id, replies_to, block) in each block arm. Under the four read indexes, over
-- 700,000 synthetic tuples with 20,000 of a comment's replies to blocks and 20,000 of a message's,
-- each read still walks graph_tuples in one index search per arm: the comment block arm and the
-- ask part arm read the four replies to one comment block or ask part in the 7 buffers each read
-- before and sort them, and each comment arm of a read into a message block now reads its four
-- message replies in 7 buffers, against 3, to keep none. A read out of one comment binds
-- (from_kind, from_id) of graph_tuples_as_of_out in the tuple arm, whose predicate proves nothing
-- of graph_tuples_current's `kind <> 'delivered_to'`, in 5 buffers, as the earlier set's
-- graph_tuples_current read the same comment, and (comment, the id, replies_to, block) of
-- graph_tuples_current in each block arm, in 3. Computing the block form inside the tuple arm
-- instead costs every keyed read that leading column, which 20261126045001 measured at 88 buffers
-- in 23 index searches against 5 in one.
--
-- The arms render `replies_to` and `answers` and nothing else of a comment's pointers. A comment's
-- `part_of`, its placement under what it answers, is no arm of this view, as a message's `part_of`
-- is not: no old arm answered it. refs.TupleKinds, the list the store's arms render and the
-- announcer writes, gains `answers`; `replies_to` is there since 20261126045001.
--
-- Each new `exists` ends in `offset 0`, as the ones before it do, for the reason 20261109215947
-- gives: PostgreSQL would otherwise be free to plan the correlated probe as a read of the whole of
-- comments. Each probes comments' (node_kind, node_address) index (migration 20261111043200) once
-- for each edge its arm would answer, and reads tuples_stored off the row it finds.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short
-- transaction, so a reference read waits behind it. The arm list re-stated is 20261126045001's,
-- with the five `not tuples_stored` tests over comments, the comment pointer case of the tuple arm
-- and the two comment block arms added, and it is the arm list a later migration re-states from.
-- Nothing here refuses or rewrites a row.

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
      offset 0);
