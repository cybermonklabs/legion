-- 20261111210103_projects_default_assignee_not_null.up.sql — every project names a default assignee.
--
-- INFRA-3090: the person a question reaches is three steps resolved at read time — the person the
-- ask was handed to (asks.asked_of, 20261109221325), then the issue's assignee, then the default
-- assignee of the ask's project. The last step reaches somebody only where the project names one,
-- and 0116_project_default_assignee added the column nullable, so a project nobody ever set a
-- default on left a question on an unassigned issue reaching nobody. This migration fills every
-- project's default and then makes the column not null, so the chain's last step always lands.
--
-- The fill reads two sources, in this order, and takes the first that names a person:
--
--   1. the login that holds most of that project's issues (issues.project_key, issues.assignee,
--      0034_issue_assignee). The majority is the project's real owner of work, and it needs no
--      operator to type it. A tie is broken by the most recent issue among the tied logins
--      (issues.created_at, then issues.key, both descending), so the login whose work is newest
--      wins a draw.
--
--   2. the person who created the project, from that project's project.created event (api/
--      projects.go writes one per create). events.actor is the jsonb of model.Actor
--      (internal/dispatch/model/model.go): an actor of kind "user" names a person's login in
--      "id", and an actor of any kind may name one in "owner" (a personal token is
--      {kind: "session", id: "", owner: "<login>"}), so the login is the "id" of a user actor,
--      else "owner". A project an agent session created with the shared bearer names nobody
--      there — its actor is {kind: "session", id: "<session>"} with no owner, which
--      api/projects_test.go pins (TestASessionBearerCreatesAProject) — and falls through to the
--      refusal below. Where a project holds more than one such event, the earliest is read.
--
-- Both sources are held to `people` (0068_people), everyone who has signed in: a login no people
-- row holds reaches nobody's Inbox, which is the defect this issue is about, so a stale assignee
-- left by a restore or a direct write is passed over rather than stored as a default that cannot
-- be reached. The logins are lowercased, the form people.email and 0116's check both require.
--
-- A project neither source names stops the migration with its key in the message, because a
-- `select` cannot raise: the test lives inside a DO block, which can. The repair is to name a
-- person on the release running now, PUT /api/v1/projects/<key>/default-assignee with
-- {"assignee": "<the person's email>"}, and then deploy again.
--
-- **Stop-the-world.** The narrowing ships with the fill in one release, which the expand-then-
-- contract rule (cmd/dispatch/README.md, "Database migrations") otherwise splits across two:
-- the deployment runs one replica under a Kubernetes Deployment's `strategy: Recreate`
-- (packages/envoy/infra/README.md), so no server of the earlier release serves while the
-- migration runs, and the release that boots over it writes a default on every project it
-- creates. A release older than this one started over the new schema again creates projects with
-- no default, which this column now refuses (23502), so that create answers 500: recover a
-- failed deploy forward, as 0113 and 0117 require.
--
-- It locks `projects` alone, in ACCESS EXCLUSIVE, taken before the fill reads anything and held
-- to commit, so no project created between the fill and the ALTER escapes with a null default;
-- the ALTER takes that lock anyway, and taking it last would be an upgrade from the fill's ROW
-- EXCLUSIVE. `issues`, `events` and `people` are read under ACCESS SHARE, which no write waits
-- behind, so neither the 5-second lock bound nor the census's one-gigabyte table limit applies to
-- them. The census counts the projects that hold no default, which is every project the fill
-- writes: it refuses a deploy whose database still has one, and the fill below is run on the
-- serving release first (cmd/dispatch/README.md prints it) so that the census answers 0.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
lock table projects in access exclusive mode;

-- 1. The login holding most of the project's issues, the most recent issue breaking a tie.
with assigned as (
	select i.project_key, i.assignee, i.created_at, i.key
	from issues i
	join people p on p.email = i.assignee
), counted as (
	select project_key, assignee, count(*) as held
	from assigned
	group by project_key, assignee
), majority as (
	select distinct on (a.project_key) a.project_key, a.assignee
	from assigned a
	join counted c on c.project_key = a.project_key and c.assignee = a.assignee
	order by a.project_key, c.held desc, a.created_at desc, a.key desc
)
update projects p
set default_assignee = m.assignee
from majority m
where p.key = m.project_key and p.default_assignee is null;

-- 2. The person who created the project, where the first step named nobody.
update projects p
set default_assignee = creator.login
from (
	select distinct on (e.project_key)
		e.project_key,
		coalesce(
			case when e.actor->>'kind' = 'user' then lower(e.actor->>'id') end,
			lower(e.actor->>'owner')) as login
	from events e
	where e.type = 'project.created' and e.project_key is not null
	order by e.project_key, e.id
) creator
join people person on person.email = creator.login
where p.key = creator.project_key and p.default_assignee is null;

-- 3. A project neither source names stops the migration, named by its key.
do $$
declare
	stranded text;
begin
	select string_agg(key, ', ' order by key) into stranded
	from projects where default_assignee is null;
	if stranded is not null then
		raise exception using
			errcode = 'not_null_violation',
			message = format(
				'20261111210103 refuses: no issue of these project(s), and no person who created them, '
				'names somebody who has signed in, so they name no default assignee: %s. '
				'Name one on the release running now, PUT /api/v1/projects/<key>/default-assignee with '
				'{"assignee": "<the person''s email>"}; then deploy again.',
				stranded);
	end if;
end $$;

-- 4. Every project names a person, so the column can refuse nothing from here on.
alter table projects alter column default_assignee set not null;
