-- 20261126133047 adds one column to project_delivery_settings with a constant default, so every
-- existing row takes that default, the empty list, without being rewritten, and the column's check,
-- which the empty list satisfies, refuses none of them. It cannot name the column, which does not
-- exist when the census is taken.
select 0;
