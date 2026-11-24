-- 20261123080000_graph_supertags_plan_task_steps.up.sql
--
-- The block schema moves to version 5. `pmdoc/schema/blocks.json` gave the `plan_task` type a
-- third attribute, `steps`, a string declared after `needs` that a build reads and creates from
-- version 5, and raised the file's version from 4 to 5 (#4611: the steps an owner's plan task
-- runs, in order). A type is a node of the store, and the server installs the schema it builds
-- from those nodes, not the file, so the change reaches a store only as a migration: a type
-- changes by a migration, in the release of the binary that handles its version (20261109051232).
-- Without this one a binary built for version 5 installs version 4, whose `plan_task` declares no
-- `steps`, so a plan task that names its steps is refused as markdown outside the schema, and the
-- daemon's register_plan reads no plan that picks its steps.
--
-- It is the first type migration written after the store's backfill. Every legion-envoy release
-- from v34.11.0, the first that creates graph_nodes and graph_tuples (20261109013400), up to the
-- release that carries this migration holds every migration through 20261121060636 and none
-- after it: v34.11.0 to v34.11.4 on 2026-11-23. A deployment may have run `backfill-graph-store`
-- on any of them, which fills graph_nodes and graph_tuples with a node and its tuples for every
-- comment, block and block reference it holds. Above the migration census's table limit the
-- census admits a write of either table only as a bounded write, and so this migration writes
-- nothing else: the node and its tuples as literal rows, with no `on conflict`, and each block
-- type's supertag node by an update of its primary key to a literal. The type migrations before
-- the backfill (20261109051232, 20261113230000 and 20261115130000) read their rows back off
-- graph_nodes with `insert … select`, `where not exists` and `on conflict … do update`, which the
-- census refuses
-- there. cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs
-- this file after a backfill, with the census at a one-byte limit, and holds what it leaves to the
-- file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build of v34.11.x leaves a
-- version schema_migrations records and it has no file of as it is (pgmigrate.Reconcile), and
-- installs a store schema above its own version at its own version (pmdoc.Install's
-- asOfVersion), so it serves version 4 and keeps serving. It reads a stored plan task that carries
-- steps as an element it does not know (pmdoc.ErrUnknownElement), which the document routes answer
-- 409 DOC_NEWER_CONTENT, and no write of it adds steps; the plan's document waits for a build at
-- version 5. A later return to this release finds the migration recorded and runs nothing.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 5, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new attribute and the new version together, or neither. The
-- triggers of 20261109051232 announce the change once, at that commit, however many rows it
-- writes, and every running server installs the schema the nodes then build. now() is the
-- transaction's start, so every row below carries one time, as the seed's rows carry its one
-- time. The migration records no event, so every row it writes carries the `source_event`
-- sentinel 0, and its rows carry a writer of their own, so a reader of the store can tell them
-- from the seed's.

-- The attribute node. Its id is `<supertag>/<store name>`, and its attributes are what the seed
-- builds for an attribute of the file: the attribute's own JSON, `{"kind": "string", "since": 5,
-- "creatable_since": 5}`, with its name, its store name and its position beside it. `steps` is the
-- name of no base attribute, so its store name is its own name
-- (pmdoc/schemanodes/schemanodestest.StoreName). The position is its place among the type's
-- attributes, counted from 0: serves and needs hold 0 and 1, so steps holds 2.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'plan_task/steps', 'attribute', 'project', 'live',
        '{"kind": "string", "since": 5, "creatable_since": 5, "name": "steps", "store_name": "steps", "position": 2}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0);

-- The node's facts as the tuples they are, as the seed states an attribute node's: its two base
-- attributes, the `attribute_of` tuple that ties it to `plan_task` (a reader of the schema follows
-- this tuple, never the spelling of the node's id, so without it the node is no attribute of any
-- type), and a `value` tuple for each value its row carries, whose kind is the value's name and
-- whose far end is the value as text. Each carries the node's time and writer.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('attribute', 'plan_task/steps', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'attribute_of', 'supertag', 'plan_task', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'creatable_since', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'name', 'value', 'steps', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'position', 'value', '2', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'since', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/steps', 'store_name', 'value', 'steps', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true);

-- The version. The seed wrote the schema's version on every block type's supertag node, and the
-- schema the nodes build takes the highest of them. Each one moves to 5, not `plan_task` alone, so
-- every type node goes on carrying the version of the schema the store holds, as 20261113230000
-- left them. A bounded write sets a column to a literal, so each node's whole attributes are
-- written out: what 20261109051232, 20261113230000 and 20261115130000 left on it, its version
-- moved from 4 to 5. Nothing but a migration writes a type node, so that is what each holds, and
-- this rewrites nothing but the version (the census says why). A supertag that declares no block
-- type carries no version and is left alone.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 5, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 5, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 5, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 5, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 5, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 5 its row now carries and the 3 and the 4 before
-- it stay in its history. The tuples carry this migration's writer and its time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-task-steps"}'::jsonb, 0, null, true);
