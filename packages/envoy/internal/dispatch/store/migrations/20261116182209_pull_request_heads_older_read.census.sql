-- 20261116182209 adds a function and a trigger that runs only on later updates of
-- pull_request_heads, and replaces the body of pull_request_heads_unread_write, which also runs
-- only on later updates: it refuses and rewrites no row.
select 0;
