-- 20261121173500_project_private_reached.up.sql — a project made private stays reached.
--
-- The live agent view relays a session's transcript, and a session's files and messages are read
-- by the same rule (access.SessionContentClause), which judges the session's people by what they
-- see now. A switch of a project from public to private takes it from every full member its list
-- does not name, while the transcripts of their sessions keep what those sessions read of it while
-- it was public. So this trigger records, at the commit of the transaction that made the switch, a
-- (session, project) pair in session_reached_projects, which the content rule reads as a project
-- the session acted in: for every session tied to a person (session_owners) who saw the project
-- before the switch and does not after it, and for every session whose stored listener list names
-- the project (route_roster_session_projects). It fires for the switch the server makes
-- (access.SetVisibility) and for one an SQL statement makes alone.
--
-- Its body is access.projectMadePrivateFunctionBody as that renders it:
-- access.TestTheMadePrivateTriggerIsTheBodyGoRenders fails while the function a migrated database
-- holds differs from that text. It takes each person's session-owner lock, exclusive, in login
-- order, as store.RecordReachedProjects does, and the trigger is deferred to commit, so those
-- locks are the last the switch takes. It names every table by its schema and sets no search_path.
-- A pair is written only where no row holds it, and nothing removes one.
--
-- This creates a function and a trigger. It neither rewrites nor refuses any existing row, and the
-- census answers 0. The release before this one makes no pair at a switch; rollback is an image
-- rollback, which leaves the trigger writing pairs that release does not read.
create function public.record_project_made_private() returns trigger language plpgsql as $$

begin
	perform count(pg_advisory_xact_lock(hashtext('dispatch-session-owner'), hashtext(lost.login)))
	from (select distinct tied.login from public.session_owners tied
		left join public.people tied_person on tied_person.email = tied.login
		where coalesce(/* project access: PersonSeesProjectSQL */ (old.owner = tied.login
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = old.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = old.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (old.visibility = 'public' and coalesce(tied_person.member_kind, 'full') = 'full')), false)
			and not coalesce(/* project access: PersonSeesProjectSQL */ (new.owner = tied.login
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = new.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = new.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (new.visibility = 'public' and coalesce(tied_person.member_kind, 'full') = 'full')), false)
		order by tied.login offset 0) as lost;
	insert into public.session_reached_projects (session_id, project_key)
	select distinct tied.session_id, new.key from public.session_owners tied
		left join public.people tied_person on tied_person.email = tied.login
		where coalesce(/* project access: PersonSeesProjectSQL */ (old.owner = tied.login
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = old.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = old.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (old.visibility = 'public' and coalesce(tied_person.member_kind, 'full') = 'full')), false)
			and not coalesce(/* project access: PersonSeesProjectSQL */ (new.owner = tied.login
		or exists (
			select 1 from public.project_access listed
			where listed.project_key = new.key and listed.principal_kind = 'person' and listed.principal = tied.login
			offset 0)
		or exists (
			select 1
			from public.project_access listed
			join public.access_group_members member on member.group_name = listed.principal
			join public.people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = new.key and listed.principal_kind = 'group' and member.person = tied.login
			offset 0)
		or (new.visibility = 'public' and coalesce(tied_person.member_kind, 'full') = 'full')), false)
		and not exists (
			select 1 from public.session_reached_projects recorded
			where recorded.session_id = tied.session_id and recorded.project_key = new.key);
	insert into public.session_reached_projects (session_id, project_key)
	select distinct held.session, new.key
	from public.route_roster_session_projects listener
	cross join lateral jsonb_each(listener.projects) as held (session, projects)
	where listener.id = true and held.session <> ''
		and jsonb_typeof(held.projects) = 'array' and held.projects ? new.key
		and not exists (
			select 1 from public.session_reached_projects recorded
			where recorded.session_id = held.session and recorded.project_key = new.key);
	return null;
end
$$;

create constraint trigger projects_made_private_reached
    after update of visibility on public.projects
    deferrable initially deferred
    for each row
    when (old.visibility = 'public' and new.visibility = 'private')
    execute function public.record_project_made_private();
