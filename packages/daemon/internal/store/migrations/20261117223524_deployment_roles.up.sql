-- 20261117223524_deployment_roles.up.sql — a role the deployment defines, and the phase step that
-- such a role works in.
--
-- A deployment defines roles of its own beside Legion's six, and a step of an issue's plan can
-- name one. A claim, a role's phases row and the idle release of a pod then name such a role, so
-- each of their role checks takes a name of the role grammar of Dispatch's step record,
-- ^[a-z][a-z0-9-]{0,31}$ (claim.IsRole), which each of the six matches. The claims check keeps the
-- controller, and claims_controller_shape still holds the controller to a claim on no issue and no
-- tree. The release of an unowned pod still names no role. An issue whose running step is of such
-- a role stands at the phase step (phase.Step), and a hold taken from it has held_from step, so
-- the two phase checks take step, after reviewing, where phase.All lists it. The phase check keeps
-- awaiting_children as well, which main's 20261114232555 wrote below this version: a database main
-- migrated can hold a parent in that phase when this file runs, and a list without it would refuse
-- that row and the upgrade (SQLSTATE 23514). Every row the upgrade finds names one of the six and a
-- phase the checks took before, so each check is written again whole and no row changes.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with the daemon stopped, before the earlier image is pinned. The release before this one has no
-- role of the deployment and no phase step, and the checks it wrote refuse a row of either, naming
-- the check and no row. So the way back first refuses while such a row stands, and names the
-- first it finds: an issue at step or held from it, then a claim, a phases row or an idle release
-- of a role outside the six (the controller's claim and an unowned release pass). Finish or stop
-- each issue whose plan names a role of the deployment first. Its rows of that role stay after the
-- issue is done, so delete each row the way back names once its issue is done or stopped
-- (delete from claims where token = '<token>'; delete from phases where issue = '<issue>' and
-- role = '<role>'; delete from issue_releases where issue = '<issue>'), and run the way back
-- again. With none left, it writes each check again as the release before wrote it and deletes
-- this migration's version row, so the roll forward's Migrate runs this file again:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   do $$
--   declare
--     found_key text;
--     found_role text;
--   begin
--     select key into found_key from issues
--       where phase = 'step' or held_from = 'step' order by key limit 1;
--     if found then
--       raise exception 'issue % stands at step or is held from it, which the earlier release cannot read: finish or stop its task, then run this again', found_key;
--     end if;
--     select token, role into found_key, found_role from claims
--       where role not in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger', 'controller')
--       order by token limit 1;
--     if found then
--       raise exception 'claim % is of the role %, which the earlier release has not: once its issue is done or stopped, delete its row, then run this again', found_key, found_role;
--     end if;
--     select issue, role into found_key, found_role from phases
--       where role not in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')
--       order by issue, role limit 1;
--     if found then
--       raise exception 'the phases row of % on % names a role the earlier release has not: once the issue is done or stopped, delete the row, then run this again', found_role, found_key;
--     end if;
--     select issue, role into found_key, found_role from issue_releases
--       where kind = 'idle' and role not in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')
--       order by issue limit 1;
--     if found then
--       raise exception 'the idle release of % names the role %, which the earlier release has not: once the release ends, or the issue is done or stopped, delete its row, then run this again', found_key, found_role;
--     end if;
--   end
--   $$;
--   alter table issues drop constraint issues_phase_check;
--   alter table issues add constraint issues_phase_check check (phase in (
--     'admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing',
--     'reviewing', 'accepting', 'retro', 'merging', 'done', 'held'));
--   alter table issues drop constraint issues_held_from_check;
--   alter table issues add constraint issues_held_from_check check (held_from in (
--     'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
--     'done'));
--   alter table claims drop constraint claims_role_check;
--   alter table claims add constraint claims_role_check check (role in (
--     'architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger', 'controller'));
--   alter table phases drop constraint phases_role_check;
--   alter table phases add constraint phases_role_check check (
--     role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger'));
--   alter table issue_releases drop constraint issue_releases_role_check;
--   alter table issue_releases add constraint issue_releases_role_check check (
--     (kind = 'idle' and role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger'))
--     or (kind = 'unowned' and role = ''));
--   delete from schema_version where version = 20261117223524;
--   commit;
--   SQL
alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'awaiting_children', 'planning', 'implementing', 'testing',
  'reviewing', 'step', 'accepting', 'retro', 'merging', 'done', 'held'));
alter table issues drop constraint issues_held_from_check;
alter table issues add constraint issues_held_from_check check (held_from in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'step', 'accepting', 'retro',
  'merging', 'done'));
alter table claims drop constraint claims_role_check;
alter table claims add constraint claims_role_check check (
  role = 'controller' or role ~ '^[a-z][a-z0-9-]{0,31}$');
alter table phases drop constraint phases_role_check;
alter table phases add constraint phases_role_check check (role ~ '^[a-z][a-z0-9-]{0,31}$');
alter table issue_releases drop constraint issue_releases_role_check;
alter table issue_releases add constraint issue_releases_role_check check (
  (kind = 'idle' and role ~ '^[a-z][a-z0-9-]{0,31}$')
  or (kind = 'unowned' and role = ''));
