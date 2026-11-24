-- 20261117022936 creates legion_current and fills it from legion_observations, which it only reads,
-- and gives legion_observations a trigger, which runs on later writes alone: no row is refused or
-- rewritten.
select 0;
