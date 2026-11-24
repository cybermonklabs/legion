-- 20261123050533_idle_release_keeps_no_workspace.up.sql — the daemon keeps no saved workspace:
-- workspace_bundles goes, idle_release_declines records the idle release the daemon last declined
-- for each issue, so no pass asks the issue's pod again until something it can see has changed,
-- and issue_releases records the newest turn of the pod's claims when a release is marked.
--
-- What this takes out: the table workspace_bundles, which 20261101222725_workspace_bundles.up.sql
-- made and 20261102174500, 20261102224250 and 20261103214558 changed. An idle release saved a
-- released pod's unpushed work there as a git bundle, and the next pod's launcher restored it.
-- This release saves nothing and restores nothing: an idle release takes a pod only when the
-- workspace read finds no commit on a working copy or a local bookmark that no branch or tag on
-- origin holds and no working-copy change (internal/daemon's outbox, checkIssueWorkspace). Every
-- row the table still holds is work that no pod restored, or a confirmation that never arrived.
-- An operator copies out the rows worth keeping before this release runs (docs/kubernetes.md,
-- "Rolling back the release that keeps no saved workspace", says how); the drop deletes the rest
-- with the table.
--
-- What this adds: idle_release_declines, one row per issue whose release, or whose left pod's
-- close, the workspace check declined (store.Store.DeclineIssueRelease, DeclineLeftPodClose). The
-- row names the role the workspace was read through and told, why the check declined (reason:
-- unpushed_work, or workspace_unread where the read failed), what the read found (head, commits,
-- changed), declined_at, the moment the role was told of that work, checked_at, the moment of the
-- last read, started_row, the newest start row of the issue's claims at the last read, and
-- pod_uid, the pod the last read was made in ('' where the runtime could not say). A read that
-- finds the same work again moves checked_at, started_row and pod_uid and keeps declined_at, and
-- tells nobody. The rules keep the pod under the decline's reason until a role of the issue starts
-- a turn after checked_at (claims.last_turn_start), a role process is started again (a start row
-- above started_row) or the runtime holds another pod than pod_uid; a workspace_unread decline
-- also ends an hour after checked_at (pods.IdleDecline).
--
-- And issue_releases.turn_start, the newest claims.last_turn_start of the issue's claims when the
-- release was marked, null where none had a turn. A claim whose last turn started after it ran a
-- turn since the mark, which cancels the release at its role stops and its teardown as a turn in
-- progress does (store's releaseOvertaken), so a whole turn between the mark and a role's stop is
-- not missed. The release before writes no turn_start, and its marks read null, which reads every
-- recorded turn as one since the mark: such a release is cancelled and asked again, which costs a
-- release and loses nothing.
--
-- The release before reads and writes workspace_bundles at every idle pass and every role start,
-- and reads no row of idle_release_declines and no turn_start. A rollback to it therefore runs the
-- way back, the last paragraph of this header (migrations.WayBack), with every daemon of the
-- database stopped, before the earlier image is pinned. It makes workspace_bundles again, empty
-- and in the shape the release before reads, drops idle_release_declines and
-- issue_releases.turn_start, which the release before never reads, and deletes this migration's
-- version row, so a roll forward runs this file again. Every daemon that shares the database moves
-- together, on the way up and on the way back, since these tables hold every project's rows.
-- docs/kubernetes.md, under the heading above, carries the same statements, and internal/store's
-- TestTheWayBackOfTheReleaseThatKeepsNoSavedWorkspace runs these.
--
-- create table public.workspace_bundles (issue text primary key, bundle bytea not null, size_bytes bigint not null, at timestamptz not null, head text not null, tips jsonb not null default '[]'::jsonb, bookmarks jsonb not null default '{}'::jsonb, release_id bigint not null check (release_id > 0), attempt_pod_uid text, attempt_at timestamptz, source_pod_uid text not null check (source_pod_uid <> ''), attempt_outcome text, constraint workspace_bundles_attempt_whole check ((attempt_pod_uid is null) = (attempt_at is null)), constraint workspace_bundles_attempt_outcome check (case when attempt_pod_uid is null then attempt_outcome is null else attempt_outcome in ('sent', 'refused_older') end));
-- drop table public.idle_release_declines;
-- alter table public.issue_releases drop column if exists turn_start;
-- delete from public.schema_version where version = 20261123050533;
drop table workspace_bundles;
create table idle_release_declines (
    project     text        not null,
    issue       text        not null,
    declined_at timestamptz not null,
    checked_at  timestamptz not null,
    role        text        not null check (role <> ''),
    reason      text        not null check (reason in ('unpushed_work', 'workspace_unread')),
    head        text        not null default '',
    commits     integer     not null default 0 check (commits >= 0),
    changed     boolean     not null default false,
    started_row bigint      not null default 0 check (started_row >= 0),
    pod_uid     text        not null default '',
    primary key (project, issue),
    check (checked_at >= declined_at)
);
alter table issue_releases add column if not exists turn_start timestamptz;
