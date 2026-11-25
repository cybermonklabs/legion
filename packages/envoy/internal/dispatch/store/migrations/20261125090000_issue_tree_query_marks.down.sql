-- Run by hand: no runner applies a rollback script. Nothing references the table, and the release
-- before reads it nowhere, so it is dropped on its own; a mark it held is a version of a query no
-- drain wrote, which that release does not owe.
drop table if exists issue_query_marks;
