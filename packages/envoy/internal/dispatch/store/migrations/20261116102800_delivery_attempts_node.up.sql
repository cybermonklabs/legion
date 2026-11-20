-- 20261116102800_delivery_attempts_node.up.sql
--
-- The two attempt tables learn the address of the node they project. Every attempt to reach one
-- session or role is a delivery_attempt node of the store (graphstore.KindDeliveryAttempt), and
-- comment_deliveries and message_deliveries stay where they are and become projections of its
-- tuples, so each row says which node it is the projection of, as 20261109022248 gave
-- document_blocks, block_refs and comments the same two columns:
--   node_kind      the `kind` of the row's node in graph_nodes, delivery_attempt.
--   node_address   its `id` there (graphstore.AttemptKey.ID).
-- The pair is also the row's marker: a row that names its node is one whose writer states the
-- attempt in the store (graphstore.Attempted), and a row an older build inserts during a roll
-- names none, which the backfill takes once.
--
-- Both columns are nullable, and nothing fills them for a row already stored: the writers of this
-- release set them in the insert that writes the row. The foreign key is not deferrable, so
-- Postgres checks it inside that insert, and the node is written before the row
-- (graphstore.AttemptOpened). Each key is added NOT VALID, so neither ALTER reads a row of its
-- table: a composite key is MATCH SIMPLE, which holds no row where a column is null, and both
-- columns are null in every row these tables hold, so a validation could only find what it cannot
-- look at. NOT VALID says nothing about a row written from here on, which Postgres checks from the
-- moment the constraint exists.
--
-- Each ALTER holds ACCESS EXCLUSIVE on its table for the rest of this transaction and then takes
-- SHARE ROW EXCLUSIVE on graph_nodes for its key. A comment write of the release still serving
-- takes the other order: it writes the comment's node (ROW EXCLUSIVE on graph_nodes) and then
-- inserts its mentions' attempts, so the two can each hold what the other waits for. Half a
-- second, shorter than deadlock_timeout (1 s by default), makes this migration give up first, as
-- 20261109022248 does for the same cycle over comments: it fails with 55P03 having applied
-- nothing, the write finishes, and the runner applies it again (pgmigrate.LockTimeoutAttempts).
set local lock_timeout = '500ms';

alter table comment_deliveries add column node_kind text, add column node_address text,
  add constraint comment_deliveries_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table message_deliveries add column node_kind text, add column node_address text,
  add constraint message_deliveries_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;
