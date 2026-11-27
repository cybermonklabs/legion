-- 20261125163555_graph_supertags_decision_reading.up.sql
--
-- The block schema moves from version 6 to version 7, for the decision and reading block types.
-- `pmdoc/schema/blocks.json` adds two typed blocks and one attribute of `ask`, each read from
-- version 7. `decision` and `reading`, every attribute of `decision`, and `ask`'s `shaped` are
-- created only from version 8 (`creatable_since` 8), so a build of version 7 reads and validates
-- a stored one and refuses to create one:
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
-- It moves `check`'s `subject` and `program`, which 20261125125701 wrote read from version 6 and
-- created from version 7, to `creatable_since` 8, and leaves their `since` at 6: this release
-- reads them and creates neither. The follow-up to #5416 that records real deploys in production
-- has not shipped; once production records them, that release sets the version that creates them.
--
-- A type is a node of the store, and the server installs the schema it builds from those nodes,
-- not the file (20261109051232), so the change reaches a store only as this migration, in the
-- release of the binary that handles version 7. The seed already holds a `supertag` node for
-- `decision` and one for `reading`, which the closed list names, carrying no attribute: this
-- moves both to block types. A build of version 6, which holds
-- 20261125125701_graph_supertags_check_subject and not this file, reads the store's schema at its
-- own version (pmdoc.Install, asOfVersion): it leaves out every element whose `since` is 7, and its
-- decoder ignores the two fields it does not know (`parents`, `markdown`). The `creatable_since` 8
-- of `subject` and `program` is above its version as the 7 before it was, so it creates neither,
-- as before this ran. So it installs the schema it installed then, but for that one number, and
-- keeps serving.
--
-- It is a type migration written after the store's backfill, as
-- 20261123080000_graph_supertags_plan_task_steps is: it is numbered above backfillVersion
-- (cmd/dispatch/supertag_nodes_test.go), the newest migration of the releases that may have run
-- `backfill-graph-store` over graph_nodes and graph_tuples, and above 20261123080000 and
-- 20261125125701, the type migrations before it, so it runs after both on every store: on a
-- fresh store in version order, and on a store that applied them in an earlier release, in the
-- release that carries this file. Both ways it finds what 20261125125701 left and leaves the same
-- values. cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs
-- it after a backfill, with the census at a one-byte limit, and holds what it leaves to the file.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 7, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field. Every value a row carries is also stated as the tuple it is, a `value`
-- tuple whose kind is the attribute's name and whose far end is its value as text, since a node
-- row's attributes column is the projection of its tuples (R1).
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes, and the triggers of 20261109051232 announce it once, at that commit, so every running
-- server installs the new types and the new version together.
--
-- Every statement is one of the two shapes the pre-deploy census admits on a table past
-- pgmigrate.CensusTableLimit, which graph_nodes and graph_tuples can be once the node store's
-- backfill has filled them: an `insert … values` of literal rows, and an `update` that sets
-- columns to literals on the rows its `where` names by their primary key, `(kind, id)`
-- (20261109013400), in literal values. So no statement reads a row to write one: each node and
-- each tuple is written out below as the row it is, a tuple states the value the node row above
-- it carries, and an update sets a node's whole `attributes`, the value the node held before this
-- migration with this migration's values in it. Every row carries now(), the start of the runner's
-- transaction, as the moment it was made or stated, as the rows of 20261123080000 and
-- 20261125125701 do. A reader at a moment (graphstore.AsOfOut, in graphstore/read.go) orders a
-- node's tuples by `at` first (`order by kind, to_kind, to_id, at desc, seq desc`), and
-- 20261125125701 stamps its `version` and `creatable_since` tuples with now() at its own apply, so
-- a literal stamp from before that apply would order this file's version-7 tuples before that
-- file's version-6 ones, and this file's `creatable_since` 8 before that file's 7. With now(), a
-- later apply always stamps later.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert into graph_nodes, on the primary key of a
-- row the first run wrote, and the runner's one transaction takes back everything it wrote. The
-- migration records no event, so every tuple it writes carries the `source_event` sentinel 0, and
-- its rows carry a writer of their own, so a reader of the store can tell them from the seed's.

-- The two supertag nodes become block types: what the seed builds for a type of the file, its
-- content rule, who draws it, the schema's version and its place among the types (callout, ask,
-- requirement, plan_task and check hold 0 to 4, so decision holds 5 and reading 6), with the
-- element versions the file gives each and, on `reading`, its parents. The seed gave neither an
-- attribute (20261109051232), so each value below is the whole of what the node carries. Each
-- node is changed in place and keeps the `made_at` and `made_by` the seed gave it. The two take
-- different values, so each is a statement of its own.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 7, "position": 5, "since": 7, "creatable_since": 8}'::jsonb
where (kind, id) in (('supertag', 'decision'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 6, "since": 7, "creatable_since": 8, "parents": ["decision", "ask"]}'::jsonb
where (kind, id) in (('supertag', 'reading'));

-- The attribute nodes. Each id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, with its name, its store name
-- and its position beside it. None is named for a base attribute, so each store name is its own
-- name (pmdoc/schemanodes.StoreName). A position is the attribute's place among its type's
-- attributes, counted from 0: `ask` holds ten before `shaped`, so `shaped` holds 10. The store
-- holds none of the nine before this runs: the seed writes no attribute of `decision` and none
-- of `ask` named `shaped`, 20261123080000 writes `plan_task/steps` alone, and 20261125125701
-- writes `check/subject` and `check/program` alone, which this file moves below.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('attribute', 'decision/uncertainty', 'attribute', 'project', 'live',
   '{"kind": "enum", "choices": ["low", "medium", "high"], "since": 7, "creatable_since": 8, "name": "uncertainty", "store_name": "uncertainty", "position": 0}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/uncertainty_reason', 'attribute', 'project', 'live',
   '{"kind": "string", "since": 7, "creatable_since": 8, "name": "uncertainty_reason", "store_name": "uncertainty_reason", "position": 1}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/shaped', 'attribute', 'project', 'live',
   '{"kind": "block[]", "since": 7, "creatable_since": 8, "name": "shaped", "store_name": "shaped", "position": 2}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/from_ask', 'attribute', 'project', 'live',
   '{"kind": "ask", "since": 7, "creatable_since": 8, "name": "from_ask", "store_name": "from_ask", "position": 3}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/replaced', 'attribute', 'project', 'live',
   '{"kind": "bool", "default": false, "since": 7, "creatable_since": 8, "name": "replaced", "store_name": "replaced", "position": 4}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/decided_by', 'attribute', 'project', 'live',
   '{"kind": "actor", "server": true, "markdown": false, "since": 7, "creatable_since": 8, "name": "decided_by", "store_name": "decided_by", "position": 5}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/kept_by', 'attribute', 'project', 'live',
   '{"kind": "actor", "server": true, "markdown": false, "since": 7, "creatable_since": 8, "name": "kept_by", "store_name": "kept_by", "position": 6}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'decision/kept_text_sum', 'attribute', 'project', 'live',
   '{"kind": "string", "server": true, "markdown": false, "since": 7, "creatable_since": 8, "name": "kept_text_sum", "store_name": "kept_text_sum", "position": 7}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0),
  ('attribute', 'ask/shaped', 'attribute', 'project', 'live',
   '{"kind": "block[]", "since": 7, "creatable_since": 8, "name": "shaped", "store_name": "shaped", "position": 10}',
   now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0);

-- `ask`'s `invalid` takes `markdown: false` and nothing else changes on it: the rest of the value
-- is what the seed gave it.
update graph_nodes
set attributes = '{"kind": "string", "server": true, "name": "invalid", "store_name": "invalid", "position": 7, "markdown": false}'::jsonb
where (kind, id) in (('attribute', 'ask/invalid'));

-- `check`'s `subject` and `program` move to `creatable_since` 8 and nothing else changes on them.
-- A bounded write sets a column to a literal, so each is set to its whole attributes: the literal
-- 20261125125701 wrote on it, with 8 in place of 7. Nothing but a migration writes a type node,
-- and none between that file and this one writes either, so that literal is what each holds, and
-- this rewrites nothing but the one number (the census says why). The two take different values,
-- so each is a statement of its own.
update graph_nodes
set attributes = '{"kind": "enum", "choices": ["code", "production", "graph"], "since": 6, "creatable_since": 8, "name": "subject", "store_name": "subject", "position": 6}'::jsonb
where (kind, id) in (('attribute', 'check/subject'));

update graph_nodes
set attributes = '{"kind": "string", "since": 6, "creatable_since": 8, "name": "program", "store_name": "program", "position": 7}'::jsonb
where (kind, id) in (('attribute', 'check/program'));

-- The two base attributes each new attribute node carries, as the tuples they are, as the seed
-- states them: at the time and by the writer of the node's row.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'ask/shaped', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true);

-- What ties each attribute to its type. A reader of the schema follows this tuple, never the
-- spelling of the node's id, so without it a node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'ask/shaped', 'attribute_of', 'supertag', 'ask', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'attribute_of', 'supertag', 'decision', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true);

-- The values each node this migration writes or moves carries, as the tuples they are, as the
-- seed states every type node's: a value the row holds as JSON is written as that JSON's text and
-- a string as itself, so `version` is `7`, `parents` is `["decision", "ask"]` and `markdown` is
-- `false`. A value a node already states as its tuple is left as the seed wrote it: `ask/invalid`
-- states its kind, server flag, name, store name and position already, so it gains `markdown`
-- alone; `check/subject` and `check/program` state every value already, so each gains the
-- `creatable_since` 8 its row now carries, a later value appended under that name, and the 7
-- 20261125125701 stated stays in its history; and the two supertag nodes, to which the seed gave
-- no attribute, gain every value above.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'ask/invalid', 'markdown', 'value', 'false', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'kind', 'value', 'block[]', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'name', 'value', 'shaped', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'position', 'value', '10', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'ask/shaped', 'store_name', 'value', 'shaped', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'check/program', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'check/subject', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'kind', 'value', 'actor', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'markdown', 'value', 'false', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'name', 'value', 'decided_by', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'position', 'value', '5', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'server', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/decided_by', 'store_name', 'value', 'decided_by', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'kind', 'value', 'ask', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'name', 'value', 'from_ask', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'position', 'value', '3', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/from_ask', 'store_name', 'value', 'from_ask', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'kind', 'value', 'actor', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'markdown', 'value', 'false', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'name', 'value', 'kept_by', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'position', 'value', '6', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'server', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_by', 'store_name', 'value', 'kept_by', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'markdown', 'value', 'false', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'name', 'value', 'kept_text_sum', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'position', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'server', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/kept_text_sum', 'store_name', 'value', 'kept_text_sum', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'default', 'value', 'false', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'kind', 'value', 'bool', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'name', 'value', 'replaced', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'position', 'value', '4', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/replaced', 'store_name', 'value', 'replaced', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'kind', 'value', 'block[]', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'name', 'value', 'shaped', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'position', 'value', '2', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/shaped', 'store_name', 'value', 'shaped', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'choices', 'value', '["low", "medium", "high"]', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'kind', 'value', 'enum', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'name', 'value', 'uncertainty', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'position', 'value', '0', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty', 'store_name', 'value', 'uncertainty', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'name', 'value', 'uncertainty_reason', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'position', 'value', '1', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('attribute', 'decision/uncertainty_reason', 'store_name', 'value', 'uncertainty_reason', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'block_type', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'content', 'value', 'block+', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'position', 'value', '5', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'render', 'value', 'host', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'decision', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'block_type', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'content', 'value', 'paragraph+', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'parents', 'value', '["decision", "ask"]', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'position', 'value', '6', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'render', 'value', 'host', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'reading', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true);

-- The version. Every block type's supertag node carries the version of the schema the store
-- holds, and the schema the nodes build takes the highest of them, so each one moves to 7, not
-- the two new types alone. A supertag that declares no block type carries no version and is left
-- alone. The block types the store holds at this version are the five below, and `decision` and
-- `reading`, which took 7 with their values above. A bounded write sets a column to a literal, so
-- each of the five is set to its whole attributes: what 20261109051232, 20261113230000,
-- 20261115130000, 20261123080000 and 20261125125701 left on it, its version moved from 6 to 7,
-- which is the literal 20261125125701 sets on it with 7 in place of 6. Nothing but a migration
-- writes a type node, so that is what each holds, and this rewrites nothing but the version (the
-- census says why). No two carry the same value, so each is a statement of its own.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 7, "position": 0}'::jsonb
where (kind, id) in (('supertag', 'callout'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 7, "position": 1}'::jsonb
where (kind, id) in (('supertag', 'ask'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'requirement'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'plan_task'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'check'));

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 7 its row now carries, and the 6 before it, and
-- the 3, the 4 and the 5 before that, stay in its history. `decision` and `reading` state their 7
-- among their values above.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('supertag', 'ask', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'callout', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'check', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'plan_task', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true),
  ('supertag', 'requirement', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-decision-reading"}', 0, null, true);
