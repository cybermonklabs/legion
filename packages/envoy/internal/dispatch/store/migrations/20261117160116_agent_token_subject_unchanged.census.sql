-- 20261117160116 creates one plpgsql function and one trigger, each written into the catalog
-- alone; the trigger runs on later updates only, so it reads, refuses and rewrites no row and
-- counts 0.
select 0;
