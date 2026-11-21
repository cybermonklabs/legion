-- The candidate sessions of the behaviour test: for a skill, the paths of the sessions with a
-- read of a given copy in the window, and no text. count.sql's rows already name, per (skill,
-- copy, kind),
-- every file with a read in the window by its hash (`md5(path)` cut to 16 hex characters); the
-- caller takes the hashes of the rows of the skill whose copy maps to the cut repository's copy
-- (kind `read`, across every day of the store), and this query names their paths. It reads only
-- public.omp_session_files, so the parts are not read again for each skill. It reads the whole
-- table and takes md5 of every path once whatever the number of hashes, so build.ts gives it all
-- of a store's hashes in one query. `sessionsSql` of countsql.ts fills it.
--
-- Placeholder, written in the query as its name inside double braces and filled with one whole SQL
-- value:
--   hashes  the hashes, a text array (`ARRAY['0123456789abcdef', …]::text[]`)
--
-- Rows, by path, each one JSON value: {"path": <the file's path>, "hash": <its hash>}. A hash of a
-- file the store no longer holds has no row.
SELECT json_build_object('path', f.path, 'hash', left(md5(f.path), 16))::text AS session
FROM public.omp_session_files f
WHERE left(md5(f.path), 16) = ANY ({{hashes}})
ORDER BY f.path COLLATE "C";
