-- 20261123041500_delivery_reverts.up.sql
--
-- Every merged revert of a deploy repository (dispatch://INFRA-978, R7 and D5): a merged pull
-- request whose body says `Reverts <owner>/<repo>#<n>`, the line GitHub's revert button writes
-- (delivery.ParseRevert). It is recorded whoever authored it: the button's author is the person who
-- pressed it, often outside every project's population, so delivery_pull_requests does not hold
-- it. The pull-request intake records one as it merges, and the reconcile's search of each deploy
-- repository records one the intake missed (delivery.RecordRevert).
--
-- The columns:
--   repo, number          the revert itself, as owner/repo and its number.
--   reverts_repo, reverts_number
--                         the pull request it reverts, as its body names it.
--   reverts_merge_commit_sha
--                         the merge commit of the reverted pull request as GitHub answered it when
--                         the revert was recorded, null where GitHub named none: the deploy that
--                         shipped it is the deploy whose commits hold it, where no deploy row
--                         names the reverted pull request among those it shipped.
--   merged_at             when the revert merged: the deploy that ships it is the first production
--                         deploy GitHub created after it (delivery.FirstShippingApply).
--   merge_commit_sha      the revert's own merge commit, null where GitHub named none.
--   recorded_at           when the row was last written.
--
-- The revert's failure row (production_events, signal revert) is written once the deploy that
-- ships the revert has its deploy row: it blames the deploy that shipped the reverted pull request,
-- and the revert's own deploy restores it (prodevents/signals).
--
-- A write notifies dispatch_delivery_reverts with the repository at its commit, which wakes the
-- production signals' loop (signals.RevertsChannel), so a revert the reconcile records after its
-- deploy row landed makes its failure then.
--
-- A new table, an index, a function and a trigger on the new table: nothing existing is read,
-- locked or rewritten, and the release before this one neither reads nor writes it, so the two
-- builds roll together. The census answers 0.
create table delivery_reverts (
  repo                     text not null check (repo <> ''),
  number                   integer not null check (number > 0),
  reverts_repo             text not null check (reverts_repo <> ''),
  reverts_number           integer not null check (reverts_number > 0),
  reverts_merge_commit_sha text check (reverts_merge_commit_sha ~ '^[0-9a-f]{40}$'),
  merged_at                timestamptz not null,
  merge_commit_sha         text check (merge_commit_sha ~ '^[0-9a-f]{40}$'),
  recorded_at              timestamptz not null default now(),
  primary key (repo, number)
);

-- The read of a repository's reverts by when they merged (delivery.ListRevertShips).
create index delivery_reverts_merged on delivery_reverts (repo, merged_at);

create function delivery_reverts_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_delivery_reverts', new.repo);
  return null;
end $$;

create trigger delivery_reverts_notify after insert or update on delivery_reverts
  for each row execute function delivery_reverts_notify();
