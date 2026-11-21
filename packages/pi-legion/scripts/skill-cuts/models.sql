SET LOCAL statement_timeout = '15min';
-- The model of each session of one session store and one UTC day (or hour) of file mtime, for the
-- behaviour test's model population: which models the sessions that read a skill ran on.
-- `countsql.ts --query models --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD>
-- [--hour <0-23>]` prints it with every placeholder filled; that text goes into the store's read
-- command unchanged, as count.sql's does. The files are the same as count.sql's query of that
-- store and day reads, so each file hash of the count has its model in the query of its day.
--
-- Placeholders, each written in the query as its name inside double braces and filled with one
-- whole SQL value:
--   store      the store's database name, a string: a value of each row, never a table name
--   day_start  the first mtime_ms of the files read, epoch milliseconds, inclusive
--   day_end    the mtime_ms bound, epoch milliseconds, exclusive
-- (A session's model does not depend on the read window, so count.sql's window placeholders are
-- not here.)
--
-- What it reads. The files of public.omp_session_files whose mtime_ms is in [day_start, day_end),
-- and of each only its head: the file row's `content`, then each part of
-- public.omp_session_files_parts whose start_offset is below 262144, in start_offset order, each
-- piece cut to its first 262144 characters and the head too (256 Ki). A file row can hold a whole
-- older file of hundreds of MB, and a part can be large, so every piece is cut before it is
-- joined; a Legion file's first part is often about 4 KB, which ends before its first model line,
-- so the head takes the parts after it too. The head's lines that hold `"type":"model_change"`,
-- `"type":"session_init"`, `"type":"assistant"` or `"attachment":{"type":"model"` are parsed as
-- jsonb; one that is not one JSON value (the head's last line, cut where the head ends) is counted
-- in bad_lines and read no further.
--
-- Memory. Each file's head is built, read and dropped inside one LATERAL, one file at a time, and
-- only each file's result (its hash, model, source and counts) is materialized, so the query's
-- temporary space does not grow with the heads (materializing every head of a day can pass the
-- server's temp_file_limit on a busy store's day). A day that still
-- passes the statement timeout is split into its hours with `--hour`, as count.sql's is.
--
-- The model of a file, from its head, the first of these sources that gives one (`source`):
--   model_change      Oh My Pi: the first `model_change` line's `model` (`provider/modelId`),
--                     which a subagent's file holds too, before its session_init line;
--   session_init      Oh My Pi writes one only in a subagent's file: its `resolvedModel`, the
--                     provider/model selector the subagent was given (a thinking suffix such as
--                     `:low` kept as written). It follows the subagent's whole system prompt, at
--                     the end of the file's longest line, so a head that ends inside that line
--                     gives none;
--   assistant         Claude Code: the first assistant message's `message.model` that is not
--                     `<synthetic>` (Claude Code's model id, which names no provider);
--   model attachment  Claude Code: the `model` attachment's `identity.modelId`, which Claude Code
--                     writes near the file's start, before its first assistant message;
-- else none: the model is null (the head holds no such line).
--
-- Rows: one JSON value per row (psql prints each on one line), the files in hash order, then the
-- summary:
--   {"row": "model", "store", "hash" (md5 of the path cut to 16 hex characters, as count.sql names
--    a file), "model" (or null), "source" (one of the sources above, or null)}
--   {"row": "summary", "store", "mtime_from", "mtime_to", "files" (files read), "with_model"
--    (files whose model is not null), "bytes" (bytes read: each head), "lines_parsed",
--    "bad_lines"}
-- No path or line text leaves the database.
WITH
per_file AS MATERIALIZED (
  SELECT left(md5(f.path), 16) AS hash, octet_length(hd.head) AS bytes, pm.model, pm.source,
    pm.lines_parsed, pm.bad_lines
  FROM public.omp_session_files f
  CROSS JOIN LATERAL (
    SELECT substr(
      coalesce(substr(f.content, 1, 262144), '') || coalesce((
        SELECT string_agg(substr(pt.content, 1, 262144), '' ORDER BY pt.start_offset)
        FROM public.omp_session_files_parts pt
        WHERE pt.path = f.path AND pt.start_offset < 262144
      ), ''),
      1,
      262144
    ) AS head
    OFFSET 0
  ) hd
  CROSS JOIN LATERAL (
    SELECT
      count(*) AS lines_parsed,
      count(*) FILTER (WHERE p.j IS NULL) AS bad_lines,
      (array_agg(m.model ORDER BY m.rank, p.n) FILTER (WHERE m.model IS NOT NULL))[1] AS model,
      (array_agg(m.source ORDER BY m.rank, p.n) FILTER (WHERE m.model IS NOT NULL))[1] AS source
    FROM (
      SELECT t.n, CASE WHEN pg_input_is_valid(t.ln, 'jsonb') THEN t.ln::jsonb END AS j
      FROM string_to_table(hd.head, E'\n') WITH ORDINALITY AS t(ln, n)
      WHERE strpos(t.ln, '"type":"model_change"') > 0
        OR strpos(t.ln, '"type":"session_init"') > 0
        OR strpos(t.ln, '"type":"assistant"') > 0
        OR strpos(t.ln, '"attachment":{"type":"model"') > 0
    ) p
    LEFT JOIN LATERAL (
      SELECT x.source, x.rank, x.model
      FROM (
        SELECT 'model_change' AS source, 0 AS rank, p.j ->> 'model' AS model
        WHERE p.j ->> 'type' = 'model_change'
        UNION ALL
        SELECT 'session_init', 1, p.j ->> 'resolvedModel'
        WHERE p.j ->> 'type' = 'session_init'
        UNION ALL
        SELECT 'assistant', 2, p.j -> 'message' ->> 'model'
        WHERE p.j ->> 'type' = 'assistant' AND p.j -> 'message' ->> 'model' <> '<synthetic>'
        UNION ALL
        SELECT 'model attachment', 3, p.j -> 'attachment' -> 'identity' ->> 'modelId'
        WHERE p.j -> 'attachment' ->> 'type' = 'model'
      ) x
      WHERE x.model <> ''
    ) m ON true
  ) pm
  WHERE f.mtime_ms >= {{day_start}} AND f.mtime_ms < {{day_end}}
)
SELECT o.total
FROM (
  SELECT 0 AS pos, pf.hash,
    json_build_object(
      'row', 'model', 'store', {{store}}, 'hash', pf.hash, 'model', pf.model, 'source', pf.source
    )::text AS total
  FROM per_file pf
  UNION ALL
  SELECT 1, NULL,
    json_build_object(
      'row', 'summary', 'store', {{store}}, 'mtime_from', {{day_start}}::bigint,
      'mtime_to', {{day_end}}::bigint,
      'files', (SELECT count(*) FROM per_file),
      'with_model', (SELECT count(*) FROM per_file WHERE model IS NOT NULL),
      'bytes', (SELECT coalesce(sum(bytes), 0) FROM per_file),
      'lines_parsed', (SELECT coalesce(sum(lines_parsed), 0) FROM per_file),
      'bad_lines', (SELECT coalesce(sum(bad_lines), 0) FROM per_file)
    )::text
) o
ORDER BY o.pos, o.hash COLLATE "C";
