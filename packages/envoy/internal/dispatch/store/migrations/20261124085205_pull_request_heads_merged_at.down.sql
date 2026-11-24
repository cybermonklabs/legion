-- 20261124085205_pull_request_heads_merged_at.down.sql
--
-- The rollback an operator runs by hand: it drops the column the migration added. A release that
-- writes it (delivery.UpsertPullRequestHead names it in its insert) fails every pull request write
-- once it is gone, so this is run only with every such release gone; the release before this
-- migration names it nowhere. Dropping a column is a catalog change that rewrites no row, under
-- ACCESS EXCLUSIVE on pull_request_heads for the catalog write alone.
alter table pull_request_heads
    drop column merged_at;
