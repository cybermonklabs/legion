-- Run by hand: no runner applies a rollback script. Run after
-- 20261125150001_issue_tree_graph_edges_block_node_listed's down, which restates graph_edges
-- without block_node_listed; refs' blockRefListed and the store arm of its mention backlinks of
-- this build call it, so roll back to a build before this one first.
drop function if exists block_node_listed(text[], text);
