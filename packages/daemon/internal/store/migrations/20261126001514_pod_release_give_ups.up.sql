-- 20261126001514_pod_release_give_ups.up.sql — a release whose teardown gave up on its record is
-- still a release of the pod for the reads of a pause's stop (dispatch://INFRA-3589).
--
-- One row is one release of an issue's pod whose teardown gave up on the release's record: every
-- write of the record (store.Store.RecordPodRelease) failed until workflow.WakeHeldForRelease
-- after the release's mark, so no pod_releases row stands for that release. issue_release_id is
-- the mark the teardown settled (issue_releases.id), one row per mark, and at is the moment the
-- teardown gave up, which is when that release ended. project is the project token, as
-- pod_releases holds it. One statement writes a row: the teardown's last write in place of the
-- record (store.Store.SettleUnrecordedRelease), in the transaction that settles the wakes taken
-- under the mark and then deletes the mark. A settle that finds a record naming the mark writes
-- none.
--
-- Two reads compare a pause's stop of a role against the moment the issue's pod last went away,
-- the later of the issue's newest pod_releases.at and its newest at here: store.StoppingPauseTx,
-- which a wake reads for the words of its task and for the pause that holds it, and
-- store.Store.StoppedByAPause, the held role message's debt. So a role that a pause stopped before
-- such a release is told the release's words at its next wake, as after a recorded release. No
-- other read counts a row here as a release: the newest release a wake pairs with
-- (store.LatestPodRelease), the close of a resume whose pod went away and the release
-- measurements read pod_releases alone. Rows stay, as pod_releases rows do.
--
-- The migration adds a table and changes none that an earlier release reads. A rollback leaves the
-- table in place, and the release before names it in no statement, so no statement is owed on the
-- way back and this header carries none.
create table pod_release_give_ups (
  id bigserial primary key,
  project text not null,
  issue text not null,
  issue_release_id bigint not null unique,
  at timestamptz not null
);
create index pod_release_give_ups_project_issue on pod_release_give_ups (project, issue);
