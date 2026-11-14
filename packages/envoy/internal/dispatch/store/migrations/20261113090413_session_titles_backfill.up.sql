-- 20261113090413_session_titles_backfill.up.sql
--
-- The fill of 20261113082156's session_titles for the events stored before 20261113090412's
-- trigger, as one row: the fill is owed while the row is there, and the row holds how far it has
-- got. This migration writes no session_titles row and reads no other table, so it takes no lock
-- that grows with any table and commits in milliseconds.
--
-- The fill itself is store.FillSessionTitles, which the boot runs in committed batches after the
-- migrations and before the server serves (cmd/dispatch/main.go), as it runs
-- store.FillDocumentSearch for 20261101182501. It is not a migration for that one's reason: a
-- migration is one transaction (store/store.go applyMigrationOnce), the statement would read
-- every session's events, events being the largest table a deployment holds, and 20261113090412's
-- trigger is live by then, so a write of a session the statement had already written would wait
-- at its commit for the whole statement while holding its owner's row, and every other write on
-- that owner would queue behind it. A committed batch holds such a key for one batch instead.
-- It is no subcommand an operator runs either: this release reads titles from session_titles
-- alone, so a process serving before the fill names every session stored before it by its id.
--
-- session_id is the keyset cursor: the highest session id the pass has covered, in the order
-- events_session_actor_created_at_idx keys them, and '' before its first batch, which sorts
-- below every id a session carries. The row is the work, not a mark beside it: the pass deletes
-- it when a batch finds no session above the cursor, so a boot after the pass has finished reads
-- an empty table and stops, and a database emptied of its rows (the e2e suite's reset, which
-- empties session_titles and events with it) owes no fill. The singleton primary key keeps it
-- one row, as 20261101182501's does.
create table session_titles_backfill (
  singleton boolean primary key default true check (singleton),
  session_id text not null default ''
);
insert into session_titles_backfill default values;
