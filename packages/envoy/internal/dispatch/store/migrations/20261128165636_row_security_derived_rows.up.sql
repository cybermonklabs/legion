-- 20261128165636_row_security_derived_rows.up.sql — row security on embeddings, a going request's rows and check result retractions (classes EM, GR and CR of plan D4, INFRA-6168).
--
-- Class EM: an embedding with a project passes by the project rule; one with none (a window of a
-- message on no issue, or of a conversation file's comment) through the row it embeds, by its kind
-- and id, under that row's own row security; an embedding window state through the row its
-- owner_key names. Class GR: a going request's events and posts through the request. Class CR: a
-- retraction through the check result it retracts.
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

alter table public.embeddings enable row level security;
create policy viewer on public.embeddings for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when embeddings.project_key is not null then (embeddings.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(embeddings.project_key))
		else case embeddings.kind when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = embeddings.id)
			when 'document' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'document_block' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when split_part(embeddings.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embeddings.id, '#', 1))::uuid end))
			when 'comment_block' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			when 'message_block' then exists (select 1 from public.messages seen_message where seen_message.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			when 'ask_part' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			else false end end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when embeddings.project_key is not null then (embeddings.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(embeddings.project_key))
		else case embeddings.kind when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = embeddings.id)
			when 'document' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when embeddings.id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embeddings.id)::uuid end))
			when 'document_block' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when split_part(embeddings.id, '#', 1) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embeddings.id, '#', 1))::uuid end))
			when 'comment_block' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			when 'message_block' then exists (select 1 from public.messages seen_message where seen_message.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			when 'ask_part' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when split_part(split_part(embeddings.id, '#', 1), '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(split_part(embeddings.id, '#', 1), '/', 2))::uuid end))
			else false end end));

alter table public.embedding_window_states enable row level security;
create policy viewer on public.embedding_window_states for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case embedding_window_states.kind when 'document_block' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when embedding_window_states.owner_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embedding_window_states.owner_key)::uuid end))
			when 'comment_block' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			when 'message_block' then exists (select 1 from public.messages seen_message where seen_message.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			when 'ask_part' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			else false end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case embedding_window_states.kind when 'document_block' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when embedding_window_states.owner_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (embedding_window_states.owner_key)::uuid end))
			when 'comment_block' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			when 'message_block' then exists (select 1 from public.messages seen_message where seen_message.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			when 'ask_part' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when split_part(embedding_window_states.owner_key, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (split_part(embedding_window_states.owner_key, '/', 2))::uuid end))
			else false end));

alter table public.going_events enable row level security;
create policy viewer on public.going_events for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.going_requests seen_request where seen_request.id = going_events.request_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.going_requests seen_request where seen_request.id = going_events.request_id));

alter table public.going_request_posts enable row level security;
create policy viewer on public.going_request_posts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.going_requests seen_request where seen_request.id = going_request_posts.request_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.going_requests seen_request where seen_request.id = going_request_posts.request_id));

alter table public.check_result_retractions enable row level security;
create policy viewer on public.check_result_retractions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.check_results seen_result where seen_result.id = check_result_retractions.result_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.check_results seen_result where seen_result.id = check_result_retractions.result_id));
