-- 20261119000230 adds projects.default_repo and copies into it the repository of each mapping row
-- that carries is_default. The copy changes no value any reader had: it fills a column only this
-- release reads, which is the rule 0133_comments_written_version_backfill states for its own fill.
-- The count is the defaults the column's check would refuse, an empty name or one not in lower
-- case: the copy of one fails the migration, and the boot with it. It reads repo_projects.is_default,
-- which 0113_repo_projects_default_and_issue_repo creates and which ships in an earlier release, so
-- the column is there when a deployment takes this census.
select count(*)
from repo_projects
where is_default and (repo = '' or repo <> lower(repo));
