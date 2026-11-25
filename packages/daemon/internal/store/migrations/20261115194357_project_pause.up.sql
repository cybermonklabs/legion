-- 20261115194357_project_pause.up.sql — the records of a project's pause (dispatch://INFRA-3759):
-- each pause, each role a pause asks to stop, each pod a pause holds, the two marks a resume of a
-- released pod gets from it, and the mark of each outbox row a pause held.
--
-- project_pauses holds one row per pause. project is the project's Dispatch key, the spelling
-- the outbox reads off an issue key (split_part(outbox.issue, '-', 1), record's outboxClaimable),
-- so the outbox's claim can ask this table directly whether a row's project is paused. by is who
-- paused: the kind `controller` with the controller's session id, or the kind `operator` with no
-- session. A pause is open until ended_at is set, with who ended it in the same shape. The checks
-- make every other row impossible: a controller with no session, an operator with one, a reason
-- that is only white space, an end without who ended it, or who ended it without an end. The
-- partial unique index allows one open pause per project, so a second pause is refused by the
-- database as well as by the pause's own transaction (store's Pause, which takes
-- treelifecycle.Serialize first).
--
-- project_pause_stops holds one row for each role a pause asks to stop: the pause, its project,
-- the claim, its issue and role, when the pause asked, and when and how the stop ended. A stop
-- ends `stopped` (the role stopped, with launch_owed saying whether a launch was owed when it
-- stopped), `taken_over` (a park, a close, a linger, an operator, an earlier pause's late stop or
-- an idle release stopped the role first), `failed` (the claim failed or retired) or `cancelled`
-- (the resume ended the pause first). A pause has at most one open stop per claim. The stop names
-- its pause by (pause_id, project), as a pod's hold does, so a stop of one project can never name
-- another project's pause, and its claim must be a claim of that project
-- (project_pause_stops_claim_of_project): a role token is `legion-<project token>-…`
-- (claim.NewToken), and the project token of every key project_pauses admits
-- (^[A-Z][A-Z0-9]*$) is the key lowercased (claim.ProjectToken), so the check reads
-- lower(project). Its issue may be another project's: the operator's spawn makes a claim of any
-- issue key under the daemon's own project token, and the daemon's pause stops it as it stops
-- every other claim. The open stops are what the state names as the roles the pause still waits
-- on. The second index serves the reads by claim (the newest stop of a claim, whatever its pause)
-- as well as by pause and claim.
--
-- project_pause_pod_holds holds one row for each pod a pause keeps: the pause, its project, the
-- issue whose pod it is, when the hold began, and when and why it ended (`resumed`: the resume
-- found a live role in the pod; `ready`: a role of the pod became ready; `deleted`: a close, a
-- park, a linger, a production check's look or a teardown deleted the pod). A pod has at most one
-- open hold. The hold names its pause by (pause_id, project), so a hold of one project can never
-- name another project's pause. Its issue may be another project's, the pod of such a claim, and
-- every read of a hold is by its project, never by its issue key's prefix.
--
-- pod_resumes.clock_at is when a resume's stuck clock starts. Every row has one: the backfill
-- copies at, and a trigger copies at into a row inserted without it. The trigger is for a daemon
-- of an earlier release on the same database, which names the column nowhere, and for a
-- hand-written insert; the daemon of this release writes the column itself (store's
-- RecordPodWakeTx). pod_resumes.pause_id is the pause that held the wake, null for every other
-- wake. A held wake pairs with no release, so the check refuses a pause_id beside a release_id.
-- The partial index on pause_id serves the reads of a pause's held wakes (store's PauseLetOutsTx
-- and PausePodHoldsSince), which are a few rows of a table that holds every wake.
--
-- outbox.held_by_pause is the pause that held the row, null for every other row. The resume
-- writes it, in its own transaction, on each row its pause holds (record's MarkPauseHeldRows),
-- so a row that runs after the resume and wakes a released role names the pause whose held
-- wake that is (store's TakePodWake), whatever the row's kind or due time. Nothing reads a row
-- by it, so it has no index.
--
-- No outbox kind is added, so outbox_kind_check stays as 20261114031356 wrote it. The pause's
-- stop is a `supervise` row whose op is `pause_stop`, and the controller's two notices are
-- `controller_notice` rows of the notice kinds `project-paused` and `project-resumed`; no SQL
-- constrains an op or a notice kind.
--
-- Migrations here are forward-only and this one has no down file. A daemon of an earlier release
-- on a shared database reads none of these tables and none of the three columns, so the tables
-- and the columns stay and are read again by the roll forward. Three steps come before the
-- earlier image runs, in this order (docs/kubernetes.md, "Rolling back the project pause").
-- First, resume the project while this release still runs: the earlier release has no pause,
-- and it launches every role a pause still holds. Second, resume one role of each pod a pause
-- still holds, until each hold has ended (select issue from project_pause_pod_holds where
-- ended_at is null): the earlier release reads no hold, and it deletes a held pod in which no
-- role is live as a pod no claim owns, with the unpushed work on its disk. Third, with the
-- project's daemon stopped, run the three statements below, with the project's key in place of
-- <KEY>: the outbox is shared by every project's daemon on the database, and split_part(issue,
-- '-', 1) is a row's project (a project-level row carries the bare key), so each takes that
-- project's rows alone. The earlier release decodes a row strictly
-- (record.DecodeOutboxPayload, DisallowUnknownFields), so a row it cannot decode fails and is
-- retried on its backoff for ever, as 20261111041503 says of its notice kind. The rows of the two
-- new notice kinds go:
--
--   delete from outbox where kind = 'controller_notice'
--     and payload->>'kind' in ('project-paused', 'project-resumed') and split_part(issue, '-', 1) = '<KEY>';
--
-- The stops of the pause go too (a resume deletes the ones that have not run, so this finds only
-- a stop that was running at the resume), the stops of claims of another project's issue among
-- them, which are keyed by the project and name their claim's issue (record.SuperviseRequest's
-- Issue), a key the earlier release does not know either:
--
--   delete from outbox where kind = 'supervise' and payload->>'op' = 'pause_stop' and split_part(issue, '-', 1) = '<KEY>';
--
-- And a start keeps its work but loses the two keys the earlier release does not know, the pause
-- and the wake's kind (record.SuperviseRequest's Pause and WakeKind):
--
--   update outbox set payload = payload - 'pause' - 'wakeKind'
--     where kind = 'supervise' and payload->>'op' = 'start'
--     and (payload ? 'pause' or payload ? 'wakeKind') and split_part(issue, '-', 1) = '<KEY>';
create table project_pauses (
  id bigserial primary key,
  project text not null check (project ~ '^[A-Z][A-Z0-9]*$'),
  began_at timestamptz not null,
  by_kind text not null check (by_kind in ('controller', 'operator')),
  by_session text not null default '',
  reason text not null check (reason ~ '[^[:space:]]'),
  ended_at timestamptz,
  ended_by_kind text check (ended_by_kind in ('controller', 'operator')),
  ended_by_session text,
  constraint project_pauses_by_session check ((by_kind = 'controller') = (by_session <> '')),
  constraint project_pauses_end_has_its_by check (
    (ended_at is null) = (ended_by_kind is null) and (ended_by_kind is null) = (ended_by_session is null)),
  constraint project_pauses_ended_by_session check (
    ended_by_kind is null or (ended_by_kind = 'controller') = (ended_by_session <> '')),
  unique (id, project)
);
create unique index project_pauses_one_open on project_pauses (project) where ended_at is null;

create table project_pause_stops (
  id bigserial primary key,
  pause_id bigint not null,
  project text not null,
  claim text not null check (claim <> ''),
  issue text not null check (issue ~ '^[A-Z][A-Z0-9]*-[0-9]+$'),
  role text not null check (role <> ''),
  asked_at timestamptz not null,
  ended_at timestamptz,
  ended_as text check (ended_as in ('stopped', 'taken_over', 'failed', 'cancelled')),
  launch_owed boolean not null default false,
  foreign key (pause_id, project) references project_pauses (id, project),
  constraint project_pause_stops_claim_of_project check (claim like 'legion-' || lower(project) || '-%'),
  constraint project_pause_stops_end_has_its_kind check ((ended_at is null) = (ended_as is null)),
  constraint project_pause_stops_owed_only_when_stopped check (not launch_owed or ended_as = 'stopped')
);
create unique index project_pause_stops_one_open on project_pause_stops (pause_id, claim) where ended_at is null;
create index project_pause_stops_claim_pause on project_pause_stops (claim, pause_id);

create table project_pause_pod_holds (
  id bigserial primary key,
  pause_id bigint not null,
  project text not null,
  issue text not null check (issue ~ '^[A-Z][A-Z0-9]*-[0-9]+$'),
  held_at timestamptz not null,
  ended_at timestamptz,
  ended_why text check (ended_why in ('resumed', 'ready', 'deleted')),
  foreign key (pause_id, project) references project_pauses (id, project),
  constraint project_pause_pod_holds_end_has_its_why check ((ended_at is null) = (ended_why is null))
);
create unique index project_pause_pod_holds_one_open on project_pause_pod_holds (project, issue) where ended_at is null;

alter table pod_resumes add column clock_at timestamptz;
update pod_resumes set clock_at = at;
alter table pod_resumes alter column clock_at set not null;

create function pod_resumes_clock_from_at() returns trigger language plpgsql as $$
begin
  if new.clock_at is null then
    new.clock_at := new.at;
  end if;
  return new;
end $$;

create trigger pod_resumes_clock_from_at before insert on pod_resumes
  for each row execute function pod_resumes_clock_from_at();

alter table pod_resumes add column pause_id bigint references project_pauses (id),
  add constraint pod_resumes_held_wake_pairs_with_no_release check (pause_id is null or release_id = 0);
create index pod_resumes_pause on pod_resumes (pause_id) where pause_id is not null;

alter table outbox add column held_by_pause bigint references project_pauses (id);
