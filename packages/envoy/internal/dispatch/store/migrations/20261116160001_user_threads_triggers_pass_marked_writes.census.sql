-- 20261116160001 replaces four trigger functions and nothing else: no row that exists before it can
-- violate anything it adds, no row is read or rewritten, no table is locked, and the ACCESS
-- EXCLUSIVE lock it takes on each function's own catalogue row is released with the migration's own
-- transaction.
select 0;
