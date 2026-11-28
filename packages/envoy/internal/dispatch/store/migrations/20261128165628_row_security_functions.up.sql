-- 20261128165628_row_security_functions.up.sql — the readers of the viewer settings, the owner-run project and repository rules, and the owner-run rule and trigger functions (INFRA-6168).
--
-- Row security (the migrations after this one) decides each row a request reads or writes by the
-- viewer the request pool's settings statement sends at the start of every viewer transaction:
-- dispatch.viewer_sees (the projects the viewer sees, comma-joined), dispatch.viewer_person,
-- dispatch.viewer_open, dispatch.viewer_session and dispatch.viewer_private, and role dispatch_app.
-- Each setting has one reader here, stable, reading it with nullif(current_setting(name, true), '')
-- and failing closed where it is unset or '' (a connection reused after a viewer's transaction
-- reads ''): no project seen, not open, no person, no session, and a deployment read as one that
-- holds a private project, which takes no fast path. dispatch_request_role says whether the
-- statement runs as dispatch_app, the one role every policy holds to its predicate, and
-- dispatch_viewer is the viewer in the shape the access clauses read ({"person", "session",
-- "open", "sees", "private"}, with "all" false). Policies and the owner-run functions read the
-- viewer through these alone. A later rename of a setting adds the new name first: two builds read
-- these while a release rolls.
--
-- dispatch_sees_project(project) is the project rule every policy but projects' asks: the viewer
-- sees the project, or the viewer is open and the project is public, so an open viewer sees every
-- public project, one made in its own request among them, and a limited member, who is never open,
-- sees only the projects it is listed on. It probes projects by its primary key as its owner
-- (security definer), so no policy reads projects under row security and none recurses.
-- dispatch_repo_visible(repo) is the repository rule, of the delivery tables' policies and of the
-- GitHub proxy: an open viewer of a deployment with no private project reads every repository;
-- otherwise a viewer reads a repository an issue of a project it sees resolves to (an issue's own
-- repository, an ancestor's, or its project's default repository where an issue names none), and
-- an open viewer one that no issue of any project resolves to. Both read every project and issue
-- as their owner and answer only whether the viewer may read the row.
--
-- Four more owner-run rules decide the rows whose rule reads another policy's table, so no
-- policy reads such a table under row security (two policies that read each other's tables would
-- recurse) and no rule skips a row the viewer may not read. Each starts with the R5 fast path.
-- dispatch_node_visible is a graph node's rule by its kind (refs' node scopes): an issue, a
-- document, an ask, a comment, a message or a document's block by its project (the project rule),
-- else its parties (access.PartyClause), else no viewer; an ask's, a comment's or a message's block
-- by its item's node; a component by the project its address names; a person, a session and a
-- role by access.PersonClause, access.SessionClause and access.RoleClause over the node's id (the
-- role's reads its held_by tuples as the owner); a page by its session's or its role's rule; the
-- block schema's own nodes (supertag, attribute) to every viewer that is set - open, or with a
-- person, a session or any project it sees - and to no transaction that set no viewer, which
-- reads every reader's fail-closed default; and a kind it does not know to
-- no viewer. dispatch_tuple_end_visible is that rule over the node an end of a tuple names, read
-- as the owner. Both are declared at a high cost, and each policy that calls them tests the fast
-- path first, so a deployment with no private project calls neither for any row.
-- dispatch_broadcast_visible is access.BroadcastClause, which reads the broadcast and every copy
-- of it as the owner; a broadcast its own insert writes, which no row holds yet and no copy
-- follows, passes an open viewer and its sender, as the clause would once it is stored.
-- dispatch_workflow_node_visible judges a workflow node by the issue it is part of, its issue
-- attribute or else its part_of tuple, read as the owner, through the project rule; a node that
-- names no issue yet (the store inserts it with no attribute and states its issue in the next
-- statement of the same transaction) passes only the viewer whose session or person made it.
-- dispatch_message_visible judges a message on no issue by its conversation's parties
-- (access.PartyClause): its node's, read as the owner, else those graphstore.ThreadParties gives
-- it from its thread root's author and target (read up in_reply_to as the owner), or its own
-- where it answers nothing, so a message whose node is not written yet passes its writer and the
-- viewers its parties admit, and no other.
-- dispatch_repo_visible follows refs.IssueRepoTerms: an issue resolves to the repository the
-- nearest issue of its own project on its parent chain names, else its project's default
-- repository, so every repository an issue of a project names is one an issue of that project
-- resolves to, and the project's default repository is read as one too. An open viewer also reads
-- a repository no issue of any project resolves to: no issue names it, and no issue of a project
-- whose default it is has a chain (within that project) that names none.
--
-- The functions a policy calls, and the trigger functions that read or write rows of another scope,
-- run as their owner: session_clause_visible and session_content_visible (which access.SessionClause
-- and access.SessionContentClause ask, so the policies that bind them), record_project_made_private
-- and sync_route_roster_session_projects (which record reached projects of sessions the writer does
-- not see), projects_session_projects_swept (which sweeps every session's pairs of a deleted
-- project), and user_threads_record (whose reply moves every other participant's row). Each is
-- replaced as security definer with search_path pinned to pg_catalog, public, so a caller's
-- search_path cannot hand it a table or a function of another schema, and names every table by its
-- schema. Each body is the one it replaces: session_content_visible's and
-- record_project_made_private's are what access.sessionContentFunctionBody and
-- access.projectMadePrivateFunctionBody render (access.TestTheContentFunctionIsTheBodyGoRenders,
-- TestTheMadePrivateTriggerIsTheBodyGoRenders), and projects_session_projects_swept's and
-- user_threads_record's name their tables and user_threads_login by schema.
-- store.TestEveryTriggerAndFunctionDeclaresHowItRunsUnderTheRequestRole holds every function of
-- schema public to how it runs.
--
-- This makes and replaces functions. It neither reads, rewrites nor refuses any row, and the census
-- answers 0. The release before this one names none of the new functions and calls the replaced
-- ones by the same names and arguments, and as their owner each answers it as before, since the
-- owner meets no policy; rollback is an image rollback alone.

create function public.dispatch_viewer_sees() returns text[]
language sql stable parallel safe
as $$
	select coalesce(string_to_array(nullif(current_setting('dispatch.viewer_sees', true), ''), ','), '{}'::text[])
$$;

create function public.dispatch_viewer_open() returns boolean
language sql stable parallel safe
as $$
	select coalesce(nullif(current_setting('dispatch.viewer_open', true), '')::boolean, false)
$$;

create function public.dispatch_viewer_person() returns text
language sql stable parallel safe
as $$
	select nullif(current_setting('dispatch.viewer_person', true), '')
$$;

create function public.dispatch_viewer_session() returns text
language sql stable parallel safe
as $$
	select nullif(current_setting('dispatch.viewer_session', true), '')
$$;

create function public.dispatch_viewer_private() returns boolean
language sql stable parallel safe
as $$
	select coalesce(nullif(current_setting('dispatch.viewer_private', true), '')::boolean, true)
$$;

create function public.dispatch_request_role() returns boolean
language sql stable parallel safe
as $$
	select current_user = 'dispatch_app'
$$;

create function public.dispatch_viewer() returns jsonb
language sql stable parallel safe
as $$
	select jsonb_build_object(
		'person', public.dispatch_viewer_person(),
		'session', public.dispatch_viewer_session(),
		'open', public.dispatch_viewer_open(),
		'sees', to_jsonb(public.dispatch_viewer_sees()),
		'private', public.dispatch_viewer_private(),
		'all', false)
$$;

create function public.dispatch_sees_project(project text) returns boolean
language sql stable parallel safe security definer set search_path = pg_catalog, public
as $$
	select coalesce($1 = any(public.dispatch_viewer_sees())
		or (public.dispatch_viewer_open() and exists (
			select 1 from public.projects seen
			where seen.key = $1 and seen.visibility = 'public')), false)
$$;

create function public.dispatch_repo_visible(repo text) returns boolean
language sql stable parallel safe security definer set search_path = pg_catalog, public
as $$
	select coalesce($1 is not null and (
		(public.dispatch_viewer_open() and not public.dispatch_viewer_private())
		or exists (
			select 1 from public.projects seen
			where (seen.key = any(public.dispatch_viewer_sees())
					or (public.dispatch_viewer_open() and seen.visibility = 'public'))
				and (seen.default_repo = $1 or exists (
					select 1 from public.issues resolved
					where resolved.repo = $1 and resolved.project_key = seen.key)))
		or (public.dispatch_viewer_open()
			and not exists (select 1 from public.issues resolved where resolved.repo = $1)
			and not exists (
				with recursive chain as (
					select i.key as start, i.parent_key, i.repo, i.project_key, 0 as depth
					from public.issues i join public.projects defaulting
						on defaulting.key = i.project_key and defaulting.default_repo = $1
					union all
					select c.start, p.parent_key, p.repo, c.project_key, c.depth + 1
					from chain c join public.issues p on p.key = c.parent_key and p.project_key = c.project_key
					where c.repo is null and c.depth < 32)
				select 1 from chain c group by c.start having bool_and(c.repo is null)))), false)
$$;

create function public.dispatch_node_visible(node_kind text, node_id text, node_project text, node_parties text[]) returns boolean
language sql stable cost 10000 security definer set search_path = pg_catalog, public
as $$
	select coalesce((public.dispatch_viewer_open() and not public.dispatch_viewer_private()) or case $1
		when 'issue' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'artifact' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'ask' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'comment' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'message' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'block' then (case when $3 is not null then public.dispatch_sees_project($3)
		when $4 is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or ((('person:' || (viewer.v::jsonb->>'person')) = any($4)
						or ('session:' || (viewer.v::jsonb->>'session')) = any($4)
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality($4), 0) > 0))
						or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality($4), 0) > 0 and not exists (
							select 1 from unnest($4) party
							where party not like 'session:%'
							offset 0)))
					and not exists (
						select 1 from unnest($4) party
						where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
						offset 0))), false)
		else false end)
		when 'ask_block' then coalesce((
			select (case when owner_item.project_key is not null then public.dispatch_sees_project(owner_item.project_key)
				when owner_item.parties is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
						or ((('person:' || (viewer.v::jsonb->>'person')) = any(owner_item.parties)
								or ('session:' || (viewer.v::jsonb->>'session')) = any(owner_item.parties)
								or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality(owner_item.parties), 0) > 0))
								or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality(owner_item.parties), 0) > 0 and not exists (
									select 1 from unnest(owner_item.parties) party
									where party not like 'session:%'
									offset 0)))
							and not exists (
								select 1 from unnest(owner_item.parties) party
								where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
							case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
								select 1 from public.session_projects_filled
								offset 0)
							else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
								offset 0))), false)
				else false end)
			from public.graph_nodes owner_item
			where owner_item.kind = split_part(split_part($2, '#', 1), '/', 1) and owner_item.id = split_part(split_part($2, '#', 1), '/', 2)), false)
		when 'comment_block' then coalesce((
			select (case when owner_item.project_key is not null then public.dispatch_sees_project(owner_item.project_key)
				when owner_item.parties is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
						or ((('person:' || (viewer.v::jsonb->>'person')) = any(owner_item.parties)
								or ('session:' || (viewer.v::jsonb->>'session')) = any(owner_item.parties)
								or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality(owner_item.parties), 0) > 0))
								or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality(owner_item.parties), 0) > 0 and not exists (
									select 1 from unnest(owner_item.parties) party
									where party not like 'session:%'
									offset 0)))
							and not exists (
								select 1 from unnest(owner_item.parties) party
								where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
							case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
								select 1 from public.session_projects_filled
								offset 0)
							else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
								offset 0))), false)
				else false end)
			from public.graph_nodes owner_item
			where owner_item.kind = split_part(split_part($2, '#', 1), '/', 1) and owner_item.id = split_part(split_part($2, '#', 1), '/', 2)), false)
		when 'message_block' then coalesce((
			select (case when owner_item.project_key is not null then public.dispatch_sees_project(owner_item.project_key)
				when owner_item.parties is not null then /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
						or ((('person:' || (viewer.v::jsonb->>'person')) = any(owner_item.parties)
								or ('session:' || (viewer.v::jsonb->>'session')) = any(owner_item.parties)
								or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality(owner_item.parties), 0) > 0))
								or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality(owner_item.parties), 0) > 0 and not exists (
									select 1 from unnest(owner_item.parties) party
									where party not like 'session:%'
									offset 0)))
							and not exists (
								select 1 from unnest(owner_item.parties) party
								where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
							case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
								select 1 from public.session_projects_filled
								offset 0)
							else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
						or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
						or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
								offset 0))), false)
				else false end)
			from public.graph_nodes owner_item
			where owner_item.kind = split_part(split_part($2, '#', 1), '/', 1) and owner_item.id = split_part(split_part($2, '#', 1), '/', 2)), false)
		when 'component' then public.dispatch_sees_project(split_part($2, '/', 1))
		when 'person' then /* project access: PersonClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or coalesce((viewer.v::jsonb->>'open')::boolean, false) or ($2) = viewer.v::jsonb->>'person'
				or exists (
					select 1 from jsonb_array_elements_text(viewer.v::jsonb->'sees') as sees (key)
					join public.projects seen on seen.key = sees.key
					where /* project access: PersonSeesProjectSQL */ (seen.owner = ($2)
				or exists (
					select 1 from public.project_access listed
					where listed.project_key = seen.key and listed.principal_kind = 'person' and listed.principal = ($2)
					offset 0)
				or exists (
					select 1
					from public.project_access listed
					join public.access_group_members member on member.group_name = listed.principal
					where listed.project_key = seen.key and listed.principal_kind = 'group' and member.person = ($2)
					offset 0)
				or (seen.visibility = 'public' and (select person_kind.member_kind from public.people person_kind where person_kind.email = ($2)) = 'full'))
					offset 0)), false)
		when 'session' then /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or ($2) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible(($2), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false)
		when 'role' then /* project access: RoleClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
				or (/* project access: RoleProjectClause */ coalesce((not (coalesce((viewer.v::jsonb->>'private')::boolean, false) and exists (
				select 1 from public.projects hidden
				where hidden.key = (translate(substring(($2) from '^legion-([a-z][a-z0-9]{1,9})-.'), 'abcdefghijklmnopqrstuvwxyz', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ')) and hidden.visibility = 'private'
					and not /* project access: ProjectClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (viewer.v::jsonb->'sees') ? (hidden.key)), false)
				offset 0))), false)
					and ((coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or not exists (
					select 1 from public.graph_tuples holder
					where holder.from_kind = 'role' and holder.from_id = ($2) and holder.kind = 'held_by'
						and holder.to_kind = 'session' and holder.asserted
						and not exists (
							select 1 from public.graph_tuples later
							where later.from_kind = holder.from_kind and later.from_id = holder.from_id
								and later.kind = holder.kind and later.to_kind = holder.to_kind
								and later.to_id = holder.to_id and later.seq > holder.seq
							offset 0)
						and not /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (holder.to_id) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((holder.to_id), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false)
					offset 0))) or exists (
					select 1 from public.graph_tuples holder
					where holder.from_kind = 'role' and holder.from_id = ($2) and holder.kind = 'held_by'
						and holder.to_kind = 'session' and holder.asserted
						and not exists (
							select 1 from public.graph_tuples later
							where later.from_kind = holder.from_kind and later.from_id = holder.from_id
								and later.kind = holder.kind and later.to_kind = holder.to_kind
								and later.to_id = holder.to_id and later.seq > holder.seq
							offset 0)
						and /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (holder.to_id) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((holder.to_id), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false)
					offset 0)))), false)
		when 'page' then case split_part($2, '/', 1)
			when 'session' then /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr($2, 9)) = viewer.v::jsonb->>'session'
					or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
					or session_clause_visible((substr($2, 9)), viewer.v::jsonb,
						case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
							select 1 from public.session_projects_filled
							offset 0)
						else true end)), false)
			when 'role' then /* project access: RoleClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
					or (/* project access: RoleProjectClause */ coalesce((not (coalesce((viewer.v::jsonb->>'private')::boolean, false) and exists (
					select 1 from public.projects hidden
					where hidden.key = (translate(substring((substr($2, 6)) from '^legion-([a-z][a-z0-9]{1,9})-.'), 'abcdefghijklmnopqrstuvwxyz', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ')) and hidden.visibility = 'private'
						and not /* project access: ProjectClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (viewer.v::jsonb->'sees') ? (hidden.key)), false)
					offset 0))), false)
						and ((coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or not exists (
						select 1 from public.graph_tuples holder
						where holder.from_kind = 'role' and holder.from_id = (substr($2, 6)) and holder.kind = 'held_by'
							and holder.to_kind = 'session' and holder.asserted
							and not exists (
								select 1 from public.graph_tuples later
								where later.from_kind = holder.from_kind and later.from_id = holder.from_id
									and later.kind = holder.kind and later.to_kind = holder.to_kind
									and later.to_id = holder.to_id and later.seq > holder.seq
								offset 0)
							and not /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (holder.to_id) = viewer.v::jsonb->>'session'
					or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
					or session_clause_visible((holder.to_id), viewer.v::jsonb,
						case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
							select 1 from public.session_projects_filled
							offset 0)
						else true end)), false)
						offset 0))) or exists (
						select 1 from public.graph_tuples holder
						where holder.from_kind = 'role' and holder.from_id = (substr($2, 6)) and holder.kind = 'held_by'
							and holder.to_kind = 'session' and holder.asserted
							and not exists (
								select 1 from public.graph_tuples later
								where later.from_kind = holder.from_kind and later.from_id = holder.from_id
									and later.kind = holder.kind and later.to_kind = holder.to_kind
									and later.to_id = holder.to_id and later.seq > holder.seq
								offset 0)
							and /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (holder.to_id) = viewer.v::jsonb->>'session'
					or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
					or session_clause_visible((holder.to_id), viewer.v::jsonb,
						case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
							select 1 from public.session_projects_filled
							offset 0)
						else true end)), false)
						offset 0)))), false)
			else false end
		when 'supertag' then (public.dispatch_viewer_open() or public.dispatch_viewer_person() is not null
			or public.dispatch_viewer_session() is not null or coalesce(cardinality(public.dispatch_viewer_sees()), 0) > 0)
		when 'attribute' then (public.dispatch_viewer_open() or public.dispatch_viewer_person() is not null
			or public.dispatch_viewer_session() is not null or coalesce(cardinality(public.dispatch_viewer_sees()), 0) > 0)
		else false end, false)
	from (select public.dispatch_viewer() as v) viewer
$$;

create function public.dispatch_tuple_end_visible(end_kind text, end_id text) returns boolean
language sql stable cost 10000 security definer set search_path = pg_catalog, public
as $$
	select public.dispatch_node_visible($1, $2, held.project_key, held.parties)
	from (select 1) one
	left join public.graph_nodes held on held.kind = $1 and held.id = $2
$$;

create function public.dispatch_broadcast_visible(broadcast_id uuid, broadcast_author jsonb) returns boolean
language sql stable security definer set search_path = pg_catalog, public
as $$
	select coalesce((public.dispatch_viewer_open() and not public.dispatch_viewer_private()) or case
		when exists (select 1 from public.broadcasts stored where stored.id = $1) then /* project access: BroadcastClause */ coalesce(exists (
				select 1 from public.broadcasts clause_broadcast
				where clause_broadcast.id = ($1)
					and (coalesce((viewer.v::jsonb->>'open')::boolean, false) or (clause_broadcast.author->>'kind' = 'user' and clause_broadcast.author->>'id' = viewer.v::jsonb->>'person'))
					and not exists (
						select 1 from public.messages clause_broadcast_copy
						where clause_broadcast_copy.broadcast_id = clause_broadcast.id
							and not /* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(clause_broadcast_copy.target, 9)) = viewer.v::jsonb->>'session'
				or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
				or session_clause_visible((substr(clause_broadcast_copy.target, 9)), viewer.v::jsonb,
					case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
						select 1 from public.session_projects_filled
						offset 0)
					else true end)), false)
						offset 0)
				offset 0), false)
		else (coalesce((viewer.v::jsonb->>'open')::boolean, false)
			or ($2->>'kind' = 'user' and $2->>'id' = viewer.v::jsonb->>'person')) end, false)
	from (select public.dispatch_viewer() as v) viewer
$$;

create function public.dispatch_workflow_node_visible(node_kind text, node_id text, node_attributes jsonb, node_made_by jsonb) returns boolean
language sql stable security definer set search_path = pg_catalog, public
as $$
	select coalesce((public.dispatch_viewer_open() and not public.dispatch_viewer_private()) or case
		when node.issue is not null then exists (
			select 1 from public.issues part_of
			where part_of.key = node.issue and public.dispatch_sees_project(part_of.project_key))
		else ($4->>'kind' = 'session' and $4->>'id' = public.dispatch_viewer_session())
			or ($4->>'kind' = 'user' and public.user_threads_login($4->>'id') = public.dispatch_viewer_person())
			or public.user_threads_login($4->>'owner') = public.dispatch_viewer_person() end, false)
	from (select coalesce($3->>'issue', (
		select part_of.to_id from public.workflow_tuples part_of
		where part_of.from_kind = $1 and part_of.from_id = $2 and part_of.kind = 'part_of' and part_of.to_kind = 'issue'
		order by part_of.seq desc limit 1)) as issue) node
$$;

create function public.dispatch_message_visible(message_id uuid, message_author jsonb, message_target text, message_in_reply_to uuid) returns boolean
language sql stable security definer set search_path = pg_catalog, public
as $$
	select coalesce((public.dispatch_viewer_open() and not public.dispatch_viewer_private()) or /* project access: PartyClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false)
			or ((('person:' || (viewer.v::jsonb->>'person')) = any(scope.parties)
					or ('session:' || (viewer.v::jsonb->>'session')) = any(scope.parties)
					or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and (not coalesce((viewer.v::jsonb->>'private')::boolean, false) or coalesce(cardinality(scope.parties), 0) > 0))
					or (not coalesce((viewer.v::jsonb->>'open')::boolean, false) and coalesce(cardinality(scope.parties), 0) > 0 and not exists (
						select 1 from unnest(scope.parties) party
						where party not like 'session:%'
						offset 0)))
				and not exists (
					select 1 from unnest(scope.parties) party
					where party like 'session:%' and not (/* project access: SessionClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
			or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
			or session_clause_visible((substr(party, 9)), viewer.v::jsonb,
				case when coalesce((viewer.v::jsonb->>'private')::boolean, false) then exists (
					select 1 from public.session_projects_filled
					offset 0)
				else true end)), false) and /* project access: SessionContentClause */ coalesce((coalesce((viewer.v::jsonb->>'all')::boolean, false) or (substr(party, 9)) = viewer.v::jsonb->>'session'
			or (coalesce((viewer.v::jsonb->>'open')::boolean, false) and not coalesce((viewer.v::jsonb->>'private')::boolean, false))
			or session_content_visible((substr(party, 9)), viewer.v::jsonb)), false))
					offset 0))), false), false)
	from (select public.dispatch_viewer() as v) viewer,
	lateral (select coalesce(
		(select stated.parties from public.graph_nodes stated where stated.kind = 'message' and stated.id = $1::text),
		(with recursive thread as (
				select m.id, m.in_reply_to, m.author, m.target from public.messages m where m.id = $4
				union all
				select m.id, m.in_reply_to, m.author, m.target from public.messages m join thread t on m.id = t.in_reply_to)
			select array_remove(array[case when root.author->>'kind' = 'user' and coalesce(root.author->>'id', '') <> '' then 'person:' || public.user_threads_login(root.author->>'id')
			when root.author->>'kind' = 'session' and coalesce(root.author->>'id', '') <> '' then 'session:' || (root.author->>'id')
			when coalesce(root.author->>'owner', '') <> '' then 'person:' || public.user_threads_login(root.author->>'owner') end, nullif(root.target, '')], null)
			from thread root where root.in_reply_to is null limit 1),
		array_remove(array[case when $2->>'kind' = 'user' and coalesce($2->>'id', '') <> '' then 'person:' || public.user_threads_login($2->>'id')
			when $2->>'kind' = 'session' and coalesce($2->>'id', '') <> '' then 'session:' || ($2->>'id')
			when coalesce($2->>'owner', '') <> '' then 'person:' || public.user_threads_login($2->>'owner') end, nullif($3, '')], null)) as parties) scope
$$;

create or replace function public.session_clause_visible(session_id text, viewer jsonb, filled boolean) returns boolean
language sql stable cost 0.0001 security definer set search_path = pg_catalog, public
as $$
    select coalesce(case
        when coalesce(($2->>'all')::boolean, false) or $1 = $2->>'session' then true
        when coalesce(($2->>'open')::boolean, false) then case
            when not coalesce(($2->>'private')::boolean, false) then true
            when not $3 then false
            when exists (
                select 1 from public.session_projects unseen
                where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
                offset 0
            ) then false
            when exists (
                select 1 from public.session_projects acted where acted.session_id = $1
                offset 0
            ) then true
            else coalesce((
                select listener.projects from public.route_roster_session_projects listener where listener.id = true
            )->$1 <@ ($2->'sees'), false)
        end
        when coalesce(($2->>'private')::boolean, false) and not $3 then false
        when exists (
            select 1 from public.session_projects acted
            where acted.session_id = $1 and ($2->'sees') ? acted.project_key
            offset 0
        ) then
            not coalesce(($2->>'private')::boolean, false) or not exists (
                select 1 from public.session_projects unseen
                join public.projects hidden on hidden.key = unseen.project_key and hidden.visibility = 'private'
                where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
                offset 0
            )
        when coalesce(($2->>'private')::boolean, false) and not exists (
            select 1 from public.session_projects acted where acted.session_id = $1
            offset 0
        ) then
            coalesce((
                select record.listed <> '[]'::jsonb and record.listed <@ ($2->'sees')
                from (
                    select listener.projects->$1 as listed
                    from public.route_roster_session_projects listener where listener.id = true
                ) record
            ), false)
        else false
    end, false)
$$;

create or replace function public.session_content_visible(session_id text, viewer jsonb) returns boolean
language sql stable cost 0.0001 security definer set search_path = pg_catalog, public
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

create or replace function public.record_project_made_private() returns trigger
language plpgsql security definer set search_path = pg_catalog, public
as $$
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

create or replace function public.sync_route_roster_session_projects() returns trigger
language plpgsql security definer set search_path = pg_catalog, public
as $$
begin
    insert into public.route_roster_session_projects (id, projects)
    values (new.id, coalesce((
        select jsonb_object_agg(listed->>'session_id', listed->'projects')
        from jsonb_array_elements(new.sessions) listed
        where listed->>'session_id' is not null and jsonb_typeof(listed->'projects') = 'array'
    ), '{}'::jsonb))
    on conflict (id) do update set projects = excluded.projects;

    insert into public.session_reached_projects (session_id, project_key)
    select distinct listed->>'session_id', named.key
    from jsonb_array_elements(new.sessions) listed
    cross join lateral jsonb_array_elements_text(
        case when jsonb_typeof(listed->'projects') = 'array' then listed->'projects' else '[]'::jsonb end
    ) as listed_project (key)
    join public.projects named on named.key = listed_project.key and named.visibility = 'private'
    where coalesce(listed->>'session_id', '') <> ''
        and not exists (
            select 1 from public.session_reached_projects recorded
            where recorded.session_id = listed->>'session_id' and recorded.project_key = named.key);
    return new;
end
$$;

create or replace function public.projects_session_projects_swept() returns trigger
language plpgsql security definer set search_path = pg_catalog, public
as $$
begin
  delete from public.session_projects where project_key = old.key;
  return old;
end $$;

create or replace function public.user_threads_record(
  p_thread uuid, p_kind text, p_author_kind text, p_author_id text, p_at timestamptz,
  p_write uuid, p_reply boolean, p_issue text, p_artifact uuid
) returns void
language plpgsql security definer set search_path = pg_catalog, public
as $$
declare
  v_writer text := case when p_author_kind = 'user' then public.user_threads_login(p_author_id) end;
  v_project text;
begin
  if p_issue is not null then
    select project_key into v_project from public.issues where key = p_issue;
  elsif p_artifact is not null then
    select project_key into v_project from public.artifacts where id = p_artifact;
  end if;
  if v_project is null then
    return;
  end if;
  if p_reply then
    update public.user_threads
    set replied_at = greatest(replied_at, p_at),
        last_reply_id = case when replied_at is null or p_at >= replied_at then p_write else last_reply_id end,
        dismissed_at = null,
        version = version + 1
    where thread_id = p_thread and login is distinct from v_writer;
  end if;
  if v_writer is not null then
    insert into public.user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through)
    values (v_writer, p_thread, p_kind, v_project, p_issue,
            case when p_issue is null then p_artifact end, p_at)
    on conflict (login, thread_id) do update
      set read_through = greatest(user_threads.read_through, excluded.read_through);
  end if;
end;
$$;
