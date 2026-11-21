SET LOCAL statement_timeout = '15min';
-- The skill read count for one session store and one UTC day (or hour) of file
-- mtime. `countsql.ts --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>]`
-- prints it with every placeholder filled; that text goes into the store's read command unchanged.
--
-- Placeholders, each written in the query as its name inside double braces and filled with one
-- whole SQL value (a number, or a string literal with its quotes):
--   store         the store's database name, a string: a value of each row, never a table name
--   day_start     the first mtime_ms of the files read, epoch milliseconds, inclusive
--   day_end       the mtime_ms bound, epoch milliseconds, exclusive
--   window_start  the window's start, an ISO instant string, inclusive: a read counts when its
--                 call line's timestamp is in the window
--   window_end    the window's end, an ISO instant string, exclusive
--
-- What it reads. The files of public.omp_session_files whose mtime_ms is in [day_start, day_end),
-- so each file is in exactly one query of a run, and each file's bytes: the file row's `content`
-- first (the bytes before every part), then public.omp_session_files_parts in start_offset order.
-- Every line is read whole, a line split between rows included, and no file is ever joined into
-- one value: a file reaches hundreds of MB, and the server is shared. Per piece, the text before
-- its first newline (head), the whole lines, and the text after its last newline (tail) are told
-- apart. A piece that holds no newline lies inside one line. A line that crosses pieces is the
-- tail of one piece, every following piece with no newline, and the head of the next piece that
-- holds one; those fragments share a group, the running count of newline-holding pieces of the
-- file, and are joined in offset order. So no file is joined whole: the memory a file takes is
-- its largest piece, which is copied whole (below), and the lines it joins (those that cross a
-- piece and those that hold a target), not the whole file. A file row that holds a whole older
-- file is one piece of up to hundreds of MB.
--
-- Passes: one over the day's files, one file at a time, and over each file's parts. The parts of
-- a file come in the primary key's order, so no sort moves their text, and each piece is
-- decompressed once: one plain copy of it is made behind an OFFSET 0 fence and every function
-- below reads that copy (the file row's copy, its first newline and its tail are made once per
-- file, and its parts read its state from them). The whole lines of a piece are split out only
-- when the piece may hold a target: when it holds `kill`, `KILL`, `/references/` or
-- `rror":true`, as every target below does. Only the fragments of lines that cross a part
-- boundary, the lines that hold a target and two total rows per file go through a sort, one file
-- at a time, to be joined. Only those lines are parsed as jsonb, each inside its file's LATERAL,
-- where it is read into its events and dropped: only the events, one small row per parsed line
-- and each file's totals are materialized, never a line's text, as models.sql materializes only
-- each file's result.
--
-- The targets a line must hold to be parsed: one of `skill://`, `SKILL.md`, `/references/`,
-- `"Skill"`, `Launching skill: `, `Base directory for this skill: `, `"isError":true` or
-- `"is_error":true`, and one of `"toolCall"`, `"toolResult"`, `"tool_use"`, `"tool_result"` or
-- `"sourceToolUseID"`. A line that holds them and is not one JSON value (a file's last line while
-- it is written) is counted in bad_lines and read no further.
--
-- A read is a call whose line's `timestamp` is in the window:
--   Oh My Pi: an assistant message's `toolCall` named `read` whose `arguments.path` is
--     `skill://<name>`, `skill://<name>/SKILL.md` or a path ending `/<name>/SKILL.md` (kind
--     `read`), or `skill://<name>/<other>` or a path holding `skills/<name>/references/` (kind
--     `reference`). A read selector after the last `/` (`:raw`, `:1-40`) is taken off first.
--     Its answer is the `toolResult` of the same file with the same `toolCallId`; the copy is the
--     path in that result's first line `[Skill file: <path>]`, else the call's own path.
--   Claude Code: an assistant `tool_use` named `Skill` whose input `skill` (or `command`) is
--     `<name>` or `<plugin>:<name>` (kind `read`; the copy is `<dir>/SKILL.md` from the line
--     `Base directory for this skill: <dir>` whose `sourceToolUseID` is the call's id, else the
--     input itself), or named `Read` whose `file_path` is a SKILL.md or references path as above
--     (the copy is that path). Its answer is the `tool_result` with the same `tool_use_id`.
--   A call whose every answer is an error (`isError`, `is_error`) is no read and is not counted.
--   A call with no answer in its file is unpaired: it is counted apart, in `unpaired`, with its own
--   path as the copy.
--   A call whose toolCallId and timestamp are both in two files of this query is one read (a forked
--   session copies its parent's lines): it counts in the file whose path sorts first, and
--   `shared_call_ids` counts such calls.
--   Each (file, skill name, copy, kind) counts once, on the UTC day of its first read in the
--   window; a file's paired reads and unpaired calls are kept apart.
--
-- Rows: one JSON value per row (psql prints each on one line), ordered by day, skill, copy, kind,
-- then the summary:
--   {"row": "count", "store", "day" (first read, YYYY-MM-DD), "skill", "copy", "kind",
--    "files" (distinct files with a paired read), "hashes", "unpaired" (distinct files with an
--    unpaired call), "unpaired_hashes"}
--   {"row": "summary", "store", "mtime_from", "mtime_to", "files" (files read), "parts", "bytes"
--    (bytes read: the file rows' content and every part), "lines_parsed", "bad_lines",
--    "shared_call_ids"}
-- Why the rows carry hashes beside the totals: a session can be written again while the count
-- runs (a resumed session), so its mtime_ms day can move after its first day was queried and the
-- same file can be in two queries' rows. Each row names its files as `md5(path)` cut to 16 hex
-- characters, sorted, in a JSON array, so the client counts distinct hashes across queries. No
-- path or line text leaves the database.
WITH
-- Per file, one file at a time: its totals and the events of its lines that hold a target. Each
-- such line is joined whole, parsed, read into its events and dropped inside the file's LATERAL;
-- what is materialized is each event, one `line` row per parsed line (`ok` false when it is not
-- one JSON value), and the file's `total` rows (`bytes` and `parts` set). An event is a call, an
-- answer or a Claude Code skill directory: `tool` is `read` for a read by path (Oh My Pi `read`,
-- Claude Code `Read`) and `skill` for a Claude Code `Skill` call; `arg` is the path or the skill
-- input; `ok` is false for an error answer; `copy` is the answer's `[Skill file: …]` path or the
-- skill directory's SKILL.md.
events AS MATERIALIZED (
  SELECT f.path, e.ev, e.id, e.ts, e.tool, e.arg, e.ok, e.copy, e.bytes, e.parts
  FROM public.omp_session_files f
  -- The file row's text, one plain copy made once behind OFFSET 0 fences, with its nl1 and tail
  -- (as for each part below), which also give the file row's state to its first part.
  CROSS JOIN LATERAL (
    SELECT x.c, strpos(x.c, E'\n') AS nl1, split_part(x.c, E'\n', -1) AS tail
    FROM (SELECT coalesce(f.content, '') || '' AS c OFFSET 0) x
    OFFSET 0
  ) fr
  -- The file's totals (bytes is set, line is null) and each line that holds a target (line is
  -- set), whole.
  CROSS JOIN LATERAL (
    SELECT g.bytes, g.parts, g.line
    FROM (
      SELECT
        sum(pc.bytes)::bigint AS bytes,
        sum(pc.parts)::bigint AS parts,
        string_agg(pc.txt, '' ORDER BY pc.ord) AS line
      FROM (
        -- Each piece of the file: the file row (offset -1), then the parts in start_offset
        -- order. c is the piece's text, nl1 its first newline (0 for none), tail the text after
        -- its last newline (all of it when it holds none, empty when it ends with one);
        -- prev_open is whether the piece before it ended inside a line, grp the group of the
        -- line its tail begins. Each is computed once, behind OFFSET 0 fences.
        SELECT -1::bigint AS off, fr.c, fr.nl1, fr.tail, false AS prev_open,
          (fr.nl1 > 0)::int::bigint AS grp, true AS last, octet_length(fr.c)::bigint AS bytes,
          0::bigint AS parts
        UNION ALL
        SELECT
          q.off,
          dc.c,
          dc.nl1,
          dc.tail,
          coalesce(lag(dc.tail <> '') OVER win, fr.tail <> ''),
          (fr.nl1 > 0)::int::bigint + count(*) FILTER (WHERE dc.nl1 > 0) OVER win,
          lead(q.off) OVER win IS NULL,
          sum(octet_length(dc.c)) OVER win,
          count(*) OVER win
        FROM (
          -- The parts in the primary key's order, so no sort moves their text.
          SELECT pt.start_offset AS off, pt.content AS raw
          FROM public.omp_session_files_parts pt
          WHERE pt.path = f.path
          ORDER BY pt.start_offset
          OFFSET 0
        ) q
        -- One plain copy of the part's text, decompressed once, read by everything above.
        CROSS JOIN LATERAL (
          SELECT x.c, strpos(x.c, E'\n') AS nl1, split_part(x.c, E'\n', -1) AS tail
          FROM (SELECT coalesce(q.raw, '') || '' AS c OFFSET 0) x
          OFFSET 0
        ) dc
        WINDOW win AS (ORDER BY q.off ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
      ) s
      -- Whether the piece may hold a target: a cheap test that every target passes (`kill`,
      -- `KILL`, `/references/`, `rror":true`); each line is held to the exact targets below.
      CROSS JOIN LATERAL (
        SELECT (strpos(s.c, 'kill') > 0 OR strpos(s.c, 'KILL') > 0
          OR strpos(s.c, '/references/') > 0 OR strpos(s.c, 'rror":true') > 0) AS hit
        OFFSET 0
      ) h
      CROSS JOIN LATERAL (
        -- A part with no newline lies inside one line: group grp.
        SELECT s.grp AS k1, -2::bigint AS k2, 0::bigint AS k3, s.off AS ord, s.c AS txt,
          NULL::bigint AS bytes, NULL::bigint AS parts
        WHERE s.nl1 = 0 AND s.c <> ''
        UNION ALL
        -- The head ends a line that began in an earlier part: group grp - 1.
        SELECT s.grp - 1, -2, 0, s.off, split_part(s.c, E'\n', 1), NULL, NULL
        WHERE s.nl1 > 0 AND s.prev_open
        UNION ALL
        -- The tail begins a line that ends in a later part: group grp.
        SELECT s.grp, -2, 0, s.off, s.tail, NULL, NULL
        WHERE s.nl1 > 0 AND s.tail <> ''
        UNION ALL
        -- The whole lines of a part that may hold a target (the head too when no line runs into
        -- the part), each a group of its own, kept when it holds the targets: the text from the
        -- first line it begins to its last newline.
        SELECT 0, s.off, t.n, 0, t.ln, NULL, NULL
        FROM string_to_table(
          CASE
            WHEN h.hit AND s.nl1 > 0 THEN
              substr(
                s.c,
                CASE WHEN s.prev_open THEN s.nl1 + 1 ELSE 1 END,
                greatest(
                  length(s.c) - length(s.tail) - CASE WHEN s.prev_open THEN s.nl1 + 1 ELSE 1 END,
                  0
                )
              )
          END,
          E'\n'
        ) WITH ORDINALITY AS t(ln, n)
        WHERE (strpos(t.ln, 'skill://') > 0 OR strpos(t.ln, 'SKILL.md') > 0
            OR strpos(t.ln, '/references/') > 0 OR strpos(t.ln, '"Skill"') > 0
            OR strpos(t.ln, 'Launching skill: ') > 0
            OR strpos(t.ln, 'Base directory for this skill: ') > 0
            OR strpos(t.ln, '"isError":true') > 0 OR strpos(t.ln, '"is_error":true') > 0)
          AND (strpos(t.ln, '"toolCall"') > 0 OR strpos(t.ln, '"toolResult"') > 0
            OR strpos(t.ln, '"tool_use"') > 0 OR strpos(t.ln, '"tool_result"') > 0
            OR strpos(t.ln, '"sourceToolUseID"') > 0)
        UNION ALL
        -- The file's totals: once for its row, once after its last part.
        SELECT 0, CASE WHEN s.off = -1 THEN -3 ELSE -4 END, 0, 0, NULL, s.bytes, s.parts
        WHERE s.last
      ) pc
      -- Most parts hold whole lines and nothing that may be a target, and give no piece.
      WHERE s.nl1 = 0 OR s.prev_open OR s.tail <> '' OR s.last OR h.hit
      GROUP BY pc.k1, pc.k2, pc.k3
    ) g
    WHERE g.bytes IS NOT NULL
      OR ((strpos(g.line, 'skill://') > 0 OR strpos(g.line, 'SKILL.md') > 0
          OR strpos(g.line, '/references/') > 0 OR strpos(g.line, '"Skill"') > 0
          OR strpos(g.line, 'Launching skill: ') > 0
          OR strpos(g.line, 'Base directory for this skill: ') > 0
          OR strpos(g.line, '"isError":true') > 0 OR strpos(g.line, '"is_error":true') > 0)
        AND (strpos(g.line, '"toolCall"') > 0 OR strpos(g.line, '"toolResult"') > 0
          OR strpos(g.line, '"tool_use"') > 0 OR strpos(g.line, '"tool_result"') > 0
          OR strpos(g.line, '"sourceToolUseID"') > 0))
  ) fl
  -- The line parsed, once: null when it is not one JSON value, and for a totals row.
  CROSS JOIN LATERAL (
    SELECT CASE WHEN pg_input_is_valid(fl.line, 'jsonb') THEN fl.line::jsonb END AS j
    OFFSET 0
  ) p
  CROSS JOIN LATERAL (
    SELECT CASE
      WHEN pg_input_is_valid(p.j ->> 'timestamp', 'timestamptz') THEN (p.j ->> 'timestamp')::timestamptz
    END AS ts
  ) lt
  CROSS JOIN LATERAL (
    -- The file's totals.
    SELECT 'total' AS ev, NULL::text AS id, NULL::timestamptz AS ts, NULL::text AS tool,
      NULL::text AS arg, NULL::boolean AS ok, NULL::text AS copy, fl.bytes, fl.parts
    WHERE fl.bytes IS NOT NULL
    UNION ALL
    -- A parsed line: ok when it is one JSON value.
    SELECT 'line', NULL, NULL, NULL, NULL, p.j IS NOT NULL, NULL, NULL, NULL
    WHERE fl.line IS NOT NULL
    UNION ALL
    -- Oh My Pi: a read call.
    SELECT 'call', x.e ->> 'id', lt.ts, 'read', x.e -> 'arguments' ->> 'path', NULL, NULL, NULL,
      NULL
    FROM jsonb_array_elements(
      CASE
        WHEN p.j ->> 'type' = 'message' AND p.j -> 'message' ->> 'role' = 'assistant'
          AND jsonb_typeof(p.j -> 'message' -> 'content') = 'array'
        THEN p.j -> 'message' -> 'content'
      END
    ) AS x(e)
    WHERE x.e ->> 'type' = 'toolCall' AND x.e ->> 'name' = 'read'
    UNION ALL
    -- Oh My Pi: a tool result.
    SELECT 'result', p.j -> 'message' ->> 'toolCallId', NULL, NULL, NULL,
      (p.j -> 'message' -> 'isError') IS DISTINCT FROM 'true'::jsonb,
      substring(
        split_part(p.j -> 'message' -> 'content' -> 0 ->> 'text', E'\n', 1)
        FROM '^\[Skill file: (.+)\]$'
      ),
      NULL, NULL
    WHERE p.j ->> 'type' = 'message' AND p.j -> 'message' ->> 'role' = 'toolResult'
    UNION ALL
    -- Claude Code: a Skill or Read call.
    SELECT 'call', x.e ->> 'id', lt.ts,
      CASE WHEN x.e ->> 'name' = 'Skill' THEN 'skill' ELSE 'read' END,
      CASE
        WHEN x.e ->> 'name' = 'Skill' THEN coalesce(x.e -> 'input' ->> 'skill', x.e -> 'input' ->> 'command')
        ELSE x.e -> 'input' ->> 'file_path'
      END,
      NULL, NULL, NULL, NULL
    FROM jsonb_array_elements(
      CASE
        WHEN p.j ->> 'type' = 'assistant' AND jsonb_typeof(p.j -> 'message' -> 'content') = 'array'
        THEN p.j -> 'message' -> 'content'
      END
    ) AS x(e)
    WHERE x.e ->> 'type' = 'tool_use' AND x.e ->> 'name' IN ('Skill', 'Read')
    UNION ALL
    -- Claude Code: a tool result.
    SELECT 'result', x.e ->> 'tool_use_id', NULL, NULL, NULL,
      (x.e -> 'is_error') IS DISTINCT FROM 'true'::jsonb, NULL, NULL, NULL
    FROM jsonb_array_elements(
      CASE
        WHEN p.j ->> 'type' = 'user' AND jsonb_typeof(p.j -> 'message' -> 'content') = 'array'
        THEN p.j -> 'message' -> 'content'
      END
    ) AS x(e)
    WHERE x.e ->> 'type' = 'tool_result'
    UNION ALL
    -- Claude Code: the directory of the skill a Skill call loaded.
    SELECT 'base', p.j ->> 'sourceToolUseID', NULL, NULL, NULL, NULL,
      rtrim(
        substring(
          split_part(
            CASE jsonb_typeof(p.j -> 'message' -> 'content')
              WHEN 'string' THEN p.j -> 'message' ->> 'content'
              WHEN 'array' THEN p.j -> 'message' -> 'content' -> 0 ->> 'text'
            END,
            E'\n',
            1
          )
          FROM '^Base directory for this skill: (.+)$'
        ),
        '/'
      ) || '/SKILL.md',
      NULL, NULL
    WHERE p.j ->> 'type' = 'user' AND p.j ? 'sourceToolUseID'
  ) e
  WHERE f.mtime_ms >= {{day_start}} AND f.mtime_ms < {{day_end}}
),
-- Each call of a skill file, with its skill name and kind.
calls AS (
  SELECT e.path, e.id, e.ts, cl.skill, cl.kind, e.arg AS own
  FROM events e
  CROSS JOIN LATERAL (SELECT regexp_replace(e.arg, ':[^/]*$', '') AS q) z
  CROSS JOIN LATERAL (
    SELECT
      CASE
        WHEN e.tool = 'skill' THEN nullif(regexp_replace(ltrim(e.arg, '/'), '^.*:', ''), '')
        WHEN z.q ~ '^skill://' THEN substring(z.q FROM '^skill://([^/]+)')
        WHEN z.q ~ '(^|/)skills/[^/]+/references/.' THEN substring(z.q FROM '(?:^|/)skills/([^/]+)/references/')
        WHEN z.q ~ '(^|/)[^/]+/SKILL\.md$' THEN substring(z.q FROM '([^/]+)/SKILL\.md$')
      END AS skill,
      CASE
        WHEN e.tool = 'skill' THEN 'read'
        WHEN z.q ~ '^skill://[^/]+(/SKILL\.md)?/?$' THEN 'read'
        WHEN z.q ~ '^skill://[^/]+/.' THEN 'reference'
        WHEN z.q ~ '(^|/)skills/[^/]+/references/.' THEN 'reference'
        WHEN z.q ~ '(^|/)[^/]+/SKILL\.md$' THEN 'read'
      END AS kind
  ) cl
  WHERE e.ev = 'call' AND cl.skill IS NOT NULL AND cl.kind IS NOT NULL
),
-- Per call id of a file: whether an answer was ok (null: no answer), its copy, and the Claude Code
-- skill directory.
answers AS (
  SELECT e.path, e.id,
    bool_or(e.ok) FILTER (WHERE e.ev = 'result') AS ok,
    min(e.copy) FILTER (WHERE e.ev = 'result') AS header,
    min(e.copy) FILTER (WHERE e.ev = 'base') AS base
  FROM events e
  WHERE e.ev IN ('result', 'base') AND e.id IS NOT NULL
  GROUP BY e.path, e.id
),
reads AS MATERIALIZED (
  SELECT c.path, c.id, c.ts, c.skill, c.kind,
    a.ok IS TRUE AS paired,
    CASE WHEN a.ok THEN coalesce(a.header, a.base, c.own) ELSE c.own END AS copy
  FROM calls c
  LEFT JOIN answers a ON a.path = c.path AND a.id = c.id
  WHERE c.ts >= {{window_start}}::timestamptz AND c.ts < {{window_end}}::timestamptz
    AND a.ok IS DISTINCT FROM false
),
shared AS (
  SELECT r.id, r.ts
  FROM reads r
  WHERE r.id IS NOT NULL
  GROUP BY r.id, r.ts
  HAVING count(DISTINCT r.path) > 1
),
firsts AS (
  SELECT r.path, r.skill, r.copy, r.kind, r.paired, min(r.ts) AS first_ts
  FROM reads r
  WHERE NOT EXISTS (
    SELECT 1 FROM reads o
    WHERE o.id = r.id AND o.ts = r.ts AND o.path COLLATE "C" < r.path COLLATE "C"
  )
  GROUP BY r.path, r.skill, r.copy, r.kind, r.paired
),
totals AS (
  SELECT
    to_char(f.first_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
    f.skill,
    f.copy,
    f.kind,
    count(*) FILTER (WHERE f.paired) AS files,
    coalesce(json_agg(left(md5(f.path), 16) ORDER BY left(md5(f.path), 16)) FILTER (WHERE f.paired), '[]'::json) AS hashes,
    count(*) FILTER (WHERE NOT f.paired) AS unpaired,
    coalesce(json_agg(left(md5(f.path), 16) ORDER BY left(md5(f.path), 16)) FILTER (WHERE NOT f.paired), '[]'::json) AS unpaired_hashes
  FROM firsts f
  GROUP BY 1, 2, 3, 4
)
SELECT o.total
FROM (
  SELECT 0 AS pos, t.day, t.skill, t.copy, t.kind,
    json_build_object(
      'row', 'count', 'store', {{store}}, 'day', t.day, 'skill', t.skill, 'copy', t.copy,
      'kind', t.kind, 'files', t.files, 'hashes', t.hashes, 'unpaired', t.unpaired,
      'unpaired_hashes', t.unpaired_hashes
    )::text AS total
  FROM totals t
  UNION ALL
  SELECT 1, NULL, NULL, NULL, NULL,
    json_build_object(
      'row', 'summary', 'store', {{store}}, 'mtime_from', {{day_start}}::bigint,
      'mtime_to', {{day_end}}::bigint,
      'files', (SELECT count(DISTINCT e.path) FROM events e WHERE e.ev = 'total'),
      'parts', (SELECT coalesce(sum(e.parts), 0) FROM events e WHERE e.ev = 'total'),
      'bytes', (SELECT coalesce(sum(e.bytes), 0) FROM events e WHERE e.ev = 'total'),
      'lines_parsed', (SELECT count(*) FROM events e WHERE e.ev = 'line'),
      'bad_lines', (SELECT count(*) FROM events e WHERE e.ev = 'line' AND NOT e.ok),
      'shared_call_ids', (SELECT count(*) FROM shared)
    )::text
) o
ORDER BY o.pos, o.day COLLATE "C", o.skill COLLATE "C", o.copy COLLATE "C", o.kind COLLATE "C";
