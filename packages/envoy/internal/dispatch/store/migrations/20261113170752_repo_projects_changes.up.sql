-- 20261113170752_repo_projects_changes.up.sql
--
-- repo_projects logs its changes (dispatch://INFRA-2916, R12 and R15), so the day-row refresher
-- rewrites the rows a repository's mapping decides. The mapping decides three things the stored
-- day rows hold: the project a pull request that names no issue counts for, the project a deploy
-- that ships none and every CI run count for (D1), and the repository an issue or a filing
-- resolves to where neither it nor an issue above it names one (a project's default mapping,
-- refs.IssueRepoTerms), which cuts the Legion rows by repository. No change kind logged a mapping
-- before this, so a remapped repository left every day row it moved standing as it was.
--
-- It logs a `settings` change, the kind a delivery_settings write logs (0100, 0156): a mapping is
-- a setting of the delivery measures as much as the deploy repository is, every reader of a
-- settings change reads it as "every window may have moved" (the Delivery page's hub marks every
-- view), and a new kind would need every one of them to learn it. repo holds the mapping's
-- repository, which a delivery_settings change leaves null, so the refresher tells the two apart:
-- a mapping moves the Legion rows' repositories as well as the delivery and CI rows, and the
-- delivery settings move those two alone. ref, t_old and t_new are null, as a settings change's
-- are.
--
-- An update that changes nothing logs nothing, as every other trigger of the log. Its own
-- migration, as 0101 to 0106 each add one table's trigger: the transaction holds repo_projects's
-- lock alone, for milliseconds. The release before this one never reads a settings change's repo,
-- and a settings change is one it already reads, so the two roll together.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function repo_projects_changed() returns trigger language plpgsql as $$
begin
  if tg_op = 'DELETE' then
    perform delivery_change_log('settings', null, old.repo, null, null, null);
  elsif tg_op = 'INSERT' or to_jsonb(old) is distinct from to_jsonb(new) then
    perform delivery_change_log('settings', null, new.repo, null, null, null);
  end if;
  return null;
end $$;

create trigger repo_projects_changed after insert or update or delete on repo_projects
  for each row execute function repo_projects_changed();
