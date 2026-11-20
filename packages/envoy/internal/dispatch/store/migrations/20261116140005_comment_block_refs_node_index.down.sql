-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row then reads comment_block_refs with no index again, as does
-- graph_edges' probe of a row of it. 20261116140000, which added the columns it covers, has no
-- rollback script.
drop index concurrently if exists comment_block_refs_node_address;
