-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the block-index tick then reads every citation of each item kind on each tick that owes
-- its catch-up.
drop index concurrently if exists refs_item_tuples_unstored;
