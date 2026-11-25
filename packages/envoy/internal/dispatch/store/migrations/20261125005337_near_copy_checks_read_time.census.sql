-- 20261125005337 adds two nullable columns to near_copy_checks, read_ms and read_stage, and fills
-- neither: every existing row holds both null, which each of the migration's checks admits, so it
-- refuses no row and rewrites none.
select 0;
