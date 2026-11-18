-- 20261116094058_asks_node.up.sql
--
-- asks and ask_followers learn the address of the node they project, as document_blocks,
-- block_refs and comments did in 20261109022248. An ask is a node of the store (kind `ask`, its
-- id the ask's own, which api's createAskFor, openApprovalAsk and docs' createAskBlock write
-- before the row), and from this release the store holds what the two tables held: the artifact an
-- ask is anchored to, owned by and part of, its answer, and the sessions that follow it. The rows
-- stay where they are and become the projection of those tuples, so each row says which node it
-- is the projection of.
--   node_kind      the `kind` of the row's node in graph_nodes: `ask` on both tables, since a
--                  follower row is a relation of the ask's node (followed_by), not a node of its own.
--   node_address   its `id` there: the ask's id.
--
-- A row whose node columns are null is a row no writer of this release stated in the store: a row
-- written before this release, or by a process of the release before it during a roll or after
-- a rollback. graph_edges' old arms over the two tables answer such a row, and the store's own arm
-- answers a row that names its node (20261116094101), so the two columns are the marker the view
-- decides each row by. The boot step's backfill (graphcensus' asks and ask_followers batches)
-- fills them for the rows written before.
--
-- Both columns are nullable and nothing fills either of them in this migration, so it rewrites
-- no row. Each foreign key is added NOT VALID, which reads no row: a composite foreign key is
-- MATCH SIMPLE, which holds no row where one of its columns is null, and both columns are added
-- by the same statement and are null in every row the tables hold, so a validation could only
-- find what it cannot look at. Postgres checks the key on every insert and update from the
-- moment it exists, which is what keeps a row from naming a node the store does not hold.
-- 20261109022248 carries the measurement: 478 ms validated against 2 ms NOT VALID over 2,000,000
-- rows.
--
-- The key is not deferrable, the default, so a write naming a node the store does not hold fails
-- inside the statement that writes the row, at the writer.
--
-- Each ALTER holds ACCESS EXCLUSIVE on its table for the rest of this one transaction, so the
-- migration holds asks while it waits for ask_followers. Every writer takes the two in that same
-- order - an ask's row before its follower rows, in each create and in a follow, which reads the
-- ask first - so no live writer closes a cycle with it. The half-second bound is 20261109022248's,
-- for the reason it gives: a lock this migration cannot take at once fails it with 55P03 having
-- applied nothing, and the runner applies it again (pgmigrate.LockTimeoutAttempts), rather than
-- holding every reader of asks behind it.
set local lock_timeout = '500ms';

alter table asks add column node_kind text, add column node_address text,
  add constraint asks_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table ask_followers add column node_kind text, add column node_address text,
  add constraint ask_followers_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;
