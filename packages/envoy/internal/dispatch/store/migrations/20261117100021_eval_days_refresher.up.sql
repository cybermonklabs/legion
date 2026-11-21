-- 20261117100021_eval_days_refresher.up.sql
--
-- The day-row refresher's own state (dispatch://INFRA-2916, R12; evals.Store.RunRefresher), which
-- keeps every project's eval_days true to its inputs: where it stands in the two feeds it reads,
-- how far each of its walks over the days has come, and the delivery settings its delivery and CI
-- rows stand built under.
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
-- evals.Store.RefresherAge reads how long ago the last pass ran or a pass last read the whole
-- change log, whichever is older. A third row, `figures` (migration 20261120072042), is the stored
-- figures' own read of the same change log, kept as the `delivery_changes` row is, which every
-- build that writes the figures reads its eval_days and eval_versions changes from, so a change the
-- refresher's page of a build with no figures read is still folded; RefresherAge reads the two
-- feeds' rows alone. It is a table of its own rather than rows of issue_facts_cursor:
-- legionfacts.CursorAge reads that table's oldest row as the age of the Legion facts, and a row of
-- another loop would stand in its answer.
--
-- eval_days_walks holds one row per walk the refresher makes over a span of days: the delivery and
-- CI rows of every project (family `delivery`, project_key ''), since one read of a day's delivery
-- rows builds every project's; the Legion rows of one project (family `legion`); the zero rows of
-- one project's logged evals (family `logged`); one project's unowned-p0 row of today (family
-- `unowned`), a walk of one day at a time that stands at tomorrow once today's row is written; and
-- the stored figures of one project (family `figures`, migration 20261120072042), a walk of the
-- ranges' end days, which stands two days past tomorrow once the ranges ending two days after
-- today are folded.
--   built_by   the build whose walk this is, part of the walk's key: each build makes walks of
--              its own, begun at its start, which is how a release rebuilds every row of a family
--              an older build wrote, and while two builds run (a rolling deploy), each continues its
--              own walk rather than beginning again the one the other recorded, and neither stops
--              the other from reaching today. A build whose walk reaches today drops another
--              build's record of it that has not moved for two days, a build no process runs.
--   next_day   the first day the walk has not built yet (of the figures walk, the first end day);
--              null while the walk is to begin, at the first input day of what it walks. A walk
--              that has built today stands at tomorrow, and the first pass of a new UTC day builds
--              that day.
--   updated_at when the walk last moved.
--
-- eval_days_settings holds at most one row (singleton, as delivery_settings is keyed): the delivery
-- settings the stored delivery and CI day rows stand built under, the four a day row reads
-- (production_job_names, production_apply_step_name, staging_e2e_job_name and
-- staging_apply_job_name; evals.settingsMoves says which evals each moves). The deploy repository
-- is none of them: the rows of the evals that read it are kept for every repository that holds a
-- delivery run (eval_days.deploy_repo, 20261117100006), and a read chooses among them. A pass that
-- reads delivery settings other than these rewrites, before it writes a day under them, the days
-- of the evals the difference moves whose rows it can move (evals.movedDeliveryDays), every day
-- of them where the delivery walk still has days to write, in one transaction that records the
-- settings it read here (evals.rewriteSettings); a pass that finds no row begins the delivery walk
-- again, which builds every day under the settings it reads, and records them.
--   updated_at when the row was last recorded.
--
-- Three new tables, all empty: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes any of them, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). None holds a foreign key, as issue_facts_computed holds none: the
-- delivery walk's project_key is '' and names no project. The census answers 0, and rolling back
-- past it is an image rollback alone.
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
    primary key (family, project_key, built_by)
);

create table eval_days_settings (
    singleton boolean primary key default true check (singleton),
    production_job_names text[] not null,
    production_apply_step_name text,
    staging_e2e_job_name text,
    staging_apply_job_name text,
    updated_at timestamptz not null default now()
);
