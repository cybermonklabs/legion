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
-- daemon's next boot starts it again (workflow's booted), whatever role it is. The check is one
-- list, and 20261115060739_claim_suspended_by_controller wrote it last, with 'controller', the
-- project's controller's own suspension (#4065), so it is dropped and written again with every
-- value that one took and the new value. Its version stands above 20261115060739's, so on a
-- fresh database, and on one a released daemon migrated, this list is the last word: Migrate
-- applies every unrecorded migration in version order, and a database main migrated has recorded
-- 20261115060739 and applies this one after it. A database that recorded this version with its
-- earlier list, written before 20261115060739 existed, is the exception: a build of this branch
-- from before its merge of main migrated it (a worker pod's shared test Postgres is one), and
-- Migrate applies 20261115060739 after this one there, which writes the check without
-- 'project_release'. No released build carries that earlier list, so such a database is a test
-- database, and it is recreated rather than migrated. No row holds the new value yet, so there is
-- nothing to backfill, and every row the check validates holds one of the values the old check
-- took, 'controller' among them.
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
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'released', 'controller', 'project_release'));
