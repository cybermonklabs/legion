-- 20261116182209_pull_request_heads_older_read.up.sql
--
-- A write of GitHub's answer that replaces a newer row no read of GitHub wrote leaves the pull
-- request owed a read (dispatch://INFRA-3992, R3: an older event does not replace a newer head).
--
-- A write of GitHub's answer wins over a pull_request_heads row with no github_read_at, whatever
-- the two times (delivery.upsertHeadRow, the guard's last arm), since a time no read of GitHub
-- wrote may be one a caller set (20261116163242). But during a roll the release before stores
-- GitHub's newer answer from an envelope, unmarked, and a slower read of this build can land an
-- older answer after it: the row then holds the older answer, and where that answer is closed or
-- merged the reconcile never reads the row again. The trigger below records, inside the same
-- update, that such a pull request is owed a read of kind failure (pull_request_reads_owed,
-- 20261116163445), so the reconcile's next pass reads it before any refusal and whatever its
-- state, and writes GitHub's answer then, which is newer than both.
--
-- The condition is the update's own pair of rows: OLD has no github_read_at and an event_at later
-- than NEW's, and NEW has a github_read_at. Only this release writes github_read_at, so no write
-- of the release before takes it, and a marked row replaced by a marked write is never older: the
-- upsert's guard refuses an answer older than a marked row. A trigger, and not a part of the
-- upsert's own statement, because only the update sees the row it replaces: a CTE reads the
-- statement's snapshot, which a concurrent write may have changed, and a CTE's `for update` skips
-- a row that the statement's own update has changed, so it compares nothing.
--
-- It runs beside pull_request_heads_unread_write (20261116163242), which acts only where OLD has
-- a github_read_at; the two never act on one update. `create function` locks no table. `create
-- trigger` takes SHARE ROW EXCLUSIVE on pull_request_heads until the commit, so a write of a pull
-- request's head waits for that moment while a read does not, and reads no row, so the time does
-- not grow with the table; the census admits both as catalog-only statements. The release before
-- writes no github_read_at, so its writes never take the branch, and it neither reads nor writes
-- pull_request_reads_owed. Rolling back past it is an image rollback alone.
create function pull_request_heads_older_read() returns trigger language plpgsql as $$
begin
  if new.github_read_at is not null and old.github_read_at is null and old.event_at > new.event_at then
    insert into pull_request_reads_owed (repo, number, reason) values (new.repo, new.number, 'failure')
    on conflict (repo, number) do update set reason = excluded.reason, owed_at = excluded.owed_at;
  end if;
  return new;
end $$;

create trigger pull_request_heads_older_read before update on pull_request_heads
  for each row execute function pull_request_heads_older_read();
