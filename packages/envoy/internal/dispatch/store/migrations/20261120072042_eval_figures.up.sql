-- 20261120072042_eval_figures.up.sql
--
-- The stored figures of the eval list (dispatch://INFRA-2916, k8; evals/figures.go): the folds of
-- the ranges the Evals tab offers, kept ahead of the read, so a paint of an offered range reads
-- its figures by key where it would read and fold every day row of its range (eval_days, migration
-- 20261117100006). A stored figure is a fold of day rows, and moves only with them: every write of
-- day rows rewrites, in its own transaction, the figures of the days whose rows it moved, in each
-- cut they reach. Each is kept for every cut a read folds: the project's own rows (eval_days.repo
-- '', its own and those of the owner keys it reads under the published defaults), which a read
-- that names no repository folds, and each repository's, which a read that names that one
-- repository folds (evals.cutOf): the rows of that repository the project reads, and, of the
-- repository it publishes as its default, its own rows stored under ':default'
-- (legionfacts.DefaultRepo, the Legion rows of an issue whose chain names no repository), which no
-- other cut folds.
--
-- eval_range_figures holds one row per project, cut, range, eval and deploy repository:
--   project_key, eval_id  the eval, of its project, as eval_days holds them.
--   repo        the cut: '' for the fold of the project's own rows, and a repository's name for
--               the fold of the rows a read naming that one repository folds; never ':default',
--               which no read names.
--   deploy_repo the deploy repository of the rows folded, as eval_days.deploy_repo keeps them: ''
--               for an eval no deploy repository moves, and for an eval that reads it, the
--               repository its rows are computed as if it were the deploy repository, one fold of
--               each repository's rows. A read folds the rows of the repository recorded with the
--               stored rows (eval_days_settings.deploy_repo, evals.DeployRepoIn), as a read of the
--               day rows does. It is the key's last column: a read's every other column is one
--               value, and its deploy repository one of a few.
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
--               day row: every build writes the figures of the rows it writes, and a release whose
--               folds differ rewrites, in its rebuild's flip (evals/rebuild.go), the figures an
--               older build folded that its folds move, and no other.
-- A range that holds no listed row of the eval in the cut has no row: its fold is the fold of no
-- parts.
--
-- eval_day_figures holds one row per project, cut, day, eval and deploy repository that holds a
-- listed day row of the eval in the cut:
--   figures     the day's figures of each headline series that holds a row that day (the per-day
--               array a read answers), encoded as eval_range_figures' are.
--   fold_key, built_by
--               as eval_range_figures'.
--   rows_stamp  a fingerprint of the listed rows of the cut the figures folded, the project's own
--               and those of the owner keys it reads: each one's series, its updated_at and its
--               via_repository as the project reads it (evals.rowsStamp). Every write that moves a
--               row's value moves its updated_at, and a publishing of the default repositories that
--               moves what the project reads of an owner key moves the rows or their
--               via_repository. A day whose rows no longer match it has moved, and its figures and
--               every range that holds it are folded again.
-- eval_day_figures_eval_day, (project_key, eval_id, day), is the stored figures' read of an eval's
-- stored day rows of every cut over a span of days: the moved-day test every write of day rows
-- makes (evals.movedFigureDays) and the figures walk's read of the day rows it deletes
-- (evals.buildFigureEnds), one range of it for each eval, where the primary key, whose column
-- after the project is the cut, bounds only the project.
--
-- eval_figures_built holds one row per project whose figures are built: the first whole walk of
-- them, by any build, sets it, and a read of an offered range folds the stored figures from then
-- on, whichever build serves it.
--   through     the last end day the figures are built through, which the figures walk moves on as
--               a new UTC day adds one, and never back.
--   built_by    the build whose walk first built them.
--   updated_at  when through last moved.
--
-- eval_figure_cuts_stale holds one row per cut ('' for the project's own rows, or a repository's
-- name; every deploy repository of it) whose figures a move of its project's default repository
-- moved, from the commit that publishes the move (evals.publishDefaults) until the refresher has
-- folded again every range and day of the evals it names (evals.refoldStaleCuts). The move's
-- commit folds the ranges and days a first paint of the cut reads (evals.firstPaintOf) of those
-- evals; the refresher's passes then fold every end day of the cut, newest first, batch after
-- batch, each a short transaction, and fold a first paint the mark still owes (a new UTC day's,
-- or a rename's) first, in a batch of its own, which the boot folds too before it serves
-- (evals.FoldFirstPaintsAtBoot). A read of the cut folds those evals' day rows until the stored
-- rows it reads of them are folded again: a first paint's from its first-paint fold on, an older
-- window's once the refold has come down past it (evals.figuresThrough). It reads every other
-- eval's stored figures throughout, and every other cut stays served.
--   generation  1 for the first mark, one more for each move that marks the cut again, which sets
--               ready_through and next_day back to null: the first-paint fold records its
--               ready_through, and the refold's batch that reaches the cut's first end day deletes
--               the row, each only where the generation is the one the batch read, so a move
--               during a refold leaves the cut stale, its first paint owed, until the refold of
--               its generation is done.
--   ready_through
--               the last end day the project's figures were folded through (evals.readFigureScope)
--               when the first-paint fold of this generation folded its first paint, which names
--               the windows ending on the three end days up to it and their compare ranges; null
--               until that fold commits. A UTC day that moves the figures on owes the first
--               paint of the new last end day, which the refresher's next pass folds first.
--   next_day    the newest end day the refold of this generation has not folded, null while it is
--               to begin at the last end day the project's figures are folded through; every end
--               day after it stands folded again from the rows the move left.
--   evals       the ids of the project's evals (as the project reads them, `<project>/<slug>`) whose
--               rows the moves that marked the cut moved into or out of it, each move's computed
--               from the rows stored at its commit; a move that marks the cut again adds its own
--               to them. Null names every eval of the cut, and stays null whatever a later move
--               adds. Every other eval's stored figures of the cut fold rows no move moved, and
--               stand.
--
-- Six statement-level triggers, three on each figures table, record the end days a statement wrote
-- or deleted figures of (a range's end day, the day after a day's) for the figures walk of the
-- building rebuild that has folded them (eval_rebuild_reverify, migration 20261117100021), whatever
-- process's statement it was: the flip's pass folds those end days again before the flip. They
-- write nothing while no rebuild is building.
--
-- Four new tables, all empty, and the triggers with their two functions: nothing existing is read
-- or rewritten, and the release before this one neither reads nor writes any, so the two builds
-- roll together (cmd/dispatch/README.md, "Database migrations"). The foreign keys to projects take
-- SHARE ROW EXCLUSIVE on projects for the length of this transaction, which holds a project write
-- and no project read, as 20261117100006's do. The census answers 0, and rolling back past it is
-- an image rollback alone.
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
-- settings (evals.rewriteSettings) updates each row it sends whose fold key, figures or
-- oldest_updated_at moved (the upsert's `is distinct from` leaves the rest), and none of its
-- updates moves a column the primary key holds, so with a fifth of each page left free the update
-- stays on its page (HOT) and writes no index entry. Measured the same way against the default of
-- 100, a first pass over 19,392 rows took 1,541 ms where it took 1,966, 39.5 % of its updates HOT
-- where 0.0 %, rising to 75.8 % by the fourth pass; over 60 passes with no vacuum the table grew
-- 4.9 MiB a pass at 100 and stopped growing by pass 19 at 80, smaller than at 100 with autovacuum
-- at its defaults.
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
-- fillfactor 80, for eval_range_figures' reason: no update a rewrite sends, of a row whose fold
-- key, figures or rows_stamp moved, moves a column the primary key or eval_day_figures_eval_day
-- holds. Measured the same way against the default of 100, a first pass over 2,182 rows took
-- 131 ms where it took 278, 99.5 % of its updates HOT where 0.0 %; over 60 passes with no vacuum
-- the table grew 0.55 MiB a pass at 100 and not at all at 80, smaller than at 100 with autovacuum
-- at its defaults.
with (fillfactor = 80);
create index eval_day_figures_eval_day on eval_day_figures (project_key, eval_id, day);

create table eval_figures_built (
    project_key text primary key references projects (key) on delete cascade,
    through date not null,
    built_by text not null,
    updated_at timestamptz not null default now()
);

create table eval_figure_cuts_stale (
    project_key text not null references projects (key) on delete cascade,
    repo text not null,
    generation bigint not null check (generation > 0),
    ready_through date,
    next_day date,
    evals text[],
    primary key (project_key, repo)
);

-- eval_rebuild_queue_range_figures and eval_rebuild_queue_day_figures record, for each project a
-- statement wrote or deleted figures of (touched), each end day they fold: a range's end day, and
-- the day after a day's.
create function eval_rebuild_queue_range_figures() returns trigger language plpgsql as $$
begin
  if not exists (select 1 from eval_rebuilds where state = 'building') then
    return null;
  end if;
  insert into eval_rebuild_reverify (rebuild_id, project_key, family, day)
  select distinct walk.rebuild_id, touched.project_key, 'figures', touched.end_day
  from touched
  join eval_rebuilds rebuild on rebuild.state = 'building'
  join eval_rebuild_walks walk on walk.rebuild_id = rebuild.id and walk.family = 'figures' and walk.project_key = touched.project_key
  where walk.next_day > touched.end_day
  on conflict do nothing;
  return null;
end $$;

create function eval_rebuild_queue_day_figures() returns trigger language plpgsql as $$
begin
  if not exists (select 1 from eval_rebuilds where state = 'building') then
    return null;
  end if;
  insert into eval_rebuild_reverify (rebuild_id, project_key, family, day)
  select distinct walk.rebuild_id, touched.project_key, 'figures', touched.day + 1
  from touched
  join eval_rebuilds rebuild on rebuild.state = 'building'
  join eval_rebuild_walks walk on walk.rebuild_id = rebuild.id and walk.family = 'figures' and walk.project_key = touched.project_key
  where walk.next_day > touched.day + 1
  on conflict do nothing;
  return null;
end $$;

create trigger eval_range_figures_rebuild_inserted after insert on eval_range_figures
  referencing new table as touched for each statement execute function eval_rebuild_queue_range_figures();
create trigger eval_range_figures_rebuild_updated after update on eval_range_figures
  referencing new table as touched for each statement execute function eval_rebuild_queue_range_figures();
create trigger eval_range_figures_rebuild_deleted after delete on eval_range_figures
  referencing old table as touched for each statement execute function eval_rebuild_queue_range_figures();
create trigger eval_day_figures_rebuild_inserted after insert on eval_day_figures
  referencing new table as touched for each statement execute function eval_rebuild_queue_day_figures();
create trigger eval_day_figures_rebuild_updated after update on eval_day_figures
  referencing new table as touched for each statement execute function eval_rebuild_queue_day_figures();
create trigger eval_day_figures_rebuild_deleted after delete on eval_day_figures
  referencing old table as touched for each statement execute function eval_rebuild_queue_day_figures();
