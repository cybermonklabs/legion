-- 20261126072843_signal_settings.up.sql — each project's settings of admission by signals, and its
-- record of the changes a reader of the signals sees (S3 and S6 of dispatch://INFRA-5899).
--
--   - signal_settings is one row per project and setting: the switch ('switch', value 1 on and 0
--     off) and the limit of each of the six series by its name of signals.Names() (value null for a
--     limit a person cleared), each with who set it last (the controller with its session, or the
--     operator with none, as project_pauses holds a pause's actor), when, and the person's cited
--     reason. A project with no row is a fresh project: admission by signals off and no limit.
--     Admission writes a row in the transaction of the fact a person's change applies as, where the
--     change moves it (store.ChangeSignalSettingTx), and each pass reads the rows in its own.
--   - signal_changes is one row per entry of the project's record of changes: its id, which rises
--     by one for each entry of the project so a reader sees a lost entry as a gap, its moment, the
--     signal and part it is of (both null for the switch, the part set for the provider's two
--     series alone), what changed ('value', 'limit', 'holds' or 'switch'), the new value (1 or 0 for
--     a hold and the switch, null for a cleared limit and for a value the daemon cannot compute),
--     the reason it cannot (set for such a value alone), and who changed a limit or the switch and
--     why (set for those two alone). The setting's change writes its entry in the same transaction
--     (store.ChangeSignalSettingTx), and each admission pass writes the entries of the holds and
--     values it computed (admit's writeSignalChanges), so the rows a pass reads as the newest of
--     each series are the last committed pass's.
--   - signal_change_ids is one row per project: the last id its record gave out. Each write takes
--     its ids from it in its own transaction (store.AppendSignalChangesTx), so the ids of one
--     project are contiguous: a transaction rolled back gives its ids back with its entries, as no
--     sequence would.
--
-- Every time is the daemon's clock's, never a column default. The migration adds three tables and
-- changes none that an earlier release reads, so a daemon of the release before runs over the
-- migrated database, as a rollback leaves it: it reads no setting, so every project admits under
-- the cap as before, and it writes no entry.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The tables hold
-- the rows of every project of the database, so every daemon of it moves together. The release
-- before needs none of it, since it names no table of these; it is owed to the roll-forward, which
-- runs this file again once the version row is gone. What the drops cost is each project's
-- settings, which a person sets again, and its record of changes, which starts again from id 1.
--
-- begin;
-- drop table if exists signal_settings;
-- drop table if exists signal_changes;
-- drop table if exists signal_change_ids;
-- delete from schema_version where version = 20261126072843;
-- commit;
create table signal_settings (
    project    text             not null check (project <> ''),
    setting    text             not null check (setting in ('switch', 'idle', 'queue', 'provider', 'refusals', 'kills', 'approvals')),
    value      double precision,
    set_at     timestamptz      not null,
    by_kind    text             not null check (by_kind in ('controller', 'operator')),
    by_session text             not null default '',
    reason     text             not null check (reason ~ '[^[:space:]]'),
    primary key (project, setting),
    constraint signal_settings_by_session check ((by_kind = 'controller') = (by_session <> '')),
    constraint signal_settings_switch check (setting <> 'switch' or value in (0, 1))
);
create table signal_changes (
    project    text             not null check (project <> ''),
    id         bigint           not null check (id >= 1),
    at         timestamptz      not null,
    signal     text             check (signal in ('idle', 'queue', 'provider', 'kills', 'approvals')),
    part       text             check (part in ('headroom', 'refusals')),
    what       text             not null check (what in ('value', 'limit', 'holds', 'switch')),
    value      double precision,
    absent     text,
    by_kind    text             check (by_kind in ('controller', 'operator')),
    by_session text,
    reason     text,
    primary key (project, id),
    constraint signal_changes_series check ((what = 'switch') = (signal is null) and (part is not null) = (signal is not distinct from 'provider')),
    constraint signal_changes_flag check (what not in ('holds', 'switch') or value in (0, 1)),
    constraint signal_changes_absent check ((absent is not null) = (what = 'value' and value is null) and (absent is null or absent <> '')),
    constraint signal_changes_set check ((what in ('limit', 'switch')) = (by_kind is not null) and (by_kind is null) = (reason is null)
        and (by_kind is null) = (by_session is null) and (by_kind is null or (by_kind = 'controller') = (by_session <> ''))
        and (reason is null or reason ~ '[^[:space:]]'))
);
create index signal_changes_newest on signal_changes (project, what, signal, part, id);
create table signal_change_ids (
    project text   primary key check (project <> ''),
    last    bigint not null check (last >= 1)
);
