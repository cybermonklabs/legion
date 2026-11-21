-- 20261116182209_pull_request_heads_older_read.up.sql
--
-- A write whose facts no read of GitHub has the last word on leaves the pull request owed a read:
-- the head and the state of a pull request come from a read of GitHub, and an older event does
-- not replace a newer head.
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
-- The first case is the update's own pair of rows: OLD has no github_read_at and an event_at
-- later than NEW's, and NEW has a github_read_at. Only this release writes github_read_at, so no
-- write of the release before takes it, and a marked row replaced by a marked write is never
-- older: the upsert's guard refuses an answer older than a marked row. A trigger, and not a part
-- of the upsert's own statement, because only the update sees the row it replaces: a CTE reads
-- the statement's snapshot, which a concurrent write may have changed, and a CTE's `for update`
-- skips a row that the statement's own update has changed, so it compares nothing.
--
-- The second case is an update of the same event_at that changes a fact GitHub answers of a row
-- GitHub's answer is in before or after it (OLD or NEW has a github_read_at). GitHub's updated_at
-- is whole seconds, so two answers of one second say nothing of which is the newer, and both
-- guards apply the one that lands last, which may be the older: a closed answer that GitHub gave
-- just before a reopen of the same second, delayed and written after the reopen's own answer,
-- would leave the pull request closed, and nothing reads a closed row again. The facts are GitHub's
-- own: the head, its branch, the base, the state, the title and the URL. The attribution is not
-- one: it is Dispatch's judgement of those facts, which a pass of the reconcile writes from the
-- stored facts with no read of GitHub (20261117111040), and which a change of the rule or a write
-- of the issues changes at the same second; a read of GitHub would answer nothing new about it.
-- The read the owe asks for writes GitHub's answer at that moment; where it changes a fact once
-- more it owes once more, and the read after it, which changes nothing, settles it.
--
-- The third case is a write of the release before over a row this release read, at any second
-- (acceptance of cybermonklabs/legion#4209 at ff73c7358d, D8). That release writes a row's head,
-- state and time from an envelope, which any Envoy /v1 bearer can publish, and
-- pull_request_heads_unread_write (20261116163242) takes the row's mark away. The reconcile reads
-- an open row again, and a merged row while its texts do not describe its head or its commit
-- messages are not read (the facts line, 20261117111040), but reads no closed row, so a close it
-- wrote dated off GitHub's own second stood until the pull request's next event. That trigger's
-- function is replaced here, now that pull_request_reads_owed exists, by one that takes the mark
-- away and owes the pull request a read in the same statement, whatever the row's state and time.
-- Postgres fires the triggers of one event in the order of their names, so
-- pull_request_heads_older_read runs first and sees the mark the write keeps: a write of the
-- release before of the same second owes in both, two owes of one row, and the read settles the
-- later revision.
--
-- Each owe takes a new revision (pull_request_reads_owed.revision, the column's default), so a
-- read that took the row's revision before this write deletes nothing of it.
--
-- `create function` and `create or replace function` lock no table: the latter takes the
-- function's own catalogue row, and the trigger that calls it runs the new body from its next
-- statement. `create trigger` takes SHARE ROW EXCLUSIVE on pull_request_heads until the commit, so
-- a write of a pull request's head waits for that moment while a read does not, and reads no row,
-- so the time does not grow with the table; the census admits all three as catalog-only
-- statements. The release before names pull_request_reads_owed in none of its own statements; its
-- updates of a marked row write it through pull_request_heads_unread_write. Rolling back past
-- this migration is an image rollback alone.
create function pull_request_heads_older_read() returns trigger language plpgsql as $$
begin
  if (new.github_read_at is not null and old.github_read_at is null and old.event_at > new.event_at)
     or (coalesce(new.github_read_at, old.github_read_at) is not null
         and old.event_at = new.event_at
         and (new.head_sha, new.head_ref, new.base_ref, new.state, new.title, new.url)
             is distinct from
             (old.head_sha, old.head_ref, old.base_ref, old.state, old.title, old.url))
  then
    insert into pull_request_reads_owed (repo, number, reason) values (new.repo, new.number, 'failure')
    on conflict (repo, number) do update
      set reason = excluded.reason, owed_at = excluded.owed_at, revision = excluded.revision;
  end if;
  return new;
end $$;

create trigger pull_request_heads_older_read before update on pull_request_heads
  for each row execute function pull_request_heads_older_read();

create or replace function pull_request_heads_unread_write() returns trigger language plpgsql as $$
begin
  if old.github_read_at is not null
     and new.github_read_at is not distinct from old.github_read_at
     and (new.head_sha, new.head_ref, new.base_ref, new.state, new.issue_key,
          new.attribution_source, new.title, new.url, new.event_at)
         is distinct from
         (old.head_sha, old.head_ref, old.base_ref, old.state, old.issue_key,
          old.attribution_source, old.title, old.url, old.event_at)
  then
    new.github_read_at := null;
    new.checked_at := null;
    new.attribution_rule := 0;
    insert into pull_request_reads_owed (repo, number, reason) values (new.repo, new.number, 'failure')
    on conflict (repo, number) do update
      set reason = excluded.reason, owed_at = excluded.owed_at, revision = excluded.revision;
  end if;
  return new;
end $$;
