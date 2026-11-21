-- 20261120072042_eval_figures.up.sql
--
-- The stored figures of the eval list (dispatch://INFRA-2916, k8; evals/figures.go): the folds of
-- the ranges the Evals tab offers, kept ahead of the read, so a paint of an offered range reads
-- its figures by key where it would read and fold every day row of its range (eval_days, migration
-- 20261117100006). A stored figure is a fold of day rows, and moves only with them: every write of
-- day rows rewrites, in its own transaction, the figures of the days whose rows it moved.
--
-- eval_range_figures holds one row per project, repository, range, eval and deploy repository:
--   project_key, eval_id  the eval, of its project, as eval_days holds them.
--   repo        '' for the fold of the project's own rows, and a repository's name for the fold of
--               that repository's rows, which a read naming that one repository folds.
--   deploy_repo the deploy repository of the rows folded, as eval_days.deploy_repo keeps them: ''
--               for an eval no deploy repository moves, and for an eval that reads it, the
--               repository its rows are computed as if it were the deploy repository, one fold of
--               each repository's rows. A read folds the rows of the repository the delivery
--               settings name, as a read of the day rows does. It is the key's last column: a
--               read's every other column is one value, and its deploy repository one of a few.
--   days, end_day
--               the range: its length in days and its end day, exclusive, so it holds the days
--               end_day - days to end_day - 1. Every length the Evals tab offers is stored, and
--               each one day longer, the window the live view reads at any moment but a midnight;
--               every end day from the day after the eval's first row through two days after today.
--   fold_key    a fingerprint of what the eval's definition folds by: its source and each series'
--               fold, listing and role. A read whose eval no longer has that fingerprint (a
--               register write moved one of them) folds the day rows instead.
--   figures     the statistics of each listed series over the range's rows and each series'
--               via-repository count, every statistic as its 64 bits (evals.encodeFigures): a
--               jsonb number would not keep -0, and a column per statistic could not hold a
--               map whose series and statistics are each eval's own.
--   oldest_updated_at
--               the oldest updated_at of the rows folded, which the read's freshness answers.
--   built_by    the build that folded the figures, as eval_days.built_by is the build that built a
--               day row: every build writes the figures of the rows it writes, and a build whose
--               folds differ rewrites the figures an older build folded once its whole walk of
--               them reaches them, writing only those that move.
-- A range that holds no listed row of the eval has no row: its fold is the fold of no parts.
--
-- eval_day_figures holds one row per project, repository, day, eval and deploy repository that
-- holds a listed day row of the eval:
--   figures     the day's figures of each headline series that holds a row that day (the per-day
--               array a read answers), encoded as eval_range_figures' are.
--   fold_key, built_by
--               as eval_range_figures'.
--   rows_stamp  a fingerprint of the listed rows the figures folded: each one's series and
--               updated_at, which every write that moves a row's value moves. A day whose rows
--               no longer match it has moved, and its figures and every range that holds it are
--               folded again.
-- eval_day_figures_eval_day, (project_key, eval_id, day), is the stored figures' read of an eval's
-- stored day rows over a span of days, which names no repository: the moved-day test every write
-- of day rows makes (evals.movedFigureDays) and the figures walk's read of the day rows it deletes
-- (evals.buildFigureEnds), one range of it for each eval, where the primary key bounds only the
-- project.
--
-- eval_figures_built holds one row per project whose figures are built: the first whole walk of
-- them, by any build, sets it, and a read of an offered range folds the stored figures from then
-- on, whichever build serves it.
--   through     the last end day the figures are built through, which each build's figures walk
--               moves on as a new UTC day adds one, and never back.
--   built_by    the build whose walk first built them.
--   updated_at  when through last moved.
--
-- Three new tables, all empty: nothing existing is read or rewritten, and the release before this
-- one neither reads nor writes any, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). The foreign keys to projects take SHARE ROW EXCLUSIVE on projects for
-- the length of this transaction, which holds a project write and no project read, as
-- 20261117100006's do. The census answers 0, and rolling back past it is an image rollback alone.
create table eval_range_figures (
    project_key text not null references projects (key) on delete cascade,
    repo text not null,
    days integer not null check (days > 0),
    end_day date not null,
    eval_id text not null,
    deploy_repo text not null default '',
    fold_key bigint not null,
    figures bytea not null,
    oldest_updated_at timestamptz not null,
    built_by text not null,
    primary key (project_key, repo, days, end_day, eval_id, deploy_repo),
    constraint eval_range_figures_eval_of_project check (starts_with(eval_id, project_key || '/'))
)
-- fillfactor 80, as eval_days has (20261117100006): a rewrite after a change of the delivery
-- settings (evals.rewriteSettings) updates every row it sends, and none of its updates moves a
-- column the primary key holds, so with a fifth of each page left free the update stays on its
-- page (HOT) and writes no index entry. Against the default of 100, measured for
-- dispatch://INFRA-2916 (round 14's fillfactor probe: the rewrite's exact upserts on farm-shaped
-- tables, medians of 7 interleaved rounds), a first pass over 19,392 rows took 1,541 ms where it
-- took 1,966, 39.5 % of its updates HOT where 0.0 %, rising to 75.8 % by the fourth pass; over 60
-- passes with no vacuum the table grew 4.9 MiB a pass at 100 and stopped growing by pass 19 at
-- 80, smaller than at 100 with autovacuum at its defaults.
with (fillfactor = 80);

create table eval_day_figures (
    project_key text not null references projects (key) on delete cascade,
    repo text not null,
    day date not null,
    eval_id text not null,
    deploy_repo text not null default '',
    fold_key bigint not null,
    figures bytea not null,
    rows_stamp bigint not null,
    built_by text not null,
    primary key (project_key, repo, day, eval_id, deploy_repo),
    constraint eval_day_figures_eval_of_project check (starts_with(eval_id, project_key || '/'))
)
-- fillfactor 80, for eval_range_figures' reason: no update a rewrite sends moves a column the
-- primary key or eval_day_figures_eval_day holds. Against the default of 100, measured for
-- dispatch://INFRA-2916 (round 14's fillfactor probe: the rewrite's exact upserts on farm-shaped
-- tables, medians of 7 interleaved rounds), a first pass over 2,182 rows took 131 ms where it took
-- 278, 99.5 % of its updates HOT where 0.0 %; over 60 passes with no vacuum the table grew
-- 0.55 MiB a pass at 100 and not at all at 80, smaller than at 100 with autovacuum at its
-- defaults.
with (fillfactor = 80);
create index eval_day_figures_eval_day on eval_day_figures (project_key, eval_id, day);

create table eval_figures_built (
    project_key text primary key references projects (key) on delete cascade,
    through date not null,
    built_by text not null,
    updated_at timestamptz not null default now()
);
