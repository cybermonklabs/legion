-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and every delete of a graph_nodes row and every followed_by probe of graph_edges' tuple arm
-- then reads ask_followers with no index again.
drop index concurrently if exists ask_followers_node_address;
