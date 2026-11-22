-- 20261112012123_issue_repository_correction.up.sql -- an issue's repository is Dispatch's fact,
-- and the record follows a correction of it instead of refusing one (dispatch://INFRA-3136/spec R3).
--
-- Until now the record's repository, default branch and GitHub spelling were write-once: the
-- upsert in record.(*Postgres).PutIssue carried a condition that matched no row whose repository
-- differed, and the write was refused. So a repository a person corrected in Dispatch could not
-- reach a running tree at all. The guard goes with this migration, the three columns are left out
-- of the upsert's update set instead — a copy of the issue read before a correction writes no old
-- repository back — and one statement of its own, record.(*Postgres).CorrectIssueRepository,
-- applies the correction and writes the four columns this migration adds to issues.
--
-- repo_before is the repository the record held before the newest correction, empty when there
-- has been none: what the next task of each worker names beside the new repository, so a role
-- reading its task knows which clone its earlier work is in, and what tells a restore that a
-- saved bundle is of the repository the issue has left.
--
-- repo_corrected_at is the moment the daemon applied that correction, null when there has been
-- none, which is what the spec means by recording the correction with the moment it happened: every
-- launch of the issue's claims from then on names it beside the repository the issue left
-- (daemon's AddressingFragment), and `legion state --json` shows it on the issue as
-- repoCorrectedAt. The pair is written together, so a null moment and an empty repo_before both
-- read as "never corrected", which is every row written before this release.
--
-- repo_change_pending and repo_change_reason are a change Dispatch has made that the daemon has
-- not applied yet and why it has not, both empty when there is none:
-- record.(*Postgres).NoteIssueRepositoryChange writes the pair for a pass that read a corrected
-- repository and could not apply it, so the next pass and a person reading the record see what is
-- waiting rather than silence, and CorrectIssueRepository clears the pair as it applies the
-- change. Text rather than null for the same reason the record's other optional names are text:
-- nothing reads the difference between empty and absent here.
--
-- workspace_bundles.repo is the repository the saved work is of, empty for a row saved before
-- this release, whose repository nothing recorded. A bundle of the old repository is not applied
-- into the new clone (dispatch://INFRA-3136/spec R3), and this column is what the restore reads
-- to tell the two apart. This release ships both halves: the idle release writes it from the
-- repository the released pod itself was built from (daemon's bundleIssueWorkspace,
-- store.PutWorkspaceBundle), and the restore refuses a bundle whose repository the issue has left
-- by it (store.TakeWorkspaceBundleForRestore, runtime.WorkspaceBundleRefusedRepository). A row written
-- before this release holds the default and refuses nothing.
--
-- workspace_bundles_refused keeps the bundles the restore refused for their repository, one row
-- per refusal under an id of its own, so two refusals of one issue are two rows. The take moves
-- the row there in the transaction that refuses it (store.TakeWorkspaceBundleForRestore). Left
-- in workspace_bundles, the row would be gone at the issue's next release: that release saves
-- its own work over the issue's one row, or deletes the row when it saves nothing (daemon's
-- bundleIssueWorkspace), and the end of the issue's work deletes it too (daemon's
-- dropReleasedWork). The issue is told that the daemon keeps this work, so no such path may reach
-- it. The row keeps what the workspace_bundles row held, with refused_pod_uid and refused_at for
-- the pod the refusal answered and when. Nothing in the daemon reads this table again, changes a
-- row of it or deletes one: it is an operator's, who finds a row by the issue and the release the
-- message on the issue names, and deletes it once the work is carried out.
--
-- claims.update_repository is the repository the pod of the claim's update round was built from,
-- as that pod named it at the round's open, whatever drift opened the round
-- (supervise.Claim.UpdateRepository, the daemon's groupDrift), and '' on a round whose pod named
-- none and while no round is open. A correction can land while any round runs, so where the
-- record holds another repository when the daemon writes the message on an issue whose pod went
-- before its round was over, that message names this repository's issue branch, and its
-- workspace where the pod's disk is a kept volume that is still there, beside the repository the
-- record holds then (the daemon's podWentMessage). On a round that a correction opened, the
-- round's reasons name both repositories too, but as text for a reader, and the record's as it
-- stood at the round's open. claims.update_repository_round is the round the name was written
-- with, and the claim store reads the name only while that round is the one update_round holds
-- (store's roundRepositoryOf). A name it reads that is no repository refuses the read of the
-- claims and names the claim, as a stored locator that does not validate does.
--
-- An older daemon binary on this schema writes none of the columns this migration adds and reads
-- none of them: its issue reads, its claim reads and writes and its bundle writes name the
-- columns it knows, and the new ones take their defaults. Its write of a claim this release wrote
-- leaves the two claims columns as they stood, through the end of a round this release opened
-- and the open of the claim's next round, which is why the name carries the round it was written
-- with: this release reads that next round as one that recorded no repository. It never reads
-- workspace_bundles_refused, so the refused work stays there for an operator through a roll back,
-- and no row of workspace_bundles is a refused one for its idle rule to count. It still carries
-- the write-once guard, so it refuses a corrected repository as it always did, and a correction
-- this release applied stands under it with repo_before and repo_corrected_at untouched for the
-- roll forward.
--
-- `refused_repository` is the third thing one restore attempt can do, beside the `sent` and the
-- `refused_older` the attempt column was written with (20261103214558_workspace_bundle_pod_binding):
-- the row's repo is a repository the issue has left, so nothing is sent. The take records the
-- attempt with this word and then moves the row, both in one transaction, so no committed row of
-- workspace_bundles holds it; the row moved keeps the attempt as refused_pod_uid and refused_at.
-- The daemon never restores such a bundle. Its commits are of another repository's history, and
-- the new clone holds none of their bases, so an apply there would either fail or push work of one
-- repository into another. The constraint is dropped and written again with the word beside the
-- two, which this migration may do because it adds the repo column the refusal reads: no
-- deployment holds a row with the new outcome, and an older daemon binary writes neither it nor
-- the column it rests on.
--
-- There is no down file: migrations here are forward-only.
alter table issues add column repo_before text not null default '';
alter table issues add column repo_corrected_at timestamptz;
alter table issues add column repo_change_pending text not null default '';
alter table issues add column repo_change_reason text not null default '';
alter table claims add column update_repository text not null default '',
  add column update_repository_round text not null default '';
alter table workspace_bundles add column repo text not null default '';
alter table workspace_bundles drop constraint workspace_bundles_attempt_outcome;
alter table workspace_bundles
  add constraint workspace_bundles_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'refused_repository') end);
create table workspace_bundles_refused (
  id              bigserial   primary key,
  issue           text        not null,
  release_id      bigint      not null,
  source_pod_uid  text        not null,
  repo            text        not null,
  bundle          bytea       not null,
  size_bytes      bigint      not null,
  at              timestamptz not null,
  head            text        not null,
  tips            jsonb       not null,
  bookmarks       jsonb       not null,
  refused_pod_uid text        not null,
  refused_at      timestamptz not null
);
