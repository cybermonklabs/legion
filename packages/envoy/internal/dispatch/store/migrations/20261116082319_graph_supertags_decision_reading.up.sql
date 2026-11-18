-- 20261116082319_graph_supertags_decision_reading.up.sql
--
-- The block schema moves to version 5 (dispatch://INFRA-1150/spec, R1 and R2).
-- `pmdoc/schema/blocks.json` adds two typed blocks and one attribute of `ask`, each read and
-- created from version 5:
--
-- - `decision`, an agent's decision, holding any blocks (`block+`), with the attributes
--   `uncertainty` (an enum of low, medium and high with no default), `uncertainty_reason`,
--   `shaped` (a `block[]` with no targets), `from_ask` (of the kind `ask`), `replaced` (a bool,
--   default false), and three the server owns and markdown never carries: `decided_by`,
--   `kept_by` and `kept_text_sum`;
-- - `reading`, an agent's reading of a decision, holding paragraphs (`paragraph+`), which names
--   its parents, `decision` and `ask`: it stands only at the end of one of them, after that
--   block's own content, and in no node group;
-- - `ask`'s `shaped`, a `block[]` with no targets, after `copied_from_document`.
--
-- The file also gives `ask`'s `invalid` the field `markdown: false`, which the server read by the
-- attribute's name before: markdown never carries it. `ask` keeps its content rule.
--
-- A type is a node of the store, and the server installs the schema it builds from those nodes,
-- not the file (20261109051232), so the change reaches a store only as this migration, in the
-- release of the binary that handles version 5. The seed already holds a `supertag` node for
-- `decision` and one for `reading`, which the closed list names, carrying no attribute: this
-- moves both to block types. A build of version 4 reads the store's schema at its own version
-- (pmdoc.Install, asOfVersion): it leaves out every element whose `since` is 5, and its decoder
-- ignores the two fields it does not know (`parents`, `markdown`), so it installs the schema it
-- installed before this ran and keeps serving.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 5, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field. Every value a row carries is also stated as the tuple it is, a `value`
-- tuple whose kind is the attribute's name and whose far end is its value as text, since a node
-- row's attributes column is the projection of its tuples (R1).
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

-- The two supertag nodes become block types: what the seed builds for a type of the file, its
-- content rule, who draws it, the schema's version and its place among the types (callout, ask,
-- requirement, plan_task and check hold 0 to 4, so decision holds 5 and reading 6), with the
-- element versions the file gives each and, on `reading`, its parents. Each node is changed in
-- place and keeps the `made_at` and `made_by` the seed gave it.
update graph_nodes node
set attributes = node.attributes || types.attributes
from (values
  ('decision', '{"block_type": true, "content": "block+", "render": "host", "version": 5, "position": 5, "since": 5, "creatable_since": 5}'::jsonb),
  ('reading', '{"block_type": true, "content": "paragraph+", "render": "host", "version": 5, "position": 6, "since": 5, "creatable_since": 5, "parents": ["decision", "ask"]}'::jsonb)
) as types(id, attributes)
where node.kind = 'supertag' and node.id = types.id;

-- The attribute nodes. Each id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, with its name, its store name
-- and its position beside it. None is named for a base attribute, so each store name is its own
-- name (pmdoc/schemanodes.StoreName). A position is the attribute's place among its type's
-- attributes, counted from 0: `ask` holds ten before `shaped`, so `shaped` holds 10.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-decision-reading"}'::jsonb as writer
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'attribute', attribute.type_name || '/' || attribute.name, 'attribute', 'project', 'live',
       attribute.schema || jsonb_build_object('name', attribute.name, 'store_name', attribute.name, 'position', attribute.position),
       change.at, change.writer, 0
from change, (values
  ('decision', 'uncertainty', 0, '{"kind": "enum", "choices": ["low", "medium", "high"], "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'uncertainty_reason', 1, '{"kind": "string", "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'shaped', 2, '{"kind": "block[]", "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'from_ask', 3, '{"kind": "ask", "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'replaced', 4, '{"kind": "bool", "default": false, "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'decided_by', 5, '{"kind": "actor", "server": true, "markdown": false, "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'kept_by', 6, '{"kind": "actor", "server": true, "markdown": false, "since": 5, "creatable_since": 5}'::jsonb),
  ('decision', 'kept_text_sum', 7, '{"kind": "string", "server": true, "markdown": false, "since": 5, "creatable_since": 5}'::jsonb),
  ('ask', 'shaped', 10, '{"kind": "block[]", "since": 5, "creatable_since": 5}'::jsonb)
) as attribute(type_name, name, position, schema)
order by attribute.type_name, attribute.name
on conflict (kind, id) do update set
  supertag = excluded.supertag,
  visibility = excluded.visibility,
  state = excluded.state,
  attributes = excluded.attributes,
  source_event = excluded.source_event;

-- `ask`'s `invalid` takes `markdown: false` and nothing else changes on it.
update graph_nodes
set attributes = attributes || '{"markdown": false}'::jsonb
where kind = 'attribute' and id = 'ask/invalid';

-- The two base attributes each new attribute node carries, as the tuples they are, read back off
-- the row just written, as the seed reads them.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (values ('visibility', node.visibility), ('state', node.state)) as fact(kind, value)
where node.kind = 'attribute'
  and node.id in ('decision/uncertainty', 'decision/uncertainty_reason', 'decision/shaped', 'decision/from_ask',
                  'decision/replaced', 'decision/decided_by', 'decision/kept_by', 'decision/kept_text_sum', 'ask/shaped')
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
  and node.id in ('decision/uncertainty', 'decision/uncertainty_reason', 'decision/shaped', 'decision/from_ask',
                  'decision/replaced', 'decision/decided_by', 'decision/kept_by', 'decision/kept_text_sum', 'ask/shaped')
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'attribute_of'
      and held.to_kind = 'supertag' and held.to_id = split_part(node.id, '/', 1)
      and held.source_event = 0 and held.asserted
  )
order by node.id;

-- The values each node this migration writes or moves carries, as the tuples they are, read back
-- off the rows just written as the seed reads every type node's: a value the row holds as JSON
-- is written as that JSON's text and a string as itself, so `version` is `5`, `parents` is
-- `["decision", "ask"]` and `markdown` is `false`. A value a node already states as its tuple,
-- `ask/invalid`'s kind among them, is left as the seed wrote it.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-decision-reading"}'::jsonb as writer
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, change.at, change.writer, 0, null, true
from change, graph_nodes node
cross join lateral (
  select attribute.key, attribute.value #>> '{}' from jsonb_each(node.attributes) as attribute
) as fact(kind, value)
where (node.kind, node.id) in (('supertag', 'decision'), ('supertag', 'reading'),
                               ('attribute', 'decision/uncertainty'), ('attribute', 'decision/uncertainty_reason'),
                               ('attribute', 'decision/shaped'), ('attribute', 'decision/from_ask'),
                               ('attribute', 'decision/replaced'), ('attribute', 'decision/decided_by'),
                               ('attribute', 'decision/kept_by'), ('attribute', 'decision/kept_text_sum'),
                               ('attribute', 'ask/shaped'), ('attribute', 'ask/invalid'))
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.kind, node.id, fact.kind;

-- The version. Every block type's supertag node carries the version of the schema the store
-- holds, and the schema the nodes build takes the highest of them, so each one moves to 5, not
-- the two new types alone. A supertag that declares no block type carries no version and is left
-- alone.
update graph_nodes
set attributes = jsonb_set(attributes, '{version}', to_jsonb(5))
where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false);

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 5 its row now carries and the 4 before it stays
-- in its history. The tuples carry this migration's writer and its own time.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-decision-reading"}'::jsonb as writer
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
