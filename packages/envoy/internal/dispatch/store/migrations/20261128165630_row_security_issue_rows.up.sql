-- 20261128165630_row_security_issue_rows.up.sql — row security on the rows of an issue, of two issues, and of an issue or a document (classes I, L and O of plan D4, INFRA-6168).
--
-- Class I: a row belongs to the issue it names, and passes where the issue passes issues' own row
-- security (an exists over issues, read under the request role). write_suggestions goes through its
-- source ask, issue or document, whichever is set; pull_request_heads through its issue, else the
-- repository rule (dispatch_repo_visible). Class L: issue_links through both its issues. Class O:
-- an ask, a comment, a comment watermark and a search queue row through the issue it names, else
-- the document; a queue row of a message on no issue, which names neither, through the message.
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

alter table public.issue_external_links enable row level security;
create policy viewer on public.issue_external_links for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_external_links.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_external_links.issue_key));

alter table public.issue_components enable row level security;
create policy viewer on public.issue_components for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_components.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_components.issue_key));

alter table public.handoffs enable row level security;
create policy viewer on public.handoffs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = handoffs.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = handoffs.issue_key));

alter table public.design_gates enable row level security;
create policy viewer on public.design_gates for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = design_gates.issue))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = design_gates.issue));

alter table public.issue_role_holders enable row level security;
create policy viewer on public.issue_role_holders for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_role_holders.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = issue_role_holders.issue_key));

alter table public.check_results enable row level security;
create policy viewer on public.check_results for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = check_results.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = check_results.issue_key));

alter table public.write_suggestions enable row level security;
create policy viewer on public.write_suggestions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when write_suggestions.source_ask_id is not null then exists (select 1 from public.asks seen_ask where seen_ask.id = write_suggestions.source_ask_id)
		when write_suggestions.source_issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = write_suggestions.source_issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = write_suggestions.source_artifact_id) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when write_suggestions.source_ask_id is not null then exists (select 1 from public.asks seen_ask where seen_ask.id = write_suggestions.source_ask_id)
		when write_suggestions.source_issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = write_suggestions.source_issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = write_suggestions.source_artifact_id) end));

alter table public.embedding_issue_sources enable row level security;
create policy viewer on public.embedding_issue_sources for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = embedding_issue_sources.key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = embedding_issue_sources.key));

alter table public.user_issue_state enable row level security;
create policy viewer on public.user_issue_state for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = user_issue_state.issue_key))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.issues seen_issue where seen_issue.key = user_issue_state.issue_key));

alter table public.pull_request_heads enable row level security;
create policy viewer on public.pull_request_heads for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when pull_request_heads.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = pull_request_heads.issue_key)
		else dispatch_repo_visible(pull_request_heads.repo) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when pull_request_heads.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = pull_request_heads.issue_key)
		else dispatch_repo_visible(pull_request_heads.repo) end));

alter table public.issue_links enable row level security;
create policy viewer on public.issue_links for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (exists (select 1 from public.issues seen_issue where seen_issue.key = issue_links.issue_key)
		and exists (select 1 from public.issues seen_issue where seen_issue.key = issue_links.target_key)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (exists (select 1 from public.issues seen_issue where seen_issue.key = issue_links.issue_key)
		and exists (select 1 from public.issues seen_issue where seen_issue.key = issue_links.target_key)));

alter table public.asks enable row level security;
create policy viewer on public.asks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when asks.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = asks.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = asks.artifact_id) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when asks.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = asks.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = asks.artifact_id) end));

alter table public.comments enable row level security;
create policy viewer on public.comments for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when comments.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = comments.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = comments.artifact_id) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when comments.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = comments.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = comments.artifact_id) end));

alter table public.comment_watermarks enable row level security;
create policy viewer on public.comment_watermarks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when comment_watermarks.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = comment_watermarks.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = comment_watermarks.artifact_id) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when comment_watermarks.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = comment_watermarks.issue_key)
		else exists (select 1 from public.artifacts seen_document where seen_document.id = comment_watermarks.artifact_id) end));

alter table public.search_term_queue enable row level security;
create policy viewer on public.search_term_queue for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when search_term_queue.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = search_term_queue.issue_key)
		when search_term_queue.artifact_id is not null then exists (select 1 from public.artifacts seen_document where seen_document.id = search_term_queue.artifact_id)
		when search_term_queue.kind = 'message' then exists (select 1 from public.messages seen_message where seen_message.id = search_term_queue.item_id)
		else false end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when search_term_queue.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = search_term_queue.issue_key)
		when search_term_queue.artifact_id is not null then exists (select 1 from public.artifacts seen_document where seen_document.id = search_term_queue.artifact_id)
		when search_term_queue.kind = 'message' then exists (select 1 from public.messages seen_message where seen_message.id = search_term_queue.item_id)
		else false end));
