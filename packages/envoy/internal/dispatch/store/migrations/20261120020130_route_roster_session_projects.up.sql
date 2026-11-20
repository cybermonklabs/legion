-- 20261120020130_route_roster_session_projects.up.sql — the listener projects SessionClause reads.
--
-- A session that has no session_projects pair is judged by the projects its listener record names.
-- SessionClause runs in every graph reader, so reading route_roster there made one stored-row scan
-- an InitPlan of unrelated statements: a roster table a route-status test made unreadable then
-- broke an issue read, and every backlink count paid its scan. This one-row projection precomputes
-- the jsonb object from each session id to its projects. A SessionClause reaches the row through
-- its primary key and reads the object once per statement; it never names route_roster or expands
-- every listener row in the statement that reads a graph.
--
-- The object holds a session only when its listener record lists its projects: a list, [] for a
-- credential that lists none. A record with no projects key, or a null one, is a listener that does
-- not know the session's projects, which is not a credential that lists none, so the object leaves
-- that session out, and SessionClause passes it to no viewer but its own while a project is
-- private.
--
-- The trigger follows every old and new writer of route_roster, including a release that does not
-- name this table and test fixtures that insert the row directly. The insert below copies the one
-- row an earlier release may already hold. Each trigger replacement is in the roster writer's
-- transaction, so a reader sees the preceding answer or the new answer, never a partly copied one.
--
-- This creates a new table and a trigger on route_roster. It neither rewrites route_roster nor
-- refuses its rows. The release before this one keeps writing route_roster and the trigger keeps
-- the projection current during a rolling deploy. The census answers 0, and rollback is an image
-- rollback alone.
create table route_roster_session_projects (
    id boolean primary key default true check (id),
    projects jsonb not null
);

insert into route_roster_session_projects (id, projects)
select roster.id, coalesce(jsonb_object_agg(listed->>'session_id', listed->'projects'), '{}'::jsonb)
from route_roster roster cross join lateral jsonb_array_elements(roster.sessions) listed
where listed->>'session_id' is not null and jsonb_typeof(listed->'projects') = 'array'
group by roster.id;

create function sync_route_roster_session_projects() returns trigger language plpgsql as $$
begin
    insert into route_roster_session_projects (id, projects)
    values (new.id, coalesce((
        select jsonb_object_agg(listed->>'session_id', listed->'projects')
        from jsonb_array_elements(new.sessions) listed
        where listed->>'session_id' is not null and jsonb_typeof(listed->'projects') = 'array'
    ), '{}'::jsonb))
    on conflict (id) do update set projects = excluded.projects;
    return new;
end
$$;

create trigger route_roster_session_projects_sync
    after insert or update of sessions on route_roster
    for each row execute function sync_route_roster_session_projects();

create function listener_session_projects() returns jsonb
language sql stable cost 0.0001
set search_path to pg_catalog, public
as $$
    select projects from route_roster_session_projects where id = true
$$;

create function session_clause_visible(session_id text, viewer jsonb, filled boolean) returns boolean
language sql stable cost 0.0001
set search_path to pg_catalog, public
as $$
    select coalesce(case
        when coalesce(($2->>'all')::boolean, false) or $1 = $2->>'session' then true
        when coalesce(($2->>'open')::boolean, false) then case
            when not coalesce(($2->>'private')::boolean, false) then true
            when not $3 then false
            when exists (
                select 1 from session_projects unseen
                where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
                offset 0
            ) then false
            when exists (
                select 1 from session_projects acted where acted.session_id = $1
                offset 0
            ) then true
            else coalesce((
                select projects from route_roster_session_projects where id = true
            )->$1 <@ ($2->'sees'), false)
        end
        when exists (
            select 1 from session_projects acted
            where acted.session_id = $1 and ($2->'sees') ? acted.project_key
            offset 0
        ) then
            not coalesce(($2->>'private')::boolean, false) or not exists (
                select 1 from session_projects unseen
                join projects hidden on hidden.key = unseen.project_key and hidden.visibility = 'private'
                where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
                offset 0
            )
        when coalesce(($2->>'private')::boolean, false) and not exists (
            select 1 from session_projects acted where acted.session_id = $1
            offset 0
        ) then
            coalesce((
                select jsonb_array_length(record.listed) > 0 and record.listed <@ ($2->'sees')
                from (select projects->$1 as listed from route_roster_session_projects where id = true) record
            ), false)
        else false
    end, false)
$$;
