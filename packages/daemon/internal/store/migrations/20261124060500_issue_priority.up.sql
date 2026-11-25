-- 20261124060500_issue_priority.up.sql — each issue's own Dispatch priority.
--
-- Dispatch serves an issue's priority as an integer from 0 for P0 to 3 for P3, or null for none,
-- on its listing, its detail read and every issue event, and refuses any other value. The daemon
-- keeps that integer here as Dispatch holds it, so an operator reads the column as Dispatch's own
-- issue: record.Issue.Priority is the level plus one, with null read as NoPriority (record's
-- issueColumns and PutIssue). It is the issue's own and never its tree root's.
--
-- It back-fills nothing. Every row written before it reads null, an issue with none, until the
-- next Dispatch observation of the issue the daemon applies (a live event, the boot listing)
-- writes its priority.
--
-- Rolling back onto a release before this migration needs nothing taken off. That release names
-- the columns of issues in each statement it runs (its record.Store's issueColumns and PutIssue),
-- so it boots with the column in place, reads none of it, and a row it inserts writes null while a
-- row it updates keeps the priority it held. Rolling forward again, a priority changed in Dispatch
-- while that release ran stands stale on the record until the issue's next observation.
alter table issues
    add column priority smallint check (priority between 0 and 3);
