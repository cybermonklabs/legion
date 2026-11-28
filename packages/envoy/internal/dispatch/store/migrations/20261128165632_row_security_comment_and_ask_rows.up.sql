-- 20261128165632_row_security_comment_and_ask_rows.up.sql — row security on the rows of a comment, of an ask, and of a post's edit history (classes C, K and PB of plan D4, INFRA-6168).
--
-- Class C: a row through the comment it names, under comments' row security. Class K: through the
-- ask it names, under asks'. Class PB: post_body_history through its comment or its message,
-- whichever is set.
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

alter table public.comment_mentions enable row level security;
create policy viewer on public.comment_mentions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_mentions.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_mentions.comment_id));

alter table public.comment_deliveries enable row level security;
create policy viewer on public.comment_deliveries for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_deliveries.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_deliveries.comment_id));

alter table public.comment_blocks enable row level security;
create policy viewer on public.comment_blocks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_blocks.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_blocks.comment_id));

alter table public.comment_block_texts enable row level security;
create policy viewer on public.comment_block_texts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_texts.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_texts.comment_id));

alter table public.comment_block_state enable row level security;
create policy viewer on public.comment_block_state for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_state.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_state.comment_id));

alter table public.comment_block_refs enable row level security;
create policy viewer on public.comment_block_refs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_refs.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_block_refs.comment_id));

alter table public.comment_fallback_copies enable row level security;
create policy viewer on public.comment_fallback_copies for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_fallback_copies.comment_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.comments seen_comment where seen_comment.id = comment_fallback_copies.comment_id));

alter table public.ask_followers enable row level security;
create policy viewer on public.ask_followers for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_followers.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_followers.ask_id));

alter table public.ask_blocks enable row level security;
create policy viewer on public.ask_blocks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_blocks.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_blocks.ask_id));

alter table public.ask_block_texts enable row level security;
create policy viewer on public.ask_block_texts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_texts.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_texts.ask_id));

alter table public.ask_block_state enable row level security;
create policy viewer on public.ask_block_state for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_state.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_state.ask_id));

alter table public.ask_block_refs enable row level security;
create policy viewer on public.ask_block_refs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_refs.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = ask_block_refs.ask_id));

alter table public.user_ask_snooze enable row level security;
create policy viewer on public.user_ask_snooze for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = user_ask_snooze.ask_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.asks seen_ask where seen_ask.id = user_ask_snooze.ask_id));

alter table public.post_body_history enable row level security;
create policy viewer on public.post_body_history for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when post_body_history.comment_id is not null then exists (select 1 from public.comments seen_comment where seen_comment.id = post_body_history.comment_id)
		else exists (select 1 from public.messages seen_message where seen_message.id = post_body_history.message_id) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when post_body_history.comment_id is not null then exists (select 1 from public.comments seen_comment where seen_comment.id = post_body_history.comment_id)
		else exists (select 1 from public.messages seen_message where seen_message.id = post_body_history.message_id) end));
