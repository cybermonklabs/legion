-- An ordinary, non-unique btree over embeddings, built concurrently. It refuses no row and
-- rewrites none, and its build holds no ordinary read or write of the table off while it scans:
-- it takes SHARE UPDATE EXCLUSIVE, whatever the table's size. The column it indexes is
-- 20261109120000's, added by a migration of the same release, so this census counts nothing
-- that reads it.
select 0;
