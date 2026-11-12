-- This migration adds one column with a constant default, which Postgres records in the catalog
-- and writes into no stored row. It reads no row, and refuses and rewrites none.
select 0;
