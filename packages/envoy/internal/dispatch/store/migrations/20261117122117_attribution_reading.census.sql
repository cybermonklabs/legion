-- 20261117122117 creates the reading, its identity and a trigger that runs only on later writes
-- of pull_request_texts: each statement writes the catalog alone, so it refuses and rewrites no
-- row. It runs no DO block and no body the census parses names a relation of pg_catalog, so it
-- takes no exemption from a later migration pending in the same deploy.
select 0;
