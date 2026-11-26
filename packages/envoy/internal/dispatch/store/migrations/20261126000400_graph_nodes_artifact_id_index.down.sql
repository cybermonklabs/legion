-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and a read of a document's edges by its uuid then scans every document's node.
drop index concurrently if exists graph_nodes_artifact_id;
