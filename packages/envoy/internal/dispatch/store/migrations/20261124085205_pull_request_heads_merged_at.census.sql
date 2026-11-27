-- 20261124085205 adds one nullable column with no default to pull_request_heads and writes no row:
-- every stored row reads null there, which the readers order after every row whose merge time is
-- known. Nothing is refused or rewritten.
select 0;
