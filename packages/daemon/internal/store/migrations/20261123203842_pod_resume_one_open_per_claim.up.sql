-- 20261123203842_pod_resume_one_open_per_claim.up.sql — every resume and backstop names a claim,
-- and one open resume and one open backstop per claim, on a database that ran the claim of each
-- in an earlier shape.
--
-- 20261123203838 names on each resume the claim its wake brought back and holds one open resume
-- per claim (pod_resumes_one_open_per_claim, in place of 20261105183200's one open resume per
-- role), and 20261123203839 does the same for the backstops (pod_wake_backstops_open_claim, in
-- place of pod_wake_backstops_open). Each names its role's own claim on every row it finds, keeps
-- no default on the column and refuses the empty string. Both took that shape before any release
-- carried them, and a database can have run either migration in an earlier shape and recorded it,
-- so it never runs the present one. Three such shapes stood:
--   - the pod_resume_claim migration naming each resume's claim and keeping the one open resume
--     per role;
--   - the same beside 20261122074350, a migration no release carries, which made the open resume
--     one per claim;
--   - the head 4caa2ff1, whose pod_resume_claim migration added pod_resumes.claim at the empty
--     string and named no claim on any row, whose 20261122074350 kept the one open resume per role
--     for the rows of no claim beside a partial one open resume per claim for the others
--     (`claim = ''` and `claim <> ''`), and whose pod_wake_backstop_claim migration did the same to
--     pod_wake_backstops.
-- On the first the one open resume per role still stands, and it refuses the wake of one claim
-- while another claim of its role holds an open resume: a role or agent message for it, and a
-- pause's owed launch of it, queue nothing, which is the defect 20261123203838 closes. On the
-- third a resume or backstop of no claim is read by no claim, which reads its own rows alone, so a
-- worker's open resume is never stamped and its backstop never fires for it.
--
-- This brings each to the present shapes, unconditionally, and on a database that ran those it
-- changes no row and leaves the same indexes and constraints. For each table: it drops both open
-- indexes where they stand, whatever their predicate, since the one per claim of the head 4caa2ff1
-- would refuse the naming below; names its role's own claim on every row that names none, as
-- 20261123203838 and 20261123203839 do (claim.NewToken's `legion-<project>-<issue,
-- lowercased>-<role>`, where project is the project token); drops the column's default and puts
-- the check that refuses the empty string back over every row; then, of two open rows of one
-- claim, which the naming can make where a row of no claim stood beside its role's own claim's, it
-- closes every open resume but the newest, at the time of the newer, as 20261105183200 closed the
-- open resumes of a role, and deletes every open backstop but the newest, as 20261123203839's way
-- back deletes those of a role; then it creates the one open row per claim over every row. Last it
-- deletes 20261122074350's version row. What the close costs is the wake of each older open resume
-- of a claim, which a later wake of its claim takes again, and the delete each older open backstop
-- of a claim, which a release arms again. The tables hold a row per wake and per release, so the
-- rewrite of each index is small.
--
-- The release before this one has none of this release's migrations, and the way backs of
-- 20261123203838 and 20261123203839 drop the indexes of one open row per claim, put those per role
-- back and drop the columns, so this migration's way back undoes nothing of the schema.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon on that database stopped and before the
-- release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. A rollback past it
-- runs this block before 20261123203838's and 20261123203839's own. The delete of this migration's
-- row is what makes the version the store reports that release's own again, and is owed to the
-- roll-forward, which then runs this file again, and it changes no row then.
--
-- begin;
-- delete from schema_version where version = 20261123203842;
-- commit;
drop index if exists pod_resumes_one_open_per_role;
drop index if exists pod_resumes_one_open_per_claim;
update pod_resumes set claim = 'legion-' || project || '-' || lower(issue) || '-' || role where claim = '';
alter table pod_resumes alter column claim drop default, drop constraint if exists pod_resumes_claim_check,
  add constraint pod_resumes_claim_check check (claim <> '');
update pod_resumes older set closed_at = newer.at
  from pod_resumes newer
  where older.first_turn_at is null and older.closed_at is null and newer.first_turn_at is null and newer.closed_at is null
    and older.project = newer.project and older.issue = newer.issue and older.claim = newer.claim and older.id < newer.id;
create unique index pod_resumes_one_open_per_claim on pod_resumes (project, issue, claim)
  where first_turn_at is null and closed_at is null;
drop index if exists pod_wake_backstops_open;
drop index if exists pod_wake_backstops_open_claim;
update pod_wake_backstops set claim = 'legion-' || project || '-' || lower(issue) || '-' || role where claim = '';
alter table pod_wake_backstops alter column claim drop default, drop constraint if exists pod_wake_backstops_claim_check,
  add constraint pod_wake_backstops_claim_check check (claim <> '');
delete from pod_wake_backstops b where not b.fired and exists (select 1 from pod_wake_backstops n
  where not n.fired and n.project = b.project and n.issue = b.issue and n.claim = b.claim and n.id > b.id);
create unique index pod_wake_backstops_open_claim on pod_wake_backstops (project, issue, claim) where not fired;
delete from schema_version where version = 20261122074350;
