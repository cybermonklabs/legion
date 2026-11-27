-- 20261127042910_graph_supertags_version_of_the_merge_with_main.up.sql — every block type's
-- supertag node moved to version 12, the version `pmdoc/schema/blocks.json` declares after this
-- branch's merge of main.
--
-- The rule this file follows: the last migration to write a value owns it. The block schema's
-- version is one value, written on all five block type supertag nodes, and each branch wrote it.
-- This branch's 20261125012814_graph_supertags_plan_task_merges wrote 11 and its
-- 20261125034832_graph_supertags_plan_task_owner wrote 12; main's
-- 20261125125701_graph_supertags_check_subject, which adds check/subject and check/program, wrote
-- 6, and it sorts above both. On a fresh database the runner applies every file in version order,
-- so main's runs after ours and 6 is the last word: the five type nodes end at version 6 while
-- the file declares 12, which is what this file exists to end. This file stands above main's, so
-- it is the last word on every database.
--
-- Nothing else of the schema is wrong without it. The attribute nodes of both branches stand:
-- main's 20261125125701 inserts check/subject and check/program and deletes nothing, so
-- plan_task/merges (since 11) and plan_task/owner (since 12) are still there. Only the version
-- the five type nodes carry is behind, and the version is what decides what a server serves: the
-- server installs the schema built from these nodes and not the embedded file (20261109051232),
-- and pmdoc.Install reads a store at its own version, leaving out every element whose `since` is
-- above it. At version 6 that is plan_task/merges and plan_task/owner, so a server would serve a
-- schema with no sub-owner attribute, a page built for 12 would be admitted read-only, and
-- cmd/dispatch's TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt would fail with the file
-- at 12 where the store's nodes build 6.
--
-- It writes no attribute node, because neither branch's attributes are missing, and it writes the
-- same five rows every type migration of this branch writes: a bounded write sets a column to a
-- literal, so each node's whole attributes are written out, what 20261109051232, 20261113230000,
-- 20261115130000, 20261123080000, 20261125012814, 20261125034832 and main's 20261125125701 left
-- on it with the version moved to 12. Those are type nodes, and nothing but a migration writes a
-- type node (cmd/dispatch's requireNoRouteOrCommandWritesAType), so that is what each holds.
--
-- Written after the store's backfill, as 20261123080000, 20261125012814, 20261125034832 and
-- main's 20261125125701 are, it writes graph_nodes and graph_tuples by bounded writes alone:
-- literal rows with no `on conflict`, and each type node by its primary key.
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs it over a backfilled
-- store with the migration census at a one-byte limit and holds what it leaves to the file.
--
-- It joins no second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go), as every type
-- migration written after the backfill does. The runner applies each file once. A second run by
-- hand would not fail, since this file inserts no node: it would set the five rows to the values
-- they already carry and append five more version tuples naming 12, which changes nothing a
-- reader reads, because a node's value is its newest value tuple.
--
-- The way back is the image alone, and the store stays as it is. A build of the release before
-- reads this store at its own version (pmdoc.Install's asOfVersion) and serves what it knows, so
-- it serves version 6 with main's check marks and without this branch's plan_task attributes, as
-- it did before this file ran; a document that holds `merges` or `owner` is an element it does
-- not know (pmdoc.ErrUnknownElement), which the document routes answer 409 DOC_NEWER_CONTENT. A
-- later return to this release finds the migration recorded and runs nothing.
--
-- The runner applies a migration in one transaction, so the commit is the one moment the version
-- changes: a server reads the five nodes at 12 together, or at 6. The triggers of 20261109051232
-- announce the change once, at that commit, and every running server installs the schema the
-- nodes then build. now() is the transaction's start, so every row below carries one time. The
-- migration records no event, so every row it writes carries the `source_event` sentinel 0, and
-- its rows carry a writer of their own, so a reader of the store can tell them from the seed's
-- and from each earlier type migration's.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 12, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 12, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- The move, as the tuple it is: a moved attribute is a later value appended under its name, so
-- each block type's newest `version` tuple is the 12 its row now carries and the 6 main's
-- 20261125125701 appended stays in its history. The tuples carry this migration's writer and its
-- time.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-version-of-the-merge-with-main"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-version-of-the-merge-with-main"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-version-of-the-merge-with-main"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-version-of-the-merge-with-main"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-version-of-the-merge-with-main"}'::jsonb, 0, null, true);
