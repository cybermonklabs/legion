-- 20261125114700_plan_roles.up.sql — the roles an owner's plan defines (record's plan.go).
--
-- plan_roles holds each role a plan defines with a role block, for an issue at a generation: its
-- name (the block's id), the version of the plan that wrote it, its type, its model ('' for the
-- default model) and its prompt, the block's body. Each version given writes its roles over the
-- earlier version's (record.Postgres.PutPlanRoles), so the rows of a generation are the roles of
-- the version in force, and the carry of a plan to a new generation copies them
-- (CarryPlan). The delete of a version deletes the roles it wrote, by the cascade from
-- issue_plans. A launch reads a role from here (PlanRoleOf) rather than from Dispatch, so it waits
-- on no read of the plan document. A role's name is lower case and at most 32 characters, the name
-- a step of a task can write.
--
-- The release before this one boots without the table. It names the table in no statement, and
-- it refuses a plan that defines a role, so no plan it takes writes a row here.
--
-- The way back below drops the table, so a rollback takes away every role a plan defines. At its
-- boot, the release before stops each task that still has a step of such a role to run. The
-- reason it gives is "step <n> names the role <role>, which the deployment no longer defines".
-- Two kinds of task are not stopped at boot. A task whose plan role has the name of a role of the
-- deployment is not stopped. The release before runs the deployment's role in its place. A root
-- that waits on a person before its first step is not stopped at boot. It is stopped when it
-- moves into that step. After the roll forward, the owner of each stopped task gives a new version
-- of its plan. Legion takes only a version later than the one in force. The same version given
-- again is dropped as a duplicate, and the task stays stopped.
--
-- There is no down file. This directory keeps none, so the way back is the transaction below, run
-- with the daemon stopped, before the earlier image is pinned. A rollback that also goes past the
-- issue plans' tables runs it first, since this table's foreign key stands on issue_plans:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   drop table if exists plan_roles;
--   delete from schema_version where version = 20261125114700;
--   commit;
--   SQL
create table plan_roles (
  issue text not null,
  generation bigint not null,
  role text not null constraint plan_roles_role_check check (role ~ '^[a-z][a-z0-9-]{0,31}$'),
  plan_version integer not null,
  type text not null,
  model text not null default '',
  prompt text not null,
  primary key (issue, generation, role),
  foreign key (issue, generation, plan_version) references issue_plans (issue, generation, version) on delete cascade
);
