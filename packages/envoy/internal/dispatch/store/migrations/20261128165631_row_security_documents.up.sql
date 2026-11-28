-- 20261128165631_row_security_documents.up.sql — row security on documents and the rows of a document (classes A and D of plan D4, INFRA-6168).
--
-- Class A: artifacts. An issue's document passes where its issue passes issues' row security, a
-- project's document by the project rule of its project_key, and a file of an agent's conversation
-- (session_id set), which no project owns, through access.PartyClause over its one session party,
-- which holds the session to access.SessionClause and access.SessionContentClause (the owner-run
-- session_clause_visible and session_content_visible). Class D: every row of a document through the
-- document it names, under artifacts' row security.
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

alter table public.artifacts enable row level security;
create policy viewer on public.artifacts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when artifacts.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = artifacts.issue_key)
		when artifacts.project_key is not null then (artifacts.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(artifacts.project_key))
		else /* project access: PartyClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false)
				or ((('person:' || ((select dispatch_viewer())::jsonb->>'person')) = any(array['session:' || artifacts.session_id])
						or ('session:' || ((select dispatch_viewer())::jsonb->>'session')) = any(array['session:' || artifacts.session_id])
						or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and (not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) or coalesce(cardinality(array['session:' || artifacts.session_id]), 0) > 0))
						or (not coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and coalesce(cardinality(array['session:' || artifacts.session_id]), 0) > 0 and not exists (
							select 1 from unnest(array['session:' || artifacts.session_id]) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest(array['session:' || artifacts.session_id]) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (substr(party, 9)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (substr(party, 9)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), (select dispatch_viewer())::jsonb)), false))
						offset 0))), false) end))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (case when artifacts.issue_key is not null then exists (select 1 from public.issues seen_issue where seen_issue.key = artifacts.issue_key)
		when artifacts.project_key is not null then (artifacts.project_key = any((select dispatch_viewer_sees())::text[]) or dispatch_sees_project(artifacts.project_key))
		else /* project access: PartyClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false)
				or ((('person:' || ((select dispatch_viewer())::jsonb->>'person')) = any(array['session:' || artifacts.session_id])
						or ('session:' || ((select dispatch_viewer())::jsonb->>'session')) = any(array['session:' || artifacts.session_id])
						or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and (not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) or coalesce(cardinality(array['session:' || artifacts.session_id]), 0) > 0))
						or (not coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and coalesce(cardinality(array['session:' || artifacts.session_id]), 0) > 0 and not exists (
							select 1 from unnest(array['session:' || artifacts.session_id]) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest(array['session:' || artifacts.session_id]) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (substr(party, 9)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), (select dispatch_viewer())::jsonb,
					case when coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false) then exists (
						select 1 from session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce(((select dispatch_viewer())::jsonb->>'all')::boolean, false) or (substr(party, 9)) = (select dispatch_viewer())::jsonb->>'session'
				or (coalesce(((select dispatch_viewer())::jsonb->>'open')::boolean, false) and not coalesce(((select dispatch_viewer())::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), (select dispatch_viewer())::jsonb)), false))
						offset 0))), false) end));

alter table public.artifact_versions enable row level security;
create policy viewer on public.artifact_versions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = artifact_versions.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = artifact_versions.artifact_id));

alter table public.doc_updates enable row level security;
create policy viewer on public.doc_updates for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_updates.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_updates.artifact_id));

alter table public.doc_snapshots enable row level security;
create policy viewer on public.doc_snapshots for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_snapshots.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_snapshots.artifact_id));

alter table public.doc_checkpoints enable row level security;
create policy viewer on public.doc_checkpoints for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_checkpoints.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_checkpoints.artifact_id));

alter table public.artifact_reviews enable row level security;
create policy viewer on public.artifact_reviews for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = artifact_reviews.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = artifact_reviews.artifact_id));

alter table public.doc_pending_authors enable row level security;
create policy viewer on public.doc_pending_authors for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_pending_authors.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_pending_authors.artifact_id));

alter table public.document_blocks enable row level security;
create policy viewer on public.document_blocks for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_blocks.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_blocks.artifact_id));

alter table public.block_refs enable row level security;
create policy viewer on public.block_refs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = block_refs.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = block_refs.artifact_id));

alter table public.document_block_versions enable row level security;
create policy viewer on public.document_block_versions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_block_versions.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_block_versions.artifact_id));

alter table public.document_block_texts enable row level security;
create policy viewer on public.document_block_texts for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_block_texts.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_block_texts.artifact_id));

alter table public.document_indexed_versions enable row level security;
create policy viewer on public.document_indexed_versions for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_indexed_versions.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_indexed_versions.artifact_id));

alter table public.document_edit_notices enable row level security;
create policy viewer on public.document_edit_notices for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_edit_notices.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_edit_notices.artifact_id));

alter table public.decision_keeps enable row level security;
create policy viewer on public.decision_keeps for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = decision_keeps.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = decision_keeps.artifact_id));

alter table public.doc_ask_block_authors enable row level security;
create policy viewer on public.doc_ask_block_authors for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_ask_block_authors.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_ask_block_authors.artifact_id));

alter table public.doc_conditional_edit_queue enable row level security;
create policy viewer on public.doc_conditional_edit_queue for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_conditional_edit_queue.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_conditional_edit_queue.artifact_id));

alter table public.document_search enable row level security;
create policy viewer on public.document_search for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_search.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = document_search.artifact_id));

alter table public.doc_closed_edit_sources enable row level security;
create policy viewer on public.doc_closed_edit_sources for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_closed_edit_sources.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = doc_closed_edit_sources.artifact_id));

alter table public.user_document_reads enable row level security;
create policy viewer on public.user_document_reads for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = user_document_reads.artifact_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.artifacts seen_document where seen_document.id = user_document_reads.artifact_id));
