-- pgmigrate:non-transactional
-- 20261125163550 gave asks a foreign key on (node_kind, node_address) and no index over those
-- columns, and Postgres indexes the referenced side of a key and never the referencing side. So
-- every delete of a graph_nodes row would read asks once for each node deleted, with nothing to
-- read it by, and graph_edges' tuple arm (20261125163553) probes asks for the row that names an
-- ask's node once for each anchored_to and owned_by edge it answers. This is the index both read.
-- 20261111043000 carries the measurement for the first: deleting 16,809 nodes with no such index
-- read its table for each one and took 2 minutes 51 seconds.
--
-- It is the one statement of this file because pgmigrate admits exactly one concurrent index
-- build in a non-transactional migration (nontransactional.go's concurrentIndex). It is a
-- performance index: no constraint depends on it. A failed concurrent build leaves an INVALID index
-- of this name on asks, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the
-- next attempt.
create index concurrently if not exists asks_node_address
    on asks (node_kind, node_address);
