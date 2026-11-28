-- 20261128062654 replaces one function's body and does nothing else: it names no table, so it
-- locks none, and it refuses and rewrites no row. Every embeddings row already stored stays as it
-- is; the function enqueues the same text it enqueued before, only fewer times in a multi-row
-- insert, and not at all for a version inserted below the document's latest, which no writer does
-- (the migration's own comment says what that skips).
select 0;
