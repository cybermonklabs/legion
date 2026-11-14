-- 20261109221325 adds three nullable columns to asks with no default and no check, so every
-- stored row takes the null without being rewritten and no row is refused. It cannot name the
-- columns, which do not exist when the census is taken.
select 0;
