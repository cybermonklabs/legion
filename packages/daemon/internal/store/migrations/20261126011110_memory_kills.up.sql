-- 20261126011110_memory_kills.up.sql — the memory kills of each project's issue pods, kept for the
-- kills signal of admission by signals, and the moment the daemon began keeping each input of a
-- signal.
--
-- The kills signal (signals.KillsAt) is the count of the memory kills of a project's issue pods in
-- the hour before a moment (signals.KillSpan): each process an issue pod's memory guard ended, and
-- each its microVM kernel stopped, of a role or of no role, as the pod's launcher reported it
-- (shimwire.LauncherMemoryKill, intake.MemoryGuardKill). Admission keeps each report in the fact's
-- own transaction, before that fact's admission pass (admit's keepSignalFacts,
-- record.Store.KeepMemoryKill), so the pass reads the kill it was run for:
--
--   - memory_kills is one row per kill: the project the issue pod is of, the kill's identity, the
--     guard's or the guest watch's id and its count of kills (the report's guard and sequence,
--     which is also the identity the fact is applied once under,
--     `memory-guard-kill:<guard>:<sequence>`), the moment the launcher's watch named the kill (the
--     report's at, so a report a reconnected launcher sends late counts at its own moment), its
--     cause as the wire spells it ('' for the guard's own kill, 'kernel', 'kernel-protected'), and
--     whether the process was of no role. A report kept twice is one row. The rows the reads need
--     are the kills of the hour before the moment read and after it, so the index is by project and
--     time, and each keep deletes the project's kills an hour or more older than the moment it
--     keeps at, which no read needs again.
--   - signal_inputs is one row per project and input of a signal: kept_since, the moment the
--     daemon began keeping that input for the project. A signal that counts its input over a span
--     is recorded only from that moment plus its span, so a series that was not kept reads as not
--     recorded rather than as zero. The admission of the first boot of a release that keeps the
--     input writes the row (intake.DaemonBooted, record.Store.BeginSignalInput) and no later boot
--     moves it. This release writes the row of the input 'kills' (signals.Kills).
--
-- Every time is written from the daemon's clock, never by a column default: a replay of a recorded
-- window writes its simulated times there.
--
-- The migration adds two tables and changes none that an earlier release reads, so a daemon of the
-- release before runs over the migrated database, as a rollback leaves it: it keeps no kill and
-- moves no row. That is also what makes the rows wrong after a roll-forward: signal_inputs still
-- says the kills were kept from the first upgrade on, while no kill of the time the release before
-- ran was kept, so for up to an hour after the roll-forward the kills signal would count too few
-- kills as if it held them all.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The tables hold
-- the rows of every project of the database, so every daemon of it moves together. The release
-- before needs none of it, since it names neither table; it is owed to the roll-forward. The delete
-- is what makes the version the store reports, its highest recorded row (store.SchemaVersionTx),
-- that release's own again, and at the roll-forward store.apply finds no row of this version and
-- runs this file again, which creates the tables anew; the drops are what let it run again at all.
-- The roll-forward's first boot then writes each project's kept_since anew, so the kills signal
-- reads as not recorded for the hour after it, rather than as a count that misses kills. What the
-- drops cost is the kills of that hour, which the signal could not have counted whole anyway.
--
-- begin;
-- drop table if exists memory_kills;
-- drop table if exists signal_inputs;
-- delete from schema_version where version = 20261126011110;
-- commit;
create table memory_kills (
    project  text        not null check (project <> ''),
    guard    text        not null check (guard <> ''),
    sequence bigint      not null check (sequence >= 1),
    at       timestamptz not null,
    cause    text        not null check (cause in ('', 'kernel', 'kernel-protected')),
    no_role  boolean     not null,
    primary key (project, guard, sequence)
);
create index memory_kills_by_time on memory_kills (project, at);
create table signal_inputs (
    project    text        not null check (project <> ''),
    input      text        not null check (input <> ''),
    kept_since timestamptz not null,
    primary key (project, input)
);
