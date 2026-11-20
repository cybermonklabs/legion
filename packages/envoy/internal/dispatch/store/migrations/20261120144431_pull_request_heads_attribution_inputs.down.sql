-- 20261120144431_pull_request_heads_attribution_inputs.down.sql
--
-- The rollback an operator runs by hand: it drops the five columns the migration added. A release
-- that writes them (delivery.UpsertPullRequestHead names them in its insert) fails every pull
-- request write once they are gone, so this is run only with every such release gone; the release
-- before this migration names none of them. Dropping a column is a catalog change that rewrites no
-- row, under ACCESS EXCLUSIVE on pull_request_heads for the catalog write alone.
alter table pull_request_heads
    drop column attribution_title_keys,
    drop column attribution_cited_issues,
    drop column attribution_branch_keys,
    drop column attribution_commit_keys,
    drop column head_in_repo;
