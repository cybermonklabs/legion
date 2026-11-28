-- 20261128165634_row_security_events.up.sql — row security on events and the rows of an event (classes E and EC of plan D4, INFRA-6168).
--
-- Class E: an event of a project passes by the project rule, one of an issue or a document where
-- that issue or document passes its own row security. An event of no owner is one of three kinds
-- (events.eventOwnerKey): a priority's write (priority.updated) and a group's write
-- (access_group.changed), which belong to no project and pass an open viewer, and a
-- session-addressed message or viewer state, whose session (its payload's session target, else its
-- session_id) passes access.SessionContentClause. Class EC: a row through the event it names.
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

alter table public.events enable row level security;
create policy viewer on public.events for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when events.project_key is not null then (events.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(events.project_key))
		when events.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = events.issue_key)
		when events.artifact_id is not null then exists (select 1 from public.artifacts seen_document where seen_document.id = events.artifact_id)
		when events.type in ('priority.updated', 'access_group.changed') then (select dispatch_viewer_open())
		else /* project access: SessionContentClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or ((case when events.payload->>'target' like 'session:%' then substr(events.payload->>'target', 9) else events.payload->>'session_id' end)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_content_visible(((case when events.payload->>'target' like 'session:%' then substr(events.payload->>'target', 9) else events.payload->>'session_id' end)), (select dispatch_viewer())::jsonb)), false) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when events.project_key is not null then (events.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(events.project_key))
		when events.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = events.issue_key)
		when events.artifact_id is not null then exists (select 1 from public.artifacts seen_document where seen_document.id = events.artifact_id)
		when events.type in ('priority.updated', 'access_group.changed') then (select dispatch_viewer_open())
		else /* project access: SessionContentClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or ((case when events.payload->>'target' like 'session:%' then substr(events.payload->>'target', 9) else events.payload->>'session_id' end)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_content_visible(((case when events.payload->>'target' like 'session:%' then substr(events.payload->>'target', 9) else events.payload->>'session_id' end)), (select dispatch_viewer())::jsonb)), false) end));

alter table public.project_access_changes enable row level security;
create policy viewer on public.project_access_changes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = project_access_changes.event_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = project_access_changes.event_id));

alter table public.outbox_role_copy_expiries enable row level security;
create policy viewer on public.outbox_role_copy_expiries for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = outbox_role_copy_expiries.event_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = outbox_role_copy_expiries.event_id));

alter table public.outbox_role_undelivered_notes enable row level security;
create policy viewer on public.outbox_role_undelivered_notes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = outbox_role_undelivered_notes.event_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.events seen_event where seen_event.id = outbox_role_undelivered_notes.event_id));
