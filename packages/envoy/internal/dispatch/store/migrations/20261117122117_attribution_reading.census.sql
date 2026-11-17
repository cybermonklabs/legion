-- 20261117122117 creates the reading, its identity and a trigger that runs only on later writes
-- of pull_request_texts: each statement writes the catalog alone, so it refuses and rewrites no
-- row. Its DO block is no catalog-only statement, so the census holds pull_request_texts, the
-- table its trigger names, to its 1 GiB limit, and the block takes the table-size exemption and
-- the concurrent index exception from every later migration pending in the same deploy.
select 0;
