-- 20261128105250_ci_check_results_coverage_counts.up.sql — the coverage counts' result class kept
-- by both tables that hold results.
--
-- Why it exists. The result kind of GET /api/v1/search/coverage counts every result Dispatch
-- holds: what a role recorded (check_results) and what a CI job recorded (ci_check_results,
-- 20261116213930), as embedqueue's notIndexedKinds and the recount read the two. 20261124011834
-- keeps that count with statement triggers on check_results alone, so a CI job's result, or its
-- delete, moved nothing until the next recount, up to 11 minutes later, and the truncate trigger
-- of check_results set the whole result class to 0, the rows of ci_check_results included. This
-- migration puts the same counting triggers on ci_check_results and makes the truncate of either
-- table take only that table's rows from the class.
--
-- The triggers, under the names 20261124011834 gives its own (search_coverage_<table>_<what>):
--
--   - search_coverage_ci_check_results_insert and _delete, after insert and after delete, for
--     each statement: one delta row of the rows added or removed, under kind result, through
--     20261124011834's search_coverage_count_rows, as check_results' own two do.
--   - search_coverage_check_results_truncate and search_coverage_ci_check_results_truncate,
--     before truncate, for each statement: one delta row of minus the rows the truncate is about
--     to take from its table (search_coverage_results_truncating). 20261124011834's trigger after
--     a truncate sets every count of the kinds it names to 0 and deletes their delta rows; with
--     two tables in one kind that would take the other table's rows from the count too. Before
--     the truncate the table still holds its rows, and the truncate has already taken ACCESS
--     EXCLUSIVE on it, so the count is of exactly the rows it takes. A truncate that cascades
--     from another table fires the trigger of each table it takes.
--
-- The rows ci_check_results holds when this migration applies are in no count until the next
-- recount, which counts both tables and starts at most 11 minutes after the one before it
-- (embedqueue's coverageRecountTimeout), as for any write the triggers missed. A deployment that
-- takes 20261116213930 in the same release creates ci_check_results with no row.
--
-- What it locks. The function comes first: creating one writes pg_proc and locks no table. Then
-- check_results, then ci_check_results. The drop of check_results' old truncate trigger takes
-- ACCESS EXCLUSIVE on check_results and each create trigger takes SHARE ROW EXCLUSIVE on its table,
-- each held to the commit: the migration reads no row, so it holds them only for its catalog
-- writes, and a read or a write of check_results, and a write of ci_check_results, waits that long.
-- SHARE ROW EXCLUSIVE does not block a read. A writer that holds one of the two tables while it
-- waits for the other closes a cycle with the migration; the lock_timeout
-- below, 500 ms, shorter than deadlock_timeout (1 s by default), has the migration give up first
-- with 55P03 and nothing applied, and pgmigrate applies it again, up to
-- pgmigrate.LockTimeoutAttempts times in the same boot, then at the next boot, as 20261124011834
-- does.
--
-- Why the census admits it. Every statement is a catalog-only kind (pgmigrate's
-- catalogOnlyStatements): the set of lock_timeout, a create function in plpgsql, a drop trigger
-- without cascade and a create trigger, which runs only on later writes. So it applies whatever
-- the size of check_results, except after an earlier pending migration whose effect the census
-- cannot read, such as 20261125125703_btree_gin's create extension: that takes the exemption
-- from every later pending migration, this one included, which is why a release that holds a
-- migration numbered above btree_gin follows the release that first holds btree_gin
-- (cmd/dispatch/README.md, 20261127114415). The census answers 0, and it names no table:
-- ci_check_results does not exist where the census is taken before 20261116213930 applies.
--
-- During a roll and after one. The triggers are the database's, so a write of either table keeps
-- the count whichever release made it; the release before this one never writes ci_check_results,
-- which 20261116213930 creates in this release. Rolled back past this release, the triggers stay
-- and keep writing delta rows, which the earlier release's counts loop folds like any other, and
-- its recount, which counts check_results alone, sets the result class to that table's rows.
set local lock_timeout = '500ms';

-- A truncate of a table of the result kind: minus the rows it is about to take from that table,
-- read before it takes them. Its own table's name decides which table is counted.
create function public.search_coverage_results_truncating() returns trigger
language plpgsql as $$
begin
  if tg_table_name = 'check_results' then
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select 'result', '', false, false, -pg_catalog.count(*) from public.check_results having pg_catalog.count(*) > 0;
  else
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select 'result', '', false, false, -pg_catalog.count(*) from public.ci_check_results having pg_catalog.count(*) > 0;
  end if;
  return null;
end
$$;

drop trigger search_coverage_check_results_truncate on public.check_results;
create trigger search_coverage_check_results_truncate before truncate on public.check_results
  for each statement execute function public.search_coverage_results_truncating();

create trigger search_coverage_ci_check_results_insert after insert on public.ci_check_results
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_rows('result');
create trigger search_coverage_ci_check_results_delete after delete on public.ci_check_results
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_rows('result');
create trigger search_coverage_ci_check_results_truncate before truncate on public.ci_check_results
  for each statement execute function public.search_coverage_results_truncating();
