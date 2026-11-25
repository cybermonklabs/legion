-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own. envoy-dispatch backfill-graph-store then refuses to run, naming it, until it is built again:
-- its version stays recorded, so no boot builds it again, and the refusal prints the statements
-- that build it by hand.
drop index concurrently if exists events_comment_created_id;
