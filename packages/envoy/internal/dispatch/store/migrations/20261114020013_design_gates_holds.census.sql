-- 20261114020013 adds one nullable column to design_gates with no default and no constraint, so
-- every stored row takes the null without being rewritten and nothing is refused. It cannot name
-- the column, which does not exist when the census is taken.
select 0;
