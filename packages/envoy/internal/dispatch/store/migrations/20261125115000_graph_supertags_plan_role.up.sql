-- 20261125115000_graph_supertags_plan_role.up.sql
--
-- `pmdoc/schema/blocks.json` gave the schema a sixth typed block type, `plan_role`, declared last,
-- after `check`, whose content is `block+` and whose two attributes are the strings `type` and
-- `model`, each with the default "", a type a build reads from version 6 and creates only from
-- version 8 (the roles an owner's plan defines: the block's id is the role's name, `type` the type
-- of its step, `model` its model and its text its prompt). The file stays at version 6, the
-- version 20261125125701 moves every block type to, so a build of this release reads a stored
-- `plan_role` and no write of it puts one into a document that holds none. A type is a node of
-- the store, and the server installs the schema it builds from those nodes, not the file, so the
-- change reaches a store only as a migration (20261109051232). Without this one a binary built
-- for this file installs a schema that declares no `plan_role`, and the documents of a later
-- release that hold one would be elements this release does not know.
--
-- It sorts below 20261125125701_graph_supertags_check_subject, so the two run in either order: a
-- store that holds neither runs this one first, and a store of legion-envoy-v35.18.0 or later,
-- which holds 20261125125701, runs it after. So it writes only its own three nodes and their
-- tuples, and moves no other type node: `plan_role` stands last in the file, at the position
-- after check's, and carries the version 6 that 20261125125701 writes on every other block type.
-- It leaves the same nodes in either order. It cannot sort after 20261125125701: 20261125125702
-- and 20261125125703 hold the two versions after it, and a type migration after
-- 20261125125703_btree_gin loses the census's admission of bounded writes wherever that migration
-- is pending, since a pending `create extension` takes it from every later pending migration
-- (cmd/dispatch/README.md, "Pre-deploy census").
--
-- It is written after the store's backfill, as 20261123080000 is. A deployment may have run
-- `backfill-graph-store` on any release that carries the graph store, which fills graph_nodes and
-- graph_tuples with a node and its tuples for every comment, block and block reference it holds.
-- Above the migration census's table limit the census admits a write of either table only as a
-- bounded write, and so this migration writes nothing else: each node and its tuples as literal
-- rows, with no `on conflict`. cmd/dispatch's
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs this file after a
-- backfill, before 20261125125701, and
-- TestThePlanRoleTypeMigrationAfterTheCheckSubjectOneLeavesTheFilesSchema runs it after that one;
-- each takes the census at a one-byte limit and holds what the store is left with to the file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build of an earlier release
-- leaves a version schema_migrations records and it has no file of as it is
-- (pgmigrate.Reconcile). A build at version 6 installs the store's schema as it is, so it reads a
-- stored `plan_role` and creates none; a build at version 5 installs it at its own version
-- (pmdoc.Install's asOfVersion), which leaves `plan_role` out. No document holds a `plan_role`
-- before a build of version 8 writes one, so no build of an earlier release meets one before that;
-- after it, a build at version 5 reads it as an element it does not know
-- (pmdoc.ErrUnknownElement), which the document routes answer 409 DOC_NEWER_CONTENT. A later
-- return to this release finds the migration recorded and runs nothing.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves, beside what 20261125125701 leaves, is what the seed
-- would build from the file as it stands at version 6, and
-- TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new type whole, or not at all. On a store that holds neither
-- migration, the schema the nodes build between this commit and 20261125125701's is at version 6,
-- with no `subject` or `program` on check yet; no document of such a store holds either, or a
-- `plan_role`, since only a build at version 7 or later creates the marks and only one at version
-- 8 or later creates `plan_role`, and such a build ran both migrations. The triggers of
-- 20261109051232 announce the change once, at that commit, however many rows it writes, and every
-- running server installs the schema the nodes then build. now() is the transaction's start, so
-- every row below carries one time, as the seed's rows carry its one time. The migration records
-- no event, so every row it writes carries the `source_event` sentinel 0, and its rows carry a
-- writer of their own, so a reader of the store can tell them from the seed's.

-- The type node. Its attributes are what the seed builds for a type of the file, with what
-- 20261115130000 adds for a type that says from which version a build reads and creates it:
-- `block_type`, its content and its render, the schema's version, 6, its position among the file's
-- types counted from 0 (callout, ask, requirement, plan_task and check hold 0 to 4, so plan_role
-- holds 5), its `since`, 6, and its `creatable_since`, 8.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('supertag', 'plan_role', 'supertag', 'project', 'live',
        '{"block_type": true, "content": "block+", "render": "host", "version": 6, "position": 5, "since": 6, "creatable_since": 8}'::jsonb,
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
       ('supertag', 'plan_role', 'creatable_since', 'value', '8', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'position', 'value', '5', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'render', 'value', 'host', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'since', 'value', '6', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
       ('supertag', 'plan_role', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-plan-role"}'::jsonb, 0, null, true),
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
