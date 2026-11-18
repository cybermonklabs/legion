-- 20261115130000_graph_supertags_element_versions.up.sql
--
-- The type nodes take the element versions of the block schema's manifest. On main, #2461 (two
-- builds serve one database and one message bus at once) made `pmdoc/schema/blocks.json` a
-- manifest in which each element may say from which schema version a build reads it (`since`)
-- and from which a build may create it (`creatable_since`). It gave `requirement`, `plan_task`
-- and `check` both numbers at 3, and `check`'s attribute `ci_check` both at 4. A type is a node of
-- the store, and the server installs the schema it builds from those nodes, not the file
-- (20261109051232), so the numbers reach a store only as a migration. Without this one the
-- schema the nodes build gives every type and attribute the numbers 1 and 1, which is not what
-- the manifest states: the version each element is read from goes into what a document needs a
-- page to read (pmdoc.DocumentSince), and so into which pages the fleet lets write.
--
-- The numbers are copied in as attribute values, under the names the manifest gives them: on the
-- supertag node of each of the three types, which the schema reader takes into the type
-- (pmdoc/schemanodes.Read), and on the attribute node check/ci_check, whose attributes are the
-- attribute's own JSON. A type or attribute the manifest gives no numbers carries none, as the
-- file writes none, and reads as 1 and 1. The manifest's `nodes` and `marks` get no node here:
-- they are the Proof schema's, which each build carries in its own code, and the server installs
-- its own beside the store's types (pmdoc.Install).
--
-- Each value is also stated as the tuple it is, a `value` tuple whose kind is the attribute's
-- name, because a node row's attributes column is the projection of its tuples (R1).
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two together field
-- by field. The runner applies a migration in one transaction, and the triggers of
-- 20261109051232 announce the change once, at its commit, so every running server installs the
-- numbers together.
--
-- Running this file again writes the same rows: each node takes the values it already holds, and
-- each tuple insert appends only a fact the node does not already carry. The migration records no
-- event, so every tuple it writes carries the `source_event` sentinel 0, and its tuples carry a
-- writer of their own, so a reader of the store can tell them from the seed's.

-- The values, on the node rows. Neither version is the name of a base attribute
-- (graphstore.IsBaseAttribute), so each is stored under its own name. A node is changed in place
-- and keeps the `made_at` and `made_by` the seed gave it.
update graph_nodes node
set attributes = node.attributes || jsonb_build_object('since', versions.since, 'creatable_since', versions.creatable_since)
from (values ('supertag', 'requirement', 3, 3),
             ('supertag', 'plan_task', 3, 3),
             ('supertag', 'check', 3, 3),
             ('attribute', 'check/ci_check', 4, 4)) as versions(kind, id, since, creatable_since)
where node.kind = versions.kind and node.id = versions.id;

-- The same values as the tuples they are, one of each kind for each node, as the values the
-- node rows above now carry. A node the store does not hold gets no tuple.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-element-versions"}'::jsonb as writer
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, change.at, change.writer, 0, null, true
from change, graph_nodes node
cross join lateral (values ('since', node.attributes->>'since'),
                           ('creatable_since', node.attributes->>'creatable_since')) as fact(kind, value)
where (node.kind, node.id) in (('supertag', 'requirement'), ('supertag', 'plan_task'),
                               ('supertag', 'check'), ('attribute', 'check/ci_check'))
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.kind, node.id, fact.kind;
