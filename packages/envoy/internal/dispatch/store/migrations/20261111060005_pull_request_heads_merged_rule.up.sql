-- 20261111060005_pull_request_heads_merged_rule.up.sql
--
-- The indexes behind the reconcile's merged arm, which R8 of INFRA-1522 widened. The arm now
-- takes a merged row an older attribution rule judged, whether or not that rule credited it, and
-- not the rows credited to nobody alone (ListStalePullRequestHeads,
-- delivery.AttributionRuleVersion is 2). Rule 2 ranks the head branch and the two links above the
-- title and the body, so a child's pull request that rule 1 credited to the parent its title
-- named belongs to the child, and only a read that judges the row again moves it.
--
-- The arm is two terms, so it is two indexes, and the statement is written as their disjunction
-- (`(issue_key is null and checked_at is null) or attribution_rule < $3`) so the planner can take
-- a bitmap of each and or them:
--
--   pull_request_heads_merged_unread  the merged rows credited to nobody that no read has
--                                     reached, which one read settles. Partial on all three
--                                     columns, which is what 20261102122300 built and what
--                                     20261105070440 widened: that release's arm took such a row
--                                     whatever its checked_at said, and this one holds it to the
--                                     unread rows again, since a row this rule has already judged
--                                     is the other term's business or nobody's.
--   pull_request_heads_merged_rule    every merged row, keyed by the rule that judged it, which
--                                     serves `attribution_rule < $3`: the rows a raised rule
--                                     re-judges, of which there are none between two deploys that
--                                     raise it.
--
-- Measured on 16,000 rows (12,000 merged, 640 of them credited to nobody and 160 of those
-- unread), on PostgreSQL 18: the batch read is a BitmapOr of the three partial indexes, 4.3 ms
-- and 343 buffers, where the same statement over one index partial on `state = 'merged'` alone
-- reads every row of the table, 8 ms and 743 buffers. The one-off pass that follows the raised
-- rule is a sequential scan whatever is indexed, since it takes every merged row there is.
--
-- Replacing an index leaves the release before this one working, which is what the expand-then-
-- contract rule asks of a drop (cmd/dispatch/README.md, "Database migrations"). That release's
-- arm is `state = 'merged' and issue_key is null and (checked_at is null or attribution_rule <
-- 1)`: its unread rows are in pull_request_heads_merged_unread as this migration narrows it, and
-- the rows an older rule judged are in pull_request_heads_merged_rule, which holds every merged
-- row. And an index decides a plan, never an answer: that release would answer the same rows from
-- a sequential scan if the planner chose one.
--
-- `drop index` takes ACCESS EXCLUSIVE on pull_request_heads for the instant it unlinks the index,
-- and `create index` SHARE while it builds, both inside the runner's one transaction: the table
-- holds a row per pull request Dispatch has seen an event for, so each build reads thousands of
-- rows, not millions. The census answers 0: this migration refuses and rewrites no row.
drop index pull_request_heads_merged_unread;
create index pull_request_heads_merged_unread on pull_request_heads (repo, number)
  where state = 'merged' and issue_key is null and checked_at is null;
create index pull_request_heads_merged_rule on pull_request_heads (attribution_rule, repo, number)
  where state = 'merged';
