-- 20261121192718_session_content_restricted_credential.up.sql — a restricted member's own session with no list needs a credential that sees all its person sees.
--
-- session_content_visible (migration 20261121151307) passed a restricted member's own session whose
-- listener sent no list to any credential of that person that lists no project, which it took to be
-- the person's browser. A restricted member's credential is held to its list for every project,
-- public ones too, and one that lists none sees only the public projects the person is named on.
-- So such a token read a session that read a public project made private since, while the person
-- still sees it and no pair records it, and a token that lists some of the person's projects read
-- none of that person's sessions with no list, whatever it saw.
--
-- The replacement passes such a session only to a credential that sees every project the viewer's
-- person sees (the person's browser, or a token whose list names each): no session with no list,
-- which nothing bounds but its person's sight, is read by a credential that sees less than its
-- person. Every other arm is the function as migration 20261121151307 wrote it. A full member's
-- credential is held the same way by the open arm's people test, which asks every private project
-- of each person tied to the session, the viewer's own person among them, to be one the viewer sees.
--
-- Its body is access.sessionContentFunctionBody as that renders it:
-- access.TestTheContentFunctionIsTheBodyGoRenders fails while the function a migrated database
-- holds differs from that text. It declares no search_path and names each table by its schema, as
-- the function it replaces does.
--
-- This replaces a function. It neither reads, rewrites nor refuses any row, and the census answers
-- 0. The release before this one calls the function by the same name and arguments, so under an
-- image rollback it answers by this rule; rollback is an image rollback alone.
create or replace function session_content_visible(session_id text, viewer jsonb) returns boolean
language sql stable cost 0.0001
as $$
select coalesce(not exists (
				select 1 from public.session_projects unseen
				where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
				offset 0)
			and not exists (
				select 1 from public.session_reached_projects reached
				where reached.session_id = $1 and not (($2->'sees') ? reached.project_key)
				offset 0)
			and case when coalesce((select listener.projects from public.route_roster_session_projects listener join public.route_roster roster on roster.id = listener.id where listener.id = true and roster.expires_at > now()) ? $1, false) then
				((select listener.projects from public.route_roster_session_projects listener join public.route_roster roster on roster.id = listener.id where listener.id = true and roster.expires_at > now())->$1) <@ ($2->'sees')
				and ((coalesce(($2::jsonb->>'open')::boolean, false)
						and exists (select 1 from public.session_projects_filled offset 0)
						and (jsonb_array_length((select listener.projects from public.route_roster_session_projects listener join public.route_roster roster on roster.id = listener.id where listener.id = true and roster.expires_at > now())->$1) > 0
							or exists (select 1 from public.session_projects acted where acted.session_id = $1 offset 0)))
					or (not coalesce(($2::jsonb->>'open')::boolean, false) and (exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login = $2::jsonb->>'person'
				offset 0)
			and not exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login is distinct from $2::jsonb->>'person'
				offset 0))))
			else
				(coalesce(($2::jsonb->>'open')::boolean, false)
					and exists (select 1 from public.session_projects_filled offset 0)
					and exists (select 1 from public.session_owners tied where tied.session_id = $1 offset 0)
					and not exists (
						select 1 from public.session_owners tied
						join public.projects hidden on hidden.visibility = 'private' and not (($2->'sees') ? hidden.key)
						left join public.people tied_person on tied_person.email = tied.login
						where tied.session_id = $1 and /* project access: PersonSeesProjectSQL */ (hidden.owner = tied.login
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = hidden.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = hidden.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (hidden.visibility = 'public' and tied_person.member_kind = 'full'))
						offset 0))
				or (not coalesce(($2::jsonb->>'open')::boolean, false) and (exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login = $2::jsonb->>'person'
				offset 0)
			and not exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login is distinct from $2::jsonb->>'person'
				offset 0)) and not exists (
				select 1 from public.projects seen
				left join public.people viewer_person on viewer_person.email = $2::jsonb->>'person'
				where not (($2->'sees') ? seen.key) and /* project access: PersonSeesProjectSQL */ (seen.owner = $2::jsonb->>'person'
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = seen.key and listed.principal_kind = 'person' and listed.principal = $2::jsonb->>'person'
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = seen.key and listed.principal_kind = 'group' and member.person = $2::jsonb->>'person'
			offset 0)
		or (seen.visibility = 'public' and viewer_person.member_kind = 'full'))
				offset 0))
			end, false)
$$;
