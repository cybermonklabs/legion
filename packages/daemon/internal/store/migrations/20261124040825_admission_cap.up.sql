-- 20261124040825_admission_cap.up.sql — the changes of the admission cap the controller or the
-- operator made while the daemon runs (dispatch://INFRA-5616).
--
-- project_admission_cap_changes holds one row per change, appended and never rewritten: the
-- controller's set_admission_cap and clear_admission_cap through the `legion` tool, and the
-- operator's `legion project set-admission-cap` and `clear-admission-cap`. project is the
-- project's Dispatch key, the spelling project_pauses holds. cap is the cap the change set, at
-- least 1, and null for a clear, which returns the project to the deployment's starting cap
-- (legion.yaml's admission_cap, else LEGION_ADMISSION_CAP). cap_before and cap_after are the caps
-- the daemon admitted under before the change and from it on, and ceiling is the deployment's
-- ceiling at the change (legion.yaml's admission_ceiling, else LEGION_ADMISSION_CEILING, else
-- admission_cap), so each row reads on its own as from what to what. Who changed it has a pause's
-- shape
-- (project_pauses_by_session): the kind `controller` with the controller's session id, or the kind
-- `operator` with no session. reason is why, which may not be white space alone and holds at most
-- 2,048 characters (store's AdmissionCapReasonMax), since the state serves the newest 50 reasons
-- on every read. changed_at is stamped inside the fact's transaction, under intake's one lock, so
-- it never falls as id rises.
--
-- The cap in force is read off the project's newest row (store's AdmissionCapTx): its cap where it
-- set one, and none after a clear. The daemon admits under the smaller of that cap and the
-- deployment's ceiling (admit's capAt), or under the starting cap where none stands, so a cap set
-- above a ceiling a later configuration lowered admits at that ceiling, and comes back when the
-- ceiling is raised again. The routes refuse a cap above the ceiling of their own boot, so only a
-- lowered ceiling leaves such a row. The index serves that read and the state's list of the newest
-- changes.
--
-- No outbox kind is added, so outbox_kind_check stays as the last migration that wrote it left it.
--
-- Migrations here are forward-only and this one has no down file. A daemon of an earlier release on
-- the same database reads no row of the table and admits under its configured cap, so a rollback
-- needs no statement: the table stays, and a roll forward reads it again.
create table project_admission_cap_changes (
  id bigserial primary key,
  project text not null check (project ~ '^[A-Z][A-Z0-9]*$'),
  cap integer check (cap >= 1),
  cap_before integer not null check (cap_before >= 0),
  cap_after integer not null check (cap_after >= 0),
  ceiling integer not null check (ceiling >= 1),
  by_kind text not null check (by_kind in ('controller', 'operator')),
  by_session text not null default '',
  reason text not null check (reason ~ '[^[:space:]]' and char_length(reason) <= 2048),
  changed_at timestamptz not null,
  constraint project_admission_cap_changes_by_session check ((by_kind = 'controller') = (by_session <> ''))
);
create index project_admission_cap_changes_newest on project_admission_cap_changes (project, id desc);
