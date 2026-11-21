-- 20261117100020_repo_projects_changes.up.sql
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
-- The trigger records the change in the change log's outbox (delivery_change_log_pending,
-- 20261117100001), which takes no lock, and a drain moves it into delivery_changes after the
-- writer commits: every writer of a mapping drains one batch right after its commit
-- (delivery.DrainRepoProjectChange). So no mapping write takes the change log's lock (6900)
-- inside its transaction, in this release or the one before it. The release before this one
-- writes a mapping under the repository's advisory lock, then its projects' owner locks, then the
-- row for update (api's lockRepoProjectWrite in that release), so a trigger that logged into
-- delivery_changes directly would take 6900 in that write's update, after those locks; a write of
-- this release that took 6900 before the repository's lock would wait for that lock holding 6900,
-- and two writes of one repository, one of each build, would each wait for the other until
-- Postgres ended one (40P01).
--
-- An update that changes nothing the mapping decides logs nothing, as every other trigger of the
-- log logs no write that leaves a row's content as it was. The row's created_by and created_at say
-- who wrote the mapping and when, and decide no day row, and the settings route's upsert
-- (api/repo_projects.go) sets created_by on every write: an unchanged mapping saved again by
-- another person would otherwise log a change that begins every walk of the day rows again. So an
-- update logs one only where the row moved in a column other than those two. Its own migration, as
-- 0101 to 0106 each add one table's trigger: the transaction holds repo_projects's lock alone, for
-- milliseconds.
--
-- The release before this one fires this trigger too, and its write leaves a pending row, which
-- this release's drains move: the boot's, the live hub's before each read, the refresher's at the
-- start of each pass, and every eval or mapping write's after its commit. The release before this
-- one never reads a settings change's repo, and a settings change is one it already reads once a
-- drain has moved it, so the two roll together.
--
-- The census answers 0: one function and one trigger, reading and rewriting no row.
create function repo_projects_changed() returns trigger language plpgsql as $$
begin
  if tg_op = 'DELETE' then
    perform delivery_change_log_pending('settings', null, old.repo, null, null, null);
  elsif tg_op = 'INSERT'
     or (to_jsonb(old) - 'created_by' - 'created_at') is distinct from (to_jsonb(new) - 'created_by' - 'created_at') then
    perform delivery_change_log_pending('settings', null, new.repo, null, null, null);
  end if;
  return null;
end $$;

create trigger repo_projects_changed after insert or update or delete on repo_projects
  for each row execute function repo_projects_changed();
