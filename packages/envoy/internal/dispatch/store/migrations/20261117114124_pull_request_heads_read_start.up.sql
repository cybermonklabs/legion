-- 20261117114124_pull_request_heads_read_start.up.sql
--
-- Of two answers of GitHub for one pull request that carry the same updated_at second, the one
-- whose read of GitHub began later is kept: an older event does not replace a newer head.
-- 20261117172917 replaces this function with one that orders the reads by a sequence.
--
-- GitHub's updated_at is whole seconds, so it cannot order two answers of one second, and the
-- upsert of a head row (delivery.upsertHeadRow) applies an answer of the stored second. So the
-- answer that arrived last won: a read that began first and was answered slowly landed its older
-- head over the newer one, and a pass at the older head stood until the next reconcile pass, 158 s
-- and 214 s in the acceptance of cybermonklabs/legion#4209 at 3a21edb2c9 (D2). The order the reads
-- began in orders the answers: a read that began later holds the later state. From this release,
-- github_read_at is when the read of GitHub whose answer the row holds began, which each reader of
-- this release takes before its request goes out (delivery.PullRequestHead.GitHubReadAt), where it
-- was the database's clock at the write; it stays null for a write that is no answer of GitHub's.
--
-- The function below replaces pull_request_heads_older_read (20261116182209). It owes a read in
-- the same two cases as before, and it then skips an update of a marked row by a marked write of
-- the same event_at whose read began before the row's: the update writes nothing, so that answer
-- replaces nothing, and an answer of the stored second that differs in a fact GitHub answers still
-- owes a read, whichever of the two the row keeps. A read that began later can still be answered
-- with the older state, where its request reached GitHub first, so no order decides two answers of
-- one second for certain, and the owed read is the repair. The rule is the trigger's and not the
-- upsert's `where`, because a row the `where` refuses fires no trigger, so nothing could owe the
-- read of the answer it refused; and a trigger sees the row the update replaces (OLD), which a CTE
-- does not (20261116182209).
--
-- The release before names no github_read_at in its statements, so its update of a marked row
-- leaves it as it was, which is never earlier than itself: the skip never takes a write of that
-- release, which pull_request_heads_unread_write handles as before. Postgres fires the triggers of
-- one event in the order of their names, so this one runs first, and an update it skips fires no
-- trigger after it.
--
-- `create or replace function` locks no table: it takes the function's own catalogue row, and the
-- trigger that calls it runs the new body from its next statement. It reads no row, so the census
-- admits it as a catalog-only statement. Rolling back past this migration is an image rollback
-- alone: the function keeps owing reads in the release before, which also writes no
-- github_read_at, so the skip never takes its writes.
create or replace function pull_request_heads_older_read() returns trigger language plpgsql as $$
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
  if old.event_at = new.event_at and new.github_read_at < old.github_read_at then
    return null;
  end if;
  return new;
end $$;
