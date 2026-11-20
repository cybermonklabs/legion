-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row then reads messages with no index again, as does
-- graph_edges' probe of a message's row. 20261116114515, which added the columns it covers, has no
-- rollback script.
drop index concurrently if exists messages_node_address;
