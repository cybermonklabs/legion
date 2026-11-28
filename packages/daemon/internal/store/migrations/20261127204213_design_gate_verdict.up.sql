-- 20261127204213_design_gate_verdict.up.sql -- a design-gate step records Dispatch's verdict on
-- whether the approval of its document stands, beside the two version numbers, and the gate opens
-- on that verdict where Dispatch gives one (dispatch://INFRA-6065 R2, Departures D1 to D3); and
-- each boot of this release records itself in design_gate_verdict_boots, so a boot can tell that
-- an earlier release booted since.
--
-- The verdict (record.GateVerdict), all null on a row in version mode:
--
-- design_gates.latest_digest_version is the newest version of the document Dispatch gave a
-- verdict for, and approval_current that verdict: whether the approval stands for that version,
-- whatever version it approved. The check design_gates_verdict_check holds the two null together
-- or set together, so a row is in hash mode exactly while latest_digest_version is set and in
-- version mode while it is null. latest_digest is that version's content_sha256 where the fact
-- named it, latest_digest_seq and latest_digest_at the mark of the fact that gave the verdict (an
-- artifact.digest, a review at or above the latest version, or a read of Dispatch whose approval
-- carried digest_owed false), and approved_digest the approved_sha256 an artifact.digest named for
-- the approval the row holds. The two digests are for a reader to see: the daemon decides nothing
-- from them.
--
-- In version mode the gate opens as before, on an approval of its latest version. In hash mode it
-- opens on the verdict: approval_current true at a latest_digest_version at or above
-- latest_version (record.GateOpen). A step a basis opened (dispatch://INFRA-3320) reads the
-- version rule in both modes, since Dispatch's verdict on the root's own document answers for an
-- approval of it that the step does not have. A fact moves a row between the two modes on what it
-- carries, not by a flag that stays set: a version, a digest or a read that carries digest_owed
-- puts it in hash mode, and a newer version or a read that carries none puts it back in version
-- mode with every verdict column cleared.
--
-- digest_owed is generated, latest_digest_version < latest_version, and stored: null in version
-- mode, true while a newer version waits for Dispatch's digest of it, during which the gate reads
-- closed to every reader that holds work on it. No write sets it (record.DigestOwed derives the
-- same value from the two versions): a flag a late artifact.version set after the digest that
-- answers it had arrived would close the gate with no digest left to clear it.
--
-- told_open (Departure D1) is, in hash mode, whether the step read open when the daemon last told
-- the tree's standing: a version that owes its digest leaves it as it was, so the fill tells
-- nobody of a change (record.ToldStanding). It is null in version mode, where the told standing
-- is the gate's own.
--
-- basis_root_version (Departure D2) is the version of the root's own document a basis
-- registration, open or pending, stood at (record.GateBasis.RootVersion). It is written whole
-- with the other basis_* columns and null with them. A version of that document that owes its
-- digest raises latest_version and leaves the basis, so nothing else on a pending row keeps that
-- version; a basis from before this migration has none, and the daemon falls back to
-- latest_digest_version for a pending one (record.RegisteredRootVersion). A step open on a
-- basis reads its root version from approved_version, which both releases write at the
-- registration and this one keeps there while a digest is owed, so only a pending step reads
-- basis_root_version.
--
-- carried (Departure D3) marks a row the re-admission's carry copied from an old root's gate
-- (admit's carriedGate), on the old root's document: such a copy is never read from Dispatch
-- before a start. The update below marks each copy this migration finds: a row whose issue holds a
-- hand-over (handed_over_roots) that carried a gate at the issue's current generation, on a
-- document a row of another issue names. A row of the issue's own document is no copy and stays
-- false.
--
-- design_gate_verdict_boots holds one row for each boot of this release, keyed by the boot's
-- daemon_boot id: the store's RecordBoot writes it in the transaction that writes the daemon_boot
-- row, so a boot of this release never leaves a daemon_boot row without one. No release before
-- this one writes it.
--
-- Every row this migration finds is in version mode, with no told standing and no root version,
-- so each gate open before it is open after it. A daemon of the release before names none of
-- these columns: its reads leave them out, its inserts leave them null and carried at its
-- default, and its upserts leave them as they are, which no check refuses. So it reads every gate
-- by the version rule. It does not know that a row owes a digest, so it never settles what such a
-- version held back: the end of a basis that the version rule ends at once (R3, "A version").
-- Its writes leave the verdict, the told standing and the root version as this release last wrote
-- them. So a rollback runs the way back below first, for the project that rolls back, with that
-- project's daemon stopped. And each boot of this release, before it applies any fact, records
-- itself in daemon_boot and in design_gate_verdict_boots in one transaction (the store's
-- RecordBoot); where the project's newest earlier daemon_boot row has no row of
-- design_gate_verdict_boots, a boot of an earlier release, that same transaction puts each of the
-- project's rows in version mode and clears every column this migration adds, ending no basis and
-- telling nothing, since the columns cannot show what the earlier release did, and the boot
-- queues the gate seed of each registered step of each tree in the workflow, whose read of
-- Dispatch gives each row its verdict again and tells each change from the standing version mode
-- reads (record.ResetGatesAfterEarlierRelease). The first boot after this migration finds the
-- release before's boot there, changes no row, which is in version mode already, and queues the
-- seeds.
--
-- The way back acts on the rows of one project's issues alone, the project whose daemon rolls
-- back: replace <PROJECT> with its key, project in its legion.yaml, the part of each of its issue
-- keys before the hyphen. It first settles what a version owing its digest held back: on each row
-- that owes its digest, it ends each basis, open or pending, whose root version is below
-- latest_version, as version mode would have ended it at once, so the step is closed. An open
-- step's root version is its approved_version, and it falls back to the person's approval it kept
-- (record.CloseBasis); a pending step's is basis_root_version, or latest_digest_version where it
-- has none, and its wait ends (record.EndPendingBasis); each end counts one move of the basis in
-- basis_moves, as the daemon's own write counts it. A row that owes no digest keeps its basis:
-- this release ended each basis below the latest version when that version settled. Then it
-- clears carried and marks the copies again as this migration does, as the boot's reset does, and
-- puts each of those rows in version mode with every other column this migration adds cleared. It
-- drops nothing and deletes no version row: the columns and design_gate_verdict_boots stay for the
-- daemons of this release that serve other projects from the same database, the release before
-- names none of them, its Migrate applies only the migrations it embeds and passes over this
-- one's row, and the roll forward finds this migration applied. The earlier release's boot writes
-- no row of design_gate_verdict_boots, so the first boot of this release after it resets the
-- project's rows as above. The way back tells nobody: after it, read each gated tree's steps
-- (legion state) and tell the merger of a tree whose step it closed (docs/kubernetes.md). Run it
-- with the project's daemon stopped; it is one transaction, and a second run changes nothing.
--
-- begin;
-- update design_gates set approved_version = kept_person_version, approved_seq = kept_person_seq, approved_at = kept_person_at,
--   kept_person_version = null, kept_person_seq = null, kept_person_at = null, basis_issue = null, basis_artifact_id = null,
--   basis_version = null, basis_approved_seq = null, basis_approved_at = null, basis_approver = null, basis_serves = null,
--   basis_reason = null, basis_session = null, basis_registered_at = null, basis_pending_ask = null, basis_moves = basis_moves + 1
--   where issue in (select key from issues where project = '<PROJECT>')
--   and digest_owed and coalesce(basis_artifact_id, '') <> '' and coalesce(basis_pending_ask, '') = ''
--   and coalesce(approved_version, basis_root_version, latest_digest_version) < latest_version;
-- update design_gates set basis_issue = null, basis_artifact_id = null, basis_version = null, basis_approved_seq = null,
--   basis_approved_at = null, basis_approver = null, basis_serves = null, basis_reason = null, basis_session = null,
--   basis_registered_at = null, basis_pending_ask = null, basis_moves = basis_moves + 1
--   where issue in (select key from issues where project = '<PROJECT>')
--   and digest_owed and coalesce(basis_artifact_id, '') <> '' and coalesce(basis_pending_ask, '') <> ''
--   and coalesce(basis_root_version, latest_digest_version) < latest_version;
-- update design_gates set carried = false where carried and issue in (select key from issues where project = '<PROJECT>');
-- update design_gates set carried = true from handed_over_roots, issues
--   where handed_over_roots.issue = design_gates.issue and issues.key = design_gates.issue and issues.project = '<PROJECT>'
--   and handed_over_roots.generation = issues.generation and handed_over_roots.gate_carried
--   and exists (select 1 from design_gates other where other.artifact_id = design_gates.artifact_id and other.issue <> design_gates.issue);
-- update design_gates set latest_digest_version = null, approval_current = null, latest_digest = null, latest_digest_seq = null,
--   latest_digest_at = null, approved_digest = null, told_open = null, basis_root_version = null
--   where issue in (select key from issues where project = '<PROJECT>');
-- commit;
alter table design_gates add column latest_digest_version integer check (latest_digest_version > 0);
alter table design_gates add column approval_current boolean;
alter table design_gates add constraint design_gates_verdict_check
  check ((latest_digest_version is null) = (approval_current is null));
alter table design_gates add column latest_digest text;
alter table design_gates add column latest_digest_seq bigint check (latest_digest_seq > 0);
alter table design_gates add column latest_digest_at timestamptz;
alter table design_gates add column approved_digest text;
alter table design_gates add column digest_owed boolean generated always as (latest_digest_version < latest_version) stored;

alter table design_gates add column told_open boolean;

alter table design_gates add column basis_root_version integer check (basis_root_version > 0);

alter table design_gates add column carried boolean not null default false;

create table design_gate_verdict_boots (
  boot bigint primary key references daemon_boot (id)
);

update design_gates set carried = true
from handed_over_roots, issues
where handed_over_roots.issue = design_gates.issue
  and issues.key = design_gates.issue
  and handed_over_roots.generation = issues.generation
  and handed_over_roots.gate_carried
  and exists (select 1 from design_gates other
              where other.artifact_id = design_gates.artifact_id and other.issue <> design_gates.issue);
