-- A new table, issue_tree_block_drain, which this migration creates empty: it reads no other
-- table, writes no row, and refuses and rewrites nothing already stored. The marking whose place
-- it records runs in the graph backfill, in committed batches outside every migration transaction
-- (graphcensus.Backfill).
select 0;
