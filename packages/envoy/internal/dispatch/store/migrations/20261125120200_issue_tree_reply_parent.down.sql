-- Run by hand: no runner applies a rollback script. Nothing references either column, and the
-- release before reads neither, so the column this migration added is dropped and the column it
-- dropped is added back on their own, before 20261123180100's rollback script drops the table.
alter table if exists issue_head_nodes drop column if exists item_answers, add column if not exists item_replies_to_block text;
