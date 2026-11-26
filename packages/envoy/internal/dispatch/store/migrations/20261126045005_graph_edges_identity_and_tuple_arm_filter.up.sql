-- 20261126045005_graph_edges_identity_and_tuple_arm_filter.up.sql
--
-- `graph_edges` restated once more, last, with every arm and every test of the two restatements a
-- database could otherwise end on. A database runs each version once, in version order, and each
-- `create or replace view` replaces the whole view, so the view it ends with is the last
-- restatement's alone. Two restatements were each written from an arm list without the other's
-- changes, so whichever of them ran last would leave the view without the other's:
--
--   20261126045004_graph_edges_identity_mentions: 20261125163553's arms, with the marker test of
--   every arm over a kind this release moved into the store (a message's replies, a comment's
--   pointers, every item's blocks and links, and the refs arm's tuples_stored), the tuple arm's
--   four further `when` arms of 20261126045001, 20261126045002 and 20261126045003, its ranges that
--   leave out every far end that is a person, a session or a role but a decided_by's or a
--   followed_by's, its leaving out the tuples out of an item's block, the identity arm of the
--   mentions of a person, a session or a role, the three reply arms with their far end's kind a
--   literal of their own select list, and the item block arm with the ranges. That migration gives
--   the reason for each.
--
--   20261125163554_graph_edges_asks_and_tuple_arm_filter: 20261125163553's arms, with
--   20261120122700's filter of the tuple arm: its sub-select keeps only the kinds the arm renders
--   before its `distinct on` sorts the tuples, the test that `projects` is not null spelled over
--   the six `when` arms that arm list has, and the outer test of it is gone.
--
-- Here the view is 20261126045004's, and the tuple arm's sub-select carries 20261120122700's filter
-- spelled over all ten `when` arms of its `projects` case, in the case's order: the three of
-- 20261109215947, the two of a comment's pointers (20261126045002), the three of an ask
-- (20261125163553), the message's reply (20261126045001) and the citation of an item
-- (20261126045003). The outer `edge.projects is not null` is gone. The kind filter and the ranges
-- are both tests of the
-- sub-select, and each does what the other cannot. The kind filter leaves out, as a read keyed
-- out of a node reads them, the kinds the arm never renders - a comment's authored_by, a block's
-- part_of its parent - but it binds nothing an index search into a person can take, so alone it
-- would leave a read of a person's list reading every delivered_to of their mail and refusing it.
-- The ranges bound that search and leave a read out of a node as it was. Every column either reads
-- - from_kind, from_id, kind and to_kind - is a column of the `distinct on` key, so each keeps or
-- drops whole groups of one edge's tuples and never changes which tuple of a group is the newest:
-- neither changes a row the view answers.
--
-- The three arms over asks and ask_followers, and the tuple arm's three joins to those rows, stay
-- for one release, as 20261125163553 keeps them: the root's own pull request deletes them.
-- kept for one release: asks-arms
--
-- Every probe ends in `offset 0`, as the ones before it do, for the reason 20261109215947 gives.
-- This is a new migration rather than an edit of either restatement, because a database that ran
-- one of them keeps the definition it wrote until a later version restates it; it must stay the
-- last migration that restates the view, which docs'
-- TestGraphEdgesAnswersEveryRowEachRestatementOfItAnswered holds.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short transaction,
-- so a reference read waits behind it. Nothing here reads, refuses or rewrites a row.

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
      -- The kinds the arm renders: each `when` of the `projects` case above, in its order.
      and ((tuple.from_kind = 'block' and tuple.kind = 'in_document')
           or (tuple.from_kind = 'block' and tuple.kind <> 'part_of')
           or (tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by'))
           or (tuple.from_kind = 'comment' and tuple.kind = 'replies_to' and tuple.to_kind in ('comment', 'ask'))
           or (tuple.from_kind = 'comment' and tuple.kind = 'answers' and tuple.to_kind = 'comment')
           or (tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by'))
           or (tuple.from_kind = 'ask' and tuple.kind = 'followed_by')
           or (tuple.from_kind = 'ask' and tuple.kind = 'shaped')
           or (tuple.from_kind = 'message' and tuple.kind = 'replies_to' and tuple.to_kind = 'message')
           or (tuple.from_kind in ('comment', 'message', 'ask') and tuple.kind = 'mentions'))
      -- No far end the identity arm answers, nor any other person, session or role but a
      -- decided_by's or a followed_by's: ranges a search into one binds.
      and (tuple.to_kind < 'person' or (tuple.to_kind > 'person' and tuple.to_kind < 'role')
           or (tuple.to_kind > 'role' and tuple.to_kind < 'session') or tuple.to_kind > 'session'
           or tuple.kind in ('decided_by', 'followed_by'))
      and not (tuple.from_kind = 'block'
               and (tuple.from_id like 'comment/%' or tuple.from_id like 'message/%' or tuple.from_id like 'ask/%'))
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
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
  select edge.from_kind, edge.from_id, 'mentions'::text, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.from_kind, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.kind = 'mentions' and tuple.to_kind in ('person', 'session', 'role')
    order by tuple.to_kind, tuple.to_id, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and case edge.from_kind
          when 'comment' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.tuples_stored
            offset 0)
          when 'message' then exists (
            select 1 from messages projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
        end
  union all
  select edge.from_kind, edge.from_id, 'replies_to'::text, 'message_block'::text, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
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
  select edge.from_kind, edge.from_id, 'replies_to'::text, 'comment_block'::text, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
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
  select edge.from_kind, edge.from_id, 'replies_to'::text, 'ask_block'::text, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_id, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
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
      -- No item's block states a tuple into a person, a session or a role (refs.TargetOf names no
      -- party): the tuple arm's ranges, so a read of one's list drops this arm's search.
      and (tuple.to_kind < 'person' or (tuple.to_kind > 'person' and tuple.to_kind < 'role')
           or (tuple.to_kind > 'role' and tuple.to_kind < 'session') or tuple.to_kind > 'session')
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
