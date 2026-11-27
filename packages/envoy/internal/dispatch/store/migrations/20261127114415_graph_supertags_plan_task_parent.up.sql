-- 20261127114415_graph_supertags_plan_task_parent.up.sql
--
-- The block schema moves to version 7. `pmdoc/schema/blocks.json` gave the `plan_task` type a
-- fourth attribute, `parent`, a `block[]` declared after `steps` that a build reads from version 7
-- and creates from version 8, and raised the file's version from 6 to 7. A task block names the
-- block it stands under in `parent`, at most one. A build of this release reads a plan that holds
-- it and refuses to put it into a plan that holds none (pmdoc's refuseUncreatable); the next
-- release creates it. Version 6 is 20261125125701's, which every release from legion-envoy-v35.18.0
-- on ships, so this one takes 7. At 7 a build also creates `check`'s `subject` and
-- `program`, which 20261125125701 declared read from 6 and created from 7; their nodes stay as that
-- migration wrote them. A type is a node of the store, and the server installs the schema it
-- builds from those nodes, not the file, so the change reaches a store only as a migration: a type
-- changes by a migration, in the release of the binary that handles its version (20261109051232).
-- Without this one a binary built for version 7 installs version 6, whose `plan_task` declares no
-- `parent`, so a plan that holds one is a document this build cannot read.
--
-- It sorts above 20261125125703_btree_gin: a pending `create extension` takes the census's size
-- exemptions from every later pending migration, and this migration needs them, so it needs a
-- store where 20261125125703 is applied already. That migration's release note requires its
-- release (from legion-envoy-v35.4.0) to be applied before any release that holds a later
-- migration, so the store this release meets has it.
--
-- It is written after the store's backfill, as 20261123080000 is, and in the same shape: the
-- migration census admits a write of graph_nodes or graph_tuples above its table limit only as a
-- bounded write, so this migration writes the node and its tuples as literal rows, with no `on
-- conflict`, and each block type's supertag node by an update of its primary key to a literal.
-- cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs this file
-- after a backfill, with the census at a one-byte limit, and holds what it leaves to the file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build of the release before
-- leaves a version schema_migrations records and it has no file of as it is (pgmigrate.Reconcile),
-- and installs a store schema above its own version at its own version (pmdoc.Install's
-- asOfVersion), so it serves version 6 and keeps serving. No build of this release puts `parent`
-- into a plan that holds none, so a plan holds one only where a build that creates it wrote it.
-- The release before reads such a plan as holding an element it does not know
-- (pmdoc.ErrUnknownElement), which the document routes answer 409 DOC_NEWER_CONTENT. A later
-- return to this release finds the migration recorded and runs nothing.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 7, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new attribute and the new version together, or neither. The
-- triggers of 20261109051232 announce the change once, at that commit, however many rows it
-- writes, and every running server installs the schema the nodes then build. now() is the
-- transaction's start, so every row below carries one time. The migration records no event, so
-- every row it writes carries the `source_event` sentinel 0, and its rows carry a writer of their
-- own, so a reader of the store can tell them from the seed's and from the earlier type
-- migrations'.

-- The attribute node. Its id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, `{"kind": "block[]", "since":
-- 7, "creatable_since": 8}`, with its name, its store name and its position beside it. `parent`
-- is the name of no base attribute, so its store name is its own name
-- (pmdoc/schemanodes/schemanodestest.StoreName). The position is its place among the type's
-- attributes, counted from 0: serves, needs and steps hold 0 to 2, so parent holds 3.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'plan_task/parent', 'attribute', 'project', 'live',
        '{"kind": "block[]", "since": 7, "creatable_since": 8, "name": "parent", "store_name": "parent", "position": 3}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0);

-- The node's facts as the tuples they are, as the seed states an attribute node's: its two base
-- attributes, the `attribute_of` tuple that ties it to `plan_task` (a reader of the schema follows
-- this tuple, never the spelling of the node's id, so without it the node is no attribute of any
-- type), and a `value` tuple for each value its row carries, whose kind is the value's name and
-- whose far end is the value as text. Each carries the node's time and writer.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('attribute', 'plan_task/parent', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'attribute_of', 'supertag', 'plan_task', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'kind', 'value', 'block[]', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'name', 'value', 'parent', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'position', 'value', '3', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/parent', 'store_name', 'value', 'parent', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true);

-- The version. The seed wrote the schema's version on every block type's supertag node, and the
-- schema the nodes build takes the highest of them. Each one moves to 7, not `plan_task` alone,
-- so every type node goes on carrying the version of the schema the store holds, as
-- 20261125125701 left them. A bounded write sets a column to a literal, so each node's whole
-- attributes are written out: what 20261125125701 left on it, its version moved from 6 to 7.
-- Nothing but a migration writes a type node, so that is what each holds, and this rewrites
-- nothing but the version (the census says why). A supertag that declares no block type carries
-- no version and is left alone.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 7, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 7, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 7, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 7 its row now carries, and the earlier ones stay
-- in its history. The tuples carry this migration's writer and its time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '7', now(), '{"kind": "system", "id": "block-schema-plan-task-parent"}'::jsonb, 0, null, true);
