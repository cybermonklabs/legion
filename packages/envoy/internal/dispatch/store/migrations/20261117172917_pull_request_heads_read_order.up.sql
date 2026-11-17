-- 20261117172917_pull_request_heads_read_order.up.sql
--
-- Of two answers of GitHub for one pull request that carry the same updated_at second, the one
-- whose read took the later place in the order of reads is kept. The place is a value of one
-- sequence, which only rises, and no longer a clock of the process that read.
--
--   github_read_order  the place the read whose answer the row holds took: a value of
--                      pull_request_reads_owed_revision_seq (20261116163445), which each reader of
--                      GitHub takes with one statement, nextval, just before its request goes out
--                      (delivery.startRead). Null on every row a release before this one
--                      inserted, and left as it was by that release's update (below).
--   link_mark          how many writes of a link to the row's pull request have marked the row
--                      unread (delivery.MarkHeadsLinked). Each head write reads it before its
--                      attribution reads the links, and its upsert leaves the row unread where the
--                      count has moved since (delivery.upsertHeadRow): where the row is stored
--                      before the link is written, a write that judged the row's issue from links
--                      a later write changed does not stamp the row read again. A link written
--                      while a write inserts the row marks nothing. Null on a row no such write
--                      has marked.
--
-- 20261117114124 ordered the two answers by github_read_at, the clock of each process that read.
-- A stored read start ahead of a reader's clock, from two replicas whose clocks differ or a clock
-- that stepped back, then made every later read look earlier, and the row kept the older head for
-- as long as the gap: through three reconcile passes and one event in the deep audit of
-- cybermonklabs/legion#4209 at ac562e9837. A sequence gives every read of every process one
-- order, whatever any clock says. The sequence is the one the owed reads' revisions already come
-- from, so every owe and every read take their places in one order, and the migration makes no
-- relation: a CREATE SEQUENCE is a statement the census does not read as catalog-only, which would
-- take its catalog reads away from every later migration of the same deploy. github_read_at stays
-- as the time a reader sees, and it orders nothing.
--
-- The function below replaces pull_request_heads_older_read (20261117114124). It owes a read in
-- the same two cases as before, and it then skips an update of the same event_at whose read took
-- an earlier place than the stored row's. A row with no place, which the release before and the
-- earlier builds of this change wrote, is never skipped onto, since `new < null` is never true;
-- and the release before names no github_read_order in its statements, so its update leaves the
-- place as it was, which is never earlier than itself. That update of a row a read of this build
-- wrote clears github_read_at and keeps the place (pull_request_heads_unread_write), so an answer
-- of GitHub of the same second whose read took an earlier place than the one the row keeps is
-- skipped there, over a row no read of GitHub wrote; the update owed a read in the same
-- statement, so the first reconcile pass that reaches that owed read writes GitHub's head. A read
-- that took the later place can still be answered with the older state, where its request reached
-- GitHub first, or where its first request failed and a later try was answered, so two answers of
-- one second that differ in a fact GitHub answers owe a read whichever of the two the row keeps,
-- and the next read writes what GitHub holds then.
--
-- The migration also widens pull_request_heads_merged_unread to every merged row no read has
-- reached, credited or not (`state = 'merged' and checked_at is null`), which the reconcile's merged
-- arm now reads (delivery.ListStalePullRequestHeads): a write of a link to a pull request marks its
-- row unread (delivery.MarkHeadsLinked), and a merged row credited to an issue is one such a link
-- can move. The release before's arm, `issue_key is null and checked_at is null`, selects a
-- subset of the rows the new index holds, and an index decides a plan, never an answer.
--
-- The migration runs in one transaction, and its first statement takes pull_request_heads' ACCESS
-- EXCLUSIVE lock, which it holds until it commits, so every read and write of a head row waits
-- for the whole migration: the two columns are nullable with no default, which writes the catalog
-- alone and rewrites no row, and the index build reads every row of the table, which holds a row
-- per pull request Dispatch has seen an event for, thousands and not millions. The runner's
-- lock_timeout bounds the migration's wait behind a writer. `create or replace function` takes
-- the function's own catalogue row. The census sizes the table, since an index built on an
-- existing table reads its rows, and answers 0: the migration refuses and rewrites no row. The
-- release before reads neither column nor the sequence's new use, so it rolls beside this one;
-- its upsert compares no link_mark, so during the roll its write of a marked row can stamp the
-- row read before the next pass judges it. Rolling back past this migration is an image rollback
-- alone: the function then orders by a column no write of that release sets, so it never skips.
alter table pull_request_heads add column github_read_order bigint, add column link_mark bigint;

drop index pull_request_heads_merged_unread;
create index pull_request_heads_merged_unread on pull_request_heads (repo, number)
  where state = 'merged' and checked_at is null;

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
  if old.event_at = new.event_at and new.github_read_order < old.github_read_order then
    return null;
  end if;
  return new;
end $$;
