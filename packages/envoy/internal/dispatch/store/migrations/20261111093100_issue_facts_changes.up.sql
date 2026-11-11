-- 20261111093100_issue_facts_changes.up.sql
--
-- issue_facts logs its changes (dispatch://INFRA-2915), so a write of the stored per-issue Legion
-- facts wakes every view built on them. Its own migration, as 0101 to 0106 each add one trigger
-- for one table: the transaction holds issue_facts's lock alone, for milliseconds, and never while
-- it waits for another table's.
--
-- The function calls delivery_change_log (0100) under the same advisory lock the five other
-- triggers take, so seq is allocated in commit order and pg_notify('delivery_changes', '') goes
-- out at commit.
--
-- delivery_changes keeps two columns for a name, repo and ref. repo holds the project key here and
-- ref holds the issue key, because a Legion fact belongs to a project and an issue rather than to
-- a repository. A reader of an issue_facts row therefore reads a project out of repo, which is
-- true of no other kind.
--
-- t_old and t_new are not the row's time before and after, as a pull request's merged_at is. They
-- are the two ends of the span of days the issue's work falls in, from work_first_at and
-- work_last_at, so a reader of per-day rows rewrites exactly the days that moved and no other:
--   an insert carries the new row's span,
--   a delete carries the old row's span,
--   an update carries the union of the old row's span and the new row's, which is the earlier of
--   the two firsts and the later of the two lasts, because a day that held work and no longer
--   does must be rewritten too.
-- An issue with no measured work carries null in both columns. least and greatest ignore a null
-- argument, so the union of a null span with a real one is the real one.
--
-- As the five other triggers do, it writes no change row when nothing a reader cares about moved:
-- updated_at is ignored, so a sweep that rewrote a row's time alone logs nothing.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function issue_facts_changed() returns trigger language plpgsql as $$
declare
  ignored constant text[] := array['updated_at'];
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log('issue_facts', null, new.project_key, new.issue_key, new.work_first_at, new.work_last_at);
  elsif tg_op = 'DELETE' then
    perform delivery_change_log('issue_facts', null, old.project_key, old.issue_key, old.work_first_at, old.work_last_at);
  elsif (to_jsonb(old) - ignored) is distinct from (to_jsonb(new) - ignored) then
    perform delivery_change_log('issue_facts', null, new.project_key, new.issue_key,
      least(old.work_first_at, new.work_first_at), greatest(old.work_last_at, new.work_last_at));
  end if;
  return null;
end $$;

create trigger issue_facts_changed after insert or update or delete on issue_facts
  for each row execute function issue_facts_changed();
