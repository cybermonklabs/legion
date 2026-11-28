-- 20261128165638_row_security_repositories.up.sql — row security on the delivery tables of a repository (class R-repo of plan D4, INFRA-6168).
--
-- Class R-repo: a row of a repository passes the repository rule, dispatch_repo_visible(repo), the
-- rule the GitHub proxy asks too; a pull request with an issue goes through the issue instead. A
-- delivery change of no repository (a session group's or the settings') passes an open viewer.
--
-- Each table below gets row security, not forced, and one policy named viewer, for all commands
-- and to public, whose USING and WITH CHECK are the same three arms: the statement does not run as
-- the request role (dispatch_request_role), or the viewer is open while no project is private
-- (the R5 fast path), or the class's predicate. Only dispatch_app meets the predicate. The table's
-- owner bypasses row security, which is not forced, so the migrations, the system pool and the
-- release before this one, which run as the owner, read and write every row as before, and every
-- other role (the read login) passes the first arm. The first two arms and the viewer the access
-- clauses read ((select dispatch_viewer())) each read only the settings and are read once per
-- statement. A policy names no role, so it is made before dispatch_app exists (the last migration
-- of this release makes it). store.RowSecurityClasses names each table's class and reason, and
-- store.TestEveryTableIsClassedForRowSecurity holds them to the catalog.
--
-- ENABLE ROW LEVEL SECURITY and CREATE POLICY change the catalog alone: each takes ACCESS
-- EXCLUSIVE on its table for the moment it runs, bounded by the runner's lock_timeout and tried
-- again as every lock wait is, and reads no row, so the census admits the migration at any table
-- size (pgmigrate's catalog-only kinds). One class group per migration keeps the locks each holds
-- few and short against the release before this one's traffic. No row is read, rewritten or
-- refused, and the census answers 0. Rollback is an image rollback: the release before this one
-- runs as the owner and meets no policy.

alter table public.delivery_pull_requests enable row level security;
create policy viewer on public.delivery_pull_requests for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when delivery_pull_requests.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = delivery_pull_requests.issue_key)
		else dispatch_repo_visible(delivery_pull_requests.repo) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when delivery_pull_requests.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = delivery_pull_requests.issue_key)
		else dispatch_repo_visible(delivery_pull_requests.repo) end));

alter table public.delivery_runs enable row level security;
create policy viewer on public.delivery_runs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_runs.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_runs.repo));

alter table public.delivery_run_jobs enable row level security;
create policy viewer on public.delivery_run_jobs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_run_jobs.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_run_jobs.repo));

alter table public.delivery_run_warnings enable row level security;
create policy viewer on public.delivery_run_warnings for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_run_warnings.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(delivery_run_warnings.repo));

alter table public.branch_commits enable row level security;
create policy viewer on public.branch_commits for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(branch_commits.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(branch_commits.repo));

alter table public.commit_holds enable row level security;
create policy viewer on public.commit_holds for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(commit_holds.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(commit_holds.repo));

alter table public.deploys enable row level security;
create policy viewer on public.deploys for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(deploys.repo))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_repo_visible(deploys.repo));

alter table public.delivery_changes enable row level security;
create policy viewer on public.delivery_changes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when delivery_changes.repo is null then (select dispatch_viewer_open())
		else dispatch_repo_visible(delivery_changes.repo) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when delivery_changes.repo is null then (select dispatch_viewer_open())
		else dispatch_repo_visible(delivery_changes.repo) end));
