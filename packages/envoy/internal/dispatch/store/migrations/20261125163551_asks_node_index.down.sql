-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row and every probe of graph_edges' tuple arm then reads
-- asks with no index again. 20261125163550, which added the columns it covers, has no rollback
-- script.
drop index concurrently if exists asks_node_address;
