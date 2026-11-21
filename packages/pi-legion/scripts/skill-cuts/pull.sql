-- The transcript pull of build.ts's `pullTranscript`: one session file's bytes, by its path, as
-- slices of at most 1 MiB, a page of slices a call, paging by place. `pullSql` of countsql.ts
-- fills it; psql-rows.ts's `PullCheck` checks each row as it comes and gives its bytes, which
-- `pullTranscript` appends to the file it pulls into.
--
-- Placeholders, each written in the query as its name inside double braces and filled with one
-- whole SQL value:
--   path     the file's path, a string literal
--   after_o  the place the rows start after, two numbers: a piece's offset and a slice of it. The
--   after_k  first call gives -2 and 0, so the file row (offset -1) comes first; each later call
--            gives the `o` and `k` of the last row of the call before
--   page     the most rows a call returns (countsql.ts's PULL_PAGE); a call that returns fewer is
--            the last
-- A path the store does not hold returns no row.
--
-- The pieces of a file are its file row's content (offset -1: the file's first bytes, a whole
-- older file of up to hundreds of MB, or none) and its parts (offset start_offset, the part's
-- byte offset in the file), each cut into slices at every 1048576 bytes, each cut moved back to
-- the first byte of the UTF-8 character it falls in, as Oh My Pi cuts its parts. So no slice is
-- over 1 MiB or splits a character, and a piece of no bytes is one slice of no text. The page's
-- slices are picked from the pieces' byte lengths alone (octet_length reads no stored text); then
-- each piece of the page is read once into one copy (convert_to, behind an OFFSET 0 fence; the
-- slices' number is read from that copy, so the copy is made before the slices and not again for
-- each), and its slices of the page are cut from the copy. So a call reads only the pieces it
-- returns, and its rows, which the last sort puts in order (in temporary space when they pass
-- work_mem), hold at most a page of MiB.
--
-- Rows, in (offset, slice) order, each one JSON value (psql prints each on one line):
--   {"o": -1, "k": <slice>, "c": <the slice's text>}              a slice of the file row
--   {"o": <start_offset>, "k": <slice>, "c": <the slice's text>}  a slice of a part
WITH page AS (
  SELECT pc.o, s.k
  FROM (
    SELECT -1::bigint AS o, coalesce(octet_length(f.content), 0) AS bytes
    FROM public.omp_session_files f
    WHERE f.path = {{path}}
    UNION ALL
    SELECT pt.start_offset, octet_length(pt.content)
    FROM public.omp_session_files_parts pt
    WHERE pt.path = {{path}} AND pt.start_offset >= {{after_o}}
  ) pc
  CROSS JOIN LATERAL generate_series(0, greatest((pc.bytes - 1) / 1048576, 0)) AS s(k)
  WHERE (pc.o, s.k) > ({{after_o}}, {{after_k}})
  ORDER BY pc.o, s.k
  LIMIT {{page}}
),
spans AS (
  SELECT pg.o, min(pg.k) AS first_k, max(pg.k) AS last_k
  FROM page pg
  GROUP BY pg.o
)
SELECT sl.part
FROM spans sp
CROSS JOIN LATERAL (
  SELECT convert_to(coalesce(x.content, ''), 'UTF8') AS b
  FROM (
    SELECT f.content
    FROM public.omp_session_files f
    WHERE sp.o = -1 AND f.path = {{path}}
    UNION ALL
    SELECT pt.content
    FROM public.omp_session_files_parts pt
    WHERE pt.path = {{path}} AND pt.start_offset = sp.o
  ) x
  OFFSET 0
) pb
CROSS JOIN LATERAL generate_series(
  sp.first_k,
  least(sp.last_k, greatest((octet_length(pb.b) - 1) / 1048576, 0))
) AS s(k)
-- The slice's first byte and the byte after it: its two cuts, each moved back over continuation
-- bytes (10xxxxxx) to the first byte of its character.
CROSS JOIN LATERAL (
  SELECT min(cut.at) AS lo, max(cut.at) AS hi
  FROM (VALUES (s.k * 1048576), ((s.k + 1) * 1048576)) AS v(nominal)
  CROSS JOIN LATERAL (
    SELECT CASE
      WHEN v.nominal >= octet_length(pb.b) THEN octet_length(pb.b)
      WHEN get_byte(pb.b, v.nominal) & 192 <> 128 THEN v.nominal
      WHEN get_byte(pb.b, v.nominal - 1) & 192 <> 128 THEN v.nominal - 1
      WHEN get_byte(pb.b, v.nominal - 2) & 192 <> 128 THEN v.nominal - 2
      ELSE v.nominal - 3
    END AS at
  ) cut
) bd
CROSS JOIN LATERAL (
  SELECT json_build_object(
    'o', sp.o,
    'k', s.k,
    'c', convert_from(substring(pb.b FROM bd.lo + 1 FOR bd.hi - bd.lo), 'UTF8')
  )::text AS part
) sl
ORDER BY sp.o, s.k;
