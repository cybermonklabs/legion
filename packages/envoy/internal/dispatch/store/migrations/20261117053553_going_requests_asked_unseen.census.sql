-- 20261117053553 adds one column to going_requests with a constant default, so every existing row
-- takes that default without being rewritten, and no row is refused. It cannot name the column,
-- which does not exist when the census is taken.
select 0;
