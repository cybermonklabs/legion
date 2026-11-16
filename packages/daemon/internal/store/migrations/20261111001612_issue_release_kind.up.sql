-- 20261111001612_issue_release_kind.up.sql — a release's mark says which rule wrote it
-- (INFRA-1341).
--
-- kind is the release rule the mark belongs to. `idle` is the rule this table was built for: a
-- pod whose roles are all quiet, released with a save of the workspace's unpushed work and one
-- role stop per live claim, where the role column names the role whose shim the bundle was asked
-- of. `unowned` is the second rule (workflow.UnownedPodRelease): an issue whose every claim is
-- failed, retired or suspended holds a pod of 3 CPU and 19 GiB that no role can use, and the
-- release of that pod suspends no role and saves no bundle — it queues the teardown alone. Such
-- a release has no role to name, so its mark leaves role empty, and the kind is what the
-- teardown and the cancel read to tell what the release did: a cancel of an unowned release has
-- no claim to resume and a finished one records no roles.
--
-- role is not null under a check naming the six workflow roles, so a mark that names no role
-- could not be written at all. The check is dropped and added again whole, one arm per kind,
-- rather than widened to let any mark leave the role empty: an idle release whose mark named no
-- role would then be a row the database takes and no reader can act on, since the save is asked
-- of that role's shim.
--
-- This migration clears nothing, where the three earlier migrations of this table each begin
-- `delete from issue_releases;`. Each of those gave an existing mark a column whose value that
-- mark had never recorded — the range of the rows it queued, its own identity, the newest wake
-- its pod had — and no default could stand in for one, a zero wake reading as every wake since
-- being new. This one only adds a column with a default and widens a check, so every mark
-- standing is an idle release and the default says exactly that; no existing row stops being
-- valid. A delete here would strand a release in flight: its rows would find no mark and each
-- would finish without acting, the teardown would take no pod, and the roles that release had
-- already suspended would stay `released` in a pod that stands, with nothing left to resume
-- them.
alter table issue_releases add column kind text not null default 'idle' check (kind in ('idle', 'unowned'));
alter table issue_releases drop constraint issue_releases_role_check;
alter table issue_releases add constraint issue_releases_role_check check (
  (kind = 'idle' and role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger'))
  or (kind = 'unowned' and role = ''));
