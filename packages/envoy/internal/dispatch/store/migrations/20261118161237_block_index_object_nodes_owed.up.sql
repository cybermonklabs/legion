-- 20261118161237_block_index_object_nodes_owed
--
-- A build's block-index event cursor (20261118115645) gains the time since which a tick of that
-- build owes the object pass: the nodes of the issues, documents, asks and messages a build before
-- this one wrote with no node, and the gone statement of each document node it left standing behind
-- no row, as this build's removal states it (docs' writeObjectNodesAnotherBuildWrote). The tick
-- that owes the pass sets it, in the transaction that moves the cursor, and the tick whose pass
-- finishes with nothing left clears it, so a pass that fails or stops at its batch bound is run
-- again by the next tick of any process of the build, whichever holds the block-index lease then.
-- Null is no debt.
--
-- It gains too whether the build's last tick found a process of another build serving
-- (another_build_seen), which that tick writes in the same statement: the first tick that finds
-- none after one did owes the pass, whichever process of the build runs it, for the rows that
-- build wrote after the tick before.
alter table block_index_event_cursors
  add column object_nodes_owed_at timestamptz,
  add column another_build_seen boolean not null default false;
