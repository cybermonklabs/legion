-- 20261117100014_eval_days_changes.up.sql
--
-- eval_days logs its changes (dispatch://INFRA-2916, R10), so a computed eval's view wakes once its
-- day rows are rewritten. The input's own change (a pull request, a run, an issue's facts) wakes
-- the views before the refresher has rewritten the day rows it touches; a view that read then
-- would answer the old rows, and with no change of the rows' own it would never read again. Its
-- own migration, as 20261117100002 is for issue_facts: the transaction holds eval_days's lock
-- alone, for milliseconds.
--
-- One change row per eval for every transaction that writes its day rows. The refresher writes a
-- series' days in one or more upsert statements (`insert ... on conflict ... do update`) of the
-- transaction, and Postgres fires an upsert's statement-level triggers once for its inserted rows
-- and once for its updated rows, each with a transition table of its own half. So two
-- statement-level triggers read the two halves, and both hand each eval's span to
-- eval_days_change_log, which records the eval's first change of the transaction in the change
-- log's outbox through delivery_change_log_pending (20261117100001), which takes no lock, so no
-- write of the day rows takes the change log's lock (6900, 0100); a drain moves the row into
-- delivery_changes after the commit (R19, delivery.DrainPending). Every later write of the eval in
-- the transaction, the other half of the upsert or another statement, widens that same pending
-- row's span. The row it widens is this transaction's own, uncommitted and so invisible to every
-- drain until the commit, which delivers the one notification. The id of each eval's pending row,
-- which delivery_change_log_pending answers, is kept in the transaction-local setting
-- dispatch.eval_days_pending, a JSON object of eval id to pending id: set_config(..., true) reverts
-- it when the transaction ends, and with a subtransaction that rolls back, as it does the row.
--
-- repo holds the project key and ref the eval id, as an eval_readings row does (20261117100012).
-- t_old and t_new are midnight UTC of the first and the last day the eval's written rows hold, so
-- the span names those days and the days between them. The rows of every deploy repository of an
-- eval (eval_days.deploy_repo, 20261117100006) are one change of the eval, its span theirs
-- together: an evals view reads one repository's rows of it, which the delivery settings choose
-- at read time, and the stored figures' fold of the span (evals.figureChanges) reads every
-- repository's.
--
-- run_kind holds the cause the writing transaction names in the transaction-local setting
-- dispatch.eval_days_cause, null where it names none. The refresher's rewrite of a delivery
-- settings change sets it to 'settings_rewrite' (delivery.EvalDaysWrittenBySettingsRewrite,
-- through evals.MarkSettingsRewrite) before its first write of a day row, and no other writer sets
-- it: the Delivery page's live hub recomputes an evals view at once for the rewrite's changes, and
-- every other change waits its quiet. The pending row takes the cause its first write finds, and
-- the drain (delivery_changes_drain, 20261117100001) copies it into delivery_changes.
-- set_config(..., true) reverts the setting when the transaction ends, so a pooled connection's
-- next transaction reads it empty, which nullif makes null.
--
-- An update logs only the rows whose content moved: as 0100's and 20261117100002's triggers ignore
-- the columns a pass rewrites whatever it finds, the update half ignores updated_at and built_by,
-- so a new build that rebuilds a row to the same values logs nothing. It compares every other
-- column of the old row with the same column of the new one, as two rows, where those row
-- triggers compare each row's JSON less the ignored keys: the update half reads a whole
-- statement's rows at once, and building two JSON objects a row to compare them was most of its
-- time. Over transition tables of the 2,182 rows a settings change rewrote, the JSON form took
-- 140 ms of the trigger's 156 and the columns 7 ms, and the trigger took 29 ms with them. A column
-- eval_days gains joins both lists of the comparison (store's
-- TestEveryColumnOfADayRowButItsBookkeepingIsComparedByItsUpdateTrigger fails until it does). The
-- old and the new rows are matched by the primary key, deploy_repo among its columns, and a row
-- whose key an update moved matches nothing on either side, so both its old day and its new one
-- are logged. A delete logs nothing: a day the refresher finds no input for keeps an explicit zero
-- row rather than losing its row, and the rows a project's deletion takes go with every view of
-- that project.
--
-- The census answers 0: three functions and two triggers, reading and rewriting no row.
create function eval_days_change_log(p_project text, p_eval text, p_first date, p_last date)
returns void language plpgsql as $$
declare
  logged constant jsonb := coalesce(nullif(current_setting('dispatch.eval_days_pending', true), ''), '{}')::jsonb;
  logged_id constant bigint := (logged ->> p_eval)::bigint;
  first_at constant timestamptz := p_first::timestamp at time zone 'UTC';
  last_at constant timestamptz := p_last::timestamp at time zone 'UTC';
  pending_id bigint;
begin
  if logged_id is not null then
    update delivery_changes_pending
       set t_old = least(t_old, first_at), t_new = greatest(t_new, last_at)
     where id = logged_id;
    if found then
      return;
    end if;
  end if;
  pending_id := delivery_change_log_pending('eval_days', nullif(current_setting('dispatch.eval_days_cause', true), ''),
    p_project, p_eval, first_at, last_at);
  perform set_config('dispatch.eval_days_pending', (logged || jsonb_build_object(p_eval, pending_id))::text, true);
end $$;

create function eval_days_inserted() returns trigger language plpgsql as $$
declare
  span record;
begin
  for span in
    select project_key, eval_id, min(day) as first_day, max(day) as last_day
    from written
    group by project_key, eval_id
    order by project_key, eval_id
  loop
    perform eval_days_change_log(span.project_key, span.eval_id, span.first_day, span.last_day);
  end loop;
  return null;
end $$;

create function eval_days_updated() returns trigger language plpgsql as $$
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
           old_row.via_repository)
      is distinct from (new_row.project_key, new_row.eval_id, new_row.writer, new_row.deploy_repo, new_row.series,
           new_row.repo, new_row.day, new_row.listed, new_row.n, new_row.sum, new_row.num, new_row.den, new_row.max,
           new_row.samples, new_row.last_value, new_row.last_at, new_row.last_recorded_at, new_row.last_reading,
           new_row.counts, new_row.via_repository)
    group by project_key, eval_id
    order by project_key, eval_id
  loop
    perform eval_days_change_log(span.project_key, span.eval_id, span.first_day, span.last_day);
  end loop;
  return null;
end $$;

create trigger eval_days_inserted after insert on eval_days
  referencing new table as written
  for each statement execute function eval_days_inserted();

create trigger eval_days_updated after update on eval_days
  referencing old table as replaced new table as written
  for each statement execute function eval_days_updated();
