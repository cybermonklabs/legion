-- 20261113082154_eval_days.up.sql
--
-- The stored per-day aggregates of every eval (dispatch://INFRA-2916, R12), which every window
-- read folds: a window's value is the fold of its days' rows, and a compare range is a second
-- read of the same rows, so no page read reads a project's population.
--
-- eval_days holds one row per project, eval, series, repository and day.
--   project_key     the project the row counts for, which is also the project of its eval id.
--   eval_id, series the series the row is a day of.
--   repo            '' for the row of all the project's repositories, and a repository's name for
--                   that repository's own row, so a read cut by repository folds those rows and
--                   a read of the whole project folds the '' rows alone.
--   day             the day. A day with no '' row reads "no data": the writer stores an explicit
--                   zero row for each day from a series' first input day on, so a missing row
--                   means the series had no input yet, never that it counted 0.
--   listed          the series' own flag, copied here so the window index holds the listed
--                   series alone.
--   n, sum, num, den, max
--                   the day's parts: a count folds by sum, a share by Σnum over Σden, a maximum
--                   by max.
--   samples         for a quantile series, every value of the day, so the fold merges the days'
--                   lists and its quantile is exact.
--   counts          named sub-counts, a JSON object whose values fold by sum.
--   last_value      the newest value the day holds, and last_at when it was measured, which the
--   last_at         `last` fold reads: the fold of a window answers the last_value of its newest
--                   last_at. A logged eval's day keeps its newest reading here; the unowned-p0
--                   day keeps its count at the day's last refresh. Both are null on a day that
--                   holds no such value.
--   via_repository  how many of the row's inputs reached the project through a repository
--                   rather than through an issue.
--   built_by        the build that wrote the row, so a build whose rules differ rewrites rows an
--                   older build wrote.
--   updated_at      when the row was last written.
-- eval_days_window, (project_key, repo, day) over the listed rows, is the list read's window: one
-- index range per project, repository and range of days, measured on 1,533,000 rows (712 MB) as
-- one Bitmap Index Scan per range, 18 to 24 ms for 84 days with an 84-day compare range
-- (dispatch://INFRA-2916/artifact/plan-md, "The design in one page"). The primary key is the
-- read and the upsert of one series' days.
--
-- eval_day_members holds, for each item a family of day rows counted, where it was last counted:
-- the day and the repository. An item whose day moves, an issue closed again on another day or a
-- pull request merged again, then rewrites the day it left as well as the day it joined.
--   project_key, family, row_key
--                   the project, the family of evals that counted the item, and the item's key,
--                   which is the row's identity.
--   day, repo       the day and the repository the item was last counted in.
--
-- Two new tables, both empty: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes either, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). The foreign keys to projects take SHARE ROW EXCLUSIVE on projects for
-- the length of this transaction, which holds a project write and no project read. The census
-- answers 0, and rolling back past it is an image rollback alone.
create table eval_days (
    project_key text not null references projects (key) on delete cascade,
    eval_id text not null,
    series text not null,
    repo text not null default '',
    day date not null,
    listed boolean not null,
    n integer not null default 0,
    sum double precision,
    num double precision,
    den double precision,
    max double precision,
    samples double precision[],
    last_value double precision,
    last_at timestamptz,
    counts jsonb not null default '{}'::jsonb check (jsonb_typeof(counts) = 'object'),
    via_repository integer not null default 0,
    built_by text not null,
    updated_at timestamptz not null default now(),
    primary key (project_key, eval_id, series, repo, day),
    constraint eval_days_eval_of_project check (starts_with(eval_id, project_key || '/'))
);
create index eval_days_window on eval_days (project_key, repo, day) where listed;

create table eval_day_members (
    project_key text not null references projects (key) on delete cascade,
    family text not null,
    row_key text not null,
    day date not null,
    repo text not null default '',
    primary key (project_key, family, row_key)
);
