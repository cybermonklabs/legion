-- 20261123002037_production_events_changes.up.sql
--
-- production_events logs its changes (dispatch://INFRA-978, R13), so the day-row refresher
-- rewrites the days of the three failure measures a write moves, and a view built on them wakes
-- once those days are rewritten (the refresher's own eval_days change, dispatch://INFRA-2916, R10).
-- Its own migration, as 20261117100019 is for issue_filings: the transaction holds
-- production_events's lock alone, for milliseconds.
--
-- The function records each change in the change log's outbox through
-- delivery_change_log_pending (20261117100001), which takes no lock, so no write of the record
-- takes the change log's lock (6900, 0100); the pending row's notification on 'delivery_changes'
-- goes out at the writer's commit, and a drain moves the row into delivery_changes (R19 of
-- dispatch://INFRA-2916, delivery.DrainPending).
--
-- repo holds the row's project key and ref the row's id. t_old and t_new are the time the row
-- counts on before and after: a deploy's finished_at; a failure's blamed deploy's finished_at,
-- read from that row, since a failure counts on its deploy's day. An insert carries the new time
-- in t_new, a delete the old one in t_old, an update both. A failure whose restored_at moves logs
-- a second row, its restored_at before and after, because a restore also counts on its own day
-- (failed deployment recovery time, INFRA-978 R11). The refresher reads the row by its id for
-- the rest (the deploy a failure blames and the projects that deploy counts for), and a deleted
-- row by the times its change carries.
--
-- As the other triggers do, it writes no change row when nothing moved.
--
-- The census answers 0: two functions and one trigger, reading and rewriting no row.
create function production_events_counts_at(p_kind text, p_finished timestamptz, p_deploy uuid)
returns timestamptz language sql stable as $$
  select case when p_kind = 'deploy' then p_finished
              else (select finished_at from production_events where id = p_deploy) end
$$;

create function production_events_changed() returns trigger language plpgsql as $$
declare
  old_at timestamptz;
  new_at timestamptz;
begin
  if tg_op = 'INSERT' then
    new_at := production_events_counts_at(new.kind, new.finished_at, new.deploy_id);
    perform delivery_change_log_pending('production_events', null, new.project_key, new.id::text, null, new_at);
    if new.restored_at is not null then
      perform delivery_change_log_pending('production_events', null, new.project_key, new.id::text, null, new.restored_at);
    end if;
  elsif tg_op = 'DELETE' then
    old_at := production_events_counts_at(old.kind, old.finished_at, old.deploy_id);
    perform delivery_change_log_pending('production_events', null, old.project_key, old.id::text, old_at, null);
    if old.restored_at is not null then
      perform delivery_change_log_pending('production_events', null, old.project_key, old.id::text, old.restored_at, null);
    end if;
  elsif to_jsonb(old) is distinct from to_jsonb(new) then
    old_at := production_events_counts_at(old.kind, old.finished_at, old.deploy_id);
    new_at := production_events_counts_at(new.kind, new.finished_at, new.deploy_id);
    perform delivery_change_log_pending('production_events', null, new.project_key, new.id::text, old_at, new_at);
    if old.restored_at is distinct from new.restored_at then
      perform delivery_change_log_pending('production_events', null, new.project_key, new.id::text, old.restored_at, new.restored_at);
    end if;
  end if;
  return null;
end $$;

create trigger production_events_changed after insert or update or delete on production_events
  for each row execute function production_events_changed();
