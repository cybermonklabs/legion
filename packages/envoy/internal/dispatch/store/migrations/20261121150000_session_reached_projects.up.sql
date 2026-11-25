-- 20261121150000_session_reached_projects.up.sql — the projects a session's person lost.
--
-- The live agent view relays a session's raw transcript, whose tool results hold whatever the
-- session read, and it judges the session's people by what they see now
-- (access.SessionContentClause). A person who loses a project keeps, in each of their sessions'
-- transcripts, what those sessions read of it before. So an access change that takes a project
-- from a person - off its list, a group that admitted them off its list, out of such a group, the
-- group deleted, or the project given to another owner - records, in the change's own transaction,
-- a (session, project) pair here for every session tied to that person (session_owners)
-- (access.recordReachedProjects). The content rule reads a pair here as it reads one of
-- session_projects: a session that reached a project the viewer does not see answers that viewer
-- as one nobody holds. Nothing removes a pair, `envoy-dispatch backfill-session-projects --reset`
-- included: what a transcript holds stays in it.
--
-- A pair is written only where no row holds it, and the table has no unique key, for the reason
-- session_projects and session_owners have none: a writer never waits on another's open insert.
--
-- This creates a new table and its index. It neither rewrites nor refuses any existing row. The
-- release before this one writes nothing here and no build reads it, so rollback is an image
-- rollback alone.
create table session_reached_projects (
    session_id text not null check (session_id <> ''),
    project_key text not null check (project_key <> ''),
    first_at timestamptz not null default now()
);

create index session_reached_projects_pair on session_reached_projects (session_id, project_key);
