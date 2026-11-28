-- 20261128165633_row_security_messages.up.sql — row security on messages, the rows of a message, and broadcasts (classes M, MC and B of plan D4, INFRA-6168).
--
-- Class M: a message on an issue passes where its issue passes issues' row security, and one on no
-- issue through dispatch_message_visible: access.PartyClause over its conversation's parties, its
-- node's or, until the node is written, those its thread root's author and target give it. Class
-- MC: a row through the message it names. Class B: a broadcast through dispatch_broadcast_visible,
-- access.BroadcastClause as the owner reads it, so a copy to a session the viewer may not read
-- refuses the broadcast whatever the viewer's own row security hides.
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

alter table public.messages enable row level security;
create policy viewer on public.messages for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when messages.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = messages.issue_key)
		else dispatch_message_visible(messages.id, messages.author, messages.target, messages.in_reply_to) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when messages.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = messages.issue_key)
		else dispatch_message_visible(messages.id, messages.author, messages.target, messages.in_reply_to) end));

alter table public.message_deliveries enable row level security;
create policy viewer on public.message_deliveries for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_deliveries.message_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_deliveries.message_id));

alter table public.message_blocks enable row level security;
create policy viewer on public.message_blocks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_blocks.message_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_blocks.message_id));

alter table public.message_block_state enable row level security;
create policy viewer on public.message_block_state for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_block_state.message_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_block_state.message_id));

alter table public.message_block_refs enable row level security;
create policy viewer on public.message_block_refs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_block_refs.message_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = message_block_refs.message_id));

alter table public.user_agent_thread_dismissed enable row level security;
create policy viewer on public.user_agent_thread_dismissed for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = user_agent_thread_dismissed.message_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.messages seen_message where seen_message.id = user_agent_thread_dismissed.message_id));

alter table public.broadcasts enable row level security;
create policy viewer on public.broadcasts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_broadcast_visible(broadcasts.id, broadcasts.author))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_broadcast_visible(broadcasts.id, broadcasts.author));
