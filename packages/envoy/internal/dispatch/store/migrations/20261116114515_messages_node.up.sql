-- 20261116114515_messages_node.up.sql
--
-- messages learns the address of the node it projects, as 20261109022248 gave document_blocks,
-- block_refs and comments the same two columns and 20261116102800 gave the two attempt tables:
--   node_kind      the `kind` of the row's node in graph_nodes, message.
--   node_address   its `id` there, the message's own id.
-- The pair is also the row's marker. A row that names its node is one whose writer stated its
-- pointers in the store - replies_to, part_of, addressed_to and broadcast_id - and who wrote it
-- (graphstore.MessageFacts, graphstore.Authored), so graph_edges answers its replies_to edges
-- from the tuples (migration 20261116114517). A row an older build inserts during a roll names
-- none, and its old arms answer it until the backfill takes it once.
--
-- Both columns are nullable, and nothing fills them for a row already stored: the writers of this
-- release set them in the insert that writes the row, which is where every trigger on that insert
-- sees them. The foreign key is not deferrable, so Postgres checks it inside that insert, and the
-- node is written before the row (api's writeMessageNode). The key is added NOT VALID, so the
-- ALTER reads no row of messages: a composite key is MATCH SIMPLE, which holds no row where a
-- column is null, and both columns are null in every row messages holds, so a validation could
-- only find what it cannot look at. NOT VALID says nothing about a row written from here on, which
-- Postgres checks from the moment the constraint exists.
--
-- The ALTER holds ACCESS EXCLUSIVE on messages for the rest of this transaction and then takes
-- SHARE ROW EXCLUSIVE on graph_nodes for its key. A message write of the release still serving
-- takes the other order: it writes the message's node (ROW EXCLUSIVE on graph_nodes) and then
-- inserts the messages row, so the two can each hold what the other waits for. Half a second,
-- shorter than deadlock_timeout (1 s by default), makes this migration give up first, as
-- 20261116102800 does for the same cycle over the attempt tables: it fails with 55P03 having
-- applied nothing, the write finishes, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts).
set local lock_timeout = '500ms';

alter table messages add column node_kind text, add column node_address text,
  add constraint messages_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;
