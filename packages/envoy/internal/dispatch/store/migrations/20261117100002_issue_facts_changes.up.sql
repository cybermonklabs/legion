-- 20261117100002_issue_facts_changes.up.sql
--
-- issue_facts logs its changes (dispatch://INFRA-2915, dispatch://INFRA-2916, R26), so a write of
-- the stored per-issue Legion facts wakes every view built on them and names the days of the
-- Legion evals' day rows it can move. Its own migration, after the outbox it records into
-- (20261117100001): the transaction holds issue_facts's lock alone, for milliseconds.
--
-- The function records each change in the change log's outbox through
-- delivery_change_log_pending (20261117100001), which takes no lock, so no write of the facts takes
-- the change log's lock (6900, 0100); the pending row's notification on 'delivery_changes' goes
-- out at the writer's commit, and a drain moves the row into delivery_changes
-- (delivery.DrainPending).
--
-- delivery_changes keeps two columns for a name, repo and ref. repo holds the project key here and
-- ref holds the issue key, because a Legion fact belongs to a project and an issue rather than to
-- a repository. A reader of an issue_facts row therefore reads a project out of repo, which is
-- true of no delivery kind.
--
-- t_old and t_new name the days that count the issue: a Legion day row counts an issue on its
-- close day alone (evals.ReadLegionDay), so the days a change of its facts can move are its close
-- day before the change and its close day after it, and no day of its work. t_old is the row's
-- closed_at before the change and t_new its closed_at after it, each null where the issue was open
-- (or had no row) before, or is open (or has no row) after:
--   an insert carries null and the new row's closed_at,
--   a delete carries the old row's closed_at and null,
--   an update carries the old row's closed_at and the new row's,
-- so an open issue's change carries null in both and names no day, though it is logged all the
-- same, since the Legion board serves every open issue's row (legionfacts.Read). The evals
-- refresher rewrites exactly the days the two name (evals' refresher_work.go), and reads no row to
-- find them.
--
-- As the delivery tables' triggers do, it writes no change row when nothing a reader cares about
-- moved: updated_at is ignored, so a write that moved a row's time alone logs nothing.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function issue_facts_changed() returns trigger language plpgsql as $$
declare
  ignored constant text[] := array['updated_at'];
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log_pending('issue_facts', null, new.project_key, new.issue_key, null, new.closed_at);
  elsif tg_op = 'DELETE' then
    perform delivery_change_log_pending('issue_facts', null, old.project_key, old.issue_key, old.closed_at, null);
  elsif (to_jsonb(old) - ignored) is distinct from (to_jsonb(new) - ignored) then
    perform delivery_change_log_pending('issue_facts', null, new.project_key, new.issue_key, old.closed_at, new.closed_at);
  end if;
  return null;
end $$;

create trigger issue_facts_changed after insert or update or delete on issue_facts
  for each row execute function issue_facts_changed();
