-- 20261126050002_graph_edges_asks_and_store_class.up.sql
--
-- graph_edges answers the relations the node and tuple store holds with no projection
-- row behind them. The kinds this child adds to the store - an interaction, a lens, a risk and a
-- constraint - are tied by tuples no table projects: a node a run changed points at the run's
-- interaction (`changed_by_run`), a lens at its lens kind (`has_kind`) and at the lens that
-- triggered it (`triggered_by`), a requirement block at a constraint it is held to
-- (`constrained_by`), and a risk at each check block that covers it (`prevented_by`); and each of
-- the four is placed by its `part_of` and names whoever wrote it by its `authored_by`. The tuple
-- arm classes each row by the projection row it consults (`projects`) and drops a row whose class
-- is null, so before this migration the view answers none of these.
--
-- This is the one migration of this child that states graph_edges. It re-states the arm list of
-- 20261125163553_graph_edges_asks (cybermonklabs/legion#4173) - its `node_kind is null` tests on the
-- two asks arms and the ask_followers arm, and the tuple arm's cases and joins for an ask's
-- relations - with two changes:
--
--   - The tuple arm gains the class `store`, which consults no projection row. A tuple is of the
--     class when its kind is `changed_by_run`, `has_kind`, `triggered_by`, `constrained_by` or
--     `prevented_by`, out of any node, or `part_of` or `authored_by`, out of a node of kind
--     `interaction`, `lens`, `risk` or `constraint`, unless it is out of a finding of a lens,
--     which the arm below answers. The class is tested before `block_ref`, so a requirement
--     block's `constrained_by` is not read as a block reference, which would look for a
--     `block_refs` row that no writer projects it into. An `authored_by` out of any other node -
--     a comment's - stays out of the view, as it was, and so does a session's, a page's or a
--     block's `part_of`. None of the kinds 20261125163553 cases out of an ask - `anchored_to`,
--     `owned_by`, `followed_by` and `shaped` - is of the class, so the tuple arm classes each such
--     tuple as 20261125163553 classed it.
--
--   - One arm more, over the store's current tuples, answers every relation of the class `store`
--     out of a finding of a lens, with the literal from_kind `lens_block` at the finding's own id.
--     A finding is a node of kind and supertag `block` whose id names its lens,
--     `lens/<uuid>#<block id>`, and the graph addresses it as `lens_block`
--     (`dispatch://lens/<uuid>#b-<id>`), the kind the read registry knows it by. The tuple arm
--     writes each from_kind as the store keys it, which would name a finding `block`: a reader
--     would look it up among a document's blocks and find nothing, and the read registry's
--     census, which reads the node kinds the view names as literals, would read no `lens_block`
--     at all. The arm's rules are the tuple arm's: the newest tuple of each relation while that
--     one asserts, out of a node whose state is not `gone`, its to side and dates rendered as the
--     tuple arm renders them. A tuple of those kinds out of a finding falls past the class `store`
--     to the tuple arm's block-reference class, which answers a tuple only while a `block_refs`
--     row of the pair carries the block's node, and no such row names a finding, so no relation
--     is answered twice.
--
-- The two changes are one migration because a migrate pass applies each file in a transaction of
-- its own, in version order, and applies every file the database does not hold, one below a
-- version it already holds too. A migration of this child that stated the view from an arm list
-- without 20261125163553's, applied to a database that already holds 20261125163553, would leave
-- the view without the ask arms a build of that release reads, from that migration's commit until
-- a later one put them back. With this child's arms stated here alone, a pass re-states the view
-- only to 20261125163553's arm list or to this one, which holds every arm of that one, so no pass
-- leaves the view without an arm a live build reads.
--
-- It drops no arm of 20261125163553 and changes no row an arm answered before: the rows it adds
-- are tuples of the class `store` and relations out of a finding, which only this child writes -
-- its writers, the lens catalogue its migrations 20261123174710 and 20261123174720 seed, and the
-- `authored_by` of the interaction node `envoy-dispatch backfill-graph-store` gives each run
-- recorded before this release - and every other row of every arm is answered as it was. So a
-- build of the release before, which reads the view this one states, reads every row it read, and
-- the kinds `refs.TupleKinds` lists grow by the seven of the class. Re-stating the view holds
-- graph_edges ACCESS EXCLUSIVE for this migration's short transaction, so a reference read waits
-- behind it. It creates nothing, and refuses and rewrites no row.

-- graph_edges re-stated: every arm of 20261125163553, its tuple arm answering the store's own
-- relations as the class `store`, and one arm after it answering those out of a finding of a lens
-- as `lens_block`.
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
  from messages where in_reply_to is not null
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
             when not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
               and (tuple.kind in ('changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
                    or (tuple.kind in ('part_of', 'authored_by')
                        and tuple.from_kind in ('interaction', 'lens', 'risk', 'constraint'))) then 'store'
             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
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
          when 'store' then true
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
        end
  union all
  select 'lens_block', finding.from_id, finding.kind, finding.to_kind, finding.to_id, finding.created_at, finding.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           tuple.kind,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.from_kind = 'block' and tuple.from_id like 'lens/%'
      and tuple.kind in ('part_of', 'authored_by', 'changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
      and tuple.to_kind <> 'value'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) finding
  join graph_nodes node on node.kind = finding.from_kind and node.id = finding.from_id
  where finding.asserted and node.state <> 'gone';
