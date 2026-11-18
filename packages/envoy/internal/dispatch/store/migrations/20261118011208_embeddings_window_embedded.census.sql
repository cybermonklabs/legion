-- An ordinary, non-unique, partial btree over embeddings, built concurrently. It refuses no row and
-- rewrites none, and its build holds no ordinary read or write of the table off while it scans:
-- it takes SHARE UPDATE EXCLUSIVE, whatever the table's size. Every column it indexes and tests
-- already exists, so this census counts nothing.
select 0;
