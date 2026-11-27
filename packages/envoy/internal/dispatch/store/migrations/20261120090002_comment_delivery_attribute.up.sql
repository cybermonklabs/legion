-- 20261120090002_comment_delivery_attribute.up.sql
--
-- The comment supertag takes its one attribute, its urgency. A comment that names targets as
-- delivery routes states one delivery mode for all of them - btw, aside or steer, the mode
-- validateCommentMentions in api picks and applies to every target - as a `delivery` value tuple out
-- of the comment's node (R5, graphstore.TupleDelivery), which comment_mentions.delivery projects for
-- each target. 20261109051232 seeded the supertag node with no attribute of its own, since the
-- kinds that move in a later child bring theirs with them; this is that child's. The attribute is a
-- node, tied to the supertag by an `attribute_of` tuple, exactly as 20261116102759 ties each of a
-- delivery attempt's, so a reader of the store learns what a comment carries from the store rather
-- than from Go.
--
-- The block schema is not changed. comment declares no block type, so the schema the server builds
-- from the type nodes (pmdoc/schemanodes.Read) passes over it and its attribute, and no block type's
-- version moves. The triggers of 20261109051232 still announce the write once, at this migration's
-- commit, and every running server reads the schema again and installs the same one.
--
-- The attribute node's attributes are what 20261116102759 builds for each of an attempt's: its own
-- schema fields, with its name, its store name and its position beside them. Its kind is the block
-- schema's `enum`, its choices the three modes comment_mentions' check admits. It is not marked
-- `server`: the writer of a comment names the mode in its request, where an attempt's values are
-- the server's alone. Its store name is its own name, since `delivery` is no base attribute of a
-- node (graphstore.IsBaseAttribute).
--
-- Every row is written out as a literal, because the pre-deploy census admits only literal rows on
-- a table above pgmigrate.CensusTableLimit (its bounded-write rule), and graph_tuples is above it
-- in production once the node store's backfill has run. The file is four `insert ... values`
-- statements, as 20261116102759 is: the node, then its visibility and state tuples, its
-- `attribute_of` tuple and its value tuples. No earlier migration writes this id and no code outside
-- a migration writes a node of kind `attribute`, so the node does not exist when this runs and every
-- tuple below is one it lacks.
--
-- The migration records no event, so every row carries the `source_event` sentinel 0, and its rows
-- carry a writer of their own, so a reader of the store can tell them from the seed's. `made_at`
-- and `at` are `now()`, the one moment of the migration's transaction; a tuple's `asserted` and
-- `legacy_source_seq` take their defaults, true and null. The runner applies this file once
-- (schema_migrations). Run again by hand, the node insert leaves the node as it stands, but every
-- tuple is appended a second time: neither unique key of graph_tuples covers a row at the sentinel.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'comment/delivery', 'attribute', 'project', 'live',
        '{"kind": "enum", "choices": ["btw", "aside", "steer"], "name": "delivery", "store_name": "delivery", "position": 0}',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0)
on conflict (kind, id) do nothing;

-- The two base attributes the node carries, as the tuples they are: its visibility, `project`, and
-- its state, `live`.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'comment/delivery', 'state', 'value', 'live',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0),
       ('attribute', 'comment/delivery', 'visibility', 'value', 'project',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0);

-- What ties the attribute to comment. A reader of the schema follows this tuple, never the spelling
-- of the node's id, so without it the node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'comment/delivery', 'attribute_of', 'supertag', 'comment',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0);

-- The values the node carries, as the tuples they are, as the seed states every type node's: a
-- `value` tuple for each key of the node's attributes, whose kind is the key and whose far end is
-- the value as text (`#>> '{}'`), so the list of choices is its JSON text.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
values ('attribute', 'comment/delivery', 'choices', 'value', '["btw", "aside", "steer"]',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0),
       ('attribute', 'comment/delivery', 'kind', 'value', 'enum',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0),
       ('attribute', 'comment/delivery', 'name', 'value', 'delivery',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0),
       ('attribute', 'comment/delivery', 'position', 'value', '0',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0),
       ('attribute', 'comment/delivery', 'store_name', 'value', 'delivery',
        now(), '{"kind": "system", "id": "block-schema-comment"}', 0);
