-- 20261122074350_one_open_resume_per_claim.up.sql — one open resume per claim, not per role.
--
-- A claim has at most one resume no first turn has stamped and nothing has closed: the one its
-- wake took (store.RecordPodWakeTx), whose insert is what takes the wake. Two claims can hold one
-- role of an issue: the agent of a key works a step of another role than its name, beside that
-- role's own claim of an earlier version of the plan. The index of one open resume per role
-- (20261105183200's pod_resumes_one_open_per_role) then refused the wake of one claim while the
-- other's resume stood open: a role or agent message for it, and a pause's owed launch of it,
-- queued nothing, so it stayed released with its message unread, and a delivery's wake resumed it
-- with no resume of its own, so its first turn was stamped nowhere. 20261121183336's header said
-- that index stays because a role's step has one worker and only that worker's wake takes a
-- resume; every claim of a role can be woken, so that was wrong.
--
-- pod_resumes_one_open_per_claim is unique on (project, issue, claim) for an open resume that names
-- a claim, which every resume this release takes does. A resume the release before wrote names no
-- claim, the empty string 20261121183336 gave every row it found, and it is each claim of its
-- role's, as that release meant it: the index of one open resume per role is kept under its name for
-- the rows that name no claim, and the take refuses the wake of any claim of a role while such a
-- resume of the role is open (RecordPodWakeTx), as that release's one open resume per role did. The
-- reads, the stamps and the closes that name a claim reach its own resume, or such a resume of the
-- role the claim holds now, and never another claim's.
--
-- The release before names each column it reads and writes, so it writes no claim and every resume
-- it takes names none; its `on conflict do nothing` meets the index kept for those rows, as it met
-- the one index before.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with this release's daemon stopped and before the release
-- before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. The release before allows one
-- open resume per role, so the way back first closes every open resume of a role but the newest, at
-- the time of the newer, as 20261105183200 closed them; then it drops this index and puts the one
-- open resume per role back over every row. A rollback past 20261121183336 runs this block before
-- that migration's own. The delete of this migration's row is what makes the version the store
-- reports that release's own again, and is owed to the roll-forward, which then runs this file
-- again. What the way back costs is the wake of each older open resume of a role, which a later wake
-- of its claim takes again.
--
-- begin;
-- update pod_resumes older set closed_at = newer.at from pod_resumes newer where older.first_turn_at is null and older.closed_at is null and newer.first_turn_at is null and newer.closed_at is null and older.project = newer.project and older.issue = newer.issue and older.role = newer.role and older.id < newer.id;
-- drop index if exists pod_resumes_one_open_per_claim;
-- drop index if exists pod_resumes_one_open_per_role;
-- create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role) where first_turn_at is null and closed_at is null;
-- delete from schema_version where version = 20261122074350;
-- commit;
drop index pod_resumes_one_open_per_role;
create unique index pod_resumes_one_open_per_role on pod_resumes (project, issue, role)
  where first_turn_at is null and closed_at is null and claim = '';
create unique index pod_resumes_one_open_per_claim on pod_resumes (project, issue, claim)
  where first_turn_at is null and closed_at is null and claim <> '';
