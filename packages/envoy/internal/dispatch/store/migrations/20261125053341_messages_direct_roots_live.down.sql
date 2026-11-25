-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the Inbox's Agent roots are then read through messages_direct_roots, every one-off root
-- of the person's, dismissed or not. 20261125052902 and 20261125053125, which keep the column it
-- is partial on, have no rollback script.
drop index concurrently if exists messages_direct_roots_live;
