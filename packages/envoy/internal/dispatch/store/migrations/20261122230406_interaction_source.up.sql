-- 20261122230406_interaction_source.up.sql
--
-- An interaction's `source`: which table projects the node. Every interaction node this release
-- writes is a run of the command bar, which `going_requests` projects, and holds `source` at the
-- value `going_requests` (graphstore.AttributeSource, graphstore.SourceGoingRequests), stated as a
-- value tuple beside its `words` and its `ends_at` (graphstore.InteractionFacts). The sweep reads
-- it: a run is readable for one hour, and then the sweep states its node gone and removes its
-- rows (api/going.go, sweepGoingRequests), and it states gone only a node whose `source` is
-- `going_requests`, so a later release's interaction node of another source is never swept at
-- one hour.
--
-- This declares the attribute on the interaction type: one `attribute` node, `interaction/source`,
-- holding its kind, its name, its store name and its position among the type's attributes, 3,
-- after the positions 0 to 2 that 20261123174710, a migration that runs after this file, declares
-- for `words`, `ends_at` and `frame` the same way; and tied to the `interaction` supertag by an
-- `attribute_of` tuple, so a reader of the store reads what an interaction holds from the store
-- and not from Go. `source` is no base attribute, so it is stored under its own name. The
-- interaction type declares no block type, so the block schema the server installs reads past the
-- node and its version does not move.
--
-- Each statement is one insert of literal `values` rows, every row written out whole, as
-- 20261123174710 writes its own: graph_nodes and graph_tuples are above the census's size limit in
-- production, and on a table above it the census admits only literal `values` rows, each statement
-- with a fixed row count. So no row is read back off another (no `select`, no CTE) and none is
-- skipped where the store already holds it (no `on conflict`, no `not exists`): the file runs
-- once, as the runner runs every migration, and a second run is refused on graph_nodes' primary
-- key and writes nothing. The node stands before its tuples, and the tuples in the order of their
-- kind and far end. Every row is dated '2026-11-22 23:04:06+00', a literal at this migration's
-- version, and signed by this migration's writer, `{"kind": "system", "id": "interaction-source"}`.
-- No event records it, so every row carries the `source_event` sentinel 0. The triggers of
-- 20261109051232 announce the attribute row once, at commit.

-- The attribute node, holding its kind, its name, its store name and its position.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('attribute', 'interaction/source', 'attribute', 'project', 'live', '{"kind": "string", "name": "source", "position": 3, "store_name": "source"}', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0);

-- The facts it carries, the tuples its row is the projection of, as 20261123174710 writes an
-- attribute node's: its two base attributes, each value its attributes column holds (a string as
-- itself, the position as its JSON text), and the attribute_of tuple that ties it to its type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'interaction/source', 'attribute_of', 'supertag', 'interaction', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'kind', 'value', 'string', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'name', 'value', 'source', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'position', 'value', '3', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'state', 'value', 'live', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'store_name', 'value', 'source', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/source', 'visibility', 'value', 'project', '2026-11-22 23:04:06+00', '{"id": "interaction-source", "kind": "system"}', 0, null, true);
