-- 20261117100001_delivery_changes_pending.up.sql
--
-- The delivery change log admits the five kinds of the eval and Legion tables, and gains the
-- outbox those tables' triggers record their changes in (dispatch://INFRA-2915,
-- dispatch://INFRA-2916, R10, R12, R19). Each kind
-- is logged by its table's trigger, so every view and loop built on those rows wakes the way a
-- delivery row's write wakes the Delivery and Pipeline pages (0100):
--   issue_facts     a write of the stored per-issue Legion facts (20261117100000; its trigger is
--                   20261117100002's).
--   eval_readings   a reading (20261117100012).
--   eval_versions   a new version of a definition or of one of its series (20261117100013).
--   eval_days       a write of the stored day rows (20261117100014). It is the kind a computed
--                   eval needs: its value moves when the day rows are rewritten, after the input's
--                   own change has already woken the views, so a view woken by the input alone
--                   would read the old rows and never wake again.
--   issue_filings   a write of a stored filing (20261117100007; its trigger is 20261117100019's),
--                   so the day-row refresher (evals.Store.RunRefresher) rewrites the splitting
--                   rows of the filing's day. None of the Legion facts consumer's writes of a
--                   filing moves an issue_facts row, so without a kind of their own no change would
--                   say that a filing day's input moved.
--
-- delivery_changes.kind carries its check inline, with no name of its own, so the check is dropped
-- by the name Postgres generated for it, delivery_changes_kind_check, and added again under that
-- name, listing 0100's five kinds and these five. The new list admits every kind the old one did,
-- so no stored row can fail it. It is added `not valid` all the same, as 20261102233625 adds its
-- widened check: a validated add reads every row of the table while it holds the table's ACCESS
-- EXCLUSIVE lock, where `not valid` changes the catalogue alone and still refuses every row written
-- or updated after it. Nothing validates it later: there is nothing to validate.
--
-- The outbox. Every insert into delivery_changes takes the advisory lock 6900 and holds it to its
-- commit, so a row's seq is allocated only after every row below it has committed or rolled back,
-- and a reader whose cursor stands at a seq never meets a row below it later: a reader's cursor
-- never skips a change row. The eval and Legion writes are long (a correction of 1,400 readings
-- refolds its days, a facts pass writes a project's rows), and to keep the lock order free of
-- cycles a write that inserted its change rows into delivery_changes would take 6900 first and
-- hold it for the whole write: measured so, a median 1.53 s at a correction of 1,400 readings,
-- which an intake statement waited out (632 ms median against 5 ms). So their change rows wait in
-- delivery_changes_pending instead, taking no lock, and only the drain takes 6900 for them, in one
-- short transaction of its own.
--
-- delivery_changes_pending holds exactly the columns a delivery_changes row takes: kind, run_kind,
-- repo, ref, t_old, t_new and at, set by clock_timestamp() when the pending row is written, which
-- the move keeps. Its kinds are the five above, the ones the eval and Legion tables' triggers
-- write, beside the eval_days rows the eval day-row refresher logs itself (20261117100014): its
-- folds' and a rebuild's flip's, its word that the day rows stand at the delivery settings, and
-- its publishing of the default repositories. Every other change
-- of the delivery intake is written into delivery_changes directly: pull_request, run, run_job and
-- session_group, and the settings change of a delivery_settings write, which logs it last, in its
-- one statement. Those writes are one statement each or transactions that take 6900 first or in
-- their last statement (delivery's lockChangesStatement). A run's jobs write
-- (delivery.UpsertRunJobs) is one statement now, the run row and every job, and a session group's
-- write takes 6900 after its reads and before its first write: 0100's header, which describes the
-- jobs write as two statements and the group write taking 6900 in the middle of its transaction,
-- stays as it shipped.
--
-- delivery_change_log_pending(kind, run_kind, repo, ref, t_old, t_new) inserts one pending row,
-- answers its id (eval_days_change_log widens its own transaction's row by it), and notifies
-- 'delivery_changes', which Postgres delivers at the writer's commit. It takes no lock.
--
-- delivery_changes_drain(batch) moves up to batch committed pending rows, the lowest ids, into
-- delivery_changes and answers how many it moved. It reads first whether any row is pending and,
-- where none is, returns at once, taking no lock. Otherwise it takes 6900 and, in one statement,
-- deletes those pending rows and inserts them into delivery_changes in id order, keeping each
-- row's at; where it moved any, it notifies 'delivery_changes', as every insert into
-- delivery_changes does at its commit. Its caller runs it as a statement of its own
-- (delivery.DrainPendingBatch, 250 rows a batch, and delivery.DrainPending, again while a whole
-- batch moved), so each call holds 6900 for one batch's statement and its commit alone, whatever
-- the burst: on the pod it was measured on, a batch of 1,000 answered in up to 94 ms under load,
-- and 10,000 rows in one statement held 6900 118-328 ms. Each statement of the function reads a
-- snapshot taken when it starts, under read committed, so the move reads every row committed
-- before it took the lock: two drains take turns on the lock, the second finds the rows the first
-- deleted gone, and each pending row is moved exactly once. A pending row's id is drawn at its
-- insert, not its commit, so two writers can commit out of id order and a drain between their
-- commits moves the higher id first: seq follows the order the drains move rows in, not the
-- writers' commit order, and is allocated under the lock all the same, so no reader's cursor
-- skips a moved row either.
--
-- Who drains: an eval write, the registry's or the refresher's, one batch right after its own
-- commit, a registry write before its route answers; a Legion facts pass, every
-- pending row, right after its commit; the Delivery page's live hub, before each read of the log,
-- while a page subscribes; the day-row refresher, at the start of each pass; and the server's
-- boot, once, before it serves. A pending row left by a writer killed between its commit and its
-- drain is moved by the next drain of any process, or else by the next process's boot before it
-- serves (delivery.DrainPending states the bound).
--
-- The release before this one writes none of the five kinds and never reads the outbox. This
-- migration locks
-- delivery_changes alone, for milliseconds (0073's reason): the outbox and the two functions are
-- new, and creating them locks no table that exists. It reads no row, and the census answers 0.
alter table delivery_changes drop constraint delivery_changes_kind_check;
alter table delivery_changes add constraint delivery_changes_kind_check check (
  kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings', 'issue_facts',
           'eval_readings', 'eval_versions', 'eval_days', 'issue_filings')
) not valid;

create table delivery_changes_pending (
  id       bigserial primary key,
  at       timestamptz not null default clock_timestamp(),
  kind     text not null check (kind in ('issue_facts', 'eval_readings', 'eval_versions', 'eval_days', 'issue_filings')),
  run_kind text,
  repo     text,
  ref      text,
  t_old    timestamptz,
  t_new    timestamptz
);

create function delivery_change_log_pending(p_kind text, p_run_kind text, p_repo text, p_ref text, p_old timestamptz, p_new timestamptz)
returns bigint language plpgsql as $$
declare
  pending_id bigint;
begin
  insert into delivery_changes_pending (kind, run_kind, repo, ref, t_old, t_new)
    values (p_kind, p_run_kind, p_repo, p_ref, p_old, p_new)
    returning id into pending_id;
  perform pg_notify('delivery_changes', '');
  return pending_id;
end $$;

create function delivery_changes_drain(p_batch integer) returns bigint language plpgsql as $$
declare
  moved bigint;
begin
  if not exists (select 1 from delivery_changes_pending) then
    return 0;
  end if;
  perform pg_advisory_xact_lock(6900, 0);
  with taken as (
    delete from delivery_changes_pending
    where id in (select id from delivery_changes_pending order by id limit p_batch)
    returning *
  )
  insert into delivery_changes (at, kind, run_kind, repo, ref, t_old, t_new)
  select at, kind, run_kind, repo, ref, t_old, t_new from taken order by id;
  get diagnostics moved = row_count;
  if moved > 0 then
    perform pg_notify('delivery_changes', '');
  end if;
  return moved;
end $$;
