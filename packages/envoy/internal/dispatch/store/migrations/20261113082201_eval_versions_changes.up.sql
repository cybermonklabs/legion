-- 20261113082201_eval_versions_changes.up.sql
--
-- eval_versions and eval_series log their changes (dispatch://INFRA-2916, R10), so a new version
-- of a definition (a registration, a changed target, a definition the seed writes) or of one of
-- its series (a changed fold, min_n, listing or unit) wakes every view built on its eval. Its own
-- migration, as 20261111093100 is for issue_facts: the transaction holds the two tables' locks
-- alone, for milliseconds.
--
-- Each trigger is row-level, after insert: each version is one change row, through
-- delivery_change_log (0100) under the advisory lock every other kind's trigger takes. A write
-- adds at most one version to each eval it names and to each series it names, the seed's write of
-- a whole catalogue included, so a row per version is a row per eval or series written. repo holds
-- the project key and ref the eval id, as an eval_readings row does (20261113082200); eval_series
-- holds no project column, and the project is the part of the eval id before its slash, as the
-- eval_readings trigger reads it.
--
-- Both log the kind `eval_versions`: a series' version is a version of its eval as much as the
-- definition's is, every reader of the kind reads it as "every window of this eval may read
-- otherwise", and a kind of its own would need each of them to learn it, and the kind check to be
-- rebuilt, for no reader that tells the two apart.
--
-- t_old and t_new are null. A definition or a series holds no time of its own, and a new version
-- (a target, a unit, a direction, a fold) changes how every window of its eval reads, which is
-- what a span of no time says, as a settings change's does: no reader of this kind narrows it to a
-- range of days.
--
-- Nothing updates or deletes a version, so only an insert logs one.
--
-- The census answers 0: two functions and two triggers, reading and rewriting no row.
create function eval_versions_changed() returns trigger language plpgsql as $$
begin
  perform delivery_change_log('eval_versions', null, new.project_key, new.eval_id, null, null);
  return null;
end $$;

create trigger eval_versions_changed after insert on eval_versions
  for each row execute function eval_versions_changed();

create function eval_series_changed() returns trigger language plpgsql as $$
begin
  perform delivery_change_log('eval_versions', null, split_part(new.eval_id, '/', 1), new.eval_id, null, null);
  return null;
end $$;

create trigger eval_series_changed after insert on eval_series
  for each row execute function eval_series_changed();
