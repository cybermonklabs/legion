-- 20261121165416_session_clause_filled.up.sql — a restricted viewer reads session_projects only once filled.
--
-- session_projects places every session in the projects it acted in only once
-- `envoy-dispatch backfill-session-projects` has written session_projects_filled's row:
-- migration 20261116081157_session_projects_proven empties both tables, and the command's --reset
-- empties them again in a deployment that may already hold a private project, then refills
-- session_projects pair by pair. session_clause_visible (migration 20261120020130) waited for the
-- marker on its open arm alone. Its arm for a viewer that is not open (a restricted member, or a
-- credential acting for one) passed a session that has a pair in a project the viewer sees and no
-- pair in a private project it does not see, and one with no pair by its listener record: in the
-- middle of a refill a session that acted in a private project the viewer does not see can have its
-- pair in the viewer's project back and the other not yet, or no pair at all.
--
-- The replacement answers every viewer the clause's own SQL does not answer first alike: while a
-- project is private and the marker holds no row ($3 false), no session passes but the viewer's
-- own, which the clause answers before it calls this function. access.SessionClause now reads the
-- marker for every such viewer of a deployment that holds a private project, once per statement
-- (an InitPlan), and passes true otherwise, so a deployment with no private project reads no
-- marker and answers as before (R9). Every other arm is the function as migration 20261120020130
-- wrote it, each table named by its schema and no search_path set.
--
-- This replaces a function. It neither reads, rewrites nor refuses any row. The census answers 0.
-- The release before this one passes the marker only for an open viewer and true for every other;
-- under it this function answers a restricted viewer as the function it replaces did, so rollback
-- is an image rollback alone.
create or replace function session_clause_visible(session_id text, viewer jsonb, filled boolean) returns boolean
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
