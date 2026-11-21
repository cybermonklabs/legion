-- 20261120171308_pull_request_reads_owed_link_backfill.down.sql: the by-hand way back of the
-- backfill of links' owed reads. It deletes each owed read that the backfill wrote and that no
-- later owe changed: its owed_at is still the time of the migration's transaction, which the
-- runner recorded as applied_at of version 20261120171308 in the same transaction. Every later owe
-- gives the row a new owed_at: a link write's and an event's (delivery.recordLinkedOwe,
-- owePullRequestRead), and a trigger's of pull_request_heads; a read that fails or that GitHub
-- refuses leaves the row as the backfill wrote it, which is no owe of its own, and a read that
-- GitHub answered has deleted its row already. A row owed before the backfill keeps its earlier
-- owed_at, since the backfill leaves such a row as it is. Run it before the version's row is
-- deleted from schema_migrations, if that row is deleted at all: with no such row it deletes
-- nothing.
delete from pull_request_reads_owed o
where o.owed_at = (select m.applied_at from schema_migrations m where m.version = 20261120171308);
