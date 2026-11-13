-- 20261113082201_eval_versions_changes.up.sql
--
-- eval_versions logs its changes (dispatch://INFRA-2916, R10), so a new version of a definition
-- (a registration, a changed target, a definition the seed writes) wakes every view built on its
-- eval. Its own migration, as 20261111093100 is for issue_facts: the transaction holds
-- eval_versions's lock alone, for milliseconds.
--
-- The trigger is row-level, after insert: each version is one change row, through
-- delivery_change_log (0100) under the advisory lock every other kind's trigger takes. A write
-- adds at most one version to each eval it names, the seed's write of a whole catalogue included,
-- so a row per version is a row per eval written. repo holds the project key and ref the eval id,
-- as an eval_readings row does (20261113082200).
--
-- t_old and t_new are null. A definition holds no time of its own, and a new version (a target, a
-- unit, a direction) changes how every window of its eval reads, which is what a span of no time
-- says, as a settings change's does: no reader of this kind narrows it to a range of days.
--
-- Nothing updates or deletes a version, so only an insert logs one.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function eval_versions_changed() returns trigger language plpgsql as $$
begin
  perform delivery_change_log('eval_versions', null, new.project_key, new.eval_id, null, null);
  return null;
end $$;

create trigger eval_versions_changed after insert on eval_versions
  for each row execute function eval_versions_changed();
