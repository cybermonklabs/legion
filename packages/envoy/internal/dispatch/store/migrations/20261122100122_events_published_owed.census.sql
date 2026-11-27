-- 20261122100122 creates one table, which holds no row, one function and one trigger after each
-- update of events' published_destinations, and nothing else: no row is read or rewritten, no row
-- that exists before it can violate anything it adds, and the trigger runs only on updates written
-- after it. The SHARE ROW EXCLUSIVE lock the trigger's catalogue write takes on events is released
-- with the migration's own transaction.
select 0;
