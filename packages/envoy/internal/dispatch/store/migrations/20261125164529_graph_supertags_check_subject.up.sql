-- 20261125164529_graph_supertags_check_subject.up.sql
--
-- The block schema moves to version 6. `pmdoc/schema/blocks.json` gave the `check` type two
-- attributes after `ci_check`: `subject`, an enum of `code`, `production` and `graph` that says
-- what the check reads (absent is `code`), and `program`, a string naming the deployed program the
-- check reads. A build reads each from version 6 and creates it only from version 7, so this
-- release reads a check that holds them and no write of it puts one into a document that holds
-- none; the next release creates them. A type is a node of the store, and the server installs the
-- schema it builds from those nodes, not the file, so the change reaches a store only as a
-- migration (20261109051232). Without this one a binary built for version 6 installs version 5,
-- whose `check` declares neither attribute, so the next release's documents that hold them would
-- be elements this release does not know.
--
-- It is a type migration written after the store's backfill, as 20261123080000 is, the first of
-- them: a deployment may have run `backfill-graph-store`, which fills graph_nodes and graph_tuples
-- with a node and its tuples for every comment, block and block reference it holds, and above the
-- migration census's table limit the census admits a write of either table only as a bounded
-- write. So this migration writes nothing else: each node and its tuples as literal rows, with no
-- `on conflict`, and each block type's supertag node by an update of its primary key to a literal.
-- cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs it after
-- a backfill, with the census at a one-byte limit, and holds what it leaves to the file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build of the release before
-- leaves a version schema_migrations records and it has no file of as it is (pgmigrate.Reconcile),
-- and installs a store schema above its own version at its own version (pmdoc.Install's
-- asOfVersion), so it serves version 5 and keeps serving. No document can hold `subject` or
-- `program` before a build of version 7 writes one, so no build of the release before meets
-- either; after that, it reads a stored check that carries one as an element it does not know
-- (pmdoc.ErrUnknownElement), which the document routes answer 409 DOC_NEWER_CONTENT. A later
-- return to this release finds the migration recorded and runs nothing.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. What it leaves is what the seed would build from the file as it
-- stands at version 6, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two
-- together field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads the new attributes and the new version together, or neither. The
-- triggers of 20261109051232 announce the change once, at that commit, however many rows it
-- writes, and every running server installs the schema the nodes then build. now() is the
-- transaction's start, so every row below carries one time. The migration records no event, so
-- every row it writes carries the `source_event` sentinel 0, and its rows carry a writer of their
-- own, so a reader of the store can tell them from the seed's and from 20261123080000's.

-- The attribute nodes. Each id is `<supertag>/<store name>`, and each node's attributes are what
-- the seed builds for an attribute of the file: the attribute's own JSON, with its name, its store
-- name and its position beside it. Neither `subject` nor `program` is the name of a base
-- attribute, so each store name is its own name (pmdoc/schemanodes/schemanodestest.StoreName). The
-- position is the attribute's place among the type's attributes, counted from 0: serves, surface,
-- run, when, negative_control and ci_check hold 0 to 5, so subject holds 6 and program 7.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'check/subject', 'attribute', 'project', 'live',
        '{"kind": "enum", "choices": ["code", "production", "graph"], "since": 6, "creatable_since": 7, "name": "subject", "store_name": "subject", "position": 6}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0),
       ('attribute', 'check/program', 'attribute', 'project', 'live',
        '{"kind": "string", "since": 6, "creatable_since": 7, "name": "program", "store_name": "program", "position": 7}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0);

-- Each node's facts as the tuples they are, as the seed states an attribute node's: its two base
-- attributes, the `attribute_of` tuple that ties it to `check` (a reader of the schema follows
-- this tuple, never the spelling of the node's id, so without it the node is no attribute of any
-- type), and a `value` tuple for each value its row carries, whose kind is the value's name and
-- whose far end is the value as text: a string as itself, any other JSON value as its text, so the
-- choices read `["code", "production", "graph"]`, as 20261109051232 writes an enum's choices. Each
-- carries the node's time and writer.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('attribute', 'check/subject', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'attribute_of', 'supertag', 'check', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'choices', 'value', '["code", "production", "graph"]', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'creatable_since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'kind', 'value', 'enum', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'name', 'value', 'subject', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'position', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'since', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/subject', 'store_name', 'value', 'subject', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'attribute_of', 'supertag', 'check', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'creatable_since', 'value', '7', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'name', 'value', 'program', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'position', 'value', '7', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'since', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('attribute', 'check/program', 'store_name', 'value', 'program', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true);

-- The version. The seed wrote the schema's version on every block type's supertag node, and the
-- schema the nodes build takes the highest of them. Each one moves to 6, not `check` alone, so
-- every type node goes on carrying the version of the schema the store holds, as 20261123080000
-- left them. A bounded write sets a column to a literal, so each node's whole attributes are
-- written out: what 20261109051232, 20261113230000, 20261115130000 and 20261123080000 left on it,
-- its version moved from 5 to 6. Nothing but a migration writes a type node, so that is what each
-- holds, and this rewrites nothing but the version (the census says why). A supertag that
-- declares no block type carries no version and is left alone.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 6, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 6, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 6, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 6, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 6, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 6 its row now carries and the earlier ones stay
-- in its history. The tuples carry this migration's writer and its time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '6', now(), '{"kind": "system", "id": "block-schema-check-subject"}'::jsonb, 0, null, true);
