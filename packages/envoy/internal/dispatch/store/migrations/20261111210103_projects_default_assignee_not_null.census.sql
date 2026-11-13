-- The projects that name no default assignee: every row 20261111210103 rewrites, and every row
-- its `alter table projects alter column default_assignee set not null` would refuse. A non-zero
-- count is repaired on the release running now by the fill the migration carries, printed in
-- cmd/dispatch/README.md under "Database migrations"; then take the census again.
--
-- default_assignee is 0116_project_default_assignee's column, which ships in an earlier release.
select count(*) from projects where default_assignee is null;
