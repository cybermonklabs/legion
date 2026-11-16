-- 20261116163242_pull_request_heads_github_read.up.sql
--
-- Which pull_request_heads rows hold GitHub's own answer to a read of the pull request, so that
-- only such a row's time can stop a read of GitHub or win against GitHub's answer
-- (dispatch://INFRA-3992, R2: the head and the state of a pull request come from a read of
-- GitHub).
--
--   github_read_at  when a read of GitHub wrote the row's head_sha, state and event_at, with
--                   event_at GitHub's own updated_at for the pull request; null where no read of
--                   GitHub wrote what the row holds.
--
-- The release before this one wrote a row's head, state, branch, title and event_at from a
-- `pull_request` envelope, and any Envoy /v1 bearer can publish one. Two guards read event_at as
-- GitHub's own time: the intake spares its read of GitHub when the stored row is newer than the
-- event (delivery.headReadCovers), and the upsert refuses an answer older than the stored
-- time (delivery.upsertHeadRow). So a row stored from an envelope an hour ahead of GitHub's clock
-- kept its head and its state against every read of GitHub until GitHub's clock passed it — for
-- a closed or merged pull request, never. This release lets both guards trust only a row with
-- github_read_at set.
--
-- The column is added nullable with no default, so every row stored before this migration reads
-- as one no read of GitHub wrote: for every such row the stored time stops no read and wins
-- against no answer, which is what clearing event_at would do for the two guards, without
-- rewriting a row and without losing GitHub's own time where a row holds it (the results read
-- shows the time a head was learned by, and delivery.BaseBranchMergedFor orders merged rows by
-- it). This release also raises delivery.AttributionRuleVersion to 3, so the reconcile reads
-- each stored merged row from GitHub once more, at most delivery.RejudgedHeadsPerPass a pass;
-- an open row is read every five minutes already.
--
-- The release before still serves beside this one while the roll lasts, and it can write a row
-- from an envelope after this migration ran. Its insert names its columns, so a row it inserts
-- has github_read_at null. Its upsert's `do update` names no github_read_at, so it would leave
-- the mark of a row this release read in place under an envelope's head, state and time. The
-- trigger below takes it away instead: an update that changes a fact of a marked row and leaves
-- github_read_at as it was is a write of no read of GitHub, so the row is marked unread
-- (github_read_at null), unchecked (checked_at null, which the reconcile's open arm reads first)
-- and judged by no rule (attribution_rule 0, which the reconcile's merged arm offers once). This
-- release's own writes set github_read_at on every write: the database's clock at the statement
-- for a write of GitHub's answer, null for any other, so a write of this release never leaves it
-- as it was. An update that changes no fact (a stamp of checked_at, a write of the same answer)
-- keeps the mark. A foreign key's `on delete set null` of issue_key is an update too, and it
-- costs that row one more read of GitHub.
--
-- Adding a nullable column with no default writes the catalog alone, under pull_request_heads'
-- ACCESS EXCLUSIVE lock for milliseconds, and rewrites no row. `create function` locks no table.
-- `create trigger` takes SHARE ROW EXCLUSIVE on pull_request_heads until the commit, so a write
-- of a pull request's head waits for that moment while a read does not. None of the three reads
-- a row, so the time the locks are held does not grow with the table, and the census admits all
-- three as catalog-only statements. The release before reads no github_read_at and writes it in
-- no statement. Rolling back past this migration is an image rollback alone: the release before
-- reads none of it, and its writes keep running the trigger, which only ever clears the column.
alter table pull_request_heads add column github_read_at timestamptz;

create function pull_request_heads_unread_write() returns trigger language plpgsql as $$
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
  end if;
  return new;
end $$;

create trigger pull_request_heads_unread_write before update on pull_request_heads
  for each row execute function pull_request_heads_unread_write();
