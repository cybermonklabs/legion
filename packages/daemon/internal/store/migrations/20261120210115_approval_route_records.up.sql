-- 20261120210115_approval_route_records.up.sql — the two facts the approval route reads that no
-- row held: the root a carried gate came from, and the spec approval a root's re-admission
-- cleared (POST /legion/v1/claims/approval, record.StandingSpecApproval).
--
-- handed_over_roots.from_root is record.HandedOverRoot.FromRoot: the root that holds the document
-- of the gate a hand-over carried to the issue. A carried copy keeps the old document's id and
-- takes the new root's key, and the promotion that opens the carried tree clears the issue's
-- handed_over_from, so no other fact names that root. The carry writes it (admit's readmit): the
-- tree the child left, or, where that tree's own row is a carried copy at its generation, the root
-- that row names. A carry of no gate writes ''. Every row the table already holds takes the
-- default, '': the route then names the document by its id, and logs one line for it.
--
-- cleared_spec_approvals holds, for a root that a person moved out of the workflow and back, the
-- spec row its re-admission deleted (record.Store.ClearTreeGeneration): the approved version, the
-- document and the approval's place in the issue's Dispatch event log. The re-admission writes it
-- right before it clears the gate rows, at any version, where a person's approval stands on the
-- root's own spec row. The route answers from it while the root holds no spec row. The next
-- registration of the spec step, and a changes request on that document, delete it. The
-- readmitted_approvals row is no such record: it is kept only while the approved version is older
-- than the latest, and the start task's delivery deletes it.
--
-- The release before this one names each column of handed_over_roots it reads and writes
-- (record.Postgres.HandedOverRoot and PutHandedOverRoot), so it neither reads nor writes from_root,
-- and every row it inserts takes the default. Its update of a row that stands leaves the column as
-- it was, so a carry it makes over an issue an earlier carry recorded keeps that carry's root. It
-- never names cleared_spec_approvals: its re-admission writes no record, and neither its
-- registration nor its changes request deletes one.
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with this release's daemon
-- stopped and before the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
--
-- The drops and the delete of this migration's row go together. The drop of the table takes every
-- record a registration or a changes request of the release before did not delete; left standing,
-- such a record would answer the route again after the roll-forward. The drop of the column takes
-- every root a carry of the release before kept from an earlier carry. The delete makes the version
-- the store reports, its highest recorded row (store.SchemaVersionTx), that release's own again,
-- and at the roll-forward store.apply finds no row of this version and runs this file again. What
-- the drops cost is the records and the roots themselves: after the roll-forward a re-admitted root
-- is answered no approval until its architect registers its spec, and a carried copy's message
-- names its document by its id.
--
-- begin;
-- drop table if exists cleared_spec_approvals;
-- alter table handed_over_roots drop column if exists from_root;
-- delete from schema_version where version = 20261120210115;
-- commit;
alter table handed_over_roots add column from_root text not null default '';
create table cleared_spec_approvals (
  issue text primary key references issues (key) on delete cascade,
  artifact_id text not null check (artifact_id <> ''),
  approved_version integer not null check (approved_version > 0),
  approved_seq bigint check (approved_seq > 0),
  approved_at timestamptz,
  check (approved_seq is not null or approved_at is not null));
