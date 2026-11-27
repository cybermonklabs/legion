-- 20261120124333 adds a nullable column with no default to doc_settlements_pending and writes no
-- row: every stored row reads null there, which the resumption reads as held by no settlement.
-- Nothing is refused or rewritten.
select 0;
