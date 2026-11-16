-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and a read of one project's nodes then scans graph_nodes again. 20261116015042, which added
-- the column it covers, has no rollback script.
drop index concurrently if exists graph_nodes_project;
