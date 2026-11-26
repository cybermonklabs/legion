-- 20261117100021_eval_days_refresher.up.sql
--
-- The day-row refresher's own state (dispatch://INFRA-2916, R12; evals.Store.RunRefresher), which
-- keeps every project's eval_days true to its inputs: where it stands in the two feeds it reads,
-- how far each of its walks over the days has come, what the stored rows are whole through and
-- which build's rules built them, the delivery settings its delivery and CI rows stand built under,
-- and the rebuilds that verify the stored rows aside (evals/rebuild.go).
--
-- eval_days_cursor holds one row per feed the refresher reads, by name: `delivery_changes` (the
-- seq of the last change a committed pass read) and `events` (the id of the last event, at or
-- below the event log's safe horizon, a committed pass read). at is, on the `delivery_changes`
-- row, when the change at that seq was recorded (its delivery_changes.at), and null on the
-- `events` row and for a seq no change holds: a pass that finds the change at its cursor gone from
-- the log reads it to tell a change pruned after its 24 hours (delivery.PruneChanges), which loses
-- nothing, from a log that lost changes the cursor never read. updated_at is, on the `events` row,
-- the moment the pass that last wrote the row ran as of, which every pass stamps whether or not its
-- feed held anything new, and on the `delivery_changes` row when a page last read the change log to
-- its end (delivery.ChangeCursor's ReadAt), which a full page leaves where it stood; so
-- evals.RefresherAgeIn reads how long ago the last pass ran or a pass last read the whole
-- change log, whichever is older. A third row, `figures` (migration 20261120072042), is the stored
-- figures' own read of the same change log, kept as the `delivery_changes` row is, which every
-- build that writes the figures reads its eval_days and eval_versions changes from, so a change the
-- refresher's page of a build with no figures read is still folded; RefresherAgeIn reads the two
-- feeds' rows alone. It is a table of its own rather than rows of issue_facts_cursor:
-- legionfacts.CursorAge reads that table's oldest row as the age of the Legion facts, and a row of
-- another loop would stand in its answer.
--
-- eval_days_walks holds one row per walk the refresher makes over a span of days, whichever build's
-- process makes it: the delivery and CI rows of every project (family `delivery`, project_key ''),
-- since one read of a day's delivery rows builds every project's; the Legion rows of one project
-- (family `legion`); the zero rows of one project's logged evals (family `logged`); one project's
-- unowned-p0 row of today (family `unowned`), a walk of one day at a time that stands at tomorrow
-- once today's row is written; and the stored figures of one project (family `figures`, migration
-- 20261120072042), a walk of the ranges' end days, which stands two days past tomorrow once the
-- ranges ending two days after today are folded. A walk builds each new UTC day as it comes, in
-- the open; a family's first walk, over a database or a project no rows are served of, builds every
-- day from its first input day in the open too, a batch a pass, and so does a walk begun again
-- where no rows are served. Where rows are served, a release, a lost change cursor and delivery
-- settings the rows stand built under that are not recorded begin no walk: they rebuild the served
-- families aside (eval_rebuilds below).
--   built_by   the build that last moved the walk; no part of its key, so every build's process
--              continues the one walk.
--   next_day   the first day the walk has not built yet (of the figures walk, the first end day);
--              null while the walk is to begin again, at the first input day of what it walks. A
--              walk that has built today stands at tomorrow, and the first pass of a new UTC day
--              builds that day.
--   updated_at when the walk last moved.
--
-- eval_days_serving holds one row per family of day rows a page writes whole days of, the
-- delivery and CI rows (family `delivery`, project_key '') and one project's Legion rows (family
-- `legion`): what a reader is served of them.
--   built_by   the build whose rules built the stored rows: the build whose walk first built them,
--              or whose rebuild last flipped them. A process whose first pass finds another build
--              here rebuilds them aside, a release; a row whose values that rebuild does not move
--              keeps the build that last moved them (eval_days.built_by).
--   next_day   the first day the stored rows are not whole through: every day before it holds the
--              rows its input builds and the zero row of every series it has no input of from that
--              series' first input day on. Every page write judges a day's zero rows by it, so a
--              page that writes a day while a walk or a rebuild runs keeps the day's zero rows. A
--              walk that builds a later day and a flip move it on, never back.
--   updated_at when the row last moved.
--
-- eval_days_settings holds at most one row (singleton, as delivery_settings is keyed): the delivery
-- settings the stored delivery and CI day rows stand built under, the four a day row reads
-- (production_job_names, production_apply_step_name, staging_e2e_job_name and
-- staging_apply_job_name; evals.settingsMoves says which evals each moves), and the deploy
-- repository (deploy_repo) whose rows a read of the evals that read it answers. A pass that
-- reads delivery settings other than these rewrites, before it writes a day under them, the days
-- of the evals the difference moves whose rows it can move (evals.movedDeliveryDays), every day
-- of them where the delivery walk still has days to write, in one transaction that records the
-- settings it read here (evals.rewriteSettings); a pass that finds no row records the settings it
-- reads and begins a rebuild (eval_rebuilds), which verifies every day under them aside and flips
-- what moved in one commit.
--   deploy_repo  the deploy repository the settings named when the row was recorded, '' for none.
--              It moves no row: the rows of the evals that read it are kept for every repository
--              that holds a delivery run (eval_days.deploy_repo, 20261117100006), and a read
--              chooses among them. A read chooses by this column rather than by delivery_settings
--              (evals.DeployRepoIn), with the four above (evals.BuiltSettingsIn), so a read between
--              a save of the settings and the commit that brings the rows to them answers the old
--              settings whole, and one after it the new: a move of the deploy repository alone is
--              recorded in the refresher's page commit that says the rows stand at the settings.
--   kept_repos and kept_since  the repositories that hold a delivery run, whose rows of the evals
--              that read the deploy repository the stored rows are kept for, in the byte order of
--              their names, and each one's start (the UTC day of its first deploy or PR-checks
--              run, before which it holds no row), pairwise: the set the delivery walk last built
--              for (evals.keptRepo). A page that reads another set builds, in the transaction of
--              its cursors, the days of each repository that differs: a new repository's from its
--              start on, the days between a moved one's two starts, and none of a gone one's,
--              whose rows go (evals.syncKept, span.go); the rewrite of a settings change leaves
--              them as they are.
--   updated_at when the row was last recorded.
--
-- eval_rebuilds holds one row per rebuild (evals/rebuild.go): a release (a process whose first
-- pass finds the stored rows served by another build), a lost change cursor, and delivery settings
-- the rows stand built under that are not recorded rebuild the families whose rows are served
-- (eval_days_serving) aside, in short transactions, and make what moved visible in one short
-- commit, the flip. A family no rows are served of yet (a
-- database or a project first seen) has nothing to keep serving: its walk builds it in the open.
-- At most one rebuild is building (eval_rebuilds_building).
--   reason     `release`, `cursor`, `settings`, or `first` (a database whose cursors are gone over
--              served rows).
--   built_by   the build whose process runs it, whose rules it builds by, and which the flip records
--              as the build that serves the rows (eval_days_serving.built_by).
--   state      `building`, then `flipped` once its flip committed, or `abandoned`: a rewrite of a
--              settings change, a page that finds a repository's runs gone and a lost cursor
--              abandon the one building and begin it again, and one whose build has not moved it
--              for two days, a build no process runs, is abandoned. The staged rows of one that is
--              not building are deleted in short chunks.
--   staged     whether the rows it holds in the tables below are left: false once they are deleted.
--   began_at, updated_at   when it began, and when it last moved.
-- eval_rebuild_walks holds one row per walk of a rebuild: the delivery and CI rows (family
-- `delivery`), one project's Legion rows (`legion`), and one project's stored figures (`figures`).
--   next_day   the first day (of the figures, end day) it has not built, null while it is to begin.
-- eval_rebuild_firsts holds, for a rebuild's walk of the delivery or the Legion rows, each series'
-- first input day the walk has found, by project, eval and deploy repository: the first input
-- days its later days are built by, as a walk's are, recorded with each batch's progress.
-- eval_rebuild_days, eval_rebuild_members, eval_rebuild_range_figures and eval_rebuild_day_figures
-- hold what a rebuild built that differs from the stored rows, members and figures, by the live
-- table's key and the rebuild's id: op `u` for a row to write (the live table's columns, as the flip
-- writes them), op `d` for one to delete. A member's day is where the rebuild counts it (null for
-- `d`), and from_day the day the stored member stood on when it was staged (null for none). A
-- figure records the stored figure it was staged against (held, and the was_ columns), which the
-- flip compares before it writes, so a figure another writer moved since is left as it stands. A
-- staged row's updated_at is the moment it was staged, which the flip writes, so the figures
-- folded over the staged rows hold the stamps and the freshness the flip leaves.
-- eval_rebuild_reverify holds each day of a family the building rebuild had already built (and,
-- of the stored figures, each end day it had folded, family `figures`) whose stored rows, members
-- or figures a statement wrote or deleted since: statement-level triggers on eval_days and
-- eval_day_members below, and on the figure tables (20261120072042), record them, whatever
-- process's statement it was, an older build's during a roll among them, and record nothing
-- while no rebuild is building. The flip's pass builds every day it holds again, immediately
-- before the flip, which replaces what was staged of it: so the flip neither undoes a write in the
-- open nor writes it twice. The flip's own statements, which move the rebuild off building first,
-- record nothing.
--
-- Twelve new tables, all empty, and seven statement-level triggers on eval_days and
-- eval_day_members whose functions write a row only while a rebuild is building, which no
-- database is at this schema: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes any of the tables, so the two builds roll together
-- (cmd/dispatch/README.md, "Database migrations"). Each trigger's statement reads one probe of
-- eval_rebuilds_building where no rebuild is building, which is every write of a day row but a
-- rebuild's. None of the tables holds a foreign key, as issue_facts_computed holds none: the
-- delivery walk's project_key is '' and names no project, and a rebuild's rows are deleted by its
-- own chunks. The census answers 0, and rolling back past it is an image rollback alone.
create table eval_days_cursor (
    name text primary key,
    position bigint not null default 0,
    at timestamptz,
    updated_at timestamptz not null default now()
);

create table eval_days_walks (
    family text not null check (family in ('delivery', 'legion', 'logged', 'unowned', 'figures')),
    project_key text not null default '',
    built_by text not null,
    next_day date,
    updated_at timestamptz not null default now(),
    primary key (family, project_key)
);

create table eval_days_serving (
    family text not null check (family in ('delivery', 'legion')),
    project_key text not null default '',
    built_by text not null,
    next_day date not null,
    updated_at timestamptz not null default now(),
    primary key (family, project_key)
);

create table eval_days_settings (
    singleton boolean primary key default true check (singleton),
    production_job_names text[] not null,
    production_apply_step_name text,
    staging_e2e_job_name text,
    staging_apply_job_name text,
    deploy_repo text not null default '',
    kept_repos text[] not null default '{}',
    kept_since date[] not null default '{}',
    updated_at timestamptz not null default now()
);

create table eval_rebuilds (
    id bigint generated always as identity primary key,
    reason text not null check (reason in ('release', 'cursor', 'settings', 'first')),
    built_by text not null,
    state text not null default 'building' check (state in ('building', 'flipped', 'abandoned')),
    staged boolean not null default true,
    began_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);
create unique index eval_rebuilds_building on eval_rebuilds (state) where state = 'building';

create table eval_rebuild_walks (
    rebuild_id bigint not null,
    family text not null check (family in ('delivery', 'legion', 'figures')),
    project_key text not null default '',
    next_day date,
    primary key (rebuild_id, family, project_key)
);

create table eval_rebuild_firsts (
    rebuild_id bigint not null,
    family text not null check (family in ('delivery', 'legion')),
    project_key text not null,
    eval_id text not null,
    deploy_repo text not null,
    day date not null,
    primary key (rebuild_id, family, project_key, eval_id, deploy_repo)
);

create table eval_rebuild_days (
    rebuild_id bigint not null,
    op text not null check (op in ('u', 'd')),
    project_key text not null,
    eval_id text not null,
    deploy_repo text not null,
    series text not null,
    repo text not null,
    day date not null,
    listed boolean not null,
    n integer not null,
    sum double precision,
    num double precision,
    den double precision,
    max double precision,
    samples double precision[],
    last_value double precision,
    last_at timestamptz,
    last_recorded_at timestamptz,
    last_reading uuid,
    counts jsonb not null,
    via_repository integer not null,
    built_by text not null,
    updated_at timestamptz not null default now(),
    primary key (rebuild_id, project_key, eval_id, deploy_repo, series, repo, day)
);

create table eval_rebuild_members (
    rebuild_id bigint not null,
    op text not null check (op in ('u', 'd')),
    project_key text not null,
    family text not null,
    deploy_repo text not null,
    row_key text not null,
    day date,
    repo text not null default '',
    shipped_by text not null default '',
    from_day date,
    primary key (rebuild_id, project_key, family, deploy_repo, row_key)
);

create table eval_rebuild_range_figures (
    rebuild_id bigint not null,
    op text not null check (op in ('u', 'd')),
    project_key text not null,
    repo text not null,
    days integer not null,
    end_day date not null,
    eval_id text not null,
    deploy_repo text not null,
    fold_key bigint,
    figures bytea,
    oldest_updated_at timestamptz,
    held boolean not null,
    was_fold_key bigint,
    was_figures bytea,
    was_oldest_updated_at timestamptz,
    primary key (rebuild_id, project_key, repo, days, end_day, eval_id, deploy_repo)
);

create table eval_rebuild_day_figures (
    rebuild_id bigint not null,
    op text not null check (op in ('u', 'd')),
    project_key text not null,
    repo text not null,
    day date not null,
    eval_id text not null,
    deploy_repo text not null,
    fold_key bigint,
    figures bytea,
    rows_stamp bigint,
    held boolean not null,
    was_fold_key bigint,
    was_figures bytea,
    was_rows_stamp bigint,
    primary key (rebuild_id, project_key, repo, day, eval_id, deploy_repo)
);

create table eval_rebuild_reverify (
    rebuild_id bigint not null,
    project_key text not null,
    family text not null check (family in ('delivery', 'legion', 'figures')),
    day date not null,
    primary key (rebuild_id, project_key, family, day)
);

-- eval_rebuild_queue_days records each day of the computation's day rows a statement wrote or
-- deleted (its transition table, touched) for each walk of the building rebuild that has built it:
-- the delivery walk's and the project's Legion walk's, either of which a day row of the project can
-- be; the flip's pass builds both again.
create function eval_rebuild_queue_days() returns trigger language plpgsql as $$
begin
  if not exists (select 1 from eval_rebuilds where state = 'building') then
    return null;
  end if;
  insert into eval_rebuild_reverify (rebuild_id, project_key, family, day)
  select distinct walk.rebuild_id, touched.project_key, walk.family, touched.day
  from touched
  join eval_rebuilds rebuild on rebuild.state = 'building'
  join eval_rebuild_walks walk on walk.rebuild_id = rebuild.id and walk.family in ('delivery', 'legion')
    and walk.project_key = case when walk.family = 'delivery' then '' else touched.project_key end
  where touched.writer = 'computation' and walk.next_day > touched.day
  on conflict do nothing;
  return null;
end $$;

create trigger eval_days_rebuild_inserted after insert on eval_days
  referencing new table as touched for each statement execute function eval_rebuild_queue_days();
create trigger eval_days_rebuild_updated after update on eval_days
  referencing new table as touched for each statement execute function eval_rebuild_queue_days();
create trigger eval_days_rebuild_deleted after delete on eval_days
  referencing old table as touched for each statement execute function eval_rebuild_queue_days();

-- eval_rebuild_queue_members records each day a statement wrote, deleted or moved a member to or
-- off (touched: the new rows, and the old ones of an update or a delete), for the walk of the
-- building rebuild of the member's family that has built it.
create function eval_rebuild_queue_members() returns trigger language plpgsql as $$
begin
  if not exists (select 1 from eval_rebuilds where state = 'building') then
    return null;
  end if;
  insert into eval_rebuild_reverify (rebuild_id, project_key, family, day)
  select distinct walk.rebuild_id, touched.project_key, walk.family, touched.day
  from touched
  join eval_rebuilds rebuild on rebuild.state = 'building'
  join eval_rebuild_walks walk on walk.rebuild_id = rebuild.id
    and walk.family = case when touched.family = 'legion' then 'legion' else 'delivery' end
    and walk.project_key = case when walk.family = 'delivery' then '' else touched.project_key end
  where walk.next_day > touched.day
  on conflict do nothing;
  return null;
end $$;

create trigger eval_day_members_rebuild_inserted after insert on eval_day_members
  referencing new table as touched for each statement execute function eval_rebuild_queue_members();
create trigger eval_day_members_rebuild_updated after update on eval_day_members
  referencing new table as touched for each statement execute function eval_rebuild_queue_members();
create trigger eval_day_members_rebuild_moved after update on eval_day_members
  referencing old table as touched for each statement execute function eval_rebuild_queue_members();
create trigger eval_day_members_rebuild_deleted after delete on eval_day_members
  referencing old table as touched for each statement execute function eval_rebuild_queue_members();
