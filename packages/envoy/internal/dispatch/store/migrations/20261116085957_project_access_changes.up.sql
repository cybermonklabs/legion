-- 20261116085957_project_access_changes.up.sql — what a project.access_changed named, kept by the
-- server, and the marker that every stored one carries no access list.
--
-- project.access_changed's payload is the project and the change alone, because GET
-- /api/v1/events/log, the /ws/live events view and the bus serve a stored payload as it was
-- stored, to readers without the person scope too. A live resume, which a later slice adds, is to
-- tell whether a change in its gap moved what its caller sees, from whose person a list change
-- added or removed, whom an owner change took the project from, and the owner and visibility the
-- change left. project_access_changes keeps those facts beside the event, one row per
-- project.access_changed, written in the event's own transaction (access.appendProjectChange) and
-- read by the server alone; no route serves it.
--
-- access_events_scrubbed is to hold one row once no stored project.access_changed carries an
-- access list on its payload. Every one this release writes carries none; a later slice adds the
-- command that checks the stored events and writes the row, and the refusal to make a project
-- private while the table holds none.
--
-- Both tables are new, so the migration touches no existing row. The foreign key to events takes
-- a SHARE ROW EXCLUSIVE lock on events for its catalog write, so a write of events waits for that
-- moment and a read does not. Its identifier is the UTC moment it was written, as `migrationid`
-- requires of every migration past the numbered series, which closed at 0161.
create table project_access_changes (
  event_id bigint primary key references events(id) on delete cascade,
  principal text not null default '',
  owner text not null default '',
  visibility text not null default ''
);

create table access_events_scrubbed (
  scrubbed_at timestamptz not null default now()
);
