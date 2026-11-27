-- 20261126040700_graph_supertags_version_8.up.sql
--
-- The block schema moves from version 7 to version 8, the bump that makes the `decision` and
-- `reading` block types, every attribute of `decision`, and `ask`'s `shaped` creatable.
-- 20261125163555_graph_supertags_decision_reading declared them at version 7, read from 7 and
-- created only from 8 (`creatable_since` 8), so a build of version 7 reads and validates a stored
-- one and refuses to create one. `pmdoc/schema/blocks.json` now declares version 8, and each of
-- those elements keeps the `since` and `creatable_since` 20261125163555 gave it. A build installs
-- the schema the store's nodes build, not the file (20261109051232), and that schema takes the
-- highest `version` its block types carry, so without this migration a build of version 8
-- installs version 7 and still refuses to create a decision.
--
-- The file changes two elements besides: `check`'s `subject` and `program`, which
-- 20261125125701_graph_supertags_check_subject declared at version 6, read from 6 and created from
-- 7, now keep their `since` 6 and take `creatable_since` 9, so this build reads them and creates
-- neither. The release after #5416 that records real deploys has not shipped yet, and until it
-- does no build creates a check that names what it reads: `creatable_since` moves with each block
-- schema bump until that release sets its final value. So this file moves the version of the
-- seven block types and the `creatable_since` of those two attribute nodes, and nothing else.
--
-- It is a type migration written after the store's backfill, as 20261123080000 and
-- 20261125163555 are: it is numbered above backfillVersion (cmd/dispatch/supertag_nodes_test.go)
-- and above 20261125163555, so it runs after that one on every store. Each statement is one of
-- the two shapes the pre-deploy census admits on a table past pgmigrate.CensusTableLimit: an
-- `update` that sets `attributes` to a literal on the rows its `where` names by their primary
-- key, `(kind, id)` (20261109013400), in literal values, and an `insert … values` of literal
-- tuples. It reads no row to write one. cmd/dispatch's
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs the type migrations
-- after a backfill, with the census at a one-byte limit, and holds what they leave to the file.
--
-- Locks. The updates take ROW EXCLUSIVE on graph_nodes and a row lock on each of the seven
-- supertag nodes and the two attribute nodes they name; the inserts take ROW EXCLUSIVE on
-- graph_tuples. Neither blocks a read, and no route or command writes a type node
-- (requireNoRouteOrCommandWritesAType in cmd/dispatch/supertag_nodes_test.go), so no request waits
-- on those row locks.
--
-- Rolling deploy. A build of version 7, which holds 20261125163555 and not this file, installs a
-- store schema above its own version at its own version (pmdoc.Install, asOfVersion): every
-- element this file leaves has `since` 7 or less, so it installs the schema it installed before
-- this ran, but for `check`'s `subject` and `program`, which it then reads and no longer creates,
-- as `creatable_since` 9 says. It reads a document this build writes with a decision, a reading or
-- an ask's `shaped`, and still refuses to create one, as `creatable_since` 8 says. So the two
-- builds serve one database side by side. The way back is the image alone, and the store stays at
-- version 8: the build before leaves a version schema_migrations records and it has no file of as
-- it is (pgmigrate.Reconcile) and installs the store's schema at version 7, as above. A later
-- return to this release finds the migration recorded and runs nothing.
--
-- It runs once, as the runner applies every file (typeMigrations in
-- cmd/dispatch/supertag_nodes_test.go). A second run all the same (the file applied by hand) sets
-- the same attributes again and appends one more `version` tuple of 8 to each type and one more
-- `creatable_since` tuple of 9 to each of the two attributes, which read as the same values.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 8, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes, and the triggers of 20261109051232 announce it once, at that commit, so every running
-- server installs the new version at once. Every row carries now(), the start of the runner's
-- transaction, as the moment it was stated, so each type's newest `version` tuple orders after
-- the 7 that 20261125163555 stamped at its own apply, and each of the two attributes' newest
-- `creatable_since` tuple after the 7 that 20261125125701 stamped at its own (graphstore.AsOfOut
-- orders a node's tuples by `at` first). The migration records no event, so every tuple it writes
-- carries the `source_event` sentinel 0, and its rows carry a writer of their own, so a reader of
-- the store can tell them from the seed's.

-- The version. Every block type's supertag node carries the version of the schema the store
-- holds, and the schema the nodes build takes the highest of them, so each of the seven block
-- types moves to 8. A supertag that declares no block type carries no version and is left alone.
-- A bounded write sets a column to a literal, so each node is set to its whole attributes: what
-- 20261125163555 left on it, its version moved from 7 to 8. Nothing but a migration writes a type
-- node, so that is what each holds, and this rewrites nothing but the version (the census says
-- why). No two carry the same value, so each is a statement of its own.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 8, "position": 0}'::jsonb
where (kind, id) in (('supertag', 'callout'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 8, "position": 1}'::jsonb
where (kind, id) in (('supertag', 'ask'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 8, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'requirement'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 8, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'plan_task'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 8, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where (kind, id) in (('supertag', 'check'));

update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 8, "position": 5, "since": 7, "creatable_since": 8}'::jsonb
where (kind, id) in (('supertag', 'decision'));

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 8, "position": 6, "since": 7, "creatable_since": 8, "parents": ["decision", "ask"]}'::jsonb
where (kind, id) in (('supertag', 'reading'));

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 8 its row now carries, and the 7 before it, and
-- the versions before that, stay in its history.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('supertag', 'ask', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'callout', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'check', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'decision', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'plan_task', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'reading', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('supertag', 'requirement', 'version', 'value', '8', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true);

-- `check`'s `subject` and `program`: each attribute node is set to its whole attributes, what
-- 20261125125701 left on it with `creatable_since` moved from 7 to 9. No migration after
-- 20261125125701 writes either node, so that is what each holds, and this rewrites nothing but
-- that number. The two take different values, so each is a statement of its own.
update graph_nodes
set attributes = '{"kind": "enum", "choices": ["code", "production", "graph"], "since": 6, "creatable_since": 9, "name": "subject", "store_name": "subject", "position": 6}'::jsonb
where (kind, id) in (('attribute', 'check/subject'));

update graph_nodes
set attributes = '{"kind": "string", "since": 6, "creatable_since": 9, "name": "program", "store_name": "program", "position": 7}'::jsonb
where (kind, id) in (('attribute', 'check/program'));

-- The move, as the tuple it is: each attribute's newest `creatable_since` tuple is the 9 its row
-- now carries, and the 7 before it stays in its history.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'check/program', 'creatable_since', 'value', '9', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true),
  ('attribute', 'check/subject', 'creatable_since', 'value', '9', now(), '{"kind": "system", "id": "block-schema-version-8"}', 0, null, true);
