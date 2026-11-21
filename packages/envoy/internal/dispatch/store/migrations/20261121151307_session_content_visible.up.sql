-- 20261121151307_session_content_visible.up.sql — the session content rule's per-row decision.
--
-- access.SessionContentClause is the rule of a session's content: the transcript the live agent
-- view relays, and, through access.PartyClause, every conversation the session is a party of - a
-- file of its conversation and that file's node, an ask or a comment on such a file, a direct
-- message and the replies under it - since what a session writes can hold whatever it read. So
-- every graph read and every read of a direct message binds it. Written into each statement as its
-- own SQL, the rule's subqueries moved the backlink count's block-ref arms off the indexes they are
-- written for and read projects by a sequential scan once per row
-- (refs.TestBacklinkCountStatementReadsEveryArmThroughAnIndex). As SessionClause does with
-- session_clause_visible, the clause now answers the server's viewer, the viewer's own session and
-- an open viewer of a deployment that holds no private project in its own SQL, and asks this
-- function about every other row. Postgres never inlines it, since its body reads tables, and runs
-- it once per row it asks about; its declared cost, 0.0001, is what keeps the statements that bind
-- the clause on their indexes.
--
-- Its body is access.sessionContentFunctionBody as that renders it, the rule SessionContentClause's
-- comment states over the session ($1) and the visibility argument ($2):
-- access.TestTheContentFunctionIsTheBodyGoRenders fails while the function a migrated database holds
-- differs from that text, so a change of the rule ships with a migration that replaces this
-- function. It declares no search_path, which would save and restore the setting on every call.
--
-- This creates a function. It neither reads, rewrites nor refuses any row. The census answers 0,
-- and rollback is an image rollback alone: the release before this one does not call it.
create function session_content_visible(session_id text, viewer jsonb) returns boolean
language sql stable cost 0.0001
as $$
select coalesce(not exists (
				select 1 from session_projects unseen
				where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
				offset 0)
			and not exists (
				select 1 from session_reached_projects reached
				where reached.session_id = $1 and not (($2->'sees') ? reached.project_key)
				offset 0)
			and case when coalesce((select listener.projects from route_roster_session_projects listener where listener.id = true) ? $1, false) then
				((select listener.projects from route_roster_session_projects listener where listener.id = true)->$1) <@ ($2->'sees')
				and ((coalesce(($2::jsonb->>'open')::boolean, false)
						and exists (select 1 from session_projects_filled offset 0)
						and (jsonb_array_length((select listener.projects from route_roster_session_projects listener where listener.id = true)->$1) > 0
							or exists (select 1 from session_projects acted where acted.session_id = $1 offset 0)))
					or (not coalesce(($2::jsonb->>'open')::boolean, false) and (exists (
				select 1 from session_owners tied
				where tied.session_id = $1 and tied.login = $2::jsonb->>'person'
				offset 0)
			and not exists (
				select 1 from session_owners tied
				where tied.session_id = $1 and tied.login is distinct from $2::jsonb->>'person'
				offset 0))))
			else
				(coalesce(($2::jsonb->>'open')::boolean, false)
					and exists (select 1 from session_projects_filled offset 0)
					and exists (select 1 from session_owners tied where tied.session_id = $1 offset 0)
					and not exists (
						select 1 from session_owners tied
						join projects hidden on hidden.visibility = 'private' and not (($2->'sees') ? hidden.key)
						left join people tied_person on tied_person.email = tied.login
						where tied.session_id = $1 and /* project access: PersonSeesProjectSQL */ (hidden.owner = tied.login
		or exists (
			select 1 from project_access listed
			where listed.project_key = hidden.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from project_access listed
			join access_group_members member on member.group_name = listed.principal
			join people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = hidden.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (hidden.visibility = 'public' and tied_person.member_kind = 'full'))
						offset 0))
				or (not coalesce(($2::jsonb->>'open')::boolean, false) and (exists (
				select 1 from session_owners tied
				where tied.session_id = $1 and tied.login = $2::jsonb->>'person'
				offset 0)
			and not exists (
				select 1 from session_owners tied
				where tied.session_id = $1 and tied.login is distinct from $2::jsonb->>'person'
				offset 0)))
			end, false)
$$;
