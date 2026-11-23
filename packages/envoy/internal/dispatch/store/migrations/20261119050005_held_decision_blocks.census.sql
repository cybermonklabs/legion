-- 20261119050005 adds a nullable column with no default to asks and a column with a constant
-- default to doc_ask_block_authors, neither with a check: every stored row takes the null or the
-- default without being rewritten, and no row is refused. It cannot name the columns, which do
-- not exist when the census is taken.
select 0;
