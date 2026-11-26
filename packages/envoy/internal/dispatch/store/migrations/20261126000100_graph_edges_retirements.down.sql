-- Run by hand; no runner applies a rollback script. Restate graph_edges with every arm first
-- (store.GraphEdgesView of the newest set, as 20261121090000 does), since once the table is gone a
-- build that reads it fails every backlink count, and a build that wrote a row of it had deleted
-- that child's arms. Nothing else depends on the table.
drop table if exists graph_edges_retirements;
