-- 20261126050007_graph_edges_store_class_last.up.sql
--
-- `graph_edges` restated once more, with every change made to it since 20261109215947
-- restated it. Three migrations restated the view after that one, each from an arm list without
-- another's change, and a database runs each version once, in version order, so whichever of the
-- three ran last would leave the view without the others' changes:
--
--   20261126050002_graph_edges_asks_and_store_class: the tuple arm's class `store`, which
--   consults no projection row and is tested before the class `block_ref`, and one arm after the
--   tuple arm that answers the relations of that class out of a finding of a lens as
--   `lens_block`;
--
--   20261120122700_graph_edges_tuple_arm_filter: the tuple arm's sub-select keeps only the kinds
--   the arm renders before its `distinct on` sorts the tuples, and the outer
--   `edge.projects is not null` test is gone;
--
--   20261125163554_graph_edges_asks_and_tuple_arm_filter: 20261125163553's arm list with
--   20261120122700's filter, its test the six `when` arms of the `projects` case that list has.
--
-- A database runs the three in version order: 20261120122700, then 20261125163554, which states
-- the view with that filter and holds neither the class `store` nor the `lens_block` arm, then
-- 20261126050002, which states the view from 20261125163553's arm list and so drops the filter.
-- Each runs this version after all three, so every database ends on this definition.
--
-- The definition is 20261125163554's with 20261126050002's two changes: the `projects` case tests
-- the class `store` first, the sub-select's test holds the same condition as one more disjunct,
-- now seven, `case edge.projects` answers `store` with true, and the `lens_block` arm follows the
-- tuple arm. Every other arm, the column list and the order of the arms are 20261125163554's, and
-- `block_pointer_targets` is as 20261109215947 created it. This is a new migration rather than an
-- edit of any of the three because a database that ran one of them keeps the definition it wrote
-- until a later version restates it.
--
-- It changes no row a database that ran 20261126050002 alone answered. The sub-select's test reads
-- the tuple's from_kind, from_id and kind, all columns of the `distinct on` key, so, as
-- 20261120122700 argues, it keeps or drops a whole group and never changes which tuple of a kept
-- group is the newest, and the groups it drops are exactly those whose `projects` is null, which
-- 20261126050002's outer test dropped. The class `store` is tested before `block` here and after
-- it there, and no tuple is of both, since `in_document` is no kind of the class, so each tuple
-- keeps its class. Beside 20261125163554's view it answers the rows of the class `store` and the
-- relations out of a finding, which 20261126050002 answered and 20261125163554 never did.
--
-- The three arms over asks and ask_followers, and the tuple arm's three joins to those rows, stay
-- for one release, as 20261125163553 keeps them: the root's own pull request deletes them.
-- kept for one release: asks-arms
--
-- Re-stating the view holds `graph_edges` ACCESS EXCLUSIVE for the migration's short transaction,
-- so a reference read waits behind it. Nothing here reads, refuses or rewrites a row.
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
  -- The store's own arm. Its sub-select keeps only the kinds the arm renders, the test that
  -- `projects` is not null spelled over the tuple's own from_kind, from_id and kind, before the
  -- `distinct on` sorts the tuples: all three are columns of the `distinct on` key, so the test
  -- keeps or drops whole groups and changes no row the arm answers, and a read keyed on one end of
  -- an edge sorts only that node's tuples of those kinds.
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
             when not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
               and (tuple.kind in ('changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
                    or (tuple.kind in ('part_of', 'authored_by')
                        and tuple.from_kind in ('interaction', 'lens', 'risk', 'constraint'))) then 'store'
             when tuple.from_kind = 'block' and tuple.kind = 'in_document' then 'block'
             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
           end as projects
    from graph_tuples tuple
    where tuple.to_kind <> 'value'
      and ((not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
            and (tuple.kind in ('changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
                 or (tuple.kind in ('part_of', 'authored_by')
                     and tuple.from_kind in ('interaction', 'lens', 'risk', 'constraint'))))
           or (tuple.from_kind = 'block' and tuple.kind = 'in_document')
           or (tuple.from_kind = 'block' and tuple.kind <> 'part_of')
           or (tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by'))
           or (tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by'))
           or (tuple.from_kind = 'ask' and tuple.kind = 'followed_by')
           or (tuple.from_kind = 'ask' and tuple.kind = 'shaped'))
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
  where edge.asserted and node.state <> 'gone'
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
