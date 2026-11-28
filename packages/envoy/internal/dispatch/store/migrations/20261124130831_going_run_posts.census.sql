-- 20261124130831 creates one table with its index: no row that exists before it can violate
-- anything it adds, no row is read or rewritten, and the lock its foreign key takes on going_runs
-- is released with the migration's own transaction.
select 0;
