-- An ordinary, non-unique btree over events, built concurrently. It refuses no row and rewrites
-- no row. Its build takes SHARE UPDATE EXCLUSIVE, whatever the table's size, and no ordinary read
-- or write of events waits on that lock. Every column that it indexes exists already, so this
-- census counts nothing.
select 0;
