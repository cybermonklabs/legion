-- 20261120105330 creates embeddings_partitioned, its partitions, its progress row and functions,
-- and a trigger on projects. It reads projects for their keys and rewrites, refuses and copies
-- no row of an existing table: the copy of embeddings is store.FillEmbeddingProjectKeys', in
-- committed batches after the boot.
select 0;
