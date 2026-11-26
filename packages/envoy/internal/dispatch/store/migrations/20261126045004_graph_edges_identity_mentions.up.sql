-- 20261126045004_graph_edges_identity_mentions.up.sql
--
-- A mention of a person, an agent's session or a role is a tuple (R5), and the view answers it. Its
-- `mentions` tuple runs from the comment, the issue message or the document block that holds it to
-- the identity's node: the comment and message index (refs.PlanItemIndex) and the document's block
-- index (refs.PlanBlocks) append one to each identity a body names, and a comment's writer one to
-- each target it names as a delivery route and a second to the session a role resolved to (api's
-- commentMentionFacts). None of them has a row of its own: the refs and block_refs rows hold no
-- party (refs.TargetOf), so no old arm answers one, and a build of the release before this one,
-- which reconciles those rows by deleting every row of the source, deletes none of these.
--
-- Such a tuple is answered by an arm of its own, the identity arm, which renders its kind as the
-- literal `mentions` and reads graph_tuples under `kind = 'mentions' and to_kind in ('person',
-- 'session', 'role')`, so a read of an identity's list binds (to_kind, to_id, kind) of
-- graph_tuples_as_of_in and walks that node's mentions alone, never the far more numerous tuples a
-- person is the far end of and no arm answers (every delivered_to of their mail, every authored_by of
-- their writes). A read of any other kind drops the arm from its plan, as it drops the reply arms
-- (20261126045001), its kind being a constant. The arm answers a tuple while its source carries the
-- marker this release's writer of that source sets:
--   a comment         its row naming the node and holding tuples_stored (20261116124447);
--   a message         its row naming the node (20261116114515);
--   a document block  its document_blocks row naming the node (20261109022248).
-- An ask states no mention of an identity (its parts name none), and the arm answers none out of
-- one. An item's block holds none either: the item states its identities out of its own node, so the
-- item block arm, which reads every tuple out of an item's block, meets none.
--
-- The tuple arm leaves out what the identity arm answers, and every other tuple into a person, a
-- session or a role but the two relations into one it renders: a decision block's decided_by, whose
-- far end is the person or the session who decided, and an ask's followed_by, whose far end is the
-- session that follows it. It stays one scan of graph_tuples, so a read out of a node keyed on a
-- tie's kind - the census's (graphcensus' graphEdgesOut) among them - reads it once beside the item
-- block arm, as before: an arm whose kind is a column cannot be dropped from a read's plan by the
-- read's kind, and a union member that tests a column against a constant with `<>` is not either,
-- since PostgreSQL folds only an equality of two constants and pulls no member carrying a WHERE
-- into the appendrel whose restrictions constraint_exclusion's `partition` tests. So the filter is
-- written as what an index search can bind: the far end before `person`, between `person` and
-- `role`, between `role` and `session` or after `session` - every kind but those three, which the
-- column's collation sorts as the alphabet does - or a tie of one of the two kinds. Under a read of
-- a person's list PostgreSQL then reads graph_tuples_as_of_in once for each of those five, each
-- search bound by (to_kind, to_id) and the range or the kinds, and combines them (BitmapOr): the
-- four ranges cannot hold the person's address, so their searches end at once, and the fifth reads
-- that person's decided_by tuples alone, never a delivered_to of their mail or an authored_by of
-- their writes. A read out of a node is keyed as before and tests the filter on what it reads, as
-- it tests the arm's other predicates. The arm's rendering and its probes are as they were.
--
-- Older arms read a person's list the same way, and are re-stated so they read none of it. The
-- three reply arms (20261126045001, 20261126045002) read their tuples into a block and render the
-- far end's kind as their own literal (message_block, comment_block, ask_block), but from inside
-- their `distinct on`, where a read's kind cannot reach it, so a read of any other kind ran them,
-- keyed by that read's address alone; each now renders it in its own select list, as it renders its
-- kind, and every read of another kind drops it. The item block arm (20261126045003) reads every
-- tuple out of an item's block, whose far end is a column; no such block states a tuple into a
-- person, a session or a role (refs.TargetOf names no party), so it takes the tuple arm's four
-- ranges, and a read of one's list searches graph_tuples_as_of_in through ranges that end at once
-- rather than reading every block's tuples by `from_kind = 'block'`.
--
-- Every probe ends in `offset 0`, as the ones before it do, for the reason 20261109215947 gives.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short transaction,
-- so a reference read waits behind it. The arm list re-stated is 20261126045003's, with the tuple
-- arm's filter, the identity arm after it, the reply arms' literal and the item block arm's ranges.
-- It is the arm list a later migration re-states from. Nothing here refuses or rewrites a row.

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
      and (tuple.to_kind < 'person' or (tuple.to_kind > 'person' and tuple.to_kind < 'role')
           or (tuple.to_kind > 'role' and tuple.to_kind < 'session') or tuple.to_kind > 'session'
           or tuple.kind in ('decided_by', 'followed_by'))
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
