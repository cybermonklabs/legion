-- The migration adds two nullable columns with no default to document_block_versions and builds a
-- partial index over one of them, and writes no row: every stored row reads null in both, which
-- the reconciliation reads as owing the store nothing, so the index is built empty. It refuses no
-- row and rewrites no value any reader had before it, since the columns are new and no earlier
-- release names them. It cannot name them, since they do not exist when the census is taken.
select 0
