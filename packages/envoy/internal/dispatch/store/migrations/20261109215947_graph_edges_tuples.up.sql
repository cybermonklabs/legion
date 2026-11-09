-- 20261109215947_graph_edges_tuples.up.sql
--
-- The view reads the store. `graph_edges` gains one arm over the current tuples, and the four
-- arms whose relations the node and tuple store now writes learn when to stop.
--
-- One arm answers every kind the store writes that is an edge today: for each
-- (from_kind, from_id, kind, to_kind, to_id) the newest tuple, which is an edge only while that
-- newest one asserts. `to_kind <> 'value'` keeps an attribute's value out, since a value is no
-- node and is the far end of no edge, and it is also the predicate three of the store's indexes
-- are partial on, so the arm's `distinct on` walks `graph_tuples_current` in its own order.
--
-- It renders a stated list and nothing else: a block's `in_document`, rendered as the `part_of`
-- to its document the view has always answered; a block's `mentions` and its pointer kinds, the
-- two kinds `block_refs` holds; and a comment's `anchored_to` and `owned_by`. That is what the
-- four arms it joins carry. Every other kind the store writes - `authored_by`, `delivered_to`,
-- `held_by`, the alias tuples, `attribute_of`, and a session's or a page's `part_of` - is no arm
-- of this view today, and adding one would add edges and backlink counts to every reader of a
-- comment; the child that owns each of those kinds decides when it joins the view. A `part_of`
-- whose `from_kind` is `block` is left out for the same reason: the view has no block-to-block
-- row today, and a top-level block, whose parent is the document itself, would otherwise be
-- rendered twice, once from that tuple and once from `in_document`.
--
-- The cutover is per row and by the tuple's own kind: the arm answers a kind only once that
-- kind's `graph_store_cutover` row reads true, and each of the four old arms answers only while
-- it reads false. `in_document` is governed by the `block` row, a block's pointer kinds by the
-- `block_ref` row, and a comment's two by the `comment` row. So at the moment the backfill
-- completes a kind the tuple arm answers that kind's rows and the old arm answers none, and the
-- view answers each row exactly once at every instant. The four `comments` arms that thread a
-- reply are untouched: a later child writes those relations.
--
-- Two rules the arm carries.
--
-- It answers no row for a node whose current `state` is `gone`, which keeps it level with the
-- old arms wherever a projection row is really deleted: a block a version no longer holds keeps
-- its node and its `in_document` tuple, since no tuple is ever deleted, and its row in
-- `document_blocks` is gone. The node row is the projection of the node's own tuples and a
-- restored block's is written `live` again, so the test is the row's, not the newest `state`
-- tuple's. It is the source's state alone: a mention of a block a version dropped is listed by
-- the old arm, whose `block_refs` row stands, so it is listed here too.
--
-- And a pointer a version dropped is retracted in the same statement that deletes its
-- `block_refs` row (`refs.replaceBlockRefs`), so the tuple arm and the old arm lose it together.
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short
-- transaction, so a reference read waits behind it. The arm list re-stated is
-- `20261105014808`'s, with the four cutover tests and the tuple arm added, and it is the arm
-- list a later migration re-states from. Nothing here refuses or rewrites a row.

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
create function block_pointer_targets(from_node text, attribute_name text) returns text[] language sql stable as $$
  select case when attribute.attributes ? 'targets'
              then array(select jsonb_array_elements_text(attribute.attributes->'targets')) end
  from document_blocks source
  join graph_tuples held on held.kind = 'attribute_of' and held.to_kind = 'supertag'
                        and held.to_id = source.type and held.asserted
  join graph_nodes attribute on attribute.kind = held.from_kind and attribute.id = held.from_id
  where source.node_id = $1 and attribute.attributes->>'name' = $2
  limit 1
$$;

-- graph_edges re-stated: every arm of 20261105014808, the four arms over the kinds this store
-- now writes stopping at their kind's cutover, and the one arm over the current tuples.
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
  where not (select complete from graph_store_cutover where kind = 'comment')
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  where not (select complete from graph_store_cutover where kind = 'comment')
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
    and not (select complete from graph_store_cutover where kind = 'block_ref')
  union all
  select 'block', d.node_id, 'part_of', 'artifact', a.ref_key, d.created_at, null
  from document_blocks d join artifacts a on a.id = d.artifact_id
  where not (select complete from graph_store_cutover where kind = 'block')
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
    select distinct on (tuple.from_kind, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id)
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
           end as cutover
    from graph_tuples tuple
    where tuple.to_kind <> 'value'
    order by tuple.from_kind, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id, tuple.seq desc
  ) edge
  join graph_store_cutover cutover on cutover.kind = edge.cutover and cutover.complete
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
    and (edge.cutover <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id));
