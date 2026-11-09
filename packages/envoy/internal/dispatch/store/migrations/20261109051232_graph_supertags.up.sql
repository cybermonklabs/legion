-- 20261109051232_graph_supertags.up.sql
--
-- A type becomes data. Every supertag of the closed list is a node of the store, and every
-- attribute a supertag holds is a node of its own, tied to its supertag by an `attribute_of`
-- tuple. The server reads these nodes at start and installs the block schema it builds from
-- them (internal/dispatch/pmdoc/schemanodes), so `pmdoc/schema/blocks.json` is the seed of these
-- rows and nothing more: the file is no longer the only place a type exists. No route and no
-- command writes one of these nodes; a type changes by a migration, in the release of the binary
-- that handles its version.
--
-- The block schema's own text is copied in below rather than read from the file, because a
-- migration states what the database held at one version and must not change when the file does.
-- The test that holds the two together is
-- TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt: it compares these nodes with
-- blocks.json field by field and fails in either direction, so a change to the file that ships
-- without a migration of its own is caught there.
--
-- It is read as `json` and not as `jsonb`: a JSON object has no order of its own in `jsonb`,
-- which sorts the keys, and an attribute's position is the order its type declares it in, which
-- is the order a typed block's opening line writes its attributes in. `json` keeps the text as
-- written, so `json_each ... with ordinality` reads that order back.
--
-- Running this file again writes the same rows: the node upsert moves every column but `made_at`
-- and `made_by`, as the store's own writer does, and each tuple insert appends only a fact the
-- node does not already carry. The seed records no event, so every tuple it writes carries the
-- `source_event` sentinel 0, whose rows the two repeat keys of 20261109013400 leave alone.

-- The channel a change to these nodes announces itself on at commit, which each server's
-- reinstall waits on (pmdoc/schemanodes.Channel). A notification carries no payload: the reader
-- reads the whole schema back, and Postgres delivers one notification per channel and payload
-- however many rows a transaction changed, so a seed of 48 rows wakes each process once.
create or replace function block_schema_changed() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_block_schema', '');
  return null;
end $$;

-- Two triggers, because one row trigger's `when` cannot read NEW on a delete nor OLD on an
-- insert. The `when` is what keeps this off the write path of every other node: a block, a
-- comment or a person write tests one column and calls nothing.
drop trigger if exists graph_nodes_block_schema_written on graph_nodes;
create trigger graph_nodes_block_schema_written
after insert or update on graph_nodes
for each row when (new.kind in ('supertag', 'attribute'))
execute function block_schema_changed();

drop trigger if exists graph_nodes_block_schema_removed on graph_nodes;
create trigger graph_nodes_block_schema_removed
after delete on graph_nodes
for each row when (old.kind in ('supertag', 'attribute'))
execute function block_schema_changed();

-- The nodes. One `supertag` node for each name of the closed list and no other, and one
-- `attribute` node for each attribute the block schema declares, whose id is
-- `<supertag>/<name>`. A supertag the block schema declares carries that type's content rule,
-- who draws it and the schema's version; one it does not declare carries no attribute of its
-- own, and the kinds that move in a later child bring theirs with them.
with seed as (
  select
    now() as at,
    '{"kind": "system", "id": "block-schema-seed"}'::jsonb as writer,
    $schema${
  "version": 3,
  "types": [
    {
      "name": "callout",
      "content": "block+",
      "render": "host",
      "attributes": {
        "kind": {
          "kind": "enum",
          "choices": ["note", "warning"],
          "default": "note"
        },
        "title": {
          "kind": "string",
          "default": ""
        }
      }
    },
    {
      "name": "ask",
      "content": "paragraph+ bullet_list?",
      "render": "host",
      "attributes": {
        "urgency": {
          "kind": "enum",
          "choices": ["low", "med", "high", "blocking"],
          "default": "med"
        },
        "multiple": {
          "kind": "bool",
          "default": false
        },
        "state": {
          "kind": "enum",
          "choices": ["open", "answered", "resolved"],
          "default": "open",
          "server": true
        },
        "answered_by": {
          "kind": "actor",
          "server": true
        },
        "answered_at": {
          "kind": "timestamp",
          "server": true
        },
        "selected": {
          "kind": "string[]",
          "server": true
        },
        "answer": {
          "kind": "string",
          "server": true
        },
        "invalid": {
          "kind": "string",
          "server": true
        },
        "copied_from": {
          "kind": "string",
          "server": true
        },
        "copied_from_document": {
          "kind": "string",
          "server": true
        }
      }
    },
    {
      "name": "requirement",
      "content": "paragraph+",
      "render": "host",
      "attributes": {}
    },
    {
      "name": "plan_task",
      "content": "paragraph+",
      "render": "host",
      "attributes": {
        "serves": {
          "kind": "block[]",
          "targets": ["requirement"]
        },
        "needs": {
          "kind": "block[]",
          "targets": ["plan_task"],
          "scope": "document"
        }
      }
    },
    {
      "name": "check",
      "content": "paragraph+",
      "render": "host",
      "attributes": {
        "serves": {
          "kind": "block[]",
          "targets": ["requirement"]
        },
        "surface": {
          "kind": "enum",
          "choices": ["browser", "cli", "api"]
        },
        "run": {
          "kind": "string"
        },
        "when": {
          "kind": "enum",
          "choices": ["before_merge", "after_merge", "default_branch"],
          "default": "before_merge"
        },
        "negative_control": {
          "kind": "string"
        }
      }
    }
  ]
}$schema$::json as document
),
supertag_names (name) as (
  values ('issue'), ('document'), ('document version'), ('block'), ('callout'), ('ask'),
         ('requirement'), ('plan_task'), ('check'), ('result'), ('decision'), ('reading'),
         ('criterion'), ('proposed'), ('constraint'), ('lens'), ('risk'), ('comment'),
         ('message'), ('person'), ('session'), ('role'), ('agent'), ('page'), ('interaction'),
         ('delivery_attempt'), ('component'), ('supertag'), ('attribute')
),
block_types as (
  select
    type.value->>'name' as name,
    jsonb_build_object(
      'block_type', true,
      'content', type.value->>'content',
      'render', type.value->>'render',
      'version', (seed.document->>'version')::int,
      'position', type.ordinality - 1
    ) as attributes
  from seed, json_array_elements(seed.document->'types') with ordinality as type(value, ordinality)
),
block_attributes as (
  select
    type.value->>'name' as type_name,
    attribute.key as name,
    attribute.value::jsonb || jsonb_build_object('name', attribute.key, 'position', attribute.ordinality - 1) as attributes
  from seed,
       json_array_elements(seed.document->'types') as type(value),
       json_each(type.value->'attributes') with ordinality as attribute(key, value, ordinality)
),
nodes as (
  select 'supertag' as kind, names.name as id, 'supertag' as supertag,
         coalesce(types.attributes, '{}'::jsonb) as attributes
  from supertag_names names
  left join block_types types on types.name = names.name
  union all
  select 'attribute', attributes.type_name || '/' || attributes.name, 'attribute', attributes.attributes
  from block_attributes attributes
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select nodes.kind, nodes.id, nodes.supertag, 'project', 'live', nodes.attributes, seed.at, seed.writer, 0
from nodes, seed
order by nodes.kind, nodes.id
on conflict (kind, id) do update set
  supertag = excluded.supertag,
  visibility = excluded.visibility,
  state = excluded.state,
  attributes = excluded.attributes,
  source_event = excluded.source_event;

-- The base attributes every node of this store carries, stated as the tuples they are. The node
-- row is the projection of these two, so they are read back off the row this seed just wrote
-- rather than built a second time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (values ('visibility', node.visibility), ('state', node.state)) as fact(kind, value)
where node.kind in ('supertag', 'attribute')
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.kind, node.id, fact.kind;

-- What ties an attribute to its supertag. The id of an attribute node is `<supertag>/<name>`, so
-- the supertag it belongs to is read back from the id here, and from this tuple everywhere else:
-- a reader of the schema follows the tuple, never the spelling of an id.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'attribute_of', 'supertag', split_part(node.id, '/', 1),
       node.made_at, node.made_by, 0, null, true
from graph_nodes node
where node.kind = 'attribute'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'attribute_of'
      and held.to_kind = 'supertag' and held.to_id = split_part(node.id, '/', 1)
      and held.source_event = 0 and held.asserted
  )
order by node.id;
