-- 20261116121500_issue_tree_query_supertags.up.sql
--
-- Two supertags join the store's types (dispatch://INFRA-1253, R8 and R16; the root's R21 list):
--
-- - `query`, the node of a view of an issue's tree: the spec, the verification, the plan or the
--   decisions, each stating the supertags it takes and the node its walk starts at as values of
--   its own (graphstore.QueryNode);
-- - `query version`, a version of a query node, as `document version` is a version of a document
--   (graphstore.VersionFacts).
--
-- A type is a node of the store, so the two reach a store only as a migration, in the release of
-- the binary that writes their nodes (20261109051232). Neither declares a block type: a block of a
-- document never carries either, so neither carries a content rule, a renderer, a position or a
-- schema version, and the block schema every server installs from the type nodes stays at the
-- version the store holds (pmdoc/schemanodes reads the supertags that declare a block type alone).
-- Each is written as the seed writes a supertag of the closed list that declares no block type:
-- its node, carrying no attribute, and its two base attributes as the tuples they are.
--
-- Running this file again writes the same rows, as running the seed again does: the node upsert
-- moves every column but `made_at` and `made_by`, and each tuple insert appends only a fact the
-- node does not already carry. The migration records no event, so every row it writes carries the
-- `source_event` sentinel 0, and its rows carry a writer of their own, so a reader of the store can
-- tell them from the seed's. The triggers of 20261109051232 announce the change at its commit, and
-- every running server reinstalls a schema the change leaves as it was.
with change as (
  select now() as at, '{"kind": "system", "id": "issue-tree-query-supertags"}'::jsonb as writer
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'supertag', names.name, 'supertag', 'project', 'live', '{}'::jsonb, change.at, change.writer, 0
from change, (values ('query'), ('query version')) as names(name)
order by names.name
on conflict (kind, id) do update set
  supertag = excluded.supertag,
  visibility = excluded.visibility,
  state = excluded.state,
  attributes = excluded.attributes,
  source_event = excluded.source_event;

-- The two base attributes each node carries, as the tuples they are, read back off the rows just
-- written, as the seed reads them.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (values ('visibility', node.visibility), ('state', node.state)) as fact(kind, value)
where node.kind = 'supertag' and node.id in ('query', 'query version')
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.id, fact.kind;
