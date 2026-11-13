-- 20261113170751_issue_filings_changes.up.sql
--
-- issue_filings logs its changes (dispatch://INFRA-2916, R12), so the day-row refresher rewrites
-- the day a filing counts on whenever the filing moves: when the Legion facts consumer writes it,
-- rewrites its class, its filer's phase or the filed issue's tree root, or deletes it. Its own
-- migration, as 20261111093100 is for issue_facts: the transaction holds issue_filings's lock
-- alone, for milliseconds.
--
-- The function calls delivery_change_log (0100) under the advisory lock every other kind's trigger
-- takes, so seq is allocated in commit order and pg_notify('delivery_changes', '') goes out at
-- commit.
--
-- repo holds the project key and ref the filed issue's key, as an issue_facts row's change does
-- (20261111093100): the project is the one whose Legion session filed the issue, which is the
-- project whose day rows count the filing. t_old and t_new are the row's filed_at before and
-- after, the time that names the filing's day:
--   an insert carries the new row's filed_at in t_new,
--   a delete carries the old row's in t_old,
--   an update carries both. An update that moves the filing to another project logs two rows,
--   the old project's with the old time alone and the new project's with the new time alone,
--   because one row names one project and both projects' days moved.
-- A reader rewrites the day of each time a row carries, in the project the row names.
--
-- As the other triggers do, it writes no change row when nothing moved: the consumer's upsert
-- rewrites a filing only where a value differs, and an update that rewrites a row to the values it
-- holds logs nothing here.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function issue_filings_changed() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log('issue_filings', null, new.project_key, new.issue_key, null, new.filed_at);
  elsif tg_op = 'DELETE' then
    perform delivery_change_log('issue_filings', null, old.project_key, old.issue_key, old.filed_at, null);
  elsif to_jsonb(old) is distinct from to_jsonb(new) then
    if old.project_key = new.project_key then
      perform delivery_change_log('issue_filings', null, new.project_key, new.issue_key, old.filed_at, new.filed_at);
    else
      perform delivery_change_log('issue_filings', null, old.project_key, old.issue_key, old.filed_at, null);
      perform delivery_change_log('issue_filings', null, new.project_key, new.issue_key, null, new.filed_at);
    end if;
  end if;
  return null;
end $$;

create trigger issue_filings_changed after insert or update or delete on issue_filings
  for each row execute function issue_filings_changed();
