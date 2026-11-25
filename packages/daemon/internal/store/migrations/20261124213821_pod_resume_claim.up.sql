-- 20261124213821_pod_resume_claim.up.sql — the claim a wake brought back, on its resume, and one
-- open resume per claim.
--
-- pod_resumes.claim is the token of the claim the wake that took the resume brings back
-- (store.PodResume.Claim). Two claims of one issue can hold one role (record.WorkerClaim). A resume
-- read or closed by its role alone was then each of those claims', so a cancel of a
-- release made while the project is paused (store's stopReleasedForPause) marked a claim owed the
-- launch a wake took for another claim of its role, and a pause's first stop of one claim closed
-- the resume of the other (store.CloseRoleOpenResumeTx). And the index of one open resume per role
-- (20261105183200's pod_resumes_one_open_per_role) refused the wake of one claim while the other's
-- resume stood open: a role or agent message for it, and a pause's owed launch of it, queued
-- nothing, so it stayed released with its message unread, and a delivery's wake resumed it with no
-- resume of its own, so its first turn was stamped nowhere. The reads, the stamps and the closes
-- now take the claim, and a claim has at most one resume no first turn has stamped and nothing has
-- closed (pod_resumes_one_open_per_claim, in place of the one open resume per role): the one its
-- wake took (store.RecordPodWakeTx), whose insert is what takes the wake.
--
-- The update below names a claim on every row the table already holds. The release before took a
-- wake for the worker of a role, the claim whose token it builds from the role, so a resume it
-- wrote is that claim's: claim.NewToken's `legion-<project>-<issue, lowercased>-<role>`, where
-- pod_resumes.project is the project token. The open resumes it left are one per role, so they are
-- one per claim too. Every resume then names a claim, and the column keeps no default and refuses
-- the empty string, so no reader meets a resume of no claim.
--
-- The release before this one names each column it reads and writes (RecordPodWakeTx and the
-- resume reads), so it never reads this one, and an insert of it would fail on the column, which
-- takes no default. Every daemon on the database moves together, and the way back below drops the
-- column before that release starts.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon on that database stopped and before the
-- release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The release before
-- allows one open resume per role, so the way back first closes every open resume of a role but
-- the newest, at the time of the newer, as 20261105183200 closed them; then it drops the index of
-- one open resume per claim, puts the one open resume per role back over every row and drops the
-- column. The drop and the delete of this migration's row go together: the delete is what makes the
-- version the store reports that release's own again, and both are owed to the roll-forward, which
-- then runs this file again, adds the column and names each resume again. What the way back costs
-- is the wake of each older open resume of a role, which a later wake of its claim takes again,
-- and the claim of each resume open at the rollback that another claim of its role took, such as a
-- key's agent on its step: the roll-forward names the role's own claim on it, whose first turn
-- then never stamps it, and the next release of the pod closes it.
--
-- begin;
-- update pod_resumes older set closed_at = newer.at from pod_resumes newer where older.first_turn_at is null and older.closed_at is null and newer.first_turn_at is null and newer.closed_at is null and older.project = newer.project and older.issue = newer.issue and older.role = newer.role and older.id < newer.id;
-- drop index if exists pod_resumes_one_open_per_claim;
-- drop index if exists pod_resumes_one_open_per_role;
-- create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role) where first_turn_at is null and closed_at is null;
-- alter table pod_resumes drop column if exists claim;
-- delete from schema_version where version = 20261124213821;
-- commit;
alter table pod_resumes add column claim text not null default '';
update pod_resumes set claim = 'legion-' || project || '-' || lower(issue) || '-' || role;
alter table pod_resumes alter column claim drop default, add constraint pod_resumes_claim_check check (claim <> '');
drop index pod_resumes_one_open_per_role;
create unique index pod_resumes_one_open_per_claim on pod_resumes (project, issue, claim)
  where first_turn_at is null and closed_at is null;
