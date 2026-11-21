-- 20261121000254_work_holds.up.sql — the record of the work hold (`gates.hold: work`): one row
-- for each tree whose standing design gate closed under the hold, naming the root's generation,
-- the step, the document and the version that closed it, and when
-- (record.WorkHold). The close writes it (workflow's workHoldClose); the opening deletes it, and so
-- does a new generation of the tree (record.Store.ClearTreeGeneration). It is how the daemon's
-- record holds the hold across a restart: the boot applies the close to a tree that holds work
-- and no row, and starts nothing of a tree that holds both (workflow's workHoldBooted). The
-- opening's task and the state's `why: held` name the version that closed the gate from it. No
-- tree is held before this release, so the table starts empty.
--
-- The release before never names work_holds. It does not know the keys the hold writes on an
-- outbox row either: a supervise suspend's `suspendedBy`, `deadline` and `turnAfter`, a start's
-- `opening`, and a role message's `wake` and `stop`. It decodes every payload strictly
-- (record.DecodeOutboxPayload, DisallowUnknownFields), so a row left carrying one of them is retried
-- for ever, and record.Postgres.RoleRuns, which decodes every supervise row of the issues an
-- admission pass reads, fails every pass of its issue with it.
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with this release's daemon
-- stopped and before the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
-- Run it before the way back of 20261120224328_claim_suspended_by_hold, which hands each claim the
-- hold stopped to the operator.
--
-- The way back deletes each queued stop of the hold, which the release before would run as the
-- daemon's own stop and which a claim it rolls back to has no hold for, and takes the hold's keys
-- off every other row it left: a start keeps its task, which already carries the opening's
-- sentences, and a close message is published as the message it is, with no stop behind it and no
-- wake. Then it drops the table and deletes this migration's row, which makes the version the
-- store reports (store.SchemaVersionTx) that release's own again; at the roll-forward store.apply
-- finds no row of this version and runs this file again. What it costs is the holds themselves:
-- after the roll-forward a tree that holds work and has no row is closed again at the boot.
--
-- begin;
-- delete from outbox where kind = 'supervise' and payload->>'suspendedBy' = 'hold';
-- update outbox set payload = payload - 'suspendedBy' - 'deadline' - 'turnAfter' - 'opening' where kind = 'supervise' and payload ?| array['suspendedBy', 'deadline', 'turnAfter', 'opening'];
-- update outbox set payload = payload - 'wake' - 'stop' where kind = 'role_message' and payload ?| array['wake', 'stop'];
-- drop table if exists work_holds;
-- delete from schema_version where version = 20261121000254;
-- commit;
create table work_holds (
  tree text primary key references issues (key) on delete cascade,
  project text not null check (project <> ''),
  root_generation bigint not null check (root_generation >= 0),
  step text not null check (step in ('decisions', 'criteria', 'spec')),
  artifact_id text not null check (artifact_id <> ''),
  version integer not null check (version > 0),
  closed_at timestamptz not null);
create index work_holds_project on work_holds (project);
