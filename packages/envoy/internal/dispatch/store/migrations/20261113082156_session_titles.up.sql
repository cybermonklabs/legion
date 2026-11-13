-- 20261113082156_session_titles.up.sql
--
-- Each session's title, stored on a row of its own keyed by the session (dispatch://INFRA-2916,
-- R13). A page that names a session reads its title here by primary key, beside the rows it names
-- it on, rather than resolving it out of the events table on every read, which costs 42 to 133 ms
-- a paint. The row is written in the transaction that inserts an event whose actor is a session
-- stamping a title, the newest event winning.
--   session_id      the session, and the row's identity.
--   title           the title the session's newest titled event stamped. The check refuses an
--                   empty one: a session with no title has no row, and a reader that finds none
--                   names the session by its id, as it did when no event carried a title.
--   titled_at       the time of that event, which a later write compares with so an older
--                   event never takes a newer title's place.
--
-- One new table: nothing existing is read or rewritten, and the release before this one neither
-- reads nor writes it, so the two builds roll together (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table session_titles (
    session_id text primary key,
    title text not null check (title <> ''),
    titled_at timestamptz not null
);
