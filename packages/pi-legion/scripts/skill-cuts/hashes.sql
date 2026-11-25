SET LOCAL statement_timeout = '15min';
-- The session files this Postgres store owns for the count, whether or not they read a skill.
-- window_start is the inclusive epoch millisecond bound from the count's configuration.
-- A file can be modified after the window ends, so there is no upper mtime bound.
SELECT json_build_object('hash', left(md5(path), 16))::text AS file_hash
FROM public.omp_session_files
WHERE mtime_ms >= {{window_start}};
