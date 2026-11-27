-- 20261125125701_graph_supertags_check_subject.up.sql
-- replaces: 20261125125701_graph_supertags_check_subject.up.sql sha256:62571a5dda3b5be584c7ffc092a1403686b5d8a0cd047c34f1aa2dff80547c2d
--
-- It sorts below 20261125125703_btree_gin on purpose: a pending `create extension` takes the
-- census's size exemptions from every later pending migration, and this one needs them.
--
-- `pmdoc/schema/blocks.json` gave the `check` type two attributes after `ci_check`: `subject`, an
-- enum of `code`, `production` and `graph` that says what the check reads (absent is `code`), and
-- `program`, a string naming the deployed program the check reads. A build reads each from version
-- 6 and creates it from version 7. A type is a node of the store, and the server installs the
-- schema it builds from those nodes, not the file, so the change reaches a store only as a
-- migration (20261109051232). Without this one a server installs a `check` that declares neither
-- attribute, so a document that holds one would be an element it does not know.
--
-- This text is an edit of the text main shipped first in legion-envoy-v35.18.0, which the replaces
-- line above names. That text also moved the five block type supertag nodes (callout, ask,
-- requirement, plan_task and check) to version 6, the version blocks.json declared on main, and
-- appended a version tuple for each. It sorts above 20261125012814_graph_supertags_plan_task_merges,
-- which moves them to 11, and 20261125034832_graph_supertags_plan_task_owner, which moves them to
-- 12. So on a fresh database, and on a store of a release before legion-envoy-v35.18.0, the runner
-- applies this file after both, and main's text left the five type nodes there at 6 while
-- blocks.json declares 12; at 6 the schema a server installs leaves out plan_task/merges (since 11)
-- and plan_task/owner (since 12).
--
-- No migration above this one can write the version again. The migration census (pgmigrate's
-- Census) admits bounded writes of graph_nodes and graph_tuples above its table limit only while no
-- earlier pending migration creates or alters an extension (pgmigrate's takesLaterExemptions).
-- 20261125125703_btree_gin creates one, and the one identifier between this file and that one is
-- taken by 20261125125702_after_merge_states. So over a store that ran
-- `backfill-graph-store` at a release before legion-envoy-v35.18.0, the census refuses every type
-- migration numbered above 20261125125703, whatever its statements, and cmd/dispatch's
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema fails on it.
--
-- So this text moves no version. It writes the two attribute nodes and their tuples and nothing
-- else, and every database ends at the 12 that 20261125034832 wrote. A fresh database, and a store
-- of a release before legion-envoy-v35.18.0, apply 20261125012814 and 20261125034832 before this
-- file. A database that ran main's text records this version, and the runner applies
-- 20261125012814 and 20261125034832 after it, as versions it has not recorded: they move the type
-- nodes from 6 to 11 and then to 12. The replaces line accepts that database as it is, and the two
-- leave it at 12 too.
--
-- It is a type migration written after the store's backfill, as 20261123080000 is, the first of
-- them: a deployment may have run `backfill-graph-store`, which fills graph_nodes and graph_tuples
-- with a node and its tuples for every comment, block and block reference it holds, and above the
-- migration census's table limit the census admits a write of either table only as a bounded
-- write. So this migration writes nothing else: each node and its tuples as literal rows, with no
-- `on conflict`. cmd/dispatch's TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema
-- runs it after a backfill, with the census at a one-byte limit, and holds what it leaves to the
-- file.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert, on graph_nodes' primary key (kind, id),
-- and the runner's one transaction takes back everything it wrote.
--
-- The way back is the image alone, and the store stays as it is. A build of a release before
-- legion-envoy-v35.18.0 leaves a version schema_migrations records and it has no file of as it is
-- (pgmigrate.Reconcile), and installs a store schema above its own version at its own version
-- (pmdoc.Install's asOfVersion), so it serves version 5 and keeps serving. It reads a stored check
-- that carries `subject` or `program`, which a build of version 7 or later writes, as an element it
-- does not know (pmdoc.ErrUnknownElement), which the document routes answer 409
-- DOC_NEWER_CONTENT. A later return to this release finds the migration recorded and runs nothing.
-- The edit costs one way back: a database this release creates from empty records this text as
-- the text it applied, and a binary of a release that holds main's text, from
-- legion-envoy-v35.18.0 to the last release before this edit, refuses to boot on it (pgmigrate's
-- Reconcile refuses an applied text that its file does not name), so such a database is reset,
-- not rolled back to one of those releases. A database that ran main's text keeps that text's
-- checksum, and every one of those releases boots on it as before.
--
-- The change is stated here as rows rather than read from the file, for the reason the seed
-- copies its text in: a migration states what the database held at one version and must not
-- change when the file does. The two attribute nodes are what the seed builds from the file for
-- `subject` and `program`, and TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the
-- store to the file field by field.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the schema
-- changes: a server reads both attributes, or neither. The triggers of 20261109051232 announce the
-- change once, at that commit, however many rows it writes, and every running server installs the
-- schema the nodes then build. now() is the transaction's start, so every row below carries one
-- time. The migration records no event, so every row it writes carries the `source_event` sentinel
-- 0, and its rows carry a writer of their own, so a reader of the store can tell them from the
-- seed's and from 20261123080000's.

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
