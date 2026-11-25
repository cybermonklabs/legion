SET LOCAL statement_timeout = '15min';
-- Read the same path hashes as count.sql. Only the hash and profile leave the store.
SELECT json_build_object(
  'hash', left(md5(f.path), 16),
  'profile', coalesce(substring(f.path COLLATE "C" FROM {{named_root}}),
    CASE WHEN f.path COLLATE "C" ~ {{default_root}} THEN 'default' ELSE 'unknown' END)
)::text AS root
FROM public.omp_session_files f
ORDER BY left(md5(f.path), 16) COLLATE "C";
