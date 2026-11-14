-- One function and one trigger, both new: it reads no table, rewrites no row already stored and
-- refuses none. The trigger fills project_key on rows inserted after it, which is new work on a
-- new write rather than a rewrite, as 20261109120000's two replaced functions are.
select 0;
