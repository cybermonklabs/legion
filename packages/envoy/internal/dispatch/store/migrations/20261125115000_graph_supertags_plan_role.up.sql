-- 20261125115000_graph_supertags_plan_role.up.sql
--
-- The block schema moves to version 10. `pmdoc/schema/blocks.json` gave the schema a sixth typed
-- block type, `plan_role`, declared after `plan_task` and before `check`, whose content is
-- `block+` and whose two attributes are the strings `type` and `model`, each with the default
-- "", a type a build reads and creates from version 10, and raised the file's version from 5 to
-- 10 (the roles an owner's plan defines: the block's id is the role's name, `type` the type of its
-- step, `model` its model and its text its prompt). A type is a node of the store, and the server
-- installs the schema it builds from those nodes, not the file, so the change reaches a store only
-- as a migration: a type changes by a migration, in the release of the binary that handles its
-- version (20261109051232). Without this one a binary built for version 10 installs version 5,
-- which declares no `plan_role`, so an owner's plan that defines a role is refused as markdown
-- outside the schema, and the daemon's register_plan reads no role a plan defines.
--
-- It is written after the store's backfill, as 20261123080000 is. A deployment may have run
-- `backfill-graph-store` on any release that carries the graph store, which fills graph_nodes and
-- graph_tuples with a node and its tuples for every comment, block and block reference it holds.
-- Above the migration census's table limit the census admits a write of either table only as a
-- bounded write, and so this migration writes nothing else: each node and its tuples as literal
-- rows, with no `on conflict`, and each block type's supertag node by an update of its primary key
-- to a literal. cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema
-- runs this file after a backfill, with the census at a one-byte limit, and holds what it leaves
-- to the file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build at version 5 leaves a
-- version schema_migrations records and it has no file of as it is (pgmigrate.Reconcile), and
-- installs a store schema above its own version at its own version (pmdoc.Install's
-- asOfVersion), so it serves version 5 and keeps serving. It reads a stored `plan_role` block as
-- an element it does not know (pmdoc.ErrUnknownElement), which the document routes answer 409
-- DOC_NEWER_CONTENT, and no write of it creates one; the plan's document waits for a build at
-- version 10. A later return to this release finds the migration recorded and runs nothing.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 10, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new type and the new version together, or neither. The triggers of
-- 20261109051232 announce the change once, at that commit, however many rows it writes, and every
-- running server installs the schema the nodes then build. now() is the transaction's start, so
-- every row below carries one time, as the seed's rows carry its one time. The migration records
-- no event, so every row it writes carries the `source_event` sentinel 0, and its rows carry a
-- writer of their own, so a reader of the store can tell them from the seed's.

-- The type node. Its attributes are what the seed builds for a type of the file, with what
-- 20261115130000 adds for a type that says from which version a build reads and creates it:
-- `block_type`, its content and its render, the schema's version, its position among the file's
-- types counted from 0 (callout, ask, requirement and plan_task hold 0 to 3, so plan_role holds
-- 4), and its `since` and `creatable_since`, both 10.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('supertag', 'plan_role', 'supertag', 'project', 'live',
        '{"block_type": true, "content": "block+", "render": "host", "version": 10, "position": 4, "since": 10, "creatable_since": 10}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0);

-- The attribute nodes. Each id is `<supertag>/<store name>`, and each node's attributes are what
-- the seed builds for an attribute of the file: the attribute's own JSON, `{"kind": "string",
-- "default": ""}`, with its name, its store name and its position beside it. Neither `type` nor
-- `model` is the name of a base attribute, so each store name is its own name
-- (pmdoc/schemanodes/schemanodestest.StoreName). Their positions are their places among the
-- type's attributes, counted from 0: type holds 0 and model 1.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'plan_role/type', 'attribute', 'project', 'live',
        '{"kind": "string", "default": "", "name": "type", "store_name": "type", "position": 0}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0),
       ('attribute', 'plan_role/model', 'attribute', 'project', 'live',
        '{"kind": "string", "default": "", "name": "model", "store_name": "model", "position": 1}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0);

-- Each node's facts as the tuples they are, as the seed states a type node's and an attribute
-- node's: its two base attributes, a `value` tuple for each value its row carries, whose kind is
-- the value's name and whose far end is the value as text (a string default "" is the empty
-- text), and for an attribute node the `attribute_of` tuple that ties it to `plan_role` (a reader
-- of the schema follows this tuple, never the spelling of the node's id, so without it the node is
-- no attribute of any type). Each carries the node's time and writer.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'plan_role', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'block_type', 'value', 'true', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'content', 'value', 'block+', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'creatable_since', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'position', 'value', '4', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'render', 'value', 'host', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'since', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'attribute_of', 'supertag', 'plan_role', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'default', 'value', '', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'name', 'value', 'type', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'position', 'value', '0', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/type', 'store_name', 'value', 'type', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'attribute_of', 'supertag', 'plan_role', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'default', 'value', '', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'name', 'value', 'model', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'position', 'value', '1', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('attribute', 'plan_role/model', 'store_name', 'value', 'model', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true);

-- The version, and the position of check. The seed wrote the schema's version on every block
-- type's supertag node, and the schema the nodes build takes the highest of them. Each one moves to
-- 10, not `plan_role` alone, so every type node goes on carrying the version of the schema the
-- store holds, as 20261123080000 left them. `plan_role` stands before `check` in the file, so
-- check's position moves from 4 to 5. A bounded write sets a column to a literal, so each node's
-- whole attributes are written out: what 20261109051232, 20261113230000, 20261115130000 and
-- 20261123080000 left on it, its version moved from 5 to 10 and check's position from 4 to 5.
-- Nothing but a migration writes a type node, so that is what each holds, and this rewrites
-- nothing else (the census says why). A supertag that declares no block type carries no version
-- and is left alone.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 10, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 10, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 10, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 10, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 10, "position": 5, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- The moves, as the tuples they are: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 10 its row now carries, check's newest
-- `position` tuple is its 5, and the values before them stay in their history. The tuples carry
-- this migration's writer and its time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'position', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '10', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true);
