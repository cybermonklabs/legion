-- 20261127230251_eval_days_sample_keys.up.sql — the pull request each sample of a day row times.
--
-- dispatch://INFRA-978/comment/9cdcf9d2-532d-4431-9332-3ced4d4329f2 and
-- dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8: a project can read one pull
-- request's lead time to production in day rows of several deploy repositories of the day it
-- merged: its repository's GitHub rows (the project's own, or an owner key's: a pull request no
-- issue attributes, or one whose attribution hangs on the published defaults), timed to that
-- repository's first deploy that ships it, and the project's own row of its events repository,
-- timed to the first deploy posted to the project's route that names it. Each row is built from
-- its own repository's deploys alone, and which repositories a project reads is chosen when it
-- reads, so the read of the project counts the pull request once: of its samples in one range,
-- day, series and repository, it keeps the smallest, the first deploy's, a GitHub deploy's where
-- it ties a post's (evals.shippedOnce). A move of a default repository or of a source then moves
-- which rows a project reads, and rewrites no other repository's row.
--
--   eval_days.sample_keys           the pull request of each of the row's samples, in their order,
--                                   each `owner/repo#n` as delivery_pull_requests spells the
--                                   repository (measures.MergeToProductionKeys and the other two
--                                   lead times' keys). The refresher writes it on every row of the
--                                   three lead times to production (merge, first commit and open
--                                   to production), and null on every other row.
--   eval_rebuild_days.sample_keys   the same of a row a rebuild stages, which its flip writes.
--
-- A row whose keys are null, or are not one for each of its samples, is read as it stands: every
-- sample counts. Every row this migration finds, and every row the release before writes, is
-- such a row, until the refresher of this release writes it again (the rebuild a release begins
-- rewrites each one whose values moved).
--
-- The release before writes samples and names no keys. Its insert leaves a row's keys null, and its
-- update would leave keys beside samples they no longer name. So eval_days_sample_keys_kept, before
-- an update that moves a row's samples, clears the row's keys unless the writing transaction says
-- it writes them: the transaction-local setting dispatch.sample_keys 'on', which this release's
-- writes of day rows set before a statement that sends keys (evals.markSampleKeys), as the union
-- figures' writes say they fold the union cuts (20261126125956). A row the release before
-- rewrites is then read as it stands.
--
-- eval_days_updated (20261117100014) compares every column of a row but its bookkeeping, so that
-- an update that moves its content logs a change; it is written again here with sample_keys among
-- those columns.
--
-- Each ADD COLUMN adds a nullable column with no default, which writes the catalog alone and
-- rewrites no row; each takes ACCESS EXCLUSIVE on its table for an instant, waiting for each
-- transaction that holds the table under the runner's lock timeout. The trigger takes SHARE ROW
-- EXCLUSIVE on eval_days for the migration's short transaction. The release before names neither
-- column in any statement and reads every column it names as it did. Its census answers 0: no row
-- is read, refused or rewritten. Rolling back past this migration is an image rollback alone.
alter table eval_days add column sample_keys text[];

alter table eval_rebuild_days add column sample_keys text[];

create or replace function eval_days_updated() returns trigger language plpgsql as $$
declare
  span record;
begin
  for span in
    select project_key, eval_id, min(day) as first_day, max(day) as last_day
    from written new_row
    full join replaced old_row using (project_key, eval_id, writer, deploy_repo, series, repo, day)
    where (old_row.project_key, old_row.eval_id, old_row.writer, old_row.deploy_repo, old_row.series, old_row.repo,
           old_row.day, old_row.listed, old_row.n, old_row.sum, old_row.num, old_row.den, old_row.max, old_row.samples,
           old_row.last_value, old_row.last_at, old_row.last_recorded_at, old_row.last_reading, old_row.counts,
           old_row.via_repository, old_row.sample_keys)
      is distinct from (new_row.project_key, new_row.eval_id, new_row.writer, new_row.deploy_repo, new_row.series,
           new_row.repo, new_row.day, new_row.listed, new_row.n, new_row.sum, new_row.num, new_row.den, new_row.max,
           new_row.samples, new_row.last_value, new_row.last_at, new_row.last_recorded_at, new_row.last_reading,
           new_row.counts, new_row.via_repository, new_row.sample_keys)
    group by project_key, eval_id
    order by project_key, eval_id
  loop
    perform eval_days_change_log(span.project_key, span.eval_id, span.first_day, span.last_day);
  end loop;
  return null;
end $$;

create function eval_days_sample_keys_kept() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.sample_keys', true) is distinct from 'on' then
    new.sample_keys := null;
  end if;
  return new;
end $$;

create trigger eval_days_sample_keys_kept before update on eval_days
  for each row when (old.samples is distinct from new.samples)
  execute function eval_days_sample_keys_kept();
