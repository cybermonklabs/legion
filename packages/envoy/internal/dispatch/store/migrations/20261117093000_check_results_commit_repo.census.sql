-- 20261117093000 adds a nullable column with no default to check_results and writes no row:
-- every stored row reads null there, which the results read compares in the repository the
-- check's issue resolves to, as before. Nothing is refused or rewritten.
select 0;
