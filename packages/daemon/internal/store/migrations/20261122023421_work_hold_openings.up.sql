-- 20261122023421_work_hold_openings.up.sql — the record of the last opening of each tree's work
-- hold (`gates.hold: work`): one row for each tree whose hold an opening ended. It names what the
-- tree's work_holds row named (the root's generation, the step, the document and the version that
-- closed the gate, and when), when the hold ended, and what ended it: today's open message, or
-- the boot's sentence where the deployment no longer holds the tree's work (record.WorkHold.OpenedAt,
-- Opening). The opening deletes the tree's work_holds row and writes this row from it in the same
-- transaction (record.Store.OpenWorkHold). The next close deletes it, and so does a new generation
-- of the tree (record.Store.DeleteWorkHold, ClearTreeGeneration). A hold stop can be written after
-- the opening read the tree's claims. The read after that stop, the stop asked again or the boot
-- then starts the claim, and each of them reads this row, so the claim gets the task the opening
-- gave (workflow's heldClaimOpening). No hold has ended before this release, so the table starts
-- empty.
--
-- The release before never names work_hold_openings. It reads and writes work_holds alone, and
-- this migration does not change that table, so the release before boots with this table and its
-- rows in place. That release starts a claim the hold stopped after an opening with its own task.
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with this release's daemon
-- stopped and before the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
-- Run it before the way back of 20261121000254_work_holds.
--
-- The way back drops the table and deletes this migration's row, which makes the version the
-- store reports (store.SchemaVersionTx) that release's own again. At the roll-forward store.apply
-- finds no row of this version and runs this file again. What it costs is the records of the
-- openings: after the roll-forward, a claim the hold stopped that an opening did not start gets a
-- task that names no version, as at a boot after a new generation of its tree.
--
-- begin;
-- drop table if exists work_hold_openings;
-- delete from schema_version where version = 20261122023421;
-- commit;
create table work_hold_openings (
  tree text primary key references issues (key) on delete cascade,
  project text not null check (project <> ''),
  root_generation bigint not null check (root_generation >= 0),
  step text not null check (step in ('decisions', 'criteria', 'spec')),
  artifact_id text not null check (artifact_id <> ''),
  version integer not null check (version > 0),
  closed_at timestamptz not null,
  opened_at timestamptz not null,
  opening text not null check (opening <> ''));
