-- 20261127215416_branch_refusals.up.sql — the refusal for good of an issue's branch row, which the
-- state names and the issue's architect is told of once (record's BranchRefusal).
--
-- branch_refusals holds, for each issue and generation, the first issue_branch row that GitHub, or
-- the deployment's private-name rules, refused in a way a retry meets again (internal/daemon's
-- permanentRefusal, ghbranch's Refusal): the row's id, whether it created or moved the branch, the
-- refusal's kind, the row's error as the row kept it, and when. Such a row is finished rather than
-- retried, so the issue's roles start, and the owning architect gets one branch-refused notice: a
-- second refused row of the same generation finds the record and tells nobody. A later row of the
-- issue whose write GitHub takes deletes the issue's records. Every database the upgrade finds
-- holds none: no release before this one ended a branch row on a refusal.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack). Run it with every project's daemon on that
-- database stopped, before the way back of 20261127135256_merged_new_work. It deletes the unsent
-- branch-refused notices, which the release before does not decode, drops the table, and deletes
-- this migration's schema_version row, so the roll forward applies this file again. Each statement
-- runs again with no error.
--
-- begin;
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' = 'branch-refused';
-- drop table if exists branch_refusals;
-- delete from schema_version where version = 20261127215416;
-- commit;
create table branch_refusals (
  issue text not null references issues (key) on delete cascade,
  generation bigint not null check (generation >= 1),
  row_id bigint not null,
  branch_write text not null check (branch_write in ('create', 'move')),
  kind text not null check (btrim(kind) <> ''),
  reason text not null,
  refused_at timestamptz not null,
  primary key (issue, generation));
