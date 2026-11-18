-- 20261116081010 replaces one function and nothing else: no row that exists before it can violate
-- anything it adds, no row is rewritten, no table is locked, and the lock it takes on the
-- function's own catalogue row is released with the migration's own transaction.
select 0;
