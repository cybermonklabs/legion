-- 20261116163700_issue_tree_block_schema.up.sql
--
-- The block schema moves to version 6 (dispatch://INFRA-1253/spec, R2, R3, R10, R11, R18 and
-- R21). `pmdoc/schema/blocks.json` adds, each read and created from version 6:
--
-- - the `criterion` block type, a person's statement of what they want to be able to do, holding
--   paragraphs (`paragraph+`) and carrying `parent`;
-- - `parent`, a `block[]` attribute holding at most one reference, whose `items` are ask, comment
--   and message and which names no `targets`, on `criterion`, `requirement`, `check` and
--   `plan_task`, after each type's other attributes: the one place a node's parent is written
--   where its rendering cannot give it;
-- - `requirement`'s `serves`, a `block[]` with the targets criterion and requirement;
-- - `plan_task`'s `size`, of the kind `days`, after `needs`.
--
-- It also widens `plan_task`'s `needs`, which every version has read: its `scope` of `document`
-- goes, so it names a plan task of its issue or of an issue above it, and it gains `items` of
-- issue, so it names an issue under its issue as well, an item kind read and created from version
-- 6 (`item_versions`). A build of version 5 reads the store's schema at its own version
-- (pmdoc.Install, asOfVersion), which leaves out every element whose `since` is 6, and its decoder
-- ignores `items` and `item_versions`, so it reads `needs` naming no scope, which takes more than it
-- took. It would read an issue a `needs` names as a value outside the schema, not as newer content,
-- so a build of version 6 introduces one only while every process serving the database reads
-- version 6 (pmdoc.CheckBlockPointers under docs' schemaFleet.creatable), as a page of version 6
-- is writable only then.
--
-- A type is a node of the store, and the server installs the schema it builds from those nodes,
-- not the file (20261109051232), so the change reaches a store only as this migration, in the
-- release of the binary that handles version 6. The seed already holds a `supertag` node for
-- `criterion`, which the closed list names, carrying no attribute: this makes it a block type.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 6, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field. Every value a row carries is also stated as the tuple it is, and the
-- value `needs` stops carrying is retracted, as the newest tuple of an attribute decides whether a
-- node holds it (graphstore.CurrentValue).
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes, and the triggers of 20261109051232 announce it once, at that commit, so every running
-- server installs the new types and the new version together.
--
-- Running this file again writes the same rows: each node takes the values it already holds,
-- each tuple insert appends only a fact the node does not already carry, and the version is set
-- to the value it already holds. The migration records no event, so every tuple it writes
-- carries the `source_event` sentinel 0, and its rows carry a writer of their own, so a reader of
-- the store can tell them from the seed's.

-- The supertag node becomes a block type: what the seed builds for a type of the file, its
-- content rule, who draws it, the schema's version and its place among the types (callout, ask,
-- requirement, plan_task, check, decision and reading hold 0 to 6, so criterion holds 7), with
-- its element versions. The node is changed in place and keeps the `made_at` and `made_by` the
-- seed gave it.
update graph_nodes
set attributes = attributes || '{"block_type": true, "content": "paragraph+", "render": "host", "version": 6, "position": 7, "since": 6, "creatable_since": 6}'::jsonb
where kind = 'supertag' and id = 'criterion';

-- The attribute nodes. Each id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, with its name, its store name
-- and its position beside it. None is named for a base attribute, so each store name is its own
-- name (pmdoc/schemanodes.StoreName). A position is the attribute's place among its type's
-- attributes, counted from 0: check holds six before parent, plan_task two before size.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-issue-tree"}'::jsonb as writer
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'attribute', attribute.type_name || '/' || attribute.name, 'attribute', 'project', 'live',
       attribute.schema || jsonb_build_object('name', attribute.name, 'store_name', attribute.name, 'position', attribute.position),
       change.at, change.writer, 0
from change, (values
  ('criterion', 'parent', 0, '{"kind": "block[]", "items": ["ask", "comment", "message"], "since": 6, "creatable_since": 6}'::jsonb),
  ('requirement', 'serves', 0, '{"kind": "block[]", "targets": ["criterion", "requirement"], "since": 6, "creatable_since": 6}'::jsonb),
  ('requirement', 'parent', 1, '{"kind": "block[]", "items": ["ask", "comment", "message"], "since": 6, "creatable_since": 6}'::jsonb),
  ('plan_task', 'size', 2, '{"kind": "days", "since": 6, "creatable_since": 6}'::jsonb),
  ('plan_task', 'parent', 3, '{"kind": "block[]", "items": ["ask", "comment", "message"], "since": 6, "creatable_since": 6}'::jsonb),
  ('check', 'parent', 6, '{"kind": "block[]", "items": ["ask", "comment", "message"], "since": 6, "creatable_since": 6}'::jsonb)
) as attribute(type_name, name, position, schema)
order by attribute.type_name, attribute.name
on conflict (kind, id) do update set
  supertag = excluded.supertag,
  visibility = excluded.visibility,
  state = excluded.state,
  attributes = excluded.attributes,
  source_event = excluded.source_event;

-- `plan_task`'s `needs` widens: its `scope` goes and it takes `items` of issue, read and created
-- from version 6.
update graph_nodes
set attributes = (attributes - 'scope') || '{"items": ["issue"], "item_versions": {"issue": {"since": 6, "creatable_since": 6}}}'::jsonb
where kind = 'attribute' and id = 'plan_task/needs';

-- The two base attributes each new attribute node carries, as the tuples they are, read back off
-- the row just written, as the seed reads them.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (values ('visibility', node.visibility), ('state', node.state)) as fact(kind, value)
where node.kind = 'attribute'
  and node.id in ('criterion/parent', 'requirement/serves', 'requirement/parent', 'plan_task/size', 'plan_task/parent', 'check/parent')
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.id, fact.kind;

-- What ties each attribute to its type. A reader of the schema follows this tuple, never the
-- spelling of the node's id, so without it a node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'attribute_of', 'supertag', split_part(node.id, '/', 1), node.made_at, node.made_by, 0, null, true
from graph_nodes node
where node.kind = 'attribute'
  and node.id in ('criterion/parent', 'requirement/serves', 'requirement/parent', 'plan_task/size', 'plan_task/parent', 'check/parent')
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'attribute_of'
      and held.to_kind = 'supertag' and held.to_id = split_part(node.id, '/', 1)
      and held.source_event = 0 and held.asserted
  )
order by node.id;

-- The values each node this migration writes or moves carries, as the tuples they are, read back
-- off the rows just written as the seed reads every type node's: a value the row holds as JSON
-- is written as that JSON's text and a string as itself, so `version` is `6` and `items` is
-- `["issue"]`. A value a node already states as its tuple is left as the seed wrote it.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-issue-tree"}'::jsonb as writer
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, change.at, change.writer, 0, null, true
from change, graph_nodes node
cross join lateral (
  select attribute.key, attribute.value #>> '{}' from jsonb_each(node.attributes) as attribute
) as fact(kind, value)
where (node.kind, node.id) in (('supertag', 'criterion'),
                               ('attribute', 'criterion/parent'), ('attribute', 'requirement/serves'),
                               ('attribute', 'requirement/parent'), ('attribute', 'plan_task/size'),
                               ('attribute', 'plan_task/parent'), ('attribute', 'check/parent'),
                               ('attribute', 'plan_task/needs'))
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.kind, node.id, fact.kind;

-- The scope `needs` no longer carries, as the retraction it is: a later tuple under the
-- attribute's name that asserts nothing, so the newest tuple of `scope` out of the node says it
-- holds none. The seed's assertion stays in its history.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-issue-tree"}'::jsonb as writer
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'scope', 'value', 'document', change.at, change.writer, 0, null, false
from change, graph_nodes node
where node.kind = 'attribute' and node.id = 'plan_task/needs'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'scope'
      and held.to_kind = 'value' and held.to_id = 'document'
      and held.source_event = 0 and not held.asserted
  );

-- The version. Every block type's supertag node carries the version of the schema the store
-- holds, and the schema the nodes build takes the highest of them, so each one moves to 6, not
-- the new type alone. A supertag that declares no block type carries no version and is left
-- alone.
update graph_nodes
set attributes = jsonb_set(attributes, '{version}', to_jsonb(6))
where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false);

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 6 its row now carries and the 5 before it stays
-- in its history. The tuples carry this migration's writer and its own time.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-issue-tree"}'::jsonb as writer
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'version', 'value', node.attributes->>'version', change.at, change.writer, 0, null, true
from change, graph_nodes node
where node.kind = 'supertag' and coalesce((node.attributes->>'block_type')::boolean, false)
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'version'
      and held.to_kind = 'value' and held.to_id = node.attributes->>'version'
      and held.source_event = 0 and held.asserted
  )
order by node.id;
