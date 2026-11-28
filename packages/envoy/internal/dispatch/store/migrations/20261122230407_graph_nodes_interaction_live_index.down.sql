-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the sweep's read of the runs whose row is gone then scans graph_nodes again.
drop index concurrently if exists graph_nodes_interaction_live;
