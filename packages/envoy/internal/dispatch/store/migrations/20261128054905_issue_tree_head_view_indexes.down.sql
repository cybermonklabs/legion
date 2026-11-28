-- Run by hand: no runner applies a rollback script. Nothing references an index, and the release
-- before reads none, so each is dropped on its own, before 20261127120803's rollback script drops
-- the table.
drop index if exists issue_head_nodes_foreign;
drop index if exists issue_head_nodes_children;
drop index if exists issue_head_nodes_fragment;
drop index if exists issue_head_nodes_opened;
drop index if exists issue_head_nodes_block_type;
drop index if exists issue_head_nodes_supertag;
