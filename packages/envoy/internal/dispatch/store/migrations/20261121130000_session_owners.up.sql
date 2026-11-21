-- 20261121130000_session_owners.up.sql — the person each proved session acts for.
--
-- The live agent view relays a session's raw transcript, whose tool results hold whatever the
-- session read, and a read records nothing: session_projects counts only what a session wrote. A
-- full member's credential reads every public project whatever it lists, so a viewer that is not
-- open (a restricted member) is relayed only a session that acts for restricted members alone,
-- whose lists bound what it read (access.SessionContentClause), and Dispatch has to know whose a
-- session is. A session is tied to a person only by a write its own credential proved
-- (events.provedSession: a broker-issued token whose act names that session), whose recorded actor
-- names the person the token acts for (Actor.Owner). Each such write records the pair here, as it
-- records the session's project in session_projects; `envoy-dispatch backfill-session-projects`
-- records the pairs of the events it reads. A session no proved write tied to anybody is no
-- restricted member's.
--
-- The pair is written only where no row holds it, and the table has no unique key, for the reason
-- session_projects has none: two open writers of one pair would otherwise wait for each other
-- while they hold their issues' locks (events.queueSessionProject).
--
-- This creates a new table and its index. It neither rewrites nor refuses any existing row; the
-- release before this one writes nothing here, and a session it tied stays untied until it writes
-- again or the backfill reads its events. The census answers 0, and rollback is an image rollback
-- alone.
create table session_owners (
    session_id text not null check (session_id <> ''),
    login text not null check (login <> '' and login = lower(login)),
    first_at timestamptz not null default now()
);

create index session_owners_pair on session_owners (session_id, login);
