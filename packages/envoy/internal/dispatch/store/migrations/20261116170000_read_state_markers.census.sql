-- 20261116170000 adds one nullable timestamptz column with no default to each of five read-state
-- tables, a catalog change, and one trigger function attached to each of them before each update.
-- No stored row is read or rewritten, every existing row reads null, which no constraint can
-- refuse, and the trigger runs only on updates written after it. The number of existing rows this
-- migration would refuse or rewrite is therefore 0, whatever the five tables hold.
select 0;
