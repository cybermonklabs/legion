-- 20261128105250 creates a function, replaces the truncate trigger of check_results and puts the
-- coverage counts' triggers on ci_check_results, all of which run only on later writes: no existing
-- row is read, refused or rewritten. It cannot name ci_check_results, which 20261116213930, pending
-- in the same release, creates.
select 0;
