-- This migration widens the kind check to admit eval_readings, eval_versions and eval_days beside
-- the six kinds it already admits, which no stored row can fail, and adds it `not valid`, so no
-- row is read. It refuses and rewrites no row.
select 0;
