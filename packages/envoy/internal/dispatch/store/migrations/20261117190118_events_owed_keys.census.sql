-- 20261117190118 adds a nullable column with no default to events and writes no row: every
-- stored row reads null there, which the mention copy order reads as an event the outbox has not
-- recorded yet. Nothing is refused or rewritten.
select 0;
