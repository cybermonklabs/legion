-- 20261113082200_eval_readings_changes.up.sql
--
-- eval_readings logs its changes (dispatch://INFRA-2916, R10), so a reading wakes every view built
-- on its eval. Its own migration, as 20261111093100 is for issue_facts: the transaction holds
-- eval_readings's lock alone, for milliseconds.
--
-- The trigger is statement-level, after insert, and reads the statement's inserted rows as one
-- transition table, so a write of any number of readings logs one change row per eval it wrote,
-- not one per reading: a batch of a thousand readings wakes a view once. Each row goes through
-- delivery_change_log (0100), under the same advisory lock every other kind's trigger takes, so
-- seq is allocated in commit order and pg_notify('delivery_changes', '') goes out at commit.
--
-- delivery_changes keeps two columns for a name, repo and ref. repo holds the project key and ref
-- the eval id, as an issue_facts row holds a project and an issue (20261111093100), because a
-- reading belongs to an eval of a project and to no repository. The project is the part of the
-- eval id before its slash: a project key holds no slash (projects_key_check), and an eval id is
-- `<project_key>/<slug>`.
--
-- t_old and t_new are the first and the last measured_at of the eval's readings that the
-- statement wrote, the span of moments the write puts values in.
--
-- No update logs a change. A reading is append-only, and the one update a reading takes is the
-- write of superseded_at on the reading a correction replaces, in the transaction that inserts the
-- correction, whose insert logs that write's change. Nothing deletes a reading.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
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
    perform delivery_change_log('eval_readings', null, split_part(written.eval_id, '/', 1), written.eval_id,
      written.first_at, written.last_at);
  end loop;
  return null;
end $$;

create trigger eval_readings_changed after insert on eval_readings
  referencing new table as inserted
  for each statement execute function eval_readings_changed();
