-- 20261116140000_item_blocks_node.up.sql
--
-- The blocks of a comment, a message and an ask, and the links each block holds, learn the address
-- of the node they project, as 20261109022248 gave document_blocks, block_refs and comments the
-- same two columns and 20261116114515 gave messages:
--   node_kind      the `kind` of the row's node in graph_nodes, block (graphstore.KindBlock): the
--                  closed supertag list holds no comment_block, message_block or ask_block, so an
--                  item's block is a block node like a document's.
--   node_address   its `id` there, the block's node id, `<item kind>/<item uuid>#<block id>`,
--                  which the row's node_id (a block row) or from_node (a link row) already holds.
-- The pair is also the row's marker. A block row that names its node is one whose writer stated
-- the block in the store - its node and its part_of to the comment, message or ask it is a block
-- of - and a link row that names its node one whose writer stated that link as a tuple out of the
-- block's node (refs.IndexItem, refs.PlanAskParts), so graph_edges answers both from the tuples
-- (migration 20261126045003). A row an older build writes during a roll names none, and its old
-- arm answers it until the backfill takes it once.
--
-- Every column is nullable, and nothing fills them for a row already stored: the writers of this
-- release set them in the insert or update that writes the row, which is where Postgres checks
-- the key. The key is not deferrable, so the block's node is written before the row that names
-- it. Each key is added NOT VALID, so no ALTER reads a row of its table: a composite key is MATCH
-- SIMPLE, which holds no row where a column is null, and both columns are null in every row these
-- tables hold, so a validation could only find what it cannot look at. NOT VALID says nothing
-- about a row written from here on, which Postgres checks from the moment the constraint exists.
--
-- Each ALTER holds ACCESS EXCLUSIVE on its table for the rest of this transaction and then takes
-- SHARE ROW EXCLUSIVE on graph_nodes for its key. A comment, message or ask write of the release
-- still serving takes the other order: it writes the item's node (ROW EXCLUSIVE on graph_nodes)
-- and then indexes the item's blocks and their links, so the two can each hold what the other
-- waits for. Half a second, shorter than deadlock_timeout (1 s by default), makes this migration
-- give up first, as 20261116114515 does for the same cycle over messages: it fails with 55P03
-- having applied nothing, the write finishes, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts).
set local lock_timeout = '500ms';

alter table comment_blocks add column node_kind text, add column node_address text,
  add constraint comment_blocks_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table message_blocks add column node_kind text, add column node_address text,
  add constraint message_blocks_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table ask_blocks add column node_kind text, add column node_address text,
  add constraint ask_blocks_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table comment_block_refs add column node_kind text, add column node_address text,
  add constraint comment_block_refs_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table message_block_refs add column node_kind text, add column node_address text,
  add constraint message_block_refs_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table ask_block_refs add column node_kind text, add column node_address text,
  add constraint ask_block_refs_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;
