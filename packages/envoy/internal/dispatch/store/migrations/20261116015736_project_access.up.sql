-- 20261116015736_project_access.up.sql — who may see a project: its owner, its access list, the
-- groups that list names, and whether the project is public or private.
--
-- INFRA-3862: a Dispatch project can be private, so its people and its agents see only that
-- project. This migration stores what that rule reads (internal/dispatch/access, access.go):
--
--   - projects.owner, the person who changes the project's access list and its visibility. No
--     foreign key and no check hold it: a project is never left without one by a write
--     (projects_owner_kept, below), and a project the fill cannot give one is named at boot by
--     store.EnsureProjectOwners, which DISPATCH_PROJECT_OWNER_FALLBACK or `envoy-dispatch
--     project-owner` repairs (cmd/dispatch/README.md, "Giving a project to another owner").
--   - projects.visibility, `public` (every full member sees the project, as today) or `private`.
--   - project_access, the people and groups a project's owner lists. principal is a person's
--     lowercase email or a group's name.
--   - access_groups, each with its one owner, the full member who made it or was given it, and
--     access_group_members, its people. Only a full member is ever admitted through a group.
--   - people.member_kind, `full` for everyone who has signed in so far, or `restricted` for a
--     person from outside the sign-in domain whom some project names directly.
--   - agent_tokens.projects, the projects a personal token may use; empty for a token that
--     names none, which is every token minted before this release.
--   - session_projects, which projects a session has acted in (events.Broker.QueueAppend writes a
--     row for a session's first event on a project), and session_projects_filled, the one row
--     `envoy-dispatch backfill-session-projects` writes when it has covered every older event.
--     It has no unique key, on purpose: a unique key makes a write that records a pair wait for
--     another open transaction that records the same pair, inside two writers that may hold
--     crossed issue locks, which is a deadlock. A writer records a pair only where no committed
--     row holds it, so two open at once may both record it; every read asks whether a row exists.
--     It has no foreign key to projects either, on purpose: the key's check takes a share lock on
--     the project row inside the event step of every session's write, which the store's lock
--     order (api's TestEveryApiStoreWriterTakesTheStoreLockOrder) puts last, after the issue or
--     document the write owns. A project's delete sweeps its rows instead
--     (projects_session_projects_swept, below), and a reader of the table only ever matches a row
--     against a project it read from projects, so a row of a project gone matches nothing.
--
-- **Every project's owner.** The fill gives each project its default assignee as owner where a
-- `people` row holds that email (the join 20261111210103 reads its defaults through): a person
-- who has signed in. A project whose default names nobody who signed in keeps a null owner here;
-- the boot names it and fills it from DISPATCH_PROJECT_OWNER_FALLBACK, or refuses to serve
-- (store.EnsureProjectOwners). Two triggers keep the rule from here on, in place of a check: a
-- NOT VALID check would still refuse every later update of a project the fill left without an
-- owner, and every issue create moves projects.next_number and last_seq.
--
--   - projects_owner_from_default, before insert: a project created with no owner takes its
--     default assignee, which is never null (20261111210103). So every insert stores an owner,
--     the seed's, the tests' and the release before this one's included, and no insert site of
--     the 93 files that write projects changes.
--   - projects_owner_kept, before update of owner: a write that would clear a stored owner is
--     refused with check_violation (23514).
--
-- **Expand only.** Every column is added nullable or with a constant default, so the release
-- before this one reads and writes the schema as it did: its project create takes an owner from
-- the insert trigger, and nothing it runs reads or writes the other new columns or tables.
-- Adding a column with a constant default writes the catalog alone (Postgres 11 on), and each
-- new check is on its new column alone, whose every existing row takes the default, so no row is
-- refused. The fill writes the new owner column of every project whose default names a person;
-- projects is a small table, read and written here under its own ACCESS EXCLUSIVE lock, which the
-- ALTER takes anyway. The three tables are locked first, in one order, so no ALTER upgrades a
-- lock it already holds.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
lock table projects, people, agent_tokens in access exclusive mode;

alter table projects
  add column owner text,
  add column visibility text not null default 'public' check (visibility in ('public', 'private'));

alter table people
  add column member_kind text not null default 'full' check (member_kind in ('full', 'restricted'));

alter table agent_tokens
  add column projects text[] not null default '{}' check (cardinality(projects) <= 16);

create table project_access (
  project_key text not null references projects (key) on delete cascade,
  principal_kind text not null check (principal_kind in ('person', 'group')),
  principal text not null check (principal <> '' and principal = lower(principal)),
  -- The actor that listed the principal (model.Actor's jsonb, as events.actor stores it).
  added_by jsonb not null,
  added_at timestamptz not null default now(),
  primary key (project_key, principal_kind, principal)
);

-- The reverse reads: the projects that list one person directly (outside sign-in's
-- access.Listed) or one group (a group's delete, and the group arm of the rule).
create index project_access_principal on project_access (principal_kind, principal);

create table access_groups (
  name text primary key check (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
  -- The full member who made the group or was given it; only that person changes its members or
  -- deletes it.
  owner text not null check (owner <> '' and owner = lower(owner)),
  created_at timestamptz not null default now()
);

create table access_group_members (
  group_name text not null references access_groups (name) on delete cascade,
  person text not null check (person <> '' and person = lower(person)),
  primary key (group_name, person)
);

create index access_group_members_person on access_group_members (person);

create table session_projects (
  session_id text not null check (session_id <> ''),
  project_key text not null check (project_key <> ''),
  first_at timestamptz not null default now()
);

create index session_projects_pair on session_projects (session_id, project_key);
create index session_projects_project on session_projects (project_key);

create table session_projects_filled (
  filled_at timestamptz not null default now()
);

create function projects_session_projects_swept() returns trigger
language plpgsql as $$
begin
  delete from session_projects where project_key = old.key;
  return old;
end $$;

create trigger projects_session_projects_swept
  after delete on projects
  for each row execute function projects_session_projects_swept();

update projects p
set owner = p.default_assignee
where p.owner is null
  and exists (select 1 from people person where person.email = p.default_assignee);

create function projects_owner_from_default() returns trigger
language plpgsql as $$
begin
  new.owner := coalesce(new.owner, new.default_assignee);
  return new;
end $$;

create trigger projects_owner_from_default
  before insert on projects
  for each row execute function projects_owner_from_default();

create function projects_owner_kept() returns trigger
language plpgsql as $$
begin
  if old.owner is not null and new.owner is null then
    raise exception using
      errcode = 'check_violation',
      message = format('project %s keeps its owner: a write cannot clear it; give the project to another full member instead', old.key);
  end if;
  return new;
end $$;

create trigger projects_owner_kept
  before update of owner on projects
  for each row execute function projects_owner_kept();
