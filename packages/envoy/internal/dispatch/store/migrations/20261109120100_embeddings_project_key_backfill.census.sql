-- A new table, embeddings_project_key_backfill, whose one row this migration inserts: it
-- writes no embeddings row, reads no other table, and refuses and rewrites nothing already
-- stored. The labelling whose progress that row records runs at boot, in committed batches
-- outside every migration transaction (store.FillEmbeddingProjectKeys).
select 0;
