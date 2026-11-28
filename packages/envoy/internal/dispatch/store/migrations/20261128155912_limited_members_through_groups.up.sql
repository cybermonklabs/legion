-- 20261128155912_limited_members_through_groups.up.sql — a group admits a limited member as a direct listing does.
--
-- store.PersonSeesProjectSQL admitted a member of a group a project's list names only through a
-- people row of a full member, so a limited member (member kind restricted), or a login nobody had
-- signed in as, was never admitted by a group. The rule now reads the group's members alone
-- (access_group_members.person), with no people row: a limited member is admitted by a group as by
-- a direct listing, and a member nobody has signed in as yet from the moment they sign in.
--
-- session_content_visible (migration 20261121192718) embeds that rule twice: in the open viewer's
-- test of the private projects each person tied to a session sees, and in the limited viewer's
-- test that its credential sees every project its person sees. The replacement drops the group
-- arm's join to people in both; every other arm is the function as migration 20261121192718 wrote
-- it. It stays an invoker's function with no search_path, naming every table by its schema.
-- record_project_made_private (migration 20261121200500) reads no person's rule, so it stays.
--
-- Its body is access.sessionContentFunctionBody as that renders it:
-- access.TestTheContentFunctionIsTheBodyGoRenders fails while the function a migrated database
-- holds differs from that text.
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
			where listed.project_key = seen.key and listed.principal_kind = 'group' and member.person = $2::jsonb->>'person'
			offset 0)
		or (seen.visibility = 'public' and viewer_person.member_kind = 'full'))
				offset 0))
			end, false)
$$;
