-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row then reads comments with no index again.
-- 20261109022248, which added the columns it covers, has no rollback script.
drop index concurrently if exists comments_node_address;
