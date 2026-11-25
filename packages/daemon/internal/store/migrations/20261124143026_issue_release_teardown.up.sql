-- 20261124143026_issue_release_teardown.up.sql — a release's teardown records the release once
-- (dispatch://INFRA-3149).
--
-- What this changes, first: issue_releases gains teardown, '' or 'reached', the note the teardown
-- of the release of a pod no role can use writes on its mark when an attempt of it comes to the
-- runtime's first write to the issue's Sandbox. A teardown whose mark says 'reached' records its
-- release whatever the runtime answers. store.ReleaseTeardown states the rule, the protocol and
-- the window they leave.
--
-- Why the column defaults to 'reached', and this release's own mark insert (Store.ReleaseIdleIssue)
-- writes '': a mark this release did not write was written by a daemon that records a release
-- whatever its teardown found, and must keep that rule. Such a mark is one standing when this runs
-- — a release in flight, whose teardown the replaced daemon may have begun: it stops a row in
-- progress at its next settle, so a teardown that had its delete accepted, or its kept Sandbox's
-- record written, leaves the mark standing — and one that a daemon still on an earlier release
-- writes after it: several projects' daemons can share one database, and a rollback runs the
-- earlier release over this schema. Read as '', this release would retry such a teardown, find no
-- Sandbox and record nothing for a release that took the pod; read as 'reached', it records the
-- release whatever the runtime answers. `add column … default 'reached'` fills every row standing
-- when it runs, so no backfill follows it, and every insert that does not name the column takes the
-- same value. An idle mark standing when this runs, or written by an earlier release, reads
-- 'reached' too, and nothing reads an idle mark's note: the idle release records its release
-- whatever the runtime answers.
--
-- What this changes, second: pod_releases gains issue_release_id, the id of the release's mark
-- (issue_releases.id) that the record stands for, and one record per mark at most
-- (pod_releases_one_per_issue_release). A teardown records the release and then clears the mark
-- in a transaction of its own, so a teardown that fails, or whose daemon dies, between the two runs
-- again over the standing mark and records the release again; its insert now names the mark and
-- takes the record it already wrote (Store.RecordPodRelease). A record of a release before this
-- migration, and one an earlier release writes, names no mark: the column is null, and the index
-- holds none of them.
--
-- There is no down file: migrations here are forward-only. An earlier release names its columns in
-- every read and write of issue_releases and pod_releases, and its inserts leave both new columns
-- to their defaults, so it reads and writes both tables as it did and records a release whatever
-- its teardown found, as it always did, and its marks read 'reached', which keeps that rule for
-- this release's teardown of them; a rollback needs nothing put back.
alter table issue_releases add column teardown text not null default 'reached'
  check (teardown in ('', 'reached'));
alter table pod_releases add column issue_release_id bigint;
create unique index pod_releases_one_per_issue_release on pod_releases (issue_release_id)
  where issue_release_id is not null;
