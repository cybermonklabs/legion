-- Run by hand: no runner applies a rollback script. Run it after the rollback scripts of
-- 20261125140001 and 20261125140002, which drop the two indexes over the column. Nothing else
-- references the column, and the release before reads it nowhere, so it is dropped on its own; a
-- comment it held a node anchor of then reads as a comment on its issue tied to no text.
alter table if exists comments drop column if exists node_anchor;
