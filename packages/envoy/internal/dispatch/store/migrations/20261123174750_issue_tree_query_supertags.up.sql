-- 20261123174750_issue_tree_query_supertags.up.sql
--
-- Two supertags join the store's types (this change's spec, R8 and R16; the root's R21 list):
--
-- - `query`, the node of a view of an issue's tree: the spec, the verification, the plan or the
--   decisions, each stating the supertags it takes and the node its walk starts at as values of
--   its own (graphstore.QueryNode);
-- - `query version`, a version of a query node, as `document version` is a version of a document
--   (graphstore.VersionFacts).
--
-- A type is a node of the store, so the two reach a store only as a migration, in the release of
-- the binary that writes their nodes (20261109051232). Neither declares a block type: a block of a
-- document never carries either, so neither carries a content rule, a renderer, a position or a
-- schema version, and the block schema every server installs from the type nodes stays at the
-- version the store holds (pmdoc/schemanodes reads the supertags that declare a block type alone).
-- Each is written as the seed writes a supertag of the closed list that declares no block type:
-- its node, carrying no attribute, and its two base attributes as the tuples they are.
--
-- It is a type migration written after the store's backfill, as
-- 20261125163555_graph_supertags_decision_reading is: no release that may have run
-- `backfill-graph-store` carries it, so it is numbered above backfillVersion
-- (cmd/dispatch/supertag_nodes_test.go) and above every type migration before it, and over a
-- store the backfill filled the pre-deploy census judges it with graph_nodes and graph_tuples past
-- pgmigrate.CensusTableLimit. So every statement is an `insert … values` of literal rows, the one
-- shape the census admits there, and no statement reads a row to write one: the store holds
-- neither node before this runs, since the seed's closed list names neither and nothing but a
-- migration writes a type node. cmd/dispatch's
-- TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema runs it after a backfill,
-- with the census at a one-byte limit. Every row carries now(), the start of the runner's
-- transaction, as the rows of 20261125163555 do.
--
-- It runs once, as the runner applies every file: its inserts are not repeatable, so it joins no
-- second run (typeMigrations in cmd/dispatch/supertag_nodes_test.go). A second run all the same
-- (the file applied by hand) stops at its first insert into graph_nodes, on the primary key of a
-- row the first run wrote, and the runner's one transaction takes back everything it wrote. The
-- migration records no event, so every row it writes carries the `source_event` sentinel 0, and
-- its rows carry a writer of their own, so a reader of the store can tell them from the seed's.
-- The triggers of 20261109051232 announce the change at its commit, and every running server
-- reinstalls a schema the change leaves as it was.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('supertag', 'query', 'supertag', 'project', 'live', '{}', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0),
  ('supertag', 'query version', 'supertag', 'project', 'live', '{}', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0);

-- The two base attributes each node carries, as the tuples they are, as the seed states them: at
-- the time and by the writer of the node's row.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('supertag', 'query', 'state', 'value', 'live', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0, null, true),
  ('supertag', 'query', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0, null, true),
  ('supertag', 'query version', 'state', 'value', 'live', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0, null, true),
  ('supertag', 'query version', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "issue-tree-query-supertags"}', 0, null, true);
