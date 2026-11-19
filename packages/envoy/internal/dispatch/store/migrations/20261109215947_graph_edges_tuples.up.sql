-- 20261109215947_graph_edges_tuples.up.sql
--
-- The view reads the store. `graph_edges` gains one arm over the current tuples, and each row of
-- the four arms whose relations the node and tuple store now writes decides which arm answers it.
--
-- One arm answers every kind the store writes that is an edge today: for each
-- (from_kind, from_id, kind, to_kind, to_id) the newest tuple, which is an edge only while that
-- newest one asserts. `to_kind <> 'value'` keeps an attribute's value out, since a value is no
-- node and is the far end of no edge, and it is also the predicate the store's two read indexes
-- that carry the far end are partial on. The `distinct on` names those five columns far end
-- first - the far end, the kind, then the near end - and the `order by` repeats that order and
-- ends at `seq desc`. The set of columns is what the newest tuple of a relation is picked by. A
-- read keyed on the far end - the edges into one node - walks `graph_tuples_as_of_in`, which
-- leads with the far end and the kind, and sorts that node's inbound tuples; a read keyed on the
-- near end walks `graph_tuples_as_of_out` and sorts what that leaves.
--
-- It renders a stated list and nothing else: a block's `in_document`, rendered as the `part_of`
-- to its document the view has always answered; a block's `mentions` and its pointer kinds, the
-- two kinds `block_refs` holds; and a comment's `anchored_to` and `owned_by`. That is what the
-- four arms it joins carry, and it is the list `refs.TupleKinds` holds in Go, which the announcer
-- writes into `server_build_announcements.tuple_kinds` (migration 20261115051200) so that the
-- release which deletes an old arm can read whether the release a rollback would reach writes
-- that arm's kinds as tuples too. Every other kind the store writes - `authored_by`,
-- `delivered_to`, `held_by`, the alias tuples, `attribute_of`, and a session's or a page's
-- `part_of` - is no arm of this view today, and adding one would add edges and backlink counts
-- to every reader of a comment; the child that owns each of those kinds decides when it joins
-- the view. A `part_of` whose `from_kind` is `block` is left out for the same reason: the view
-- has no block-to-block row today, and a top-level block, whose parent is the document itself,
-- would otherwise be rendered twice, once from that tuple and once from `in_document`.
--
-- Each row decides which arm answers it. Each of the four old arms answers only the rows of its
-- own table whose `node_kind` is null, which is a row no store writer marked. The tuple arm
-- answers an edge of those kinds only while the projection row that names it carries its node:
-- the `document_blocks` row of the block for `in_document`, the `block_refs` row of the pair for
-- a pointer or a mention, and the `comments` row of the comment for `anchored_to` and
-- `owned_by`. The two split the rows the old view read, exactly and at every moment, because one
-- transaction writes both sides of a row: the writer that states a block's or a comment's tuples
-- names that node on the row in the same transaction (`refs.IndexBlocks`, api's comment write,
-- and the backfill), and a row this release writes with no node has no tuple behind it. So a row
-- this release wrote goes to the tuple arm; a row the release before it wrote, during a roll or
-- after a rollback, goes to its old arm; and a row either release deleted goes to neither. The
-- four `comments` arms that thread a reply are untouched: a later child writes those relations,
-- and `comments.node_kind` is no marker of theirs, since this release names a comment's node
-- before its reply relations are tuples.
--
-- Per row and not per kind, because a row that names no node outlives the backfill of its kind
-- in three ways, and a test over one row per kind would hide every one of them while it read
-- true: a pod of the release before this one, writing while a roll is half done; a build rolled
-- back to that release, which writes rows and no nodes; and a block whose address is past
-- `refs.MaxBlockNodeBytes`, which is no node of this store by design and whose row therefore
-- keeps its null columns for good. Each of those rows keeps its answer from the old arm.
--
-- The join is what keeps a rollback whole. A build of the release before this one deletes a row
-- and appends no retraction, since this release retracts only what its own deletes return; the
-- tuple stands, but no row names it, so neither arm answers it. That build may also delete a row
-- and write it again with no node; the row then names none, so its old arm answers it once and
-- the tuple arm not at all. While two releases write the projections and only one writes the
-- store, the projection row is the one record both keep, so it alone can say an edge still
-- stands. This is the one read of the tuples that consults a projection row, and the join lasts
-- as long as an old arm over its kind does.
--
-- The tests cost little. Each old arm's is a null test on a column of the row the arm has already
-- read, so no table, index or subquery is added to any of the four. The tuple arm's join is one
-- probe of its projection table's (node_kind, node_address) index (migrations 20261111043000,
-- 20261111043100 and 20261111043200) for each edge the arm would answer, and `edge.projects`,
-- which picks that table, is the list of kinds the arm renders; a deployment that has not
-- backfilled holds no tuple of those kinds for the rows written before the store, so the arm
-- finds none of them and answers none.
--
-- Each of the three `exists` ends in `offset 0`, which keeps each join an index probe per edge.
-- PostgreSQL plans a correlated `exists` whose test is equalities two ways - the probe, run for
-- each edge, and a hashed `= any` that reads the whole table once a statement - and runs the one
-- its cost estimate prefers (make_subplan, fix_alternative_subplan); enable_seqscan takes no part
-- in that choice. On a small table the whole read is the cheaper estimate: over an empty
-- block_refs the arm read every row of it for an edge read keyed on one node, and once analyzed
-- at a test's size document_blocks and comments are hashed from a full scan of their own index.
-- The planner makes the hashed form only of an `exists` with no OFFSET, and leaves `offset 0`
-- alone because it is the traditional optimization fence (simplify_EXISTS_query), so each join's
-- one plan is the probe. An offset of 0 skips no row, so each `exists` answers as it did. The
-- server reads the view keyed on one end of an edge (refs.Edges, refs.Outgoing), where the probe
-- reads only the rows of the edges it answers; a read of the whole view probes once an edge.
--
-- Two more rules the arm carries.
--
-- It answers no row for a node whose current `state` is `gone`. For this release's own writes the
-- join already holds that, since the write that states a block gone deletes the row the join
-- reads. The test stays because the backfill's pointer guard takes this same test of the node
-- row (graphcensus' readBlockRefRows), and a guard that tested less than the arm would stamp a
-- pointer neither arm answers. The node row is the projection of the node's own tuples and a
-- restored block's is written `live` again, so the test is the row's, not the newest `state`
-- tuple's. It is the source's state alone: a mention of a block a version dropped is listed by
-- the old arm, whose `block_refs` row stands, so it is listed here too.
--
-- And a pointer a version dropped is retracted in the same statement that deletes its
-- `block_refs` row (`refs.replaceBlockRefs`), so the store and the row agree on it.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short
-- transaction, so a reference read waits behind it. The arm list re-stated is
-- `20261105014808`'s, with the four `node_kind is null` tests and the tuple arm added, and it
-- is the arm list a later migration re-states from. Nothing here refuses or rewrites a row.

-- What a block's pointer may point at, read from the schema rather than from the row: the
-- `targets` of the attribute node the link's kind names, or null where the attribute declares
-- none and where there is no such attribute at all, both of which mean no limit.
--
-- `block_ref_listed` takes it as the `to_types` the citing row used to carry alone, so the test
-- stays exactly the one the old arm makes, with a fresher input: retyping a target hides or
-- shows the link with no rewrite of the citing document's rows, and a change to an attribute's
-- targets reaches every link of that attribute at once, with no pass over documents nobody
-- edits.
--
-- The block's own type is `document_blocks.type`, as the type of the link's target is inside
-- `block_ref_listed`: a block's node carries the supertag `block`, the one of the closed list
-- that says the node is a block, because a paragraph's type is no supertag at all. From that
-- type the `attribute_of` tuple of migration 20261109051232 names every attribute the type
-- declares, and the attribute's own node holds what it points at. The tuple is followed rather
-- than the attribute id read, since the id's spelling is no reader's business.
--
-- It reads the schema as `pmdoc/schemanodes.Read` installs it: an `attribute_of` counts only
-- while the newest tuple of its (from, kind, to) asserts, and only from an attribute node whose
-- state is `live`. Read otherwise, an attribute a schema change retracted from a type, or ended,
-- would still limit the type's links, which the installed schema no longer does. A valid schema
-- declares one attribute of a name on a type (pmdoc's validateBlockSchema refuses a second), so
-- the order only makes the answer one where the store breaks that.
create function block_pointer_targets(from_node text, attribute_name text) returns text[] language sql stable as $$
  select case when attribute.attributes ? 'targets'
              then array(select jsonb_array_elements_text(attribute.attributes->'targets')) end
  from document_blocks source
  join graph_tuples held on held.kind = 'attribute_of' and held.to_kind = 'supertag'
                        and held.to_id = source.type and held.from_kind = 'attribute' and held.asserted
  join graph_nodes attribute on attribute.kind = held.from_kind and attribute.id = held.from_id
                            and attribute.state = 'live'
  where source.node_id = $1 and attribute.attributes->>'name' = $2
    and not exists (select 1 from graph_tuples later
                    where later.from_kind = held.from_kind and later.from_id = held.from_id
                      and later.kind = held.kind and later.to_kind = held.to_kind
                      and later.to_id = held.to_id and later.seq > held.seq)
  order by held.seq desc
  limit 1
$$;

-- graph_edges re-stated: every arm of 20261105014808, the four arms over the kinds this store
-- now writes answering only the rows that carry no node, and the one arm over the current tuples,
-- which answers an edge of those kinds only while the row that names it carries its node.
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
  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  where c.node_kind is null
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
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
  from messages where in_reply_to is not null
  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null
  from ask_followers
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
  from messages m where m.replies_to_block is not null
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
        end;
