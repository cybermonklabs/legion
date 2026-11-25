-- 20261120122600 adds store_owed_rule to document_block_versions and marking_rule to
-- block_index_event_cursors and graph_backfill_horizon, each integer not null default 1, and keys
-- graph_backfill_horizon by marking_rule in place of its id. Postgres keeps a constant default in
-- the catalog, so no row is rewritten and none can violate the not null, and the one row the id's
-- check let graph_backfill_horizon hold takes rule 1, which the new key refuses no row over. The
-- census cannot name the new columns, which do not exist when it is taken, nor the two tables
-- 20261118115645 creates in the same release.
select 0
