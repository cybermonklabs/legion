-- One function and two triggers, all new: it reads no table and rewrites or refuses no row
-- already stored. The triggers carry later writes of embeddings to embeddings_partitioned, which
-- is new work on a new write rather than a rewrite.
select 0;
