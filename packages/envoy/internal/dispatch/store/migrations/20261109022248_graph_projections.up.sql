-- 20261109022248_graph_projections.up.sql
--
-- The three projected tables learn the address of the node they project. A node is a thing
-- Dispatch holds and a tuple is one fact stated about it (20261109013400_graph_store.up.sql);
-- document_blocks, block_refs and comments stay where they are and become projections of those
-- tuples, so each row has to say which node it is the projection of.
--   node_kind      the `kind` of the row's node in graph_nodes.
--   node_address   its `id` there. Two columns rather than one text address, because that is how
--                  graph_nodes is keyed and a foreign key references a key as it is written.
--   parent_block_id  document_blocks' alone: the block of the same document that holds this one,
--                  null for a block at the document's top level. A block_refs row is one link a
--                  block holds and a comment is no block, so neither nests and neither takes it.
--
-- Every column is nullable and nothing fills any of them in this commit: the writers that fill
-- them are this branch's own, and a row that names no node is answered by the arm of
-- `graph_edges` over its table until one does (20261109215947).
--
-- The foreign key is not deferrable, which is the default: Postgres checks it inside the
-- statement that writes the projection row, so a write naming a node the store does not hold
-- fails there, at the writer, and no deferred trigger queues work for COMMIT to run through.
-- That matters because a projection is written row by row on the hot write path, and a deferred
-- check would hold every such row's work until the transaction ends.
--
-- Each key is added NOT VALID, which is what keeps this migration off the three tables' rows. A
-- key added without it validates there and then: the ALTER holds ACCESS EXCLUSIVE on the table
-- while it reads every row of it, and all three run in this migration's one transaction, so
-- every reader and writer of comments, document_blocks and block_refs waits for the slowest of
-- the scans. Measured on this pod over 2,000,000 rows whose new columns are all null: 478 ms
-- validated against 2 ms with NOT VALID, and 1.63 s validated on the review's own reading of
-- document_blocks at that size.
--
-- No scan is owed, and none is run later: a composite foreign key is MATCH SIMPLE, which holds
-- no row where any of its columns is null, and the two columns of each key are added by this
-- same statement and are null in every row these tables already hold. So a validation could
-- only find what it cannot look at. NOT VALID says nothing about rows written from here on:
-- Postgres checks the key on every insert and update from the moment the constraint exists,
-- which is the rule R16 rests on, and the only thing it gives up is the claim in the catalogue
-- that every stored row was read once.
--
-- NOT VALID takes the scan away and leaves the lock: each ALTER holds ACCESS EXCLUSIVE on its
-- table for the rest of this one transaction, so the migration holds document_blocks while it
-- waits for block_refs and holds both while it waits for comments. A live writer that takes two
-- of these tables in the other order closes a cycle with it: acceptComment reads comments first
-- (api/comment_actions.go's lockedComment) and then writes document_blocks through
-- AcceptSuggestion's version. Under the runner's five-second bound (pgmigrate.LockTimeout)
-- either side can lose that race, and the runner retries only 55P03 (store/store.go
-- RetryLockTimeouts), so a 40P01 deadlock either answers a request 500 or fails the boot. Half a
-- second, shorter than deadlock_timeout (1 s by default), makes this migration give up first
-- instead, before either deadlock check runs: it fails with 55P03 having applied nothing, the
-- comment write finishes, and the runner applies it again, up to pgmigrate.LockTimeoutAttempts
-- times in the same boot, then at the next boot. This is what 0066 and 20261101182522 do, for
-- the same cycle on the same tables, and it carries their cost: Postgres cancels an autovacuum
-- that holds a lock only after the waiter has waited deadlock_timeout, so an autovacuum of these
-- three tables outlasts this migration's wait and fails that attempt.
set local lock_timeout = '500ms';

alter table document_blocks add column node_kind text, add column node_address text,
  add column parent_block_id text,
  add constraint document_blocks_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table block_refs add column node_kind text, add column node_address text,
  add constraint block_refs_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;

alter table comments add column node_kind text, add column node_address text,
  add constraint comments_node foreign key (node_kind, node_address) references graph_nodes (kind, id) not valid;
