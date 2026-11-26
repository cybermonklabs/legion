-- 20261117100012_eval_readings_changes.up.sql
--
-- eval_readings logs its changes (dispatch://INFRA-2916, R10), so a reading wakes every view built
-- on its eval. Its own migration, as 20261117100002 is for issue_facts: the transaction holds
-- eval_readings's lock alone, for milliseconds.
--
-- One change row per eval for every transaction that writes its readings. The trigger is
-- statement-level, after insert, and reads the statement's inserted rows as one transition table,
-- so a statement of any number of readings hands each eval's span to eval_readings_change_log once,
-- not once a reading. That records the eval's first change of the transaction in the change log's
-- outbox through delivery_change_log_pending (20261117100001), which takes no lock, so a readings
-- write never takes the change log's lock (6900, 0100); every later statement of the transaction
-- that writes the eval's readings (the readings write sends a large batch in several, each of at
-- most the statement bound its parameters keep: evals.insertReadings) widens that same pending
-- row's span, as 20261117100014 widens an eval_days change. So a batch of a thousand readings wakes
-- a view once, however many statements it takes. The row it widens is this transaction's own,
-- uncommitted and so invisible to every drain until the commit, which delivers the one
-- notification; a drain then moves it into delivery_changes (R19, delivery.DrainPending). The id
-- of each eval's pending row, which delivery_change_log_pending answers, is kept in the
-- transaction-local setting dispatch.eval_readings_pending, a JSON object of eval id to pending
-- id: set_config(..., true) reverts it when the transaction ends, and with a subtransaction that
-- rolls back, as it does the row.
--
-- delivery_changes keeps two columns for a name, repo and ref. repo holds the project key and ref
-- the eval id, as an issue_facts row holds a project and an issue (20261117100002), because a
-- reading belongs to an eval of a project and to no repository. The project is the part of the
-- eval id before its slash: a project key holds no slash (projects_key_check), and an eval id is
-- `<project_key>/<slug>`.
--
-- t_old and t_new are the first and the last measured_at of the eval's readings that the
-- transaction wrote, the span of moments the write puts values in.
--
-- No update logs a change. A reading is append-only, and the one update a reading takes is the
-- write of superseded_at on the reading a correction replaces, in the transaction that inserts the
-- correction, whose insert logs that write's change. Nothing deletes a reading.
--
-- The census answers 0: two functions and one trigger, reading and rewriting no row.
create function eval_readings_change_log(p_project text, p_eval text, p_first timestamptz, p_last timestamptz)
returns void language plpgsql as $$
declare
  logged constant jsonb := coalesce(nullif(current_setting('dispatch.eval_readings_pending', true), ''), '{}')::jsonb;
  logged_id constant bigint := (logged ->> p_eval)::bigint;
  pending_id bigint;
begin
  if logged_id is not null then
    update delivery_changes_pending
       set t_old = least(t_old, p_first), t_new = greatest(t_new, p_last)
     where id = logged_id;
    if found then
      return;
    end if;
  end if;
  pending_id := delivery_change_log_pending('eval_readings', null, p_project, p_eval, p_first, p_last);
  perform set_config('dispatch.eval_readings_pending', (logged || jsonb_build_object(p_eval, pending_id))::text, true);
end $$;

create function eval_readings_changed() returns trigger language plpgsql as $$
declare
  written record;
begin
  for written in
    select eval_id, min(measured_at) as first_at, max(measured_at) as last_at
    from inserted
    group by eval_id
    order by eval_id
  loop
    perform eval_readings_change_log(split_part(written.eval_id, '/', 1), written.eval_id, written.first_at, written.last_at);
  end loop;
  return null;
end $$;

create trigger eval_readings_changed after insert on eval_readings
  referencing new table as inserted
  for each statement execute function eval_readings_changed();
