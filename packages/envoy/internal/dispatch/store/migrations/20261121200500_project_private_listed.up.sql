-- 20261121200500_project_private_listed.up.sql — a switch to private records only the listed sessions.
--
-- record_project_made_private (migration 20261121173500) recorded, at a switch of a project from
-- public to private, the project as reached (session_reached_projects) for every session tied to
-- a person who saw it before the switch and does not after it. At such a switch every person who
-- sees the project by its owner, its list or a group its list names sees it after as before; the
-- people who lose it are every full member who saw it only because it was public. So the pass hid
-- every session of every such member from every viewer the list does not name, the member's own
-- browser included, for what those sessions read while the project was public to all of them.
--
-- A switch to private hides what is written in the project from the switch on; it does not
-- withdraw what sessions read while the project was public. The replacement keeps the pass for
-- every session whose stored listener list names the project (route_roster_session_projects):
-- that session's credential still reads it after the switch, so its transcript carries the
-- project's private content from the switch on. It drops the pass over the people tied to the
-- sessions, and with it the session-owner locks that pass took: it takes no lock of its own.
--
-- Its body is access.projectMadePrivateFunctionBody as that renders it:
-- access.TestTheMadePrivateTriggerIsTheBodyGoRenders fails while the function a migrated database
-- holds differs from that text. It names every table by its schema and sets no search_path.
--
-- This replaces a function. It neither reads, rewrites nor refuses any row, and the census
-- answers 0. Pairs the function it replaces wrote stay. The release before this one records the
-- tied sessions' pairs too; rollback is an image rollback, which leaves this function in place,
-- recording fewer pairs than that release would.
create or replace function public.record_project_made_private() returns trigger language plpgsql as $$
begin
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
