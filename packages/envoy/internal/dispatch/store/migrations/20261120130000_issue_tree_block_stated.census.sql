-- The migration adds two nullable columns with no default to document_block_versions and writes
-- no row: every stored row reads null in both, which the backfill's marking reads as a document no
-- pass of this release's rule has stated, so its first run after the deploy marks each one once.
-- It refuses no row and rewrites no value any reader had before it, since the columns are new and
-- no earlier release names them. It cannot name them, since they do not exist when the census is
-- taken.
select 0
