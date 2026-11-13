-- 20261113170753_eval_days_refresher.up.sql
--
-- The day-row refresher's own state (dispatch://INFRA-2916, R12; evals.RunRefresher), which keeps
-- every project's eval_days true to its inputs: where it stands in the two feeds it reads, and how
-- far each of its walks over the days has come.
--
-- eval_days_cursor holds one row per feed the refresher reads, by name: `delivery_changes` (the
-- seq of the last change a committed pass read) and `events` (the id of the last event, at or
-- below the event log's safe horizon, a committed pass read). updated_at is the moment the pass
-- that last wrote the row ran as of, which every pass stamps whether or not its feed held anything
-- new, so evals.Store.RefresherAge reads how long ago the last pass committed. It is a table of
-- its own rather than rows of issue_facts_cursor: legionfacts.CursorAge reads that table's oldest
-- row as the age of the Legion facts, and a row of another loop would stand in its answer.
--
-- eval_days_walks holds one row per walk the refresher makes over a span of days: the delivery and
-- CI rows of every project (family `delivery`, project_key ''), since one read of a day's delivery
-- rows builds every project's; the Legion rows of one project (family `legion`); and the zero rows
-- of one project's logged evals (family `logged`).
--   built_by   the build whose walk this is. A walk whose build is not the running one is begun
--              again by that build, which is how a release rebuilds every row of a family an
--              older build wrote, and while two builds run, each rebuilds in turn.
--   next_day   the first day the walk has not built yet; null while the walk is to begin, at the
--              first input day of what it walks. A walk that has built today stands at tomorrow,
--              and the first pass of a new UTC day builds that day.
--   updated_at when the walk last moved.
--
-- Two new tables, both empty: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes either, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). Neither holds a foreign key, as issue_facts_computed holds none: the
-- delivery walk's project_key is '' and names no project. The census answers 0, and rolling back
-- past it is an image rollback alone.
create table eval_days_cursor (
    name text primary key,
    position bigint not null default 0,
    updated_at timestamptz not null default now()
);

create table eval_days_walks (
    family text not null check (family in ('delivery', 'legion', 'logged')),
    project_key text not null default '',
    built_by text not null,
    next_day date,
    updated_at timestamptz not null default now(),
    primary key (family, project_key)
);
