-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own. The Dismissed state's reads then go through user_threads_dismissed again, and walk past
-- the rows a person's own reply dismissed.
drop index concurrently if exists user_threads_dismissed_replied;
