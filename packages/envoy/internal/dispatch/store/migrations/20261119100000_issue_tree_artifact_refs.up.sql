-- 20261119100000_issue_tree_artifact_refs.up.sql
--
-- The references a document makes are tuples from this release: its version index and
-- rebuild-refs append, in the transaction that writes its refs rows, a mentions tuple out of the
-- document's node to each far end a row it inserts names, dated at that row's created_at and
-- carrying the row's source_seq, and a retraction of each a row it deletes named (refs'
-- artifactMentions); a comment's, a message's and an ask's references write as before. This
-- re-states graph_edges so the arm over refs answers a document's row only while the document's
-- artifacts row names no node, or while its far end is past the store's address bound, of which no
-- tuple is stated (store.ReferenceNotStated), and every other source's row as before; and so the
-- store answers the rest, while the refs row that names each edge stands: a row a process of the
-- release before deletes takes its edge with it, and a row it writes beside a document this release
-- wrote, which states no tuple, is answered by neither until the backfill states it (graphcensus'
-- pass over the references, which the census reads as a difference first). The tuple arm reads a
-- document's mentions through a join to the artifacts row naming the document's node, which names
-- the document by its uuid, as the arm over refs does and every reader of the view reads it, and
-- beside the refs row of the pair, which dates and sequences the edge as the arm over refs does: a
-- row rebuild-refs inserted with no source_seq takes the one a later version's event stamps on it.
-- The log's other tuples are read as before.
--
-- The statement below is store.GraphEdgesView(store.GraphEdgesArtifactRefs), held to it byte for
-- byte by TestEveryGraphEdgesMigrationHoldsTheViewItsSourceGives. The arm over refs probes the
-- document's row under an OR, and the tuple arm probes the refs row of each document's mention,
-- each ending in `offset 0`, so a graph_edges read of one node probes refs and artifacts through
-- their own indexes and scans neither. It adds no column and drops no arm. Re-stating the view holds
-- graph_edges ACCESS EXCLUSIVE for the migration's short transaction, so a reference read waits
-- behind it. Nothing here refuses or rewrites a row.
create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from refs r
  where (r.from_kind <> 'artifact' or octet_length(r.to_id) > 1024
      or not exists (
        select 1 from artifacts mentioning
        where mentioning.id::text = r.from_id and mentioning.node_kind is not null
        offset 0))
  union all
  select 'issue', i.key, 'child_of', 'issue', i.parent_key, i.created_at, null
  from issues i
  where i.parent_key is not null
    and (i.node_kind is null or not exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = i.node_kind and tuple.from_id = i.node_address and tuple.kind = 'child_of'
          and tuple.to_kind = 'issue' and tuple.to_id = i.parent_key
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted
      offset 0))
  union all
  select 'issue', issue_key, 'blocked_by', 'issue', target_key, created_at, null
  from issue_links where kind = 'blocked_by' and node_kind is null
  union all
  select 'artifact', id::text, 'attached_to', 'issue', issue_key, created_at, null
  from artifacts where issue_key is not null and node_kind is null
  union all
  select 'artifact', a.id::text, 'attached_to', 'issue', a.issue_key, a.created_at, null
  from artifacts a
  join graph_nodes node on node.kind = a.node_kind and node.id = a.node_address
  where a.issue_key is not null and node.state <> 'gone'
    and exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = a.node_kind and tuple.from_id = a.node_address and tuple.kind = 'part_of'
          and tuple.to_kind = 'issue' and tuple.to_id = a.issue_key
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted)
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
    and (c.node_kind is null or not exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = c.node_kind and tuple.from_id = c.node_address and tuple.kind = 'part_of'
          and tuple.to_kind = 'component' and tuple.to_id = c.project_key || '/' || c.parent
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted
      offset 0))
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id, s.imported_at, null
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  where d.node_kind is null
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, null
  from issue_component_members m join issue_components c using (issue_key)
  where m.node_kind is null
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
           tuple.node_id,
           case
             when tuple.kind = 'in_document' then 'part_of'
             when tuple.from_kind = 'issue' and tuple.kind = 'needs' and tuple.to_kind = 'issue' then 'blocked_by'
             else tuple.kind
           end as kind,
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
             when tuple.from_kind = 'issue' and tuple.kind in ('duplicate_of', 'filed_from') then 'store'
             when tuple.from_kind = 'issue' and tuple.kind = 'child_of' and tuple.to_kind = 'issue' then 'issue_parent'
             when tuple.from_kind = 'issue' and tuple.kind = 'needs' and tuple.to_kind = 'issue' then 'issue_link'
             when tuple.from_kind = 'component' and tuple.kind = 'part_of' and tuple.to_kind = 'component' then 'component_parent'
             when tuple.from_kind = 'component' and tuple.kind = 'depends_on' and tuple.to_kind = 'component' then 'component_depends'
             when tuple.from_kind = 'issue' and tuple.kind = 'affects' and tuple.to_kind = 'component' then 'component_member'
             when tuple.from_kind = 'artifact' and tuple.kind = 'mentions' then 'artifact_ref'
             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
           end as projects
    from (
      select tuple.from_kind, tuple.from_id, tuple.from_id as node_id, tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where tuple.from_kind <> 'artifact' or tuple.kind <> 'mentions'
      union all
      select 'artifact', mentioning.id::text, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id,
             projected.created_at, projected.source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      join artifacts mentioning on mentioning.ref_key = tuple.from_id and mentioning.node_kind is not null
      cross join lateral (
        select stated.created_at, stated.source_seq from refs stated
        where stated.from_kind = tuple.from_kind and stated.from_id = mentioning.id::text
          and stated.to_kind = tuple.to_kind and stated.to_id = tuple.to_id
        offset 0) projected
      where tuple.from_kind = 'artifact' and tuple.kind = 'mentions'
    ) tuple
    where tuple.to_kind <> 'value'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.node_id
  where edge.asserted and node.state <> 'gone' and edge.projects is not null
    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
    and case edge.projects
          when 'store' then true
          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'issue_parent' then exists (
            select 1 from issues projected
            where projected.key = edge.from_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id and projected.parent_key = edge.to_id
            offset 0)
          when 'issue_link' then exists (
            select 1 from issue_links projected
            where projected.issue_key = edge.from_id and projected.kind = 'blocked_by'
              and projected.target_key = edge.to_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id
            offset 0)
          when 'component_parent' then exists (
            select 1 from components projected
            where projected.project_key || '/' || projected.id = edge.from_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id and projected.project_key || '/' || projected.parent = edge.to_id
            offset 0)
          when 'component_depends' then exists (
            select 1 from component_depends projected
            where projected.project_key || '/' || projected.from_id = edge.from_id
              and projected.project_key || '/' || projected.to_id = edge.to_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id
            offset 0)
          when 'component_member' then exists (
            select 1 from issue_component_members projected
            where projected.issue_key = edge.from_id and projected.project_key || '/' || projected.component_id = edge.to_id
              and projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'artifact_ref' then true
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
