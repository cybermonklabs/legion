-- 20261125181828_p0_bound.up.sql — the P0 start and the P0 bound set while the daemon runs
-- (dispatch://INFRA-5636).
--
-- A P0 issue starts outside the admission cap, and the P0 bound limits how many P0 issues may hold
-- a slot at once. legion.yaml's admission_p0_bound gives the bound the daemon boots with, none when
-- the file gives none. project_p0_bound_changes holds one row per change the controller (the
-- `legion` tool's set_p0_bound and clear_p0_bound) or the operator (`legion project set-p0-bound`
-- and `clear-p0-bound`) made while the daemon runs, appended and never rewritten. project is the
-- project's Dispatch key, the spelling project_pauses holds. mode is what the change did: `bound`
-- sets the bound to bound, at least 1; `unbounded` sets it to none, whatever legion.yaml says; and
-- `cleared` takes the live value away, which returns the project to legion.yaml's. Who made the
-- change has a pause's shape (project_pauses_by_session): the kind `controller` with the
-- controller's session id, or the kind `operator` with no session. reason is why, which may not be
-- white space alone.
--
-- The bound in force is read off the project's newest row on every admission pass (store's
-- P0BoundTx and EffectiveP0Bound), so a change is in force from the pass of its own fact on, with no
-- restart, and a change made before a restart holds after it. The index serves that read.
--
-- The three columns of issues hold the priority Dispatch resolves each issue to, the one admission
-- reads (record.Resolved, J1 of dispatch://INFRA-5793). resolved_known says Dispatch sent
-- `resolved_priority` at all; false on every row this migration finds, and on every row a Dispatch
-- release before INFRA-5793 wrote, which admission then reads by the issue's own priority alone.
-- resolved_priority is Dispatch's level, 0 for P0 to 3 for P3, null for none, as the priority
-- column holds the issue's own, and null wherever resolved_known is false. priority_from is
-- `priority_from`, the key of the issue the resolved priority comes from, empty where it is the
-- issue's own or none. The first boot listing after Dispatch resolves priorities replaces each row
-- the listing shows, at the row's own sequence too (admit's applyListed), so an inherited P0 that
-- Dispatch's upgrade made takes effect with no edit of the issue.
--
-- No outbox kind is added, so outbox_kind_check stays as the last migration that wrote it left it.
--
-- Migrations here are forward-only and this one has no down file. A daemon of an earlier release on
-- the same database reads no row of the table and names none of the three columns: its insert
-- leaves them at their defaults and its update leaves them as they stand. So a rollback needs no
-- statement: the table and the columns stay, and a roll forward reads each row's resolved priority
-- as this release last wrote it, until the issue's next event or the boot listing, which replaces
-- it where Dispatch shows another. That release refuses `admission_p0_bound` in legion.yaml as a
-- key it does not know, so a rollback takes the key out of the file first.
create table project_p0_bound_changes (
  id bigserial primary key,
  project text not null check (project ~ '^[A-Z][A-Z0-9]*$'),
  mode text not null check (mode in ('bound', 'unbounded', 'cleared')),
  bound integer check (bound >= 1),
  by_kind text not null check (by_kind in ('controller', 'operator')),
  by_session text not null default '',
  reason text not null check (reason ~ '[^[:space:]]'),
  changed_at timestamptz not null,
  constraint project_p0_bound_changes_bound check ((mode = 'bound') = (bound is not null)),
  constraint project_p0_bound_changes_by_session check ((by_kind = 'controller') = (by_session <> ''))
);
create index project_p0_bound_changes_newest on project_p0_bound_changes (project, id desc);
alter table issues
  add column resolved_known boolean not null default false,
  add column resolved_priority smallint check (resolved_priority between 0 and 3),
  add column priority_from text not null default '',
  add constraint issues_resolved_priority_known check (resolved_known or resolved_priority is null);
