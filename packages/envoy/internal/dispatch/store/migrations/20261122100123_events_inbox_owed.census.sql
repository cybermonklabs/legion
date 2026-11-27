-- 20261122100123 creates one table, which holds no row, one function and one trigger after each
-- insert into events, and nothing else: no row is read or rewritten, no row that exists before it
-- can violate anything it adds, and the trigger runs only on inserts written after it. The SHARE
-- ROW EXCLUSIVE lock the trigger's catalogue write takes on events is released with the
-- migration's own transaction.
select 0;
