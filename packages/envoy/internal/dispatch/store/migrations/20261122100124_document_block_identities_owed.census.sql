-- 20261122100124 adds one column with a constant default, which Postgres records in the catalog
-- and writes into no row, sets its default for later inserts, and creates one function and one
-- trigger before each insert and update of document_block_versions: no row is read or rewritten,
-- no row that exists before it can violate anything it adds, and the trigger runs only on writes
-- after it.
select 0;
