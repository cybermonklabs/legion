-- 20261121130000_session_owners.up.sql — the person each proved session acts for.
--
-- The live agent view relays a session's raw transcript, whose tool results hold whatever the
-- session read, and a read records nothing: session_projects counts only what a session wrote. So
-- a viewer that is not open (a restricted member) is relayed only a session its own person's
-- credential proved (access.SessionContentClause), and Dispatch has to know whose a session is.
-- Neither the listener's record of a session nor its known-session record names a person: the one
-- tie Dispatch holds is a write the session's own credential proved (events.provedSession: a
-- broker-issued token whose act names that session), whose recorded actor names the person the
-- token acts for (Actor.Owner). Each such write records the pair here, as it records the
-- session's project in session_projects; `envoy-dispatch backfill-session-projects` records the
-- pairs of the events it reads. A session no proved write tied to anybody is nobody's.
--
-- The pair is written only where no row holds it, and the table has no unique key, for the reason
-- session_projects has none: two open writers of one pair would otherwise wait for each other
-- while they hold their issues' locks (events.queueSessionProject).
--
-- This creates a new table and its index. It neither rewrites nor refuses any existing row. The
-- release before this one writes nothing here, so a session whose proved writes only that release
-- stored stays untied, and is relayed to no viewer that is not open, until it writes again or the
-- backfill reads its events. The census answers 0, and rollback is an image rollback alone.
--
-- The login is the person's canonical one (model.CanonicalLogin), which folds ASCII letters alone,
-- so the table checks no lower(): Postgres's lower() folds other letters too.
create table session_owners (
    session_id text not null check (session_id <> ''),
    login text not null check (login <> ''),
    first_at timestamptz not null default now()
);

create index session_owners_pair on session_owners (session_id, login);
