-- 20261117100006_eval_days.up.sql
--
-- The stored per-day aggregates of every eval (dispatch://INFRA-2916, R12), which every window
-- read folds: a window's value is the fold of its days' rows, and a compare range is a second
-- read of the same rows, so no page read reads a project's population.
--
-- eval_days holds one row per project, eval, writer, deploy repository, series, repository and day.
--   project_key     the project the row counts for, which is also the project of its eval id.
--   eval_id, series the series the row is a day of.
--   writer          which writer made the row (R3): `computation`, the refresher and the day
--                   builders, which write a computed eval's rows; or `readings`, the readings
--                   write, which keeps a logged eval's rows, beside the zero rows the refresher
--                   fills on that eval's days with no reading. A computed eval's fold reads only
--                   the computation's rows, and a logged eval's only the readings write's, so an
--                   eval whose source changes (a register write, or the seed making computed a
--                   slug a reading registered as logged) never folds a row the other writer made:
--                   a reading logged while the eval was logged is in no row its computed fold
--                   reads, even before the refresher rewrites that day. The writer is in the key,
--                   so the two writers' rows of one eval, series, repository and day stand side by
--                   side, and an eval whose source changes back finds its own rows as it left
--                   them. The default is the computation, which writes every row the readings
--                   write does not.
--   deploy_repo     '' for the row of an eval no deploy repository moves, and for each of the
--                   delivery and CI evals that read the deploy repository (evals.deployRepoSlugs:
--                   the three lead times to production, deploy-frequency, deploy-run-success,
--                   pr-ci and the deploy cards) the repository the row is computed as if it were
--                   the deploy repository: one set of those evals' rows for every repository that
--                   holds a delivery run, each from that repository's own runs. The eval list reads
--                   the rows of the repository the delivery settings name (evals.DeployRepoOf), so
--                   a move of the deploy repository is a selection at read time, which rewrites no
--                   row and whose move back finds the old repository's rows as they stand; a
--                   repository that holds no run has no such row, and reads "no data".
--   repo            '' for the row of all the project's repositories, and a repository's name for
--                   that repository's own row, so a read cut by repository folds those rows and
--                   a read of the whole project folds the '' rows alone.
--   day             the day. A day with no '' row reads "no data": each writer stores an explicit
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
--   last_recorded_at, last_reading
--                   which write stored last_value: the recorded_at and the id of the reading it
--                   is, on a logged eval's day. Of rows whose last_at is one moment (two
--                   repositories' rows of one day, which a read cut by repository folds), the
--                   `last` fold answers the later written value, by last_recorded_at and then
--                   last_reading, the order the readings write breaks a tie of one day's
--                   readings by, so a read of repositories answers what the project's own row
--                   holds. Both are null where the row records no write.
--   via_repository  how many of the row's inputs reached the project through a repository
--                   rather than through an issue.
--   built_by        the build that wrote the row, so a build whose rules differ rewrites rows an
--                   older build wrote.
--   updated_at      when the row was last written.
-- eval_days_window, (project_key, repo, day) over the listed rows, is the list read's window: one
-- index range per project, repository and range of days, measured on 1,533,000 rows (712 MB) as
-- one Bitmap Index Scan per range, 18 to 24 ms for 84 days with an 84-day compare range
-- (dispatch://INFRA-2916/artifact/plan-md, "The design in one page"); the read keeps each eval's
-- rows of its own writer and deploy repository from what the range holds. The primary key is the
-- read and the upsert of one writer's days of one series of one deploy repository, and its first
-- day (evals.Store.FirstDays) is one probe of it.
-- eval_days_stamps, (project_key, eval_id, writer, day) over the listed rows, is the stored
-- figures' read of the rows a write may have moved (evals.movedFigureDays): one range of an eval's
-- days of every series, repository and deploy repository, which every write of day rows reads for
-- the evals it wrote, where the primary key bounds only the project, the eval and the writer. It
-- holds no updated_at, so a write that moves a row's values and not its listing can stay a
-- heap-only (HOT) update.
--
-- eval_day_members holds, for each item a family of day rows counted, where it was last counted:
-- the day and the repository. An item whose day moves, an issue closed again on another day or a
-- pull request merged again, then rewrites the day it left as well as the day it joined.
--   project_key, family, deploy_repo, row_key
--                   the project, the family of evals that counted the item, the deploy repository
--                   it was counted under, and the item's key, which are the row's identity.
--                   deploy_repo is eval_days': '' for an item counted whatever the deploy
--                   repository is (an issue of the Legion family, and a pull request of the
--                   delivery family as pr-open-to-merge and rework-share count it, which no deploy
--                   ships), and a repository's name for an item of the delivery and CI evals that
--                   read the deploy repository, counted as if that repository were it: each pull
--                   request with the run of that repository that ships it, that repository's
--                   deploy runs, and its CI runs.
--   day, repo       the day and the repository the item was last counted in.
--   shipped_by      for a pull request of the delivery family, the row key of the deploy run that
--                   shipped it when it was last counted (`run:<repo>/<run id>`), '' for one no
--                   deploy shipped and for every other item. A change of that run, or of the pull
--                   request, moves which run ships it (containment), and the run it left counts it
--                   in its own day's rows: this is how the refresher finds that day once the
--                   stored rows no longer say it.
-- eval_days_owed holds each project's day of a family whose rows count an item that a write of
-- another day took from them: a write that moves a member's day off this one (an issue closed
-- again on another day, a pull request merged again), or moves a pull request's shipped_by off a
-- deploy run, or stops counting a pull request a run shipped (the run's own day), records the day
-- it leaves, in its own transaction, and the next write of that day clears it. The refresher
-- writes every owed day, so a pass that fails between the write that moves an item and the one
-- that rewrites the day the item left still finds that day on its retry, though no stored member
-- names it any more.
-- Only the computation counts items: a logged eval's day folds readings, each of which is its own
-- row of eval_readings, so no member names a writer.
-- eval_day_members_day, (project_key, family, day), is the read of the items one write of a day
-- counted before it: the write of a project's day deletes the members of its families recorded on
-- that day that the day no longer holds, which the primary key answers only by walking every item
-- of the project's family, one row per pull request, run or issue it ever counted.
-- eval_day_members_shipped, (project_key, shipped_by) over the pull requests a deploy shipped, is
-- the read of the pull requests one deploy run shipped, by project.
-- eval_days_owed's primary key, (project_key, family, day), is both the refresher's read of the
-- owed days, a handful between two writes, and the read a write of a day clears its own with: a
-- day's write writes the day's rows and items of every deploy repository, so the day owed names
-- none.
--
-- Three new tables, all empty: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes any, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). The foreign keys to projects take SHARE ROW EXCLUSIVE on projects for
-- the length of this transaction, which holds a project write and no project read. The census
-- answers 0, and rolling back past it is an image rollback alone.
create table eval_days (
    project_key text not null references projects (key) on delete cascade,
    eval_id text not null,
    writer text not null default 'computation' check (writer in ('computation', 'readings')),
    deploy_repo text not null default '',
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
    last_recorded_at timestamptz,
    last_reading uuid,
    counts jsonb not null default '{}'::jsonb check (jsonb_typeof(counts) = 'object'),
    via_repository integer not null default 0,
    built_by text not null,
    updated_at timestamptz not null default now(),
    primary key (project_key, eval_id, writer, deploy_repo, series, repo, day),
    constraint eval_days_eval_of_project check (starts_with(eval_id, project_key || '/'))
)
-- fillfactor 80: a rewrite after a change of the delivery settings (evals.rewriteSettings)
-- updates every row it sends, and none of its updates moves a column an index holds, so with a
-- fifth of each page left free the new version of a row stays on its page, a heap-only (HOT)
-- update, and writes no index entry. Against the default of 100, measured for
-- dispatch://INFRA-2916 (round 14's fillfactor probe: the rewrite's exact upserts on farm-shaped
-- tables, medians of 7 interleaved rounds), a first pass over 2,181 day rows took 416 ms where it
-- took 713, 91.6 % of its updates HOT where 0.2 %, and wrote 0.93 MB of WAL where 1.89; over 60
-- passes with no vacuum the table grew 0.69 MiB a pass at 100 and stopped growing by pass 8 at
-- 80, smaller than at 100 with autovacuum at its defaults. The cost is up to a quarter more heap
-- as the rows are loaded.
with (fillfactor = 80);
create index eval_days_window on eval_days (project_key, repo, day) where listed;
create index eval_days_stamps on eval_days (project_key, eval_id, writer, day) where listed;

create table eval_day_members (
    project_key text not null references projects (key) on delete cascade,
    family text not null,
    deploy_repo text not null default '',
    row_key text not null,
    day date not null,
    repo text not null default '',
    shipped_by text not null default '',
    primary key (project_key, family, deploy_repo, row_key)
);
create index eval_day_members_day on eval_day_members (project_key, family, day);
create index eval_day_members_shipped on eval_day_members (project_key, shipped_by) where shipped_by <> '';
create table eval_days_owed (
    project_key text not null references projects (key) on delete cascade,
    family text not null,
    day date not null,
    primary key (project_key, family, day)
);
