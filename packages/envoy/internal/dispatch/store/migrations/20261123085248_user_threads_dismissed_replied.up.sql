-- pgmigrate:non-transactional
-- The Dismissed state lists a person's dismissed threads that somebody else wrote in after they
-- took part: `u.replied_at is not null and u.dismissed_at is not null` (api/replies.go,
-- myRepliesDismissed). Every reader of the state carries both tests: the list route's page, the
-- one read's Dismissed arm (api/me_inbox.go) and the restore's match (api/inbox_dismiss.go).
--
-- A person's own answer or reply dismisses its thread for them in the write's own transaction
-- (inboxwrite's queueKeepRow). It sets dismissed_at on their row even where nobody else wrote, so
-- the row keeps a null replied_at and is no row of the state. user_threads_dismissed
-- (20261105093503) is partial on dismissed_at alone, so it holds those rows too. A page read
-- through it walks every such row newer than the page and reads each one's table row to test
-- replied_at, then drops it. A person who answers often keeps many of them, newest first.
--
-- This index holds the rows the state lists and no other. Its keys are user_threads_dismissed's:
-- the login, then the dismissal and the thread id, which are the order a page answers in and the
-- cursor it resumes from. Its predicate is the state's own, so the planner tests no column of the
-- table to keep a row it reads.
--
-- user_threads_dismissed stays. The release before this one reads the state with
-- `dismissed_at is not null` alone, which this predicate does not serve, and it serves beside this
-- release while it rolls. A later release drops the old index.
--
-- It is a performance index: no constraint and no later migration depends on it. The build is
-- concurrent, so it holds off no read or write of user_threads while it scans the table, and the
-- census admits it at any size. A failed concurrent build leaves an INVALID index of this name on
-- user_threads, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the next
-- attempt.
create index concurrently if not exists user_threads_dismissed_replied
    on user_threads (login, dismissed_at desc, thread_id desc)
    where dismissed_at is not null and replied_at is not null;
