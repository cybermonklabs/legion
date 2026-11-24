-- 20261124034909 replaces one function's body and does nothing else: it names no table, so it
-- locks none, and it refuses and rewrites no row. Every vector already stored stays as it is; the
-- function writes the same vector it wrote before, only fewer times in a multi-row insert.
select 0;
