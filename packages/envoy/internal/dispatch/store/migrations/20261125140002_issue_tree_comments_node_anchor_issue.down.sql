-- Run by hand outside a transaction, before rolling back past 20261125140002 and before the
-- rollback script of 20261125140000, which drops the column the index reads.
drop index concurrently if exists comments_issue_node_anchor;
