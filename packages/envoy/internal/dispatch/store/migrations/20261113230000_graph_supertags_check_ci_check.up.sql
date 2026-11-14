-- 20261113230000_graph_supertags_check_ci_check.up.sql
--
-- The block schema moves to version 4. `pmdoc/schema/blocks.json` on main gave the `check` type a
-- sixth attribute, `ci_check`, a string declared after `negative_control`, and raised the file's
-- version from 3 to 4 (#3972). A type is a node of the store, and the server installs the schema
-- it builds from those nodes, not the file, so the change reaches a store only as a migration: a
-- type changes by a migration, in the release of the binary that handles its version
-- (20261109051232). Without this one a binary built for version 4 installs version 3, whose
-- `check` declares no `ci_check`, so the attribute that binary's routes read and write is no
-- attribute of the schema it validates against.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 4, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new attribute and the new version together, or neither. The
-- triggers of 20261109051232 announce the change once, at that commit, however many rows it
-- writes, and every running server installs the schema the nodes then build.
--
-- Running this file again writes the same rows, as running the seed again does: the node upsert
-- moves every column but `made_at` and `made_by`, each tuple insert appends only a fact the node
-- does not already carry, and the version is set to the value it already holds. The migration
-- records no event, so every row it writes carries the `source_event` sentinel 0, and its rows
-- carry a writer of their own, so a reader of the store can tell them from the seed's.

-- The attribute node. Its id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, `{"kind": "string"}`, with its
-- name, its store name and its position beside it. `ci_check` is the name of no base attribute,
-- so its store name is its own name (pmdoc/schemanodes.StoreName). The position is its place
-- among the type's attributes, counted from 0: serves, surface, run, when and negative_control
-- hold 0 to 4, so ci_check holds 5.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-check-ci-check"}'::jsonb as writer
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'attribute', 'check/ci_check', 'attribute', 'project', 'live',
       '{"kind": "string"}'::jsonb || jsonb_build_object('name', 'ci_check', 'store_name', 'ci_check', 'position', 5),
       change.at, change.writer, 0
from change
on conflict (kind, id) do update set
  supertag = excluded.supertag,
  visibility = excluded.visibility,
  state = excluded.state,
  attributes = excluded.attributes,
  source_event = excluded.source_event;

-- The two base attributes the node carries, as the tuples they are, read back off the row just
-- written, as the seed reads them.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (values ('visibility', node.visibility), ('state', node.state)) as fact(kind, value)
where node.kind = 'attribute' and node.id = 'check/ci_check'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by fact.kind;

-- What ties the attribute to `check`. A reader of the schema follows this tuple, never the
-- spelling of the node's id, so without it the node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'attribute_of', 'supertag', 'check', node.made_at, node.made_by, 0, null, true
from graph_nodes node
where node.kind = 'attribute' and node.id = 'check/ci_check'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'attribute_of'
      and held.to_kind = 'supertag' and held.to_id = 'check'
      and held.source_event = 0 and held.asserted
  );

-- The version. The seed wrote the schema's version on every block type's supertag node, and the
-- schema the nodes build takes the highest of them. Each one moves to 4, not `check` alone, so
-- every type node goes on carrying the version of the schema the store holds, as the seed left
-- them. A supertag that declares no block type carries no version and is left alone.
update graph_nodes
set attributes = jsonb_set(attributes, '{version}', to_jsonb(4))
where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false);
