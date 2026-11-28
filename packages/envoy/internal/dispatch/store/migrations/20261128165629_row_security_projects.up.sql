-- 20261128165629_row_security_projects.up.sql — row security on the tables of one project's own rows (class P of plan D4, INFRA-6168).
--
-- Class P: a row of a table with a project column of its own belongs to that project. projects reads
-- its own key and visibility: the key is one the viewer sees, or the viewer is open and the project
-- is public. Every other table asks the project rule of its column: the key is one the viewer sees
-- (read once per statement), else dispatch_sees_project, which probes projects as its owner, so no
-- policy here reads projects under row security.
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

alter table public.projects enable row level security;
create policy viewer on public.projects for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (projects.key = any((select dispatch_viewer_sees())::text[]) or ((select dispatch_viewer_open()) and projects.visibility = 'public')))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (projects.key = any((select dispatch_viewer_sees())::text[]) or ((select dispatch_viewer_open()) and projects.visibility = 'public')));

alter table public.issues enable row level security;
create policy viewer on public.issues for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (issues.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(issues.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (issues.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(issues.project_key)));

alter table public.repo_projects enable row level security;
create policy viewer on public.repo_projects for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (repo_projects.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(repo_projects.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (repo_projects.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(repo_projects.project)));

alter table public.architecture_sources enable row level security;
create policy viewer on public.architecture_sources for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (architecture_sources.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(architecture_sources.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (architecture_sources.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(architecture_sources.project_key)));

alter table public.architecture_snapshots enable row level security;
create policy viewer on public.architecture_snapshots for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (architecture_snapshots.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(architecture_snapshots.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (architecture_snapshots.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(architecture_snapshots.project_key)));

alter table public.components enable row level security;
create policy viewer on public.components for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (components.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(components.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (components.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(components.project_key)));

alter table public.component_depends enable row level security;
create policy viewer on public.component_depends for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (component_depends.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(component_depends.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (component_depends.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(component_depends.project_key)));

alter table public.issue_component_members enable row level security;
create policy viewer on public.issue_component_members for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (issue_component_members.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(issue_component_members.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (issue_component_members.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(issue_component_members.project_key)));

alter table public.component_facts enable row level security;
create policy viewer on public.component_facts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (component_facts.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(component_facts.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (component_facts.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(component_facts.project_key)));

alter table public.related_checks enable row level security;
create policy viewer on public.related_checks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (related_checks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(related_checks.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (related_checks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(related_checks.project_key)));

alter table public.search_term_stats enable row level security;
create policy viewer on public.search_term_stats for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_term_stats.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_term_stats.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_term_stats.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_term_stats.project_key)));

alter table public.search_term_totals enable row level security;
create policy viewer on public.search_term_totals for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_term_totals.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_term_totals.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_term_totals.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_term_totals.project_key)));

alter table public.near_copy_checks enable row level security;
create policy viewer on public.near_copy_checks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (near_copy_checks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(near_copy_checks.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (near_copy_checks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(near_copy_checks.project_key)));

alter table public.spec_check_results enable row level security;
create policy viewer on public.spec_check_results for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (spec_check_results.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(spec_check_results.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (spec_check_results.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(spec_check_results.project_key)));

alter table public.project_access enable row level security;
create policy viewer on public.project_access for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (project_access.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(project_access.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (project_access.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(project_access.project_key)));

alter table public.project_role_holder_marks enable row level security;
create policy viewer on public.project_role_holder_marks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (project_role_holder_marks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(project_role_holder_marks.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (project_role_holder_marks.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(project_role_holder_marks.project_key)));

alter table public.legion_settings enable row level security;
create policy viewer on public.legion_settings for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_settings.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_settings.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_settings.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_settings.project)));

alter table public.legion_reads enable row level security;
create policy viewer on public.legion_reads for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_reads.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_reads.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_reads.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_reads.project)));

alter table public.legion_observations enable row level security;
create policy viewer on public.legion_observations for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_observations.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_observations.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_observations.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_observations.project)));

alter table public.legion_current enable row level security;
create policy viewer on public.legion_current for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_current.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_current.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_current.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_current.project)));

alter table public.legion_event_cursors enable row level security;
create policy viewer on public.legion_event_cursors for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_event_cursors.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_event_cursors.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_event_cursors.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_event_cursors.project)));

alter table public.legion_event_parts enable row level security;
create policy viewer on public.legion_event_parts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_event_parts.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_event_parts.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (legion_event_parts.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(legion_event_parts.project)));

alter table public.going_requests enable row level security;
create policy viewer on public.going_requests for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (going_requests.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(going_requests.project)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (going_requests.project = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(going_requests.project)));

alter table public.search_text_versions enable row level security;
create policy viewer on public.search_text_versions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_text_versions.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_text_versions.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (search_text_versions.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(search_text_versions.project_key)));

alter table public.user_threads enable row level security;
create policy viewer on public.user_threads for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (user_threads.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(user_threads.project_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (user_threads.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(user_threads.project_key)));
