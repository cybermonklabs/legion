-- 20261116102759_delivery_attempt_attributes.up.sql
--
-- The delivery_attempt supertag takes its attributes. One attempt to reach one session or role -
-- a row of comment_deliveries or message_deliveries - is a node of the store (dispatch://INFRA-1152,
-- R5), and its number, state, envelope, errors, claim and acceptance are its values, because they
-- belong to no comment and to no person. 20261109051232 seeded the supertag node with no attribute
-- of its own, since the kinds that move in a later child bring theirs with them; this is that
-- child's. Each attribute is a node, tied to the supertag by an `attribute_of` tuple, exactly as
-- the seed ties a block type's, so a reader of the store learns what an attempt carries from the
-- store rather than from Go.
--
-- The block schema is not changed. delivery_attempt declares no block type, so the schema the
-- server builds from the type nodes (pmdoc/schemanodes.Read) passes over it and its attributes, and
-- no block type's version moves. The triggers of 20261109051232 still announce the write once, at
-- this migration's commit, and every running server reads the schema again and installs the same
-- one.
--
-- An attribute node's attributes are what the seed builds for an attribute of the file: the
-- attribute's own schema fields, with its name, its store name and its position beside them. Every
-- one of them is the server's own (`server`), since no client writes an attempt. Their kinds are the
-- block schema's: the state and the acceptance are the values their tables' checks admit, and the
-- attempt's number is `string`, because that schema has no integer kind and the store states every
-- value as text anyway (graphstore.Attempted writes it in decimal). The store name is each
-- attribute's own name but the state's: `state` is a base attribute every node carries
-- (graphstore.IsBaseAttribute), whose value tuple would read as the node's own, so it is stored as
-- the type's name, `_` and its own name (pmdoc/schemanodes.StoreName), `delivery_attempt_state`.
-- The positions are the order R5 names the values in.
--
-- Running this file again writes the same rows, as running the seed again does: the node upsert
-- moves every column but `made_at` and `made_by`, and each tuple insert appends only a fact the
-- node does not already carry. The migration records no event, so every row it writes carries the
-- `source_event` sentinel 0, and its rows carry a writer of their own, so a reader of the store can
-- tell them from the seed's.
with change as (
  select now() as at, '{"kind": "system", "id": "block-schema-delivery-attempt"}'::jsonb as writer
),
declared (name, store_name, position, schema) as (
  values ('attempt', 'attempt', 0, '{"kind": "string", "server": true}'::jsonb),
         ('state', 'delivery_attempt_state', 1,
          '{"kind": "enum", "choices": ["pending", "sent", "queued", "failed"], "server": true}'::jsonb),
         ('envelope_id', 'envelope_id', 2, '{"kind": "string", "server": true}'::jsonb),
         ('error', 'error', 3, '{"kind": "string", "server": true}'::jsonb),
         ('resolve_error', 'resolve_error', 4, '{"kind": "string", "server": true}'::jsonb),
         ('duplicate', 'duplicate', 5, '{"kind": "bool", "server": true}'::jsonb),
         ('claimed_at', 'claimed_at', 6, '{"kind": "timestamp", "server": true}'::jsonb),
         ('accepted_at', 'accepted_at', 7, '{"kind": "timestamp", "server": true}'::jsonb),
         ('accepted_as', 'accepted_as', 8, '{"kind": "enum", "choices": ["user_turn"], "server": true}'::jsonb)
)
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
select 'attribute', 'delivery_attempt/' || declared.store_name, 'attribute', 'project', 'live',
       declared.schema || jsonb_build_object(
         'name', declared.name, 'store_name', declared.store_name, 'position', declared.position
       ),
       change.at, change.writer, 0
from change, declared
order by declared.store_name
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
where node.kind = 'attribute' and node.id like 'delivery_attempt/%'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.id, fact.kind;

-- What ties each attribute to delivery_attempt. A reader of the schema follows this tuple, never
-- the spelling of the node's id, so without it the node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, 'attribute_of', 'supertag', 'delivery_attempt', node.made_at, node.made_by, 0, null, true
from graph_nodes node
where node.kind = 'attribute' and node.id like 'delivery_attempt/%'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = 'attribute_of'
      and held.to_kind = 'supertag' and held.to_id = 'delivery_attempt'
      and held.source_event = 0 and held.asserted
  )
order by node.id;

-- The values each node carries, as the tuples they are, read back off the rows just written as the
-- seed reads every type node's: a `value` tuple whose kind is the attribute's name and whose far
-- end is its value as text, at the `source_event` sentinel.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select node.kind, node.id, fact.kind, 'value', fact.value, node.made_at, node.made_by, 0, null, true
from graph_nodes node
cross join lateral (
  select attribute.key, attribute.value #>> '{}' from jsonb_each(node.attributes) as attribute
) as fact(kind, value)
where node.kind = 'attribute' and node.id like 'delivery_attempt/%'
  and not exists (
    select 1 from graph_tuples held
    where held.from_kind = node.kind and held.from_id = node.id and held.kind = fact.kind
      and held.to_kind = 'value' and held.to_id = fact.value
      and held.source_event = 0 and held.asserted
  )
order by node.id, fact.kind;
