-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the store census's count of copied_from and moved_to then reads graph_tuples whole
-- again.
drop index concurrently if exists graph_tuples_store_only;
