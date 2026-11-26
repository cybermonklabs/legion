-- 20261121130000_session_owners.up.sql — the person each proved session acts for.
--
-- The live agent view relays a session's raw transcript, whose tool results hold whatever the
-- session read, and a read records nothing: session_projects counts only what a session wrote. So
-- a viewer that is not open (a restricted member) is relayed only a session its own person's
-- credential proved (access.SessionContentClause), and Dispatch has to know whose a session is.
-- Neither the listener's record of a session nor its known-session record names a person: the one
-- tie Dispatch holds is a broker-issued token whose act names that session, which names the person
-- the token acts for (Actor.Owner). access.TieSession records the pair here; a later slice adds its
-- callers: each verification of such a token, on a read as on a write, each write the token
-- proved, and the backfill of older events. A session no such token proved is nobody's.
--
-- The pair is written only where no row holds it, and the table has no unique key, for the reason
-- session_projects has none: two open writers of one pair would otherwise wait for each other
-- while they hold their issues' locks.
--
-- This creates a new table and its index. It neither rewrites nor refuses any existing row. Nothing
-- in this release writes here, so every session stays untied, and is relayed to no viewer that is
-- not open, until the later slice ties it. The census answers 0, and rollback is an image rollback
-- alone.
--
-- The login is the person's canonical one (model.CanonicalLogin), which folds ASCII letters alone,
-- so the table checks no lower(): Postgres's lower() folds other letters too.
create table session_owners (
    session_id text not null check (session_id <> ''),
    login text not null check (login <> ''),
    first_at timestamptz not null default now()
);

create index session_owners_pair on session_owners (session_id, login);
