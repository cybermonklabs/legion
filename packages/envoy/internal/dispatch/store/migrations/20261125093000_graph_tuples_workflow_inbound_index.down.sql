-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the release before's read of an issue's runs and plans through workflow_tuples then
-- fetches every other `part_of` child of the issue from the heap.
drop index concurrently if exists graph_tuples_workflow_inbound;
