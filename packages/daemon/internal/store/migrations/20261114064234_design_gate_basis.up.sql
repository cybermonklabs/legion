-- 20261114064234_design_gate_basis.up.sql -- a root's design-gate step can open on an ancestor's
-- approval, or wait on an ancestor's open approval request, and the root records which approval
-- opened it (dispatch://INFRA-3320 R1 to R3, R8); and an issue records the ancestor its spec is
-- a view on (R4).
--
-- The basis (record.GateBasis), all null on a step no basis opened or waits on:
--
-- design_gates.basis_issue and basis_artifact_id are the ancestor and its primary document: the
-- ancestor's document events reach the steps that name it through basis_artifact_id
-- (record.Store.GatesForBasisArtifact), and the ancestor's sign-off waits on the roots whose step
-- names it now through basis_issue (GatesRestingOn). basis_version is the version of that document
-- a person approved, or, for a pending step, the version the open request asks them to approve.
-- basis_approved_seq and basis_approved_at are where Dispatch recorded that person's approval in
-- the ancestor's own event log, and basis_approver who gave it (`user:<login>`): null while the
-- step is pending. They are places in another issue's log than the root's, so no rule compares
-- them with the root's own review marks. basis_serves is the ancestor's requirement references
-- the root's checks serve, basis_reason the architect's reason and basis_session the architect
-- session that registered the step, which the root's message and its state name.
-- basis_registered_at is the daemon's own time of the registration: the waiting line takes it as
-- the moment a step a basis opened came back (record.TreeGate.OpenedAt), so that moment is never
-- a zero time. basis_pending_ask is the ancestor's open approval request a pending step waits on,
-- null for a basis that stands: the request's close finds the pending steps by it
-- (GatesByPendingAsk), since the root-only ask mirror records no ancestor's ask.
--
-- While a basis stands, approved_version is the version the root registered and approved_seq and
-- approved_at are null, so every reader of whether the step is open reads it open with no rule of
-- its own (record.GateOpen). kept_person_version, kept_person_seq and kept_person_at are the
-- person's approval of an earlier version of the root's own document that approved_version held
-- before the basis opened the step: they are held apart while the basis stands, so no reader shows
-- the person's review beside a version the person did not approve, and the step falls back to them
-- when the basis ends (record.CloseBasis), never to the version the basis opened. They are null
-- whenever no basis stands.
--
-- rests_on is the ancestor a basis first opened the step on in this generation: the ancestor's
-- sign-off waits on the root from then until the generation ends, open or closed later (R8). Only
-- a re-admission takes it away, by deleting the generation's rows, and record.Postgres.PutGate
-- keeps the first value written. A pending step rests on its ancestor through basis_issue alone,
-- so a wait that ends without opening leaves no rest behind.
--
-- issues.view_of is the ancestor whose requirement blocks the issue's spec is a view on, written
-- at each registration of the spec step, null where there is none.
--
-- The indexes are the reads above: a step by its basis document, by its pending request, and by
-- the ancestor it rests on, through either column. design_gates holds a few rows per root, and
-- each read crosses every tree of the database, so each is partial over the rows that carry one.
--
-- No check ties these columns to approved_version or to each other: the daemon keeps that shape
-- itself (record.Postgres.PutGate writes a basis whole and a kept approval only while a basis
-- stands, and its reader, record.scanGate, reads a basis an earlier release's write moved as
-- ended). A daemon of an earlier release running on this schema names none of these columns: its
-- reads leave them out, its inserts leave them null and its upserts leave them as they are. So it
-- reads a step a basis opened as approved at the version the root registered, which opens the
-- tree as the basis does, and calls that version a person's approval in its texts; it reads a
-- pending step as registered and not approved. A new version, a changes request or a person's
-- approval of the root's document that it applies to an open basis step moves approved_version
-- and leaves the basis columns behind; when this release runs again, its reader takes the moved
-- row as that act's end of the basis (a newer version falling back to the kept approval), and its
-- next write of the row clears what is left. A version or a review it applies to a pending step
-- ends no wait, and this release finds that step still pending; its pending-basis seed at boot
-- reads the ancestor's request again. It finds no step by its basis, so an ancestor's sign-off
-- does not wait on resting roots and a request's close ends no pending step while it runs, and
-- its phase tasks name no view. Its PutIssue leaves view_of as the row holds it.
--
-- There is no down file: migrations here are forward-only.
alter table design_gates add column basis_issue text;
alter table design_gates add column basis_artifact_id text;
alter table design_gates add column basis_version integer check (basis_version > 0);
alter table design_gates add column basis_approved_seq bigint check (basis_approved_seq > 0);
alter table design_gates add column basis_approved_at timestamptz;
alter table design_gates add column basis_approver text;
alter table design_gates add column basis_serves text[];
alter table design_gates add column basis_reason text;
alter table design_gates add column basis_session text;
alter table design_gates add column basis_registered_at timestamptz;
alter table design_gates add column basis_pending_ask text;

alter table design_gates add column kept_person_version integer check (kept_person_version > 0);
alter table design_gates add column kept_person_seq bigint check (kept_person_seq > 0);
alter table design_gates add column kept_person_at timestamptz;

alter table design_gates add column rests_on text;

create index design_gates_basis_artifact_id on design_gates (basis_artifact_id) where basis_artifact_id is not null;
create index design_gates_basis_pending_ask on design_gates (basis_pending_ask) where basis_pending_ask is not null;
create index design_gates_basis_issue on design_gates (basis_issue) where basis_issue is not null;
create index design_gates_rests_on on design_gates (rests_on) where rests_on is not null;

alter table issues add column view_of text;
