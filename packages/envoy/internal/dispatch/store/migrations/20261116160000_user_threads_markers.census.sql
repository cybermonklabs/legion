-- 20261116160000 adds two nullable columns to user_threads with no default, a catalog change, and
-- one trigger function with its trigger before each update. No stored row is read or rewritten,
-- every existing row reads null, which no constraint can refuse, and the trigger runs only on
-- updates written after it. The number of existing rows this migration would refuse or rewrite is
-- therefore 0, whatever user_threads holds.
select 0;
