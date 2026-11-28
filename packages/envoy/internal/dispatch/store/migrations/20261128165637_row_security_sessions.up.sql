-- 20261128165637_row_security_sessions.up.sql — row security on a session's rows and the listener's answer (classes S and RR of plan D4, INFRA-6168).
--
-- Class S: a row of a session passes access.SessionClause over its session_id, which asks the
-- owner-run session_clause_visible about every session its own SQL does not answer. Class RR: the
-- listener's answer (route_roster and its projection, one row of every session's projects) passes
-- an open viewer; the rule functions read it as its owner.
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

alter table public.session_projects enable row level security;
create policy viewer on public.session_projects for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_projects.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_projects.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_projects.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_projects.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.session_reached_projects enable row level security;
create policy viewer on public.session_reached_projects for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_reached_projects.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_reached_projects.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_reached_projects.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_reached_projects.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.session_owners enable row level security;
create policy viewer on public.session_owners for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_owners.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_owners.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_owners.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_owners.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.session_titles enable row level security;
create policy viewer on public.session_titles for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_titles.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_titles.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (session_titles.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((session_titles.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.user_agent_state enable row level security;
create policy viewer on public.user_agent_state for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_state.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_state.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_state.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_state.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.user_agent_read enable row level security;
create policy viewer on public.user_agent_read for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_read.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_read.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_read.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_read.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.user_agent_reply_read enable row level security;
create policy viewer on public.user_agent_reply_read for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_reply_read.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_reply_read.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_reply_read.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_reply_read.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.user_agent_pin enable row level security;
create policy viewer on public.user_agent_pin for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_pin.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_pin.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (user_agent_pin.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((user_agent_pin.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.delivery_session_group_members enable row level security;
create policy viewer on public.delivery_session_group_members for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (delivery_session_group_members.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((delivery_session_group_members.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or /* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (delivery_session_group_members.session_id) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((delivery_session_group_members.session_id), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false));

alter table public.route_roster enable row level security;
create policy viewer on public.route_roster for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()));

alter table public.route_roster_session_projects enable row level security;
create policy viewer on public.route_roster_session_projects for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()));
