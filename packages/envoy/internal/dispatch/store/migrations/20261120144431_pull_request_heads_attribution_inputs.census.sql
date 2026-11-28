-- 20261120144431 adds five nullable columns with no default to pull_request_heads and writes no
-- row: every stored row reads null there, which a change of an issue's links judges as the release
-- before judged every row. Nothing is refused or rewritten.
select 0;
