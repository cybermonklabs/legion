-- 20261116150000 creates one function and attaches it to eight tables as a trigger before each
-- update, and nothing else: no row is read or rewritten, no row that exists before it can violate
-- anything it adds, and the trigger runs only on updates written after it. The SHARE ROW EXCLUSIVE
-- locks the triggers' catalogue writes take are released with the migration's own transaction.
select 0;
