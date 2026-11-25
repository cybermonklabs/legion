-- 20261119000230_projects_default_repo.up.sql — a project's default repository, on the project.
--
-- INFRA-4402: Dispatch stops reading the repository mapping (repo_projects, 0005) for a project's
-- default repository. This column holds it instead: the `owner/name` an issue resolves to when
-- neither the issue nor an issue on its parent chain names a repository of its own, or null for a
-- project with no default. The check holds it to the form the mapping's writes stored, never empty
-- and in lower case, as 0116 holds default_assignee.
--
-- The fill copies each project's default from the mapping: the repository of the repo_projects row
-- that carries is_default. repo_projects_default_per_project (0113) admits one such row per
-- project, so each project takes one value or none. A default the check refuses fails the fill, and
-- the boot with it; the census counts those defaults first. The migration only reads repo_projects
-- and writes no row of it, so a build that still reads the mapping sees the same rows, and a
-- rollback finds it as it was. It writes no row of any other table. A default that the release
-- before this one writes to the mapping after this commits stays in the mapping alone.
--
-- The release before this one names its own columns of projects in every read and write, so it
-- serves on this schema, and a project it creates has a null default_repo, which reads as no
-- default: the rule 0116_project_default_assignee states.
--
-- The ALTER takes projects' ACCESS EXCLUSIVE lock, and the migration holds it to commit, the fill
-- included. projects holds one row per project, so the fill is short, and the runner's lock_timeout
-- (pgmigrate.LockTimeout, 5 s) bounds the wait for the lock. The fill takes no lock on
-- repo_projects above ACCESS SHARE. The identifier is the UTC moment this was written, as
-- `migrationid` requires of every migration past the numbered series.
alter table projects add column default_repo text
  check (default_repo <> '' and default_repo = lower(default_repo));

update projects p
set default_repo = r.repo
from repo_projects r
where r.project = p.key and r.is_default;
