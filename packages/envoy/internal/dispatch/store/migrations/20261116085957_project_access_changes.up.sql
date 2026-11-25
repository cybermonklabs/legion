-- 20261116085957_project_access_changes.up.sql — what a project.access_changed named, kept by the
-- server, and the marker that every stored one carries no access list.
--
-- project.access_changed's payload is the project and the change alone, because GET
-- /api/v1/events/log, the /ws/live events view and the bus serve a stored payload as it was
-- stored, to readers without the person scope too. A live resume still has to tell whether a change
-- in its gap moved what its caller sees (api: lostSightSQL, gainedSightSQL): whose person a list
-- change added or removed, whom an owner change took the project from, the owner and visibility the
-- change left. project_access_changes keeps those facts beside the event, one row per
-- project.access_changed, written in the event's own transaction (access.appendProjectChange) and
-- read by the server alone; no route serves it.
--
-- Migration 20261116015736_project_access's release stored the whole access on the payload.
-- `envoy-dispatch scrub-access-events` copies what such a payload named into project_access_changes
-- and rewrites the payload to {project, change}, reading `events` in id order a batch at a time,
-- beside the running server, and then writes access_events_scrubbed's one row. No project can be made
-- private while it holds none. The rewrite is a command and not this migration because no index of
-- `events` leads with `type`: a migration's `where type = 'project.access_changed'` would read the
-- whole of `events`, the largest table, in one transaction at boot, which under Recreate is an outage.
--
-- Both tables are new, so the migration touches no existing row. Its identifier is the UTC moment it
-- was written, as `migrationid` requires of every migration past the numbered series, which closed at
-- 0161.
create table project_access_changes (
  event_id bigint primary key references events(id) on delete cascade,
  principal text not null default '',
  owner text not null default '',
  visibility text not null default ''
);

create table access_events_scrubbed (
  scrubbed_at timestamptz not null default now()
);
