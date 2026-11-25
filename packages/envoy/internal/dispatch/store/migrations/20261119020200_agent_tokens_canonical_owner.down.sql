-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and each read of one address's personal tokens then scans agent_tokens again.
drop index concurrently if exists agent_tokens_canonical_owner;
