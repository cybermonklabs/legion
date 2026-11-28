-- 20261120204136_spec_writer_guard.up.sql — the project setting and the project mark that the
-- check of an issue's spec and Verification writers reads.
--
-- dispatch://INFRA-4764: on an issue that Legion runs, Dispatch refuses a version of the issue's
-- spec or Verification document unless its writer is a person, the issue's architect or an
-- architect above it (api/spec_writer.go). Two facts of a project decide that check, and this
-- migration stores both.
--
-- projects.proven_sessions is the project's setting. When it is on, an architect's write passes
-- only on a credential that proves the architect's session (a broker token), and a role-holder
-- publication passes only on a credential that proves the session legion-daemon:<project>.
-- PUT /api/v1/projects/{key}/proven-sessions turns it on and DELETE on the same path, which needs
-- a person, turns it off. It is off by default, so every project keeps the behaviour it has until
-- that PUT. The release before this one inserts a project naming its own columns alone, and that
-- row reads as off (cmd/dispatch/README.md, "Database migrations").
--
-- project_role_holder_marks holds one row for each project whose role holders Dispatch has taken
-- a publication of (PUT /api/v1/issues/{key}/role-holders). The check applies only to the issues
-- of a marked project, so a project whose sessions never published a role holder changes nothing.
-- The row is written in the publication's transaction, and nothing deletes it. It is a table of
-- its own and not a column of projects: a write of a projects row from the role-holder route would
-- take that row's lock under the issue's owner lock, while an issue create takes the project's
-- owner lock and then waits for that row to number the issue, the order that deadlocked
-- (api/project_gated_steps.go). The insert here meets no lock an issue create takes: its foreign
-- key takes FOR KEY SHARE on the project's row, which the update of a column that is no key does
-- not conflict with. No project is deleted, so the key does not cascade.
--
-- The fill marks each project that already has a role holder: a project whose daemon published
-- before this release is marked as it would be by its next publication.
--
-- Adding a column with a constant default writes the catalog alone and rewrites no row, and the
-- new table holds no row before the fill, which only reads issue_role_holders and issues: nothing
-- existing is refused or rewritten, and the census answers 0. The column holds projects' ACCESS
-- EXCLUSIVE lock and the foreign key its SHARE ROW EXCLUSIVE lock until the migration commits, the
-- fill included, which reads one row of issue_role_holders for each role a daemon published. The
-- identifier is the UTC moment this file was created, as every migration past the numbered series
-- is named.
alter table projects add column proven_sessions boolean not null default false;

create table project_role_holder_marks (
  project_key text primary key references projects(key),
  marked_at timestamptz not null default now()
);

insert into project_role_holder_marks (project_key)
select distinct i.project_key
from issue_role_holders h
join issues i on i.key = h.issue_key
on conflict do nothing;
