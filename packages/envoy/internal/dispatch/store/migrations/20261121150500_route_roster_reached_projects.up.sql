-- 20261121150500_route_roster_reached_projects.up.sql — a listed private project stays reached.
--
-- The live agent view judges a session whose listener record lists its credential's projects by
-- that list as it stands (access.SessionContentClause). A list narrows: a session's token traded
-- again without a private project is listed without it, while the session's transcript keeps
-- what it read under the wider list. So every write of the listener record that lists a private
-- project for a session also records that pair as one the session reached
-- (session_reached_projects), which the content rule reads as a project the session acted in,
-- and nothing removes it.
--
-- The record is written through route_roster, whose trigger keeps the one-row projection
-- route_roster_session_projects (migration 20261120020130); the same trigger writes the pairs, so
-- every writer of route_roster, an old release's and a test fixture's included, writes them in its
-- own transaction. A pair is written only where no row holds it. A listed project that is public
-- when the record is written records nothing: what the session read of it was public then.
--
-- A row written already past its expires_at records its pairs too. A pair only narrows what the
-- content rule passes, and every pair the row lists is a private project the listener said the
-- session's credential names, so it is a fact, not a guess: dropping it would let a viewer who
-- does not see that project watch the session once its list narrows, and treating such a row as
-- naming every private project would record pairs no listener ever sent and hide the session from
-- everyone not on every private project for good. Expiry decides what is read, not what is
-- written: session_content_visible trusts no list of a row past its expires_at
-- (access.TestAnExpiredListenerRecordStillRecordsTheProjectsItReached).
--
-- The function runs under the search_path of whoever writes route_roster and sets none, so it names
-- each table it writes or reads by its schema, as session_clause_visible and
-- session_content_visible name theirs: a writer whose search_path puts another schema first still
-- writes public's projection and pairs
-- (access.TestTheListenerTriggerWritesPublicsTablesWhateverTheWritersSearchPath).
--
-- This replaces the trigger's function. It neither rewrites nor refuses any existing row, and the
-- census answers 0. An image rollback runs no down migration, so the database keeps this function
-- under the release before, and every roster write that release makes still records its reached
-- pairs: that release reads no pair, so they change nothing it answers, and they are there again
-- for the release that reads them.
create or replace function sync_route_roster_session_projects() returns trigger language plpgsql as $$
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
