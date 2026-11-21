-- 20261115073241_project_release.up.sql — the record of a project release, and the suspension
-- source of the claims it stops (dispatch://INFRA-3583).
--
-- A project release stops every role process of a project and deletes the project's Sandboxes,
-- so the project's pods can be moved to another cluster. project_releases is its record, one row
-- per release: when it started (started_at), the time it deletes every pod still waiting
-- (bound_at), when it finished its acts (finished_at), and its result as it grows (result, the
-- daemon's own JSON: each Sandbox's outcome and each claim it stopped). The record is durable, and
-- its acts are not: the release runs in the daemon's process, and only a boot of the daemon on
-- this database ends it, which stamps ended_at and ended_by = 'boot' on every row of the project
-- that has none (store.EndProjectReleasesAtBoot). No release is ever resumed after a boot, so a
-- row with ended_at null is a release of the process that is running now.
--
-- project is the claim token's spelling of the project (claims.project, "legion"), the one the
-- idle release's marks carry too (issue_releases.project), since the release reads both.
--
-- 'project_release' joins the sources of supervise.Claim.SuspendedBy: the release stopped the
-- claim and deleted its pod. The claim keeps its phase, its session and its pending task, and the
-- daemon's next boot starts it again (workflow's booted), whatever role it is. 20261101201850
-- wrote the check last, so it is dropped and written again with the new value. No row holds the
-- value yet, so there is nothing to backfill, and every row the check validates holds one of the
-- values the old check took.
create table project_releases (
  id bigint generated always as identity primary key,
  project text not null,
  started_at timestamptz not null,
  bound_at timestamptz not null,
  finished_at timestamptz,
  ended_at timestamptz,
  ended_by text check (ended_by in ('boot')),
  result jsonb not null default '{}');

alter table claims drop constraint if exists claims_suspended_by_check;
alter table claims add constraint claims_suspended_by_check
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'released', 'project_release'));
