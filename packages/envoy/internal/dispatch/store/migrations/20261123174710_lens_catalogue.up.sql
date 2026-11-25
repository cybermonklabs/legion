-- 20261123174710_lens_catalogue.up.sql
--
-- The lens catalogue is nodes of the store (spec requirement r-lens-catalogue).
-- This migration writes three things, each the way 20261113230000 writes a type node: as rows,
-- with the tuples each row is the projection of, and no event.
--
-- One. The `lens_kind` supertag. The kinds `lens`, `risk`, `constraint` and `interaction` are
-- already types of the store (20261109051232's closed list), and a lens's kind is a node of the
-- catalogue, so the type of those nodes is one more row. It declares no block type: a lens kind is
-- never a block of a document, so the block schema the server installs reads past it and its
-- version does not move.
--
-- Two. The attribute nodes of the supertags that declare no block type, each tied to its type by an
-- `attribute_of` tuple, so a reader of the store reads what each of this tree's types holds from
-- the store and not from Go: a lens kind's `words`; a lens's `verdict`, and the `artifact_id` a
-- lens on a document or on a block of one records, the `artifacts.id` of the row the artifacts
-- table held at the document's address when a write placed the lens there; a risk's `heading`,
-- `early_warning` and `prevention`; a constraint's `standard` and `reference`, or its
-- `measurement`, `value` and where it was `taken_where`; and an interaction's `words`, the time it
-- `ends_at` and each `frame` of it. An interaction's project is the base attribute `project`
-- (graphstore.AttributeProject), which no attribute node declares. Each node carries what the seed
-- gives an attribute of a block type - its kind, its name, its store name and its position among
-- its type's attributes - and since none of these names is a base attribute, each is stored under
-- its own name.
--
-- Three. The ten lens kinds of the lens research the catalogue is seeded from (a record outside
-- this repository), each a `lens_kind` node holding its words, and each with one `constrained_by`
-- tuple to a `constraint` node holding the kind's primary source (`standard`) and that source's
-- `reference`, as this change's research task read them in the sources themselves. Every catalogue
-- constraint is the far end of exactly one constrained_by tuple from a lens_kind node, and this
-- writes no other constraint node: that is the shape cybermonklabs/legion#4177's project clause
-- (the catalogue list of its refs/catalogue.go) reads as the catalogue, which every viewer sees.
-- Every node is project-visible and live, in no project and between no parties, made by this
-- migration's writer. A constraint's id is a uuid fixed here. A kind added later is
-- `envoy-dispatch lens-kind-add`, which writes the same shape (graphstore.AddLensKind).
--
-- It is a type migration written after the store's backfill, as
-- 20261123174700_graph_supertags_decision_reading is: it is numbered above backfillVersion
-- (cmd/dispatch/supertag_nodes_test.go), the newest migration of the releases that may have run
-- `backfill-graph-store` over graph_nodes and graph_tuples, so a store of those releases applies
-- it after the backfill, as a fresh store applies it after 20261123174700. cmd/dispatch's
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs it over a backfilled
-- store with the census at a one-byte limit.
--
-- Each statement is one insert of literal `values` rows, every row written out whole: graph_nodes
-- and graph_tuples are above the census's size limit in production, and on a table above it the
-- census admits only literal `values` rows, each statement with a fixed row count. So no row is
-- read back off another (no `select`, no CTE) and none is skipped where the store already holds
-- it (no `on conflict`, no `not exists`): the file runs once, as the runner runs every migration,
-- and a second run is refused on graph_nodes' primary key and writes nothing. The nodes stand in
-- ascending (kind, id), all before any tuple, and the tuples in the order of their node, then
-- their kind and far end. Every row is dated '2026-11-16 12:33:54+00', a literal, and signed by
-- the catalogue's writer, `{"kind": "system", "id": "lens-catalogue"}`. The stamp is not now(),
-- as 20261123174700's is, since no later `at` has to win over one of these: a reader at a moment
-- (graphstore.AsOfOut) orders a node's tuples by `at` first, which matters where two writers state
-- one attribute of one node, as 20261123174700 and 20261123080000 both state each block type's
-- `version`. Nothing else states a tuple out of a node this file writes: no route or command
-- writes a type node, each constraint's id is one only this file writes, and
-- `envoy-dispatch lens-kind-add` refuses a kind whose id the catalogue holds.
-- The migration records no event, so every row carries the `source_event` sentinel 0. The
-- triggers of 20261109051232 announce the supertag and attribute rows once, at commit, and the
-- server installs the block schema those nodes build, which is the one it installed before.

-- The type nodes: the lens_kind supertag, and the attribute nodes of it and of the other
-- store-only types, each holding its kind, its name, its store name and its position.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('attribute', 'constraint/measurement', 'attribute', 'project', 'live', '{"kind": "string", "name": "measurement", "position": 2, "store_name": "measurement"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'constraint/reference', 'attribute', 'project', 'live', '{"kind": "string", "name": "reference", "position": 1, "store_name": "reference"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'constraint/standard', 'attribute', 'project', 'live', '{"kind": "string", "name": "standard", "position": 0, "store_name": "standard"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'constraint/taken_where', 'attribute', 'project', 'live', '{"kind": "string", "name": "taken_where", "position": 4, "store_name": "taken_where"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'constraint/value', 'attribute', 'project', 'live', '{"kind": "string", "name": "value", "position": 3, "store_name": "value"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'interaction/ends_at', 'attribute', 'project', 'live', '{"kind": "timestamp", "name": "ends_at", "position": 1, "store_name": "ends_at"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'interaction/frame', 'attribute', 'project', 'live', '{"kind": "string", "name": "frame", "position": 2, "store_name": "frame"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'interaction/words', 'attribute', 'project', 'live', '{"kind": "string", "name": "words", "position": 0, "store_name": "words"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'lens_kind/words', 'attribute', 'project', 'live', '{"kind": "string", "name": "words", "position": 0, "store_name": "words"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'lens/artifact_id', 'attribute', 'project', 'live', '{"kind": "string", "name": "artifact_id", "position": 1, "store_name": "artifact_id"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'lens/verdict', 'attribute', 'project', 'live', '{"kind": "string", "name": "verdict", "position": 0, "store_name": "verdict"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'risk/early_warning', 'attribute', 'project', 'live', '{"kind": "string", "name": "early_warning", "position": 1, "store_name": "early_warning"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'risk/heading', 'attribute', 'project', 'live', '{"kind": "string", "name": "heading", "position": 0, "store_name": "heading"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('attribute', 'risk/prevention', 'attribute', 'project', 'live', '{"kind": "string", "name": "prevention", "position": 2, "store_name": "prevention"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('supertag', 'lens_kind', 'supertag', 'project', 'live', '{}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0);

-- The facts each node carries, the tuples its row is the projection of, as the seed writes a type
-- node's: its two base attributes, each value its attributes column holds (a string as itself,
-- any other value as its JSON text, so a position is `2`), and, for an attribute node, the
-- attribute_of tuple that ties it to its type. A reader of the schema follows that tuple, never
-- the spelling of the node's id.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'constraint/measurement', 'attribute_of', 'supertag', 'constraint', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'name', 'value', 'measurement', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'position', 'value', '2', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'store_name', 'value', 'measurement', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/measurement', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'attribute_of', 'supertag', 'constraint', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'name', 'value', 'reference', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'position', 'value', '1', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'store_name', 'value', 'reference', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/reference', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'attribute_of', 'supertag', 'constraint', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'name', 'value', 'standard', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'position', 'value', '0', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'store_name', 'value', 'standard', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/standard', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'attribute_of', 'supertag', 'constraint', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'name', 'value', 'taken_where', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'position', 'value', '4', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'store_name', 'value', 'taken_where', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/taken_where', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'attribute_of', 'supertag', 'constraint', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'name', 'value', 'value', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'position', 'value', '3', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'store_name', 'value', 'value', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'constraint/value', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'attribute_of', 'supertag', 'interaction', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'kind', 'value', 'timestamp', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'name', 'value', 'ends_at', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'position', 'value', '1', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'store_name', 'value', 'ends_at', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/ends_at', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'attribute_of', 'supertag', 'interaction', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'name', 'value', 'frame', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'position', 'value', '2', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'store_name', 'value', 'frame', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/frame', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'attribute_of', 'supertag', 'interaction', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'name', 'value', 'words', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'position', 'value', '0', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'store_name', 'value', 'words', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'interaction/words', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'attribute_of', 'supertag', 'lens_kind', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'name', 'value', 'words', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'position', 'value', '0', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'store_name', 'value', 'words', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens_kind/words', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'attribute_of', 'supertag', 'lens', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'name', 'value', 'artifact_id', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'position', 'value', '1', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'store_name', 'value', 'artifact_id', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/artifact_id', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'attribute_of', 'supertag', 'lens', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'name', 'value', 'verdict', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'position', 'value', '0', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'store_name', 'value', 'verdict', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'lens/verdict', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'attribute_of', 'supertag', 'risk', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'name', 'value', 'early_warning', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'position', 'value', '1', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'store_name', 'value', 'early_warning', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/early_warning', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'attribute_of', 'supertag', 'risk', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'name', 'value', 'heading', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'position', 'value', '0', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'store_name', 'value', 'heading', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/heading', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'attribute_of', 'supertag', 'risk', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'kind', 'value', 'string', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'name', 'value', 'prevention', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'position', 'value', '2', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'store_name', 'value', 'prevention', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('attribute', 'risk/prevention', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('supertag', 'lens_kind', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('supertag', 'lens_kind', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true);

-- The catalogue: each kind's lens_kind node and its constraint node, and the one constrained_by
-- tuple from the first to the second. The words, the sources and the references are this change's
-- research task's reading of each source itself (the pull request body names each), and
-- `second-order` takes Bastiat, whose essay states the idea, where that lens research cited
-- Chesterton's fence, which the further kinds of 20261123174720 hold as a kind of its own.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('constraint', '5eed0000-1e45-4000-8000-000000000001', 'constraint', 'project', 'live', '{"standard": "Gary Klein, \"Performing a Project Premortem\", Harvard Business Review, September 2007 (Forethought section)", "reference": "Harvard Business Review vol. 85, September 2007; HBR reprint F0709A; https://hbr.org/2007/09/performing-a-project-premortem"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000002', 'constraint', 'project', 'live', '{"standard": "Dan McKinley, \"Choose Boring Technology\", mcfunley.com, 30 March 2015", "reference": "https://mcfunley.com/choose-boring-technology"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000003', 'constraint', 'project', 'live', '{"standard": "Robert C. Camp, Benchmarking: The Search for Industry Best Practices that Lead to Superior Performance, ASQC Quality Press, 1989", "reference": "Milwaukee, WI: ASQC Quality Press; New York: Quality Resources, 1989. ISBN 0-87389-058-2. Ch. 1 \"Introduction\", section \"Benchmarking Defined\", pp. 10–12."}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000004', 'constraint', 'project', 'live', '{"standard": "Aristotle, Physics, book I, chapter 1", "reference": "Physics I.1, 184a10–16 (Bekker); trans. R. P. Hardie and R. K. Gaye"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000005', 'constraint', 'project', 'live', '{"standard": "Charles T. Munger, Harvard School commencement speech, Harvard School, Los Angeles, 13 June 1986", "reference": "Poor Charlie''s Almanack, ch. 4, \"Talk One: Harvard School Commencement Speech\", Stripe Press online edition: https://www.stripe.press/poor-charlies-almanack/talk-one"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000006', 'constraint', 'project', 'live', '{"standard": "Rick Kazman, Mark Klein, Paul Clements, ATAM: Method for Architecture Evaluation, Software Engineering Institute, Carnegie Mellon University, August 2000", "reference": "Technical Report CMU/SEI-2000-TR-004 (ESC-TR-2000-004); §7.2 \"Sensitivity and Tradeoff Points\", pp. 22–23; https://www.sei.cmu.edu/documents/629/2000_005_001_13706.pdf"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000007', 'constraint', 'project', 'live', '{"standard": "Jeffrey P. Bezos, 2015 Letter to Shareholders, Amazon.com, Inc.", "reference": "\"To our shareowners\", section \"Invention Machine\"; Exhibit 99.1 to Amazon.com, Inc.''s Form 8-K filed 5 April 2016 (SEC accession 0001193125-16-530910); https://www.sec.gov/Archives/edgar/data/1018724/000119312516530910/d168744dex991.htm"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000008', 'constraint', 'project', 'live', '{"standard": "Frédéric Bastiat, \"What Is Seen and What Is Not Seen\" (\"Ce qu''on voit et ce qu''on ne voit pas\"), 1850", "reference": "Selected Essays on Political Economy, trans. Seymour Cain, ed. George B. de Huszar (Irvington-on-Hudson, NY: Foundation for Economic Education, 1995), ch. 1, opening paragraph; https://www.econlib.org/book-chapters/chapter-chapter-1-what-is-seen-and-what-is-not-seen/"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000009', 'constraint', 'project', 'live', '{"standard": "Melvin E. Conway, \"How Do Committees Invent?\", Datamation, April 1968", "reference": "Datamation vol. 14, no. 4 (April 1968), pp. 28–31, F. D. Thompson Publications. Author''s reprint: https://www.melconway.com/research/committees.html"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('constraint', '5eed0000-1e45-4000-8000-000000000010', 'constraint', 'project', 'live', '{"standard": "Jon Bentley, \"Programming Pearls: The Back of the Envelope\", Communications of the ACM, March 1984", "reference": "Communications of the ACM 27(3), March 1984, pp. 180–184; doi:10.1145/357994.381168"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'best-practice', 'lens_kind', 'project', 'live', '{"words": "best practice"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'boring-solution', 'lens_kind', 'project', 'live', '{"words": "the boring or standard solution"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'botec', 'lens_kind', 'project', 'live', '{"words": "BOTEC, the back-of-the-envelope calculation"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'conway', 'lens_kind', 'project', 'live', '{"words": "the Conway check"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'first-principles', 'lens_kind', 'project', 'live', '{"words": "first principles"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'inversion', 'lens_kind', 'project', 'live', '{"words": "inversion"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'pre-mortem', 'lens_kind', 'project', 'live', '{"words": "the pre-mortem over failure modes"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'reversibility', 'lens_kind', 'project', 'live', '{"words": "reversibility as one-way against two-way doors"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'second-order', 'lens_kind', 'project', 'live', '{"words": "second-order effects"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0),
  ('lens_kind', 'trade-off', 'lens_kind', 'project', 'live', '{"words": "trade-off analysis"}', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0);

-- Each node's two base attributes and each value it holds, and each kind's tie to its source,
-- dated and signed as its lens_kind node.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('constraint', '5eed0000-1e45-4000-8000-000000000001', 'reference', 'value', 'Harvard Business Review vol. 85, September 2007; HBR reprint F0709A; https://hbr.org/2007/09/performing-a-project-premortem', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000001', 'standard', 'value', 'Gary Klein, "Performing a Project Premortem", Harvard Business Review, September 2007 (Forethought section)', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000001', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000001', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000002', 'reference', 'value', 'https://mcfunley.com/choose-boring-technology', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000002', 'standard', 'value', 'Dan McKinley, "Choose Boring Technology", mcfunley.com, 30 March 2015', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000002', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000002', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000003', 'reference', 'value', 'Milwaukee, WI: ASQC Quality Press; New York: Quality Resources, 1989. ISBN 0-87389-058-2. Ch. 1 "Introduction", section "Benchmarking Defined", pp. 10–12.', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000003', 'standard', 'value', 'Robert C. Camp, Benchmarking: The Search for Industry Best Practices that Lead to Superior Performance, ASQC Quality Press, 1989', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000003', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000003', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000004', 'reference', 'value', 'Physics I.1, 184a10–16 (Bekker); trans. R. P. Hardie and R. K. Gaye', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000004', 'standard', 'value', 'Aristotle, Physics, book I, chapter 1', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000004', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000004', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000005', 'reference', 'value', 'Poor Charlie''s Almanack, ch. 4, "Talk One: Harvard School Commencement Speech", Stripe Press online edition: https://www.stripe.press/poor-charlies-almanack/talk-one', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000005', 'standard', 'value', 'Charles T. Munger, Harvard School commencement speech, Harvard School, Los Angeles, 13 June 1986', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000005', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000005', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000006', 'reference', 'value', 'Technical Report CMU/SEI-2000-TR-004 (ESC-TR-2000-004); §7.2 "Sensitivity and Tradeoff Points", pp. 22–23; https://www.sei.cmu.edu/documents/629/2000_005_001_13706.pdf', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000006', 'standard', 'value', 'Rick Kazman, Mark Klein, Paul Clements, ATAM: Method for Architecture Evaluation, Software Engineering Institute, Carnegie Mellon University, August 2000', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000006', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000006', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000007', 'reference', 'value', '"To our shareowners", section "Invention Machine"; Exhibit 99.1 to Amazon.com, Inc.''s Form 8-K filed 5 April 2016 (SEC accession 0001193125-16-530910); https://www.sec.gov/Archives/edgar/data/1018724/000119312516530910/d168744dex991.htm', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000007', 'standard', 'value', 'Jeffrey P. Bezos, 2015 Letter to Shareholders, Amazon.com, Inc.', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000007', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000007', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000008', 'reference', 'value', 'Selected Essays on Political Economy, trans. Seymour Cain, ed. George B. de Huszar (Irvington-on-Hudson, NY: Foundation for Economic Education, 1995), ch. 1, opening paragraph; https://www.econlib.org/book-chapters/chapter-chapter-1-what-is-seen-and-what-is-not-seen/', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000008', 'standard', 'value', 'Frédéric Bastiat, "What Is Seen and What Is Not Seen" ("Ce qu''on voit et ce qu''on ne voit pas"), 1850', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000008', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000008', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000009', 'reference', 'value', 'Datamation vol. 14, no. 4 (April 1968), pp. 28–31, F. D. Thompson Publications. Author''s reprint: https://www.melconway.com/research/committees.html', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000009', 'standard', 'value', 'Melvin E. Conway, "How Do Committees Invent?", Datamation, April 1968', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000009', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000009', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000010', 'reference', 'value', 'Communications of the ACM 27(3), March 1984, pp. 180–184; doi:10.1145/357994.381168', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000010', 'standard', 'value', 'Jon Bentley, "Programming Pearls: The Back of the Envelope", Communications of the ACM, March 1984', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000010', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('constraint', '5eed0000-1e45-4000-8000-000000000010', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'best-practice', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000003', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'best-practice', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'best-practice', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'best-practice', 'words', 'value', 'best practice', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'boring-solution', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000002', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'boring-solution', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'boring-solution', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'boring-solution', 'words', 'value', 'the boring or standard solution', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'botec', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000010', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'botec', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'botec', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'botec', 'words', 'value', 'BOTEC, the back-of-the-envelope calculation', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'conway', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000009', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'conway', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'conway', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'conway', 'words', 'value', 'the Conway check', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'first-principles', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000004', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'first-principles', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'first-principles', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'first-principles', 'words', 'value', 'first principles', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inversion', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000005', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inversion', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inversion', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'inversion', 'words', 'value', 'inversion', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'pre-mortem', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000001', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'pre-mortem', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'pre-mortem', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'pre-mortem', 'words', 'value', 'the pre-mortem over failure modes', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'reversibility', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000007', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'reversibility', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'reversibility', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'reversibility', 'words', 'value', 'reversibility as one-way against two-way doors', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'second-order', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000008', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'second-order', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'second-order', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'second-order', 'words', 'value', 'second-order effects', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'trade-off', 'constrained_by', 'constraint', '5eed0000-1e45-4000-8000-000000000006', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'trade-off', 'state', 'value', 'live', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'trade-off', 'visibility', 'value', 'project', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true),
  ('lens_kind', 'trade-off', 'words', 'value', 'trade-off analysis', '2026-11-16 12:33:54+00', '{"id": "lens-catalogue", "kind": "system"}', 0, null, true);
