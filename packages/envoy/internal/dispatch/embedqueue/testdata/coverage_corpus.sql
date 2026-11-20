-- coverage_corpus.sql builds the synthetic corpus the search coverage statement is measured on
-- (BenchmarkCoverageStatementScale, coverage_scale_test.go; dispatch://INFRA-4241, R2): the tables
-- the statement reads, in production's counts and proportions of 2026-11-17 times a scale. At
-- scale 1 that is 11,151 issues, each with a primary document of 5 versions, 216,414 document
-- versions in all, and the window rows of each kind: document_block 738,255, comment_block
-- 184,555, message_block 48,386 and ask_part 12,816.
--
-- What the session holds before it runs, since the script carries no psql meta-command (the test
-- runs it over one pgx connection, as one simple-protocol statement):
--
--   - the settings coverage_corpus.scale (a number above 0), coverage_corpus.identity (the
--     embedder identity the corpus is embedded by) and coverage_corpus.foreign_identity (the
--     identity of the few rows another embedder left), set with set_config or SET;
--   - a temporary table coverage_corpus_prose (n bigint, line text) of English prose, one line a
--     row in reading order by n, with no empty line: the test fills it with this repository's
--     own docs/ and skills/ markdown through COPY FROM STDIN, so the texts compress and hash as
--     Dispatch's do.
--
-- It runs on a migrated database that holds no issue and no embeddings row, and commits once.
--
-- The texts. A window's text is 300 to 2,400 characters of the prose (itemblocks.WindowTextChars
-- 1200 of block text plus up to 1200 of context), with the row's number after it so no two rows
-- share a text but the planted blank ones. Every text and every title starts at a word, never
-- mid-word or on white space, so the only white-space-led and non-ASCII-led ones are those planted
-- below. An issue's latest primary version is a spec: it begins with `## Summary` and is, byte for
-- byte, the next of the sizes of 30 sampled INFRA specs in turn (median 4,488 bytes, mean 14,627,
-- maximum 100,970), the read whose cost the issue arm of the statement is about.
--
-- The rows. Every issue's row holds its current text, embedded by coverage_corpus.identity, as
-- the trigger and the queue leave it. Of each window kind's rows 0.5% wait for their first vector
-- and 0.5% are dead; every 10,000th row from the second is blank (white space alone, settled with
-- no vector), the next begins with white space and the next was embedded by
-- coverage_corpus.foreign_identity; every other row is embedded at its text. Every 1,000th issue
-- from the seventh has a title that begins with a space, and the next one a title that begins
-- outside ASCII.
--
-- Disk. Every vector is the zero vector and the vector column is stored EXTENDED here, so a
-- vector compresses to under 100 bytes instead of 6 KB out of line: the coverage statements read
-- only whether embedding is null, and real vectors at twice production's size would be 12 GB of
-- TOAST that no reading of them touches.
--
-- Speed. The triggers of the four tables the load fills (issues, artifacts, artifact_versions,
-- embeddings) are off for the load (`disable trigger user`; the test's login is no superuser,
-- so session_replication_role is not open to it) and on again before the commit, inside the
-- one transaction, so no other session sees them off. Every index of embeddings that backs no
-- constraint is dropped before the load: the HNSW index embeddings_cosine is not built again,
-- since no coverage statement reads it, and every other one is built again after the load
-- from its own definition as the migrations left it (the two the coverage statement reads,
-- embeddings_window_embedded and embeddings_window_unembedded, among them), which is faster than
-- keeping each up to date row by row. VACUUM and ANALYZE are the caller's, since VACUUM cannot
-- run inside a transaction. A vacuum sets the visibility map, whose states
-- BenchmarkCoverageStatementScale measures the coverage statement in (freshly vacuumed, and with
-- the statistics recording pages all-visible that no longer are), only where every commit near
-- this one in the commit log is flushed, so BenchmarkCoverageStatementScale makes its databases
-- commit synchronously.

begin;

do $$
begin
  if current_setting('coverage_corpus.scale')::numeric <= 0 then
    raise exception 'coverage_corpus.scale is %, and must be above 0', current_setting('coverage_corpus.scale');
  end if;
  if current_setting('coverage_corpus.identity') = '' or current_setting('coverage_corpus.foreign_identity') = ''
     or current_setting('coverage_corpus.identity') = current_setting('coverage_corpus.foreign_identity') then
    raise exception 'coverage_corpus.identity and coverage_corpus.foreign_identity must name two identities';
  end if;
  if exists (select 1 from issues) or exists (select 1 from embeddings) then
    raise exception 'coverage_corpus.sql builds its corpus on a database with no issue and no embeddings row';
  end if;
end
$$;

alter table issues disable trigger user;
alter table artifacts disable trigger user;
alter table artifact_versions disable trigger user;
alter table embeddings disable trigger user;
alter table embeddings alter column embedding set storage extended;

-- The prose as one stream, each line with the byte offset at which it starts in the lines joined
-- by newlines, so a spec of a given size reads the lines it needs and no more.
create temp table coverage_corpus_line on commit drop as
select row_number() over (order by n) as n, line,
       sum(octet_length(line) + 1) over (order by n) - octet_length(line) - 1 as off
from coverage_corpus_prose;
create unique index on coverage_corpus_line (n);
create index on coverage_corpus_line (off);
analyze coverage_corpus_line;

-- What the scale makes: the issues, the document versions, and the lines a spec may start at,
-- each far enough from the prose's end that the largest spec's lines and their slack (half the
-- size again and 4 KB, below) fit after it.
create temp table coverage_corpus_plan on commit drop as
select round(11151 * s.scale)::int as issues,
       round(216414 * s.scale)::int as versions,
       (select count(*) from coverage_corpus_line where off < (select max(off) from coverage_corpus_line) - 160000) as spec_starts,
       (select count(*) from coverage_corpus_line) as lines
from (select current_setting('coverage_corpus.scale')::numeric as scale) s;

do $$
begin
  if (select spec_starts from coverage_corpus_plan) < 1000 then
    raise exception 'coverage_corpus_prose holds too little prose for a 100 KB spec: % lines', (select lines from coverage_corpus_plan);
  end if;
end
$$;

-- from_word is t from its first word on: from the first ASCII letter at its start or after white
-- space, looked for in its first 4,096 characters, and null when there is none there. The match
-- is found once and then located with strpos, which finds that very match first, since an
-- earlier occurrence of the same characters would itself be an earlier match; regexp_instr
-- collects every match in the 4,096 characters first, which measured about nine times slower.
create function pg_temp.coverage_corpus_from_word(t text) returns text
language sql immutable as $$
  select case when found.word is not null then substr(t, strpos(t, found.word) + length(found.word) - 1) end
  from (select substring(left(t, 4096) from '(?:^|[[:space:]])[A-Za-z]') as word) found
$$;

-- 20,000 texts of 300 to 2,400 characters, each from 81 lines of the prose, starting at a word.
create temp table coverage_corpus_pool on commit drop as
select p, left(pg_temp.coverage_corpus_from_word(body.text), 300 + (p * 7919) % 2101) as t
from coverage_corpus_plan c
cross join generate_series(0, 19999) p
cross join lateral (
  select string_agg(l.line, E'\n' order by l.n) as text
  from coverage_corpus_line l
  where l.n between 1 + (p::bigint * 104729) % (c.lines - 80) and 1 + (p::bigint * 104729) % (c.lines - 80) + 80
) body;
create unique index on coverage_corpus_pool (p);
analyze coverage_corpus_pool;

create temp table coverage_corpus_vector on commit drop as
select array_fill(0::real, array[1536])::vector(1536) as v;

create temp table coverage_corpus_indexes on commit drop as
select x.indexrelid::regclass::text as name, pg_get_indexdef(x.indexrelid) as definition
from pg_index x
where x.indrelid = 'embeddings'::regclass
  and not exists (select 1 from pg_constraint k where k.conindid = x.indexrelid);

do $$
declare
  dropped record;
begin
  for dropped in select name from coverage_corpus_indexes loop
    execute 'drop index ' || dropped.name;
  end loop;
end
$$;

insert into projects (key, name, default_assignee) values ('CORPUS', 'Coverage corpus', 'corpus');

insert into issues (key, project_key, number, title, created_by, rank, created_at, updated_at)
select 'CORPUS-' || i, 'CORPUS', i,
       case i % 1000 when 7 then ' ' || title.t when 8 then 'Ünïcödé ' || title.t else title.t end,
       '{"kind": "user", "id": "corpus"}'::jsonb, lpad(to_hex(i), 8, '0'),
       now() - make_interval(days => i % 400), now() - make_interval(days => i % 400)
from coverage_corpus_plan c
cross join generate_series(1, c.issues) i
cross join lateral (
  select rtrim(left(regexp_replace(pool.t, '[[:space:]]+', ' ', 'g'), 40 + i % 60)) as t
  from coverage_corpus_pool pool where pool.p = (i * 13) % 20000
) title;

insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by, project_key)
select md5('primary:' || i)::uuid, 'CORPUS-' || i, 'spec', 'spec.md', 'doc', true,
       '{"kind": "user", "id": "corpus"}'::jsonb, 'CORPUS'
from coverage_corpus_plan c
cross join generate_series(1, c.issues) i;

-- Each spec's four earlier versions: three pooled texts under the spec's heading.
insert into artifact_versions (artifact_id, number, markdown, mime, size)
select md5('primary:' || i)::uuid, n, version.markdown, 'text/markdown', octet_length(version.markdown)
from coverage_corpus_plan c
cross join generate_series(1, c.issues) i
cross join generate_series(1, 4) n
cross join lateral (
  select '## Summary' || E'\n\n' || string_agg(pool.t, E'\n\n' order by pool.p) as markdown
  from coverage_corpus_pool pool
  where pool.p in ((i * 3 + n) % 20000, (i * 5 + n) % 20000, (i * 11 + n) % 20000)
) version;

-- Each spec's latest version, of the next sampled size in turn, exactly: the prose from a word
-- under the spec's heading (read from the lines that start within half the size again and 4 KB
-- past the start, so that a stretch of multi-byte characters still leaves enough), cut to that
-- many characters, then short of as many characters as the cut has bytes beyond the size (each
-- at least one byte, so the text is then at most the size), then padded to the size with spaces
-- where a multi-byte character fell in what was cut.
insert into artifact_versions (artifact_id, number, markdown, mime, size)
select md5('primary:' || i)::uuid, 5, spec.markdown, 'text/markdown', octet_length(spec.markdown)
from coverage_corpus_plan c
cross join generate_series(1, c.issues) i
cross join lateral (
  select (array[6568, 2203, 19057, 8454, 1661, 533, 4488, 1500, 1383, 2190,
                1692, 2015, 1687, 1972, 2873, 27483, 48689, 1123, 100033, 100970,
                3272, 29335, 3380, 5772, 22233, 6240, 8548, 4371, 10955, 8145])[1 + (i - 1) % 30] as bytes
) sample
cross join lateral (
  select off from coverage_corpus_line where n = 1 + (i::bigint * 7907) % c.spec_starts
) start
cross join lateral (
  select '## Summary' || E'\n\n' || pg_temp.coverage_corpus_from_word(string_agg(l.line, E'\n' order by l.n)) as text
  from coverage_corpus_line l
  where l.off >= start.off and l.off < start.off + sample.bytes + sample.bytes / 2 + 4096
) body
cross join lateral (select left(body.text, sample.bytes) as text) cut
cross join lateral (select left(cut.text, sample.bytes - (octet_length(cut.text) - sample.bytes)) as text) fit
cross join lateral (select fit.text || repeat(' ', sample.bytes - octet_length(fit.text)) as markdown) spec;

-- The other documents, 10 versions each, which bring the versions to the scale's total.
insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by, project_key)
select md5('other:' || d)::uuid, 'CORPUS-' || (1 + (d * 7) % c.issues), 'doc-' || d, 'doc-' || d || '.md', 'doc', false,
       '{"kind": "user", "id": "corpus"}'::jsonb, 'CORPUS'
from coverage_corpus_plan c
cross join generate_series(1, ceil(greatest(c.versions - 5 * c.issues, 0) / 10.0)::int) d;

insert into artifact_versions (artifact_id, number, markdown, mime, size)
select md5('other:' || (1 + (g - 1) / 10))::uuid, 1 + (g - 1) % 10, pool.t, 'text/markdown', octet_length(pool.t)
from coverage_corpus_plan c
cross join generate_series(1, greatest(c.versions - 5 * c.issues, 0)) g
join coverage_corpus_pool pool on pool.p = (g * 13) % 20000;

-- The issue rows, as embeddings_enqueue_issue writes them, embedded at their current text.
insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding,
                        next_attempt_at, embedded_at, created_at, pending_since, project_key)
select 'issue', i.key, item.text, item.hash, item.hash, current_setting('coverage_corpus.identity'),
       (select v from coverage_corpus_vector),
       i.created_at, i.created_at + interval '1 minute', i.created_at, i.created_at, i.project_key
from issues i
cross join lateral (select issue_embedding_text(i.title, issue_embedding_spec(i.key)) as text) built
cross join lateral (select built.text, encode(sha256(convert_to(built.text, 'UTF8')), 'hex') as hash) item;

-- The window rows, each in its state (the header says which share is in which).
insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding,
                        attempt_count, confirmed_failures, dead, dead_model,
                        next_attempt_at, embedded_at, created_at, pending_since, covers, project_key)
select w.kind, w.id, w.text, w.hash,
       case when w.state in ('pending', 'dead') then null else w.hash end,
       case w.state
         when 'pending' then null
         when 'dead' then null
         when 'foreign' then current_setting('coverage_corpus.foreign_identity')
         else current_setting('coverage_corpus.identity')
       end,
       case when w.state in ('pending', 'dead', 'blank') then null else (select v from coverage_corpus_vector) end,
       case w.state when 'dead' then 12 else 0 end,
       case w.state when 'dead' then 10 else 0 end,
       w.state = 'dead',
       case w.state when 'dead' then current_setting('coverage_corpus.identity') end,
       case w.state when 'pending' then now() else now() - interval '2 days' end,
       case when w.state in ('pending', 'dead') then null else now() - interval '2 days' end,
       now() - interval '30 days',
       case when w.state in ('pending', 'dead') then now() else now() - interval '3 days' end - make_interval(secs => w.g % 86400),
       array['b' || w.g], 'CORPUS'
from (
  select k.kind, g, row_state.state,
         k.owner || md5(k.kind || ':' || (g / 8))::uuid || '#b' || g as id,
         written.text, encode(sha256(convert_to(written.text, 'UTF8')), 'hex') as hash
  from (values ('document_block', 738255, ''), ('comment_block', 184555, 'comment/'),
               ('message_block', 48386, 'message/'), ('ask_part', 12816, 'ask/')) k (kind, n, owner)
  cross join lateral generate_series(1, round(k.n * current_setting('coverage_corpus.scale')::numeric)::int) g
  cross join lateral (
    select case
             when g % 200 = 0 then 'pending'
             when g % 200 = 1 then 'dead'
             when g % 10000 = 2 then 'blank'
             when g % 10000 = 3 then 'leading'
             when g % 10000 = 4 then 'foreign'
             else 'current'
           end as state
  ) row_state
  cross join lateral (
    select case row_state.state
             when 'blank' then (array[' ', E'\n', E'\t \n', E'  \n\n  '])[1 + (g / 10000) % 4]
             when 'leading' then E'\n  ' || pool.t || ' ' || g
             else pool.t || ' ' || g
           end as text
    from coverage_corpus_pool pool
    where pool.p = (g::bigint * 7919 + length(k.kind)) % 20000
  ) written
) w;

do $$
declare
  built record;
begin
  for built in select definition from coverage_corpus_indexes where name <> 'embeddings_cosine' order by name loop
    execute built.definition;
  end loop;
end
$$;

drop function pg_temp.coverage_corpus_from_word(text);

alter table issues enable trigger user;
alter table artifacts enable trigger user;
alter table artifact_versions enable trigger user;
alter table embeddings enable trigger user;

commit;
