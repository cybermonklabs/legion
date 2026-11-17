-- 20261116163813_issue_role_holders.up.sql — the session that holds each Legion role of an issue.
--
-- The Legion daemon gives each of its sessions one role of one issue, and it is the only party
-- that does. It publishes the live set of each issue here (PUT /api/v1/issues/{key}/role-holders,
-- api/issue_role_holders.go): when a claim readies, when a claim ends and at each boot. A later
-- release reads this table in the write transaction of a result, so that the stored role of a
-- result is the role that its session holds and not a role that its caller named.
--
-- The key is (issue_key, role): one session holds a role of an issue, as the daemon keeps one
-- claim per issue and role. The unique index on (issue_key, session) keeps a session to one role
-- of an issue, so a read by session finds at most one row. The daemon publishes the whole set of
-- an issue at once, and the route replaces the rows of the issue with it, so a session whose
-- claim ended leaves the set and no delete route is necessary. The issue cascades, since the row
-- has nothing to say once its issue is gone. The check holds role to the six Legion roles
-- (legionagents.Roles), so a misspelt role is refused where it is written.
--
-- The row is the daemon's own fact, not a person's. Only that daemon writes here, and the write
-- appends no issue event (api/issue_role_holders.go says why), so the table grows no event log of
-- its own.
--
-- A new table that no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The foreign key holds issues in SHARE ROW EXCLUSIVE for
-- the short transaction of the migration, so an issue write waits behind it while a read does not.
-- The identifier is the UTC moment this was written, as `migrationid` requires of every migration
-- past the numbered series (cmd/dispatch/README.md, "Database migrations").
create table issue_role_holders (
  issue_key text not null references issues(key) on delete cascade,
  role text not null check (role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')),
  session text not null check (btrim(session) <> ''),
  recorded_at timestamptz not null default now(),
  primary key (issue_key, role)
);

-- The results route of a later release reads the role of one session on one issue. The primary
-- key answers nothing about session, and the index also keeps a session to one role of an issue.
-- The table is new and empty, so the index is built with it.
create unique index issue_role_holders_issue_session on issue_role_holders (issue_key, session);
