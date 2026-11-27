-- 20261116102759_delivery_attempt_attributes.up.sql
--
-- The delivery_attempt supertag takes its attributes. One attempt to reach one session or role -
-- a row of comment_deliveries or message_deliveries - is a node of the store (the spec's
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
-- Every row is written out as a literal, because the pre-deploy census admits only literal rows on
-- a table above pgmigrate.CensusTableLimit (its bounded-write rule), and graph_tuples is above it
-- in production once the node store's backfill has run. The file is four `insert ... values`
-- statements: the nine nodes, then their visibility and state tuples, their `attribute_of` tuples
-- and their value tuples, each in the order of the node's id. No earlier migration writes one of
-- these ids and no code outside a migration writes a node of kind `attribute`, so none of the nodes
-- exists when this runs and every tuple below is one they lack.
--
-- The migration records no event, so every row carries the `source_event` sentinel 0, and its rows
-- carry a writer of their own, so a reader of the store can tell them from the seed's. `made_at`
-- and `at` are `now()`, the one moment of the migration's transaction; a tuple's `asserted` and
-- `legacy_source_seq` take their defaults, true and null. The runner applies this file once
-- (schema_migrations). Run again by hand, the node insert leaves each node as it stands, but every
-- tuple is appended a second time: neither unique key of graph_tuples covers a row at the sentinel.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'delivery_attempt/accepted_as', 'attribute', 'project', 'live',
        '{"kind": "enum", "choices": ["user_turn"], "server": true, "name": "accepted_as", "store_name": "accepted_as", "position": 8}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'attribute', 'project', 'live',
        '{"kind": "timestamp", "server": true, "name": "accepted_at", "store_name": "accepted_at", "position": 7}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'attribute', 'project', 'live',
        '{"kind": "string", "server": true, "name": "attempt", "store_name": "attempt", "position": 0}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'attribute', 'project', 'live',
        '{"kind": "timestamp", "server": true, "name": "claimed_at", "store_name": "claimed_at", "position": 6}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'attribute', 'project', 'live',
        '{"kind": "enum", "choices": ["pending", "sent", "queued", "failed"], "server": true, "name": "state", "store_name": "delivery_attempt_state", "position": 1}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'attribute', 'project', 'live',
        '{"kind": "bool", "server": true, "name": "duplicate", "store_name": "duplicate", "position": 5}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'attribute', 'project', 'live',
        '{"kind": "string", "server": true, "name": "envelope_id", "store_name": "envelope_id", "position": 2}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'attribute', 'project', 'live',
        '{"kind": "string", "server": true, "name": "error", "store_name": "error", "position": 3}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'attribute', 'project', 'live',
        '{"kind": "string", "server": true, "name": "resolve_error", "store_name": "resolve_error", "position": 4}',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0)
on conflict (kind, id) do nothing;

-- The two base attributes each node carries, as the tuples they are: its visibility, `project`,
-- and its state, `live`.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'delivery_attempt/accepted_as', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0);

-- What ties each attribute to delivery_attempt. A reader of the schema follows this tuple, never
-- the spelling of the node's id, so without it the node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'delivery_attempt/accepted_as', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'attribute_of', 'supertag', 'delivery_attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0);

-- The values each node carries, as the tuples they are, as the seed states every type node's: a
-- `value` tuple for each key of the node's attributes, whose kind is the key and whose far end is
-- the value as text (`#>> '{}'`), so a list of choices is its JSON text.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'delivery_attempt/accepted_as', 'choices', 'value', '["user_turn"]',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'kind', 'value', 'enum',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'name', 'value', 'accepted_as',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'position', 'value', '8',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_as', 'store_name', 'value', 'accepted_as',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'kind', 'value', 'timestamp',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'name', 'value', 'accepted_at',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'position', 'value', '7',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/accepted_at', 'store_name', 'value', 'accepted_at',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'kind', 'value', 'string',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'name', 'value', 'attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'position', 'value', '0',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/attempt', 'store_name', 'value', 'attempt',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'kind', 'value', 'timestamp',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'name', 'value', 'claimed_at',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'position', 'value', '6',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/claimed_at', 'store_name', 'value', 'claimed_at',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'choices', 'value', '["pending", "sent", "queued", "failed"]',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'kind', 'value', 'enum',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'name', 'value', 'state',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'position', 'value', '1',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/delivery_attempt_state', 'store_name', 'value', 'delivery_attempt_state',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'kind', 'value', 'bool',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'name', 'value', 'duplicate',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'position', 'value', '5',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/duplicate', 'store_name', 'value', 'duplicate',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'kind', 'value', 'string',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'name', 'value', 'envelope_id',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'position', 'value', '2',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/envelope_id', 'store_name', 'value', 'envelope_id',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'kind', 'value', 'string',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'name', 'value', 'error',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'position', 'value', '3',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/error', 'store_name', 'value', 'error',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'kind', 'value', 'string',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'name', 'value', 'resolve_error',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'position', 'value', '4',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'server', 'value', 'true',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0),
       ('attribute', 'delivery_attempt/resolve_error', 'store_name', 'value', 'resolve_error',
        now(), '{"kind": "system", "id": "block-schema-delivery-attempt"}', 0);
