SET LOCAL statement_timeout = '15min';
-- The head of each session of a list, for the capture's harness-string rule (capture.ts, "The
-- harness strings"): what a captured body shows of the session it stands for, in place of the
-- capture's own working directory and date. `sessionHeadSql` of countsql.ts fills it, and
-- capture.ts's `sessionHeads` runs it through the store's read command and saves each head.
--
-- Placeholder, written in the query as its name inside double braces and filled with one whole SQL
-- value:
--   hashes  the sessions' path hashes, a text array (`ARRAY['<64 hex>', …]::text[]`), each the
--           SHA-256 in hex of the file's path, as cases.ts's `pathHash` names a case's session
--
-- What it reads. The path of every file of public.omp_session_files, once, to hash it (the store
-- names a file by its path alone, so a hash is matched by hashing each path, as sessions.sql does
-- with md5); then of each file of a hash only its head, as models.sql reads one: the file row's
-- `content`, then each part of public.omp_session_files_parts whose start_offset is below 262144,
-- in start_offset order, each piece cut to its first 262144 characters and the head too (256 Ki).
-- Each head is built, read and dropped inside one LATERAL, one file at a time. The head's lines
-- that hold `"type":"session"` (the header; not `"type":"session_init"`, whose closing quote comes
-- later), `"customType":"date-cwd-reminder"` (a reminder Oh My Pi 18.18.6 journaled as it sent
-- it) or `"role":"user"` are parsed as jsonb; one that is not one JSON value (the head's last
-- line, cut where the head ends) is counted in bad_lines and read no further.
--
-- Rows, one JSON value per file of a hash (psql prints each on one line), in hash order; a hash of
-- a file the store does not hold has no row:
--   {"hash": <the path hash>,
--    "cwd": the header's `cwd`, or null; "start": the header's `timestamp`, or null,
--    "first_user": {"timestamp", "attribution"} of the first message line whose message is a user
--      turn (its `message.timestamp` in epoch milliseconds, and its `message.attribution`), or
--      null,
--    "reminders": [{"turn", "reminder", "placement"}, …] of each `date-cwd-reminder` entry's
--      `data`, in file order (`turn` is the key of the turn it went on, `user:<timestamp>:
--      <attribution>`; `placement` is `prefix` for a reminder put first in that user turn),
--    "bytes": the head's bytes, "bad_lines": the lines that did not parse}
-- The path never leaves the database; the header's working directory and the reminders' text do,
-- since a capture shows them.
WITH wanted AS MATERIALIZED (
  SELECT h.path, h.hash
  FROM (
    SELECT f.path, encode(sha256(convert_to(f.path, 'UTF8')), 'hex') AS hash
    FROM public.omp_session_files f
  ) h
  WHERE h.hash = ANY ({{hashes}})
)
SELECT json_build_object(
  'hash', w.hash,
  'cwd', ph.cwd,
  'start', ph.start,
  'first_user', ph.first_user,
  'reminders', ph.reminders,
  'bytes', octet_length(hd.head),
  'bad_lines', ph.bad_lines
)::text AS head
FROM wanted w
CROSS JOIN LATERAL (
  SELECT substr(
    coalesce((
      SELECT substr(f.content, 1, 262144) FROM public.omp_session_files f WHERE f.path = w.path
    ), '') || coalesce((
      SELECT string_agg(substr(pt.content, 1, 262144), '' ORDER BY pt.start_offset)
      FROM public.omp_session_files_parts pt
      WHERE pt.path = w.path AND pt.start_offset < 262144
    ), ''),
    1,
    262144
  ) AS head
  OFFSET 0
) hd
CROSS JOIN LATERAL (
  SELECT
    (array_agg(p.j ->> 'cwd' ORDER BY p.n) FILTER (WHERE p.j ->> 'type' = 'session'))[1] AS cwd,
    (array_agg(p.j ->> 'timestamp' ORDER BY p.n) FILTER (WHERE p.j ->> 'type' = 'session'))[1]
      AS start,
    (array_agg(
      json_build_object(
        'timestamp', p.j -> 'message' -> 'timestamp',
        'attribution', p.j -> 'message' ->> 'attribution'
      ) ORDER BY p.n
    ) FILTER (WHERE p.j ->> 'type' = 'message' AND p.j -> 'message' ->> 'role' = 'user'))[1]
      AS first_user,
    coalesce(
      json_agg(
        json_build_object(
          'turn', p.j -> 'data' ->> 'turn',
          'reminder', p.j -> 'data' ->> 'reminder',
          'placement', p.j -> 'data' ->> 'placement'
        ) ORDER BY p.n
      ) FILTER (WHERE p.j ->> 'type' = 'custom' AND p.j ->> 'customType' = 'date-cwd-reminder'),
      '[]'::json
    ) AS reminders,
    count(*) FILTER (WHERE p.j IS NULL) AS bad_lines
  FROM (
    SELECT t.n, CASE WHEN pg_input_is_valid(t.ln, 'jsonb') THEN t.ln::jsonb END AS j
    FROM string_to_table(hd.head, E'\n') WITH ORDINALITY AS t(ln, n)
    WHERE strpos(t.ln, '"type":"session"') > 0
      OR strpos(t.ln, '"customType":"date-cwd-reminder"') > 0
      OR strpos(t.ln, '"role":"user"') > 0
  ) p
) ph
ORDER BY w.hash COLLATE "C";
