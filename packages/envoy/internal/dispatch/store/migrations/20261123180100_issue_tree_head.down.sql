-- Run by hand: no runner applies a rollback script. The seven triggers go first, so no write between
-- these statements fires a function whose table is gone; nothing else references either table or
-- the functions, so each can be dropped on its own. The tree read of the release before walks the
-- tuples and reads neither table.
drop trigger if exists comment_block_texts_head_marks on comment_block_texts;
drop trigger if exists comment_blocks_head_marks on comment_blocks;
drop trigger if exists comments_head_marks_update on comments;
drop trigger if exists comments_head_marks on comments;
drop trigger if exists document_blocks_head_marks on document_blocks;
drop trigger if exists graph_nodes_head_marks on graph_nodes;
drop trigger if exists graph_tuples_head_marks on graph_tuples;
drop function if exists issue_head_items_drop();
drop function if exists issue_head_marks_drop();
drop table if exists issue_head_marks;
drop table if exists issue_head_nodes;
