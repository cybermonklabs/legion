-- 20261126005349_drop_pull_request_heads_lifecycle.up.sql
--
-- The record of each write that opens or closes a row of pull_request_heads
-- (pull_request_heads_lifecycle, migration 20261126005345), its two triggers on
-- pull_request_heads and their function pull_request_heads_lifecycle_keep have one reader: the
-- former heads install (20261126005348), which reads the record once, in its own transaction, to
-- find where each pull request's last closed period began. Nothing reads it after that transaction
-- commits, and its triggers would otherwise append a row on every write that opens or closes a row
-- of pull_request_heads for good. So this migration, the next after the install, drops the
-- triggers, the function and the table, with its index and its sequence.
-- pull_request_canonical_repo, which 20261126005345 made too, stays: the install's functions and
-- the settler call it.
--
-- A row the triggers append between the install's commit and this migration's is read by nothing.
-- A deploy stopped between the two leaves the triggers appending until the next boot's migrate
-- applies this one, which is harmless for the same reason. The release before this one neither
-- reads nor writes the table, and the triggers it fires go with it, so the two roll side by side.
--
-- DROP TRIGGER takes ACCESS EXCLUSIVE on pull_request_heads, so every read and write of the table
-- waits behind this transaction from its lock below to its commit: the catalog changes alone, which
-- read no row. pull_request_heads is asked for before the record's table, in one statement: a
-- transaction that holds the record's table wrote pull_request_heads, in a trigger, so once this
-- one holds pull_request_heads no such transaction is left, and the record's table waits for
-- none. Asked the other way, a transaction that read pull_request_heads and then writes it would
-- wait, in the record's trigger, for the record's table this one held while it waited for that
-- reader to end: a cycle Postgres breaks by failing one of the two.
--
-- As the install does, the lock is asked for in tries of half a second each (lock_timeout, shorter
-- than deadlock_timeout, 1 s by default, the wait after which Postgres checks for a cycle), inside
-- a block whose handler gives back what a try that ran out took, so a transaction caught in a
-- cycle with it goes on, and asks again at once, for as long as the runner lets any migration wait
-- for a lock (pgmigrate.LockTimeout, 5 s); past that it fails with 55P03, having applied nothing,
-- and the runner applies it again after a pause, up to its attempts (store/store.go
-- applyMigration, pgmigrate.RetryLockTimeouts). The census answers 0: the drops refuse and
-- rewrite no row of pull_request_heads, and the record's rows they take with the table are of
-- this release, which nothing reads.
set local lock_timeout = '500ms';

do $$
declare
  started timestamptz := clock_timestamp();
begin
  loop
    begin
      lock table pull_request_heads, pull_request_heads_lifecycle in access exclusive mode;
      exit;
    exception when lock_not_available then
      if clock_timestamp() - started >= interval '5 seconds' then
        raise;
      end if;
    end;
  end loop;
end $$;

drop trigger pull_request_heads_lifecycle_insert on pull_request_heads;
drop trigger pull_request_heads_lifecycle_update on pull_request_heads;
drop function pull_request_heads_lifecycle_keep();
drop table pull_request_heads_lifecycle;
