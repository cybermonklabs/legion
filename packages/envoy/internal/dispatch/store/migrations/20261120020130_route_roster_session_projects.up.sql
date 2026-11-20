-- 20261120020130_route_roster_session_projects.up.sql — the listener projects SessionClause reads.
--
-- A session that has no session_projects pair is judged by the projects its listener record names.
-- SessionClause runs in every graph reader, so reading route_roster there made one stored-row scan
-- an InitPlan of unrelated statements: a roster table a route-status test made unreadable then
-- broke an issue read, and every backlink count paid its scan. This one-row projection precomputes
-- the jsonb object from each session id to its projects. session_clause_visible, below, reaches the
-- row through its primary key; it never names route_roster or expands every listener row in the
-- statement that reads a graph.
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

-- A delete of the stored roster's row, and a truncate of the table, take the projection with them,
-- so no session keeps a listener record the roster no longer holds.
create function clear_route_roster_session_projects() returns trigger language plpgsql as $$
begin
    if tg_op = 'TRUNCATE' then
        delete from route_roster_session_projects;
    else
        delete from route_roster_session_projects where id = old.id;
    end if;
    return null;
end
$$;

create trigger route_roster_session_projects_delete
    after delete on route_roster
    for each row execute function clear_route_roster_session_projects();

create trigger route_roster_session_projects_truncate
    after truncate on route_roster
    for each statement execute function clear_route_roster_session_projects();

-- session_clause_visible is access.SessionClause's decision for a session, by id ($1), a viewer
-- in the shape of the visibility argument ($2) and whether session_projects_filled holds its row
-- ($3), for every viewer the clause's own SQL does not answer first: the clause passes the
-- server's viewer, the viewer's own session and every session for an open viewer of a deployment
-- that holds no private project without calling it, so those rows read nothing.
--
-- It reads tables, so Postgres runs it once per row it tests and never inlines it: a scalar SQL
-- function is inlined only when its body holds no subquery, whatever else it declares. Its cost,
-- 0.0001, is what the planner charges a statement that binds the clause, so a graph read's arms
-- keep the indexes they are written for (written as the statement's own SQL instead, the clause's
-- subqueries cost a document's backlink count its artifacts_ref_key_key probe:
-- refs.TestBacklinkCountStatementReadsEveryArmThroughAnIndex). It declares no search_path, which
-- would save and restore the setting on every call, and names each table by its schema instead.
--
-- A session with no pair is judged by its listener record (the projection above): an open viewer
-- passes it when every project the record names is one the viewer sees, [] included, and a viewer
-- that is not open when the record names a project and every one it names is one the viewer sees.
-- A session the projection does not hold, one whose listener sent no list, passes neither.
create function session_clause_visible(session_id text, viewer jsonb, filled boolean) returns boolean
language sql stable cost 0.0001
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
