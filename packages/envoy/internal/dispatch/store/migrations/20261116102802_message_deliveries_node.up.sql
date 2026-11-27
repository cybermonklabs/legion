-- pgmigrate:non-transactional
-- 20261116102800 gave message_deliveries a foreign key on (node_kind, node_address) and no index
-- over those columns, and Postgres indexes the referenced side of a key and never the referencing
-- side. So every delete of a graph_nodes row would read this table once for each node deleted,
-- with nothing to read it by, as 20261116102801 says of comment_deliveries.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on message_deliveries, which the runner
-- drops with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists message_deliveries_node_address
    on message_deliveries (node_kind, node_address);
