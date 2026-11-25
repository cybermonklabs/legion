-- 20261117100006_eval_days.up.sql
--
-- The stored per-day aggregates of every eval (dispatch://INFRA-2916, R12), which every window
-- read folds: a window's value is the fold of its days' rows, and a compare range is a second
-- read of the same rows, so no page read reads a project's population.
--
-- eval_days holds one row per owner, eval, writer, deploy repository, series, repository and day.
--   project_key     the owner of the row: the project it counts for whatever the projects'
--                   default repositories are, which is also the project of its eval id; or, for
--                   what counts for a project only through a default repository
--                   (dispatch://INFRA-2916, R15 and plan D1: a pull request whose issue is held
--                   through its project's default repository, or that has no issue, a CI run of
--                   such a pull request or of none, and a deploy run where it ships such a pull
--                   request or ships none, with each job of it a deploy card follows), an owner
--                   key of its repository, ':' || repo || '|' || sharers, then '|' || lists where
--                   any list holds a project (evals.OwnerKey, the one place it is made and
--                   parsed): repo the repository's name (canonical, as every delivery table keeps
--                   it; the boot's rename pass rewrites it, store/repo_rename_pass.go); sharers
--                   the sorted keys, comma separated, of the projects a deploy run counts for in
--                   their own rows through the pull requests it ships ('' for every other row);
--                   and lists the sorted, distinct attribution lists, ';' separated, of the pull
--                   requests the row counts for that a default decides (evals.attributionList:
--                   the projects of the issues that own no repository ranked above its terminal,
--                   ',' separated, '>', then the project of the first issue that owns the
--                   repository or ''; a deploy's list of a pull request of no project is '>').
--                   So one owner key stands for each distinct sharers set and set of lists a
--                   repository holds rows of, and both are part of the key. No project key starts
--                   with ':' or holds ',', ';' or '>' (projects_key_check, ^[A-Z][A-Z0-9]{1,9}$,
--                   migration 0001), so an owner key never names a project and reads back as it
--                   was made. An owner key's rows are written once, whatever the projects' default
--                   repositories are, by the same writers as a project's (its zero rows from its
--                   own first input day on); a project reads them at read time, through
--                   eval_project_owners below, where the published defaults make it one of the
--                   key's readers, less the key's sharers.
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
--                   the rows of the repository recorded with the stored rows (evals.DeployRepoIn,
--                   DeployRepoOf), so a move of the deploy repository is a selection at read time,
--                   which rewrites no row and whose move back finds the old repository's rows as
--                   they stand; a repository that holds no run has no such row, and reads "no data".
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
--                   rather than through an issue: none of a project's own row, all (n) of an
--                   owner key's, which a project reads as n where it reads the key through the
--                   repository and as 0 where a list of the key picks it (eval_project_owners.via).
--   built_by        the build that last moved the row's values: a write that moves no value
--                   rewrites no row, so a release whose rules agree leaves it, and one whose
--                   rules differ rewrites what they move in its rebuild's flip (evals/rebuild.go).
--   updated_at      when the row was last written.
-- eval_days_window, (project_key, repo, day) over the listed rows, is the list read's window: one
-- index range per project, repository and range of days, measured on 1,533,000 rows (712 MB) as
-- one Bitmap Index Scan per range, 18 to 24 ms for 84 days with an 84-day compare range
-- (dispatch://INFRA-2916/artifact/plan-md, "The design in one page"); the read keeps each eval's
-- rows of its own writer and deploy repository from what the range holds. The primary key is the
-- read and the upsert of one writer's days of one series of one deploy repository, and its first
-- day (evals.FirstDaysIn) is one probe of it.
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
-- Three new tables, all empty, two more below, two views, two functions and a trigger: nothing
-- existing is read or rewritten, and the release before this one neither reads nor writes any, so
-- the two builds roll together (cmd/dispatch/README.md, "Database migrations"). The three tables
-- hold an owner key that names no project as well as project keys, so none of them references
-- projects: a project's deletion deletes its own rows of them by the trigger
-- eval_days_project_deleted below, and leaves a repository's, which belong to no project.
-- eval_days_defaults references projects; that foreign key and the trigger take SHARE ROW
-- EXCLUSIVE on projects for the length of this transaction, which holds a project write and no
-- project read. The census answers 0, and rolling back past it is an image rollback alone.
create table eval_days (
    project_key text not null,
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
-- updates each row it sends whose values or build moved (the upsert's `is distinct from` leaves
-- the rest), and none of its updates moves a column an index holds, so with a fifth of each page
-- left free the new version of a row stays on its page, a heap-only (HOT) update, and writes no
-- index entry. Measured for dispatch://INFRA-2916 by the rewrite's exact upserts on tables shaped
-- as the farm's ten-times load, medians of 7 interleaved rounds, against the default of 100: a
-- first pass over 2,181 day rows took 416 ms where it took 713, 91.6 % of its updates HOT where
-- 0.2 %, and wrote 0.93 MB of WAL where 1.89; over 60 passes with no vacuum the table grew
-- 0.69 MiB a pass at 100 and stopped growing by pass 8 at 80, smaller than at 100 with autovacuum
-- at its defaults. The cost is up to a quarter more heap as the rows are loaded.
--
-- The insert threshold and the index cleanup: a rewrite also deletes the rows the new settings no
-- longer build and inserts the rows they build anew (60 of each in a pass of a production rule at
-- INFRA's size), and an insert finds the space a delete freed only in the free space map, which a
-- vacuum alone writes. At the defaults autovacuum waits for inserts or dead rows of a fifth of the
-- table, and pruning keeps taking the dead rows off its count, so over 60 such passes it never ran
-- and the table grew 66 KB a pass, 236.0 to 240.0 MB, and was growing still (dispatch://INFRA-2916,
-- test round 15). With a vacuum after every 1,000 inserts, whatever the table holds, over 120 such
-- passes on the farm the table grew 235.7 to 238.6 MB in the first 19, until its first vacuum, and
-- not a byte in the 101 after it, 99.4 % of its 854,640 updates HOT. The index cleanup is on
-- because a vacuum left to choose skips its index pass while few pages hold dead item pointers,
-- and with it the pass that frees those pointers: the same 120 passes with it left to choose took
-- 65 KB more late in the run, between two vacuums 22 passes apart.
with (fillfactor = 80, autovacuum_vacuum_insert_threshold = 1000, autovacuum_vacuum_insert_scale_factor = 0, vacuum_index_cleanup = on);
create index eval_days_window on eval_days (project_key, repo, day) where listed;
create index eval_days_stamps on eval_days (project_key, eval_id, writer, day) where listed;

create table eval_day_members (
    project_key text not null,
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
    project_key text not null,
    family text not null,
    day date not null,
    primary key (project_key, family, day)
);

-- eval_day_owners holds one row per owner key that eval_days has held a row of (evals.OwnerKey):
-- the key, its repository, its sharers and its lists (each as the key carries it, '{}' for a plain
-- key), so a read finds the owner keys of a repository it publishes in one probe of
-- eval_day_owners_repo, whatever the database's collation, where a prefix of the key would need
-- byte order, and eval_owner_readers reads a key's lists without parsing it. The repository is
-- compared without case, as GitHub compares names and as a project's default repository is
-- compared with the deploy repository (evals.Store.DeployRepoEvals). The eval_days change trigger
-- records a key the first time a statement writes a row of it (eval_days_change_log, migration
-- 20261117100014), and a rebuild the first time it stages one (evals.recordOwners), whose fold
-- reads the staged rows through eval_project_owners. A key whose rows are all gone keeps its row,
-- which a read joins to no row; the rename pass rewrites the repository in the key and in repo,
-- and the sharers and the lists, which name projects alone, stand.
create table eval_day_owners (
    owner text primary key check (starts_with(owner, ':')),
    repo text not null,
    sharers text[] not null default '{}',
    lists text[] not null default '{}'
);
create index eval_day_owners_repo on eval_day_owners (lower(repo));

-- eval_days_defaults holds the default repository each project's reads stand under, published:
-- the projects.default_repo the evals refresher's page last made visible, in the one commit that
-- also makes visible the stored figures folded under it (evals.syncDefaults, publishDefaults). A
-- person's write of projects.default_repo moves no read until that commit, and a read in one
-- snapshot answers the old defaults whole or the new ones whole.
-- A project with no row publishes no default repository, and reads its own rows and the owner
-- keys whose lists name it as a terminal alone.
create table eval_days_defaults (
    project_key text primary key references projects (key) on delete cascade,
    repo text not null
);

-- eval_project_repos is the one seam every read of a project's day rows takes the repositories it
-- reads the owner-key rows of from: today the one-element set of the project's published default.
create view eval_project_repos as
    select project_key, repo from eval_days_defaults;

-- eval_owner_readers is the readers of an owner key that carries lists (evals.OwnerKey), of the
-- repository p_repo, the sharers p_sharers and the lists p_lists, each list its conditions in rank
-- order joined by ',', then '>', then its terminal or '' (evals.attributionList): the projects whose
-- reads fold the key's rows under the published defaults, each with whether it reads them through
-- the repository. Each list picks its first condition whose published default (eval_project_repos)
-- is p_repo, compared without case, else its terminal, else none. The readers are the projects some
-- list picks, through an issue (via false), and, where some list picks none, each project whose
-- published default is p_repo, through the repository (via true), less the sharers, which count
-- the key's deploy run in their own rows; a project reached both ways reads through its issue, and
-- a terminal that names no project any more reads nothing. A pull request's key carries one list,
-- so it counts for one project, or for its repository's owners; a deploy run's carries a list for
-- each pull request it ships that a default decides, so it counts once in each project any of them
-- reaches. One SQL statement over the published defaults, a few dozen rows, for each key.
create function eval_owner_readers(p_repo text, p_sharers text[], p_lists text[])
returns table (project_key text, via boolean) language sql stable as $$
  with picks as (
    select coalesce(
      (select r.project_key
         from unnest(string_to_array(split_part(l.list, '>', 1), ',')) with ordinality as c(project, rank)
         join eval_project_repos r on r.project_key = c.project and lower(r.repo) = lower(p_repo)
        order by c.rank
        limit 1),
      nullif(split_part(l.list, '>', 2), '')) as pick
    from unnest(p_lists) as l(list)
  ), reached as (
    select pick as project_key, false as via from picks where pick is not null
    union all
    select r.project_key, true from eval_project_repos r
    where lower(r.repo) = lower(p_repo) and exists (select 1 from picks where pick is null)
  )
  select reached.project_key, bool_and(reached.via)
  from reached join projects p on p.key = reached.project_key
  where not (reached.project_key = any(p_sharers))
  group by reached.project_key
$$;

-- eval_project_owners is, for each project, the keys whose day rows a read of the project folds,
-- each with whether the project reads it through a repository (via): its own key (false); each
-- plain owner key of the repository eval_project_repos names whose sharers do not hold the project
-- (true), since a project that counts a deploy in its own rows skips the repository's row of that
-- deploy; and each owner key that carries lists whose readers hold the project
-- (eval_owner_readers, its via). Every read (evals.DayWindowIn, FirstDaysIn, EvalDaysIn, CountedIn,
-- the stored figures' fold) and the change trigger's lookup of the projects an owner key's write
-- moves go through it, and a read that answers via_repository counts it only where via is true
-- (evals.viaAsRead).
create view eval_project_owners as
    select key as project_key, key as owner, false as via from projects
    union all
    select r.project_key, o.owner, true
    from eval_project_repos r join eval_day_owners o on lower(o.repo) = lower(r.repo)
    where o.lists = '{}' and not (r.project_key = any(o.sharers))
    union all
    select reader.project_key, o.owner, reader.via
    from eval_day_owners o cross join lateral eval_owner_readers(o.repo, o.sharers, o.lists) as reader
    where o.lists <> '{}';

create function eval_days_project_deleted() returns trigger language plpgsql as $$
begin
  delete from eval_days where project_key = old.key;
  delete from eval_day_members where project_key = old.key;
  delete from eval_days_owed where project_key = old.key;
  return null;
end $$;

create trigger eval_days_project_deleted after delete on projects
  for each row execute function eval_days_project_deleted();
