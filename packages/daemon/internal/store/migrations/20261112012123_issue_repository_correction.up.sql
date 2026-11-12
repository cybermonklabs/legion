-- 20261112012123_issue_repository_correction.up.sql -- an issue's repository is Dispatch's fact,
-- and the record follows a correction of it instead of refusing one (INFRA-3136, R3).
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
-- none, which is what R3 means by recording the correction with the moment it happened: the
-- reconcile pass that replaces the issue's pod reads it to tell work saved before the correction
-- from work saved after it. The pair is written together, so a null moment and an empty
-- repo_before both read as "never corrected", which is every row written before this release.
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
-- into the new clone (R3), and this column is what the restore reads to tell the two apart; the
-- release that writes it and the restore that reads it are the later half of R3's work, so on
-- this schema alone every row takes the default and the restore behaves as it does today.
--
-- An older daemon binary on this schema writes none of the five columns and reads none of them:
-- its issue reads and its bundle writes name the columns it knows, and the new ones take their
-- defaults. It still carries the write-once guard, so it refuses a corrected repository as it
-- always did, and a correction this release applied stands under it with repo_before and
-- repo_corrected_at untouched for the roll forward.
--
-- `refused_repository` is the third thing one restore attempt can do, beside the `sent` and the
-- `refused_older` the attempt column was written with (20261103214558_workspace_bundle_pod_binding):
-- the row's repo is a repository the issue has left, so nothing is sent and the row stays. The
-- daemon never restores such a bundle. Its commits are of another repository's history, and the
-- new clone holds none of their bases, so an apply there would either fail or push work of one
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
alter table workspace_bundles add column repo text not null default '';
alter table workspace_bundles drop constraint workspace_bundles_attempt_outcome;
alter table workspace_bundles
  add constraint workspace_bundles_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older', 'refused_repository') end);
