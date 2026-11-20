-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row then reads message_deliveries with no index again.
-- 20261116102800, which added the columns it covers, has no rollback script.
drop index concurrently if exists message_deliveries_node_address;
