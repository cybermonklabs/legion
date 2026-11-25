-- 20261121065041_check_states.up.sql — the daemon keeps the newest state of each before_merge
-- check at each head of an issue, each head's set of checks, and the serials of the state events
-- it applied, from Dispatch's check.state_changed (intake.DispatchCheckState).
--
-- check_states holds one row for each issue, repository, head and check: the check's state (yes,
-- no or unknown), why it is not yes, the roles whose pass counts there and the ci.case of a check
-- CI proves (record.CheckState). check_state_sets holds one row for each issue, repository and
-- head: the head's whole set of checks, empty for a head with no check. Each row keeps dispatch_seq,
-- the sequence of the event that wrote it in the issue's Dispatch event log, and a write at a lower
-- sequence changes nothing (record.Store.PutCheckState, PutCheckSet), so an event delivered late,
-- behind a newer one, never takes a state back. check_state_serials holds each serial of the
-- issue's state events the daemon applied, which the move of a tested issue waits on: every serial
-- from the lowest held through the serial of the tester's handoff (record.Store.StatesFenceReached).
-- The merge that closes an issue deletes its rows of all three (record.Store.DeleteCheckStates).
--
-- issues.check_states_seq is the cursor of the boot's catch-up of state events: the sequence of the
-- issue's Dispatch event log up to which the daemon has read them. Every row the table holds takes
-- 0, so the first catch-up of each issue reads its log from the start, and a state event Dispatch
-- published while a release before this one ran, which took it as no fact, is held too. The column
-- is nullable, with 0 for its default and its check kept: a migration that a rolling release runs
-- adds only tables, nullable columns and non-unique indexes, so the release before still runs on
-- the database this one migrated, during a roll or a rollback. No statement of this release writes
-- a null, and its read of the cursor takes one as 0 (record.Postgres.CheckStatesSeqs).
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists. The release before this one names each column of issues it reads and
-- writes, so it neither reads nor writes check_states_seq, and it names none of the three tables.
-- So the way back is the one transaction below, run with this release's daemon stopped and before
-- the release before starts:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   drop table if exists check_states;
--   drop table if exists check_state_sets;
--   drop table if exists check_state_serials;
--   alter table issues drop column if exists check_states_seq;
--   delete from schema_version where version = 20261121065041;
--   commit;
--   SQL
--
-- The drops and the delete of this migration's row go together, and are owed to the roll-forward
-- rather than to the release before, which needs none of them. With the tables and the column gone
-- and the row deleted, store.apply finds no row of this version and runs this file again, which
-- creates the tables empty and adds the column at 0. What the drops cost is what the release before
-- never kept: the roll-forward's first catch-up of each issue reads its event log from the start
-- again and applies each state event in it whether or not processed_events, which the way back
-- leaves standing, records the event (the daemon's catchUpCheckStates), so each state event this
-- release held before the drops is held again, and each one the release before took as no fact
-- while it ran is held as well.
create table check_states (
  issue text not null,
  repo text not null,
  head text not null,
  check_id text not null,
  state text not null check (state in ('yes', 'no', 'unknown')),
  reason text not null default '',
  roles text[] not null default '{}',
  ci_case text not null default '',
  dispatch_seq bigint not null check (dispatch_seq > 0),
  primary key (issue, repo, head, check_id));
create table check_state_sets (
  issue text not null,
  repo text not null,
  head text not null,
  checks text[] not null default '{}',
  dispatch_seq bigint not null check (dispatch_seq > 0),
  primary key (issue, repo, head));
create table check_state_serials (
  issue text not null,
  serial bigint not null check (serial > 0),
  primary key (issue, serial));
alter table issues add column check_states_seq bigint default 0 check (check_states_seq >= 0);
