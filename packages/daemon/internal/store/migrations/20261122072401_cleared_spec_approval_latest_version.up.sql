-- 20261122072401_cleared_spec_approval_latest_version.up.sql — the newest version of the document
-- that the record of a spec approval a root's re-admission cleared knows of
-- (record.ClearedSpecApproval.LatestVersion). The record stands for the spec row the re-admission
-- deleted while the root holds no spec row (workflow's clearedSpecRow). That row ignores a changes
-- request on a version older than its latest (workflow's artifactIgnored), so the record needs
-- that version too. The re-admission writes it from the row's latest version (admit's
-- recordSpecApproval). A new version of that document raises it while no row stands, and a later
-- approval moves it (workflow's clearedSpecEvent). A record the table holds when this file runs
-- takes its approved version, the lowest latest version a row can have with that approval. A
-- late changes request on a version above it then deletes that record. On a database that never
-- ran this release the table holds no record, since 20261120210115_approval_route_records of this
-- same release makes it.
--
-- The release before has no cleared_spec_approvals table, so no release reads or writes the table
-- without this column.
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with this release's daemon
-- stopped and before the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
-- Run it before the way back of 20261120210115_approval_route_records. That way back drops the
-- table, and with this migration's row still recorded, the roll-forward would create the table
-- again without the column.
--
-- The way back drops the column and deletes this migration's row, which makes the version the
-- store reports (store.SchemaVersionTx) that release's own again. A rollback to the release before
-- also runs the way back of 20261120210115_approval_route_records, which drops the table. So no
-- record is left for a roll-forward: it creates the table again and runs this file over no row.
-- An operator can still stop after this way back alone and start this release again. Then
-- store.apply runs this file over the records the table holds, and each record takes its approved
-- version as its latest version. That costs the newest version each record knew of: a late changes
-- request on a version between a record's approved version and that newest version deletes the
-- record, where the row would have ignored it.
--
-- begin;
-- alter table if exists cleared_spec_approvals drop column if exists latest_version;
-- delete from schema_version where version = 20261122072401;
-- commit;
alter table cleared_spec_approvals add column latest_version integer;
update cleared_spec_approvals set latest_version = approved_version;
alter table cleared_spec_approvals alter column latest_version set not null,
  add constraint cleared_spec_approvals_latest_version_check
    check (latest_version >= approved_version);
