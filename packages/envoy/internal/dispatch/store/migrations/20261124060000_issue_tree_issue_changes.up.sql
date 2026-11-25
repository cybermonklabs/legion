-- pgmigrate:non-transactional
-- 20261124060000_issue_tree_issue_changes.up.sql - the latest change of an issue, by its key.
--
-- An issue's node answers its last change: the newest event that wrote the issue's own fields,
-- issue.created, issue.updated or issue.closed, and that event's actor (refs' loadIssueNodes). Its
-- read is the issue's events newest first, through events_issue_key_seq_key, until the first of
-- those three types, and the events of comments, asks and claims stand between: the tree read of
-- an issue of 9,252 comments read all 9,253 of its events to find its one issue.created, a sixth of
-- that read's database time. This index holds the three types alone, keyed as the read orders
-- them, so the read is one probe of it. Its predicate is the read's own filter, written as the
-- read writes it, which is what lets the planner prove the read keeps to it.
--
-- events grows with every write of every issue. A concurrent build does not hold off its writers
-- while it scans the table, so the migration runs outside a transaction. It is a performance
-- index: no constraint and no later migration depends on it. A failed concurrent build leaves an
-- INVALID index of this name on events, which the runner drops with DROP INDEX CONCURRENTLY and
-- builds again on the next attempt.
create index concurrently if not exists events_issue_changes
    on events (issue_key, seq)
    where type = any('{issue.created,issue.updated,issue.closed}');
