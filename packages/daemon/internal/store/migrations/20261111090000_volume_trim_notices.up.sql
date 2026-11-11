-- 20261111090000_volume_trim_notices.up.sql — what the daemon has told an issue's architect about
-- the volume its pod's roles share, so it can tell it again without telling it endlessly.
--
-- The cache trimmer in each issue pod's launcher process reports every sweep that ends with the
-- volume still above its low mark (shimwire.LauncherVolumeReport). A pod that is filling reports
-- one a minute, and nothing else in Legion sees that volume at all, so the daemon's answer is a
-- notice to the issue's architect under bounds: at most three for one trimmer and episode, and at
-- most one an hour for one pod. Those bounds are counted here rather than in the daemon's memory,
-- because a daemon that restarts in the middle of a filling pod's episode would otherwise start
-- the count over and tell that architect three more times, which is exactly the pod already
-- reporting every minute (INFRA-2009).
--
-- One row per project, issue, pod uid, trimmer and episode, shaped as pod_holds is
-- (20261102002500_pod_release_records.up.sql): a counted streak rather than a row per report,
-- upserted on the key, with at the newest notice of that line and notices counting them. The
-- trimmer is in the key because it is what the daemon counts a restarted roles container apart
-- by: a new launcher process mints a new identifier, and its first episode is news again rather
-- than the fourth report of the episode before it. The pod uid is in the key for the same reason
-- one level up, so a replaced pod never inherits the counts of the pod it replaced.
--
-- The hour is read off this table too, by the newest row of (project, issue, pod_uid) whatever
-- trimmer or episode it names, which is what makes the hour the pod's and not the episode's.
-- pod_volume_notices_pod_at serves that read; the unique index serves the count.
--
-- Nothing deletes a row here, and nothing deletes a pod_holds row either: a tree's close takes
-- neither table's rows with it. A row is one line of counters per pod, trimmer and episode of a
-- project that ever filled a volume, so the table grows with those and not with the reports,
-- which is the cost this shape was chosen for; what it buys is the account of what an architect
-- was told, which the issue's production check reads beside the daemon's log lines.
create table pod_volume_notices (
  id bigserial primary key,
  project text not null,
  issue text not null,
  pod_uid text not null,
  trimmer text not null,
  episode bigint not null,
  notices bigint not null default 1,
  at timestamptz not null
);
create unique index pod_volume_notices_line on pod_volume_notices (project, issue, pod_uid, trimmer, episode);
create index pod_volume_notices_pod_at on pod_volume_notices (project, issue, pod_uid, at desc);
