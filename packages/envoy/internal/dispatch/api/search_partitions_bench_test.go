package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The meaning-search bench: a synthetic corpus of production's size and shape, the partition pass
// over it, and a meaning search measured on it, by a full member or a restricted one, at each
// search width the run names. Each step is env-gated so CI never runs it, and each is run by a
// build of its own over its own copy of the corpus: the corpus is built by the build before
// embeddings was partitioned (the base of this change), so both builds read the same rows, and
// the build after it partitions a copy (TestPartitionEmbeddingsOnBenchCorpus) before it searches
// it. The order is TestBuildMeaningSearchBenchCorpus with the earlier build, a copy of the
// database for each build, TestPartitionEmbeddingsOnBenchCorpus with this one on its copy, then
// TestMeaningSearchOnBenchCorpus with each build on its own copy, in turn, and
// TestExactNearestOnBenchCorpus once, for the exact answers each build's is read against.

// benchCorpusSQL builds the corpus on a migrated database that holds no issue: production's
// counts of 2026-11-17 times bench_corpus.scale (embedqueue/testdata/coverage_corpus.sql's,
// which this follows) - 11,151 issues, and document_block 738,255, comment_block 184,555,
// message_block 48,386 and ask_part 12,816 window rows, with their owners: 29,531 documents of 25
// windows each, 92,278 comments, 24,193 messages and 6,408 asks of two windows each. They are spread
// over 20 projects as production's 20 are, the n-th of them holding a share proportional to 1/n
// (production's own split was not readable; its largest project holds most of its rows).
//
// Every row is embedded by embed.BedrockModel at its text, and its vector is one of eight topics of
// its project (a random unit vector each) plus three random vectors of a pool of 4,096 at weights
// 0.7, 0.35 and 0.2, normalised, each chosen by a hash of the row's kind and id: rows of one topic
// lie near each other and near no other topic, and two rows share a vector only where all four
// choices meet, one in 8 x 4,096^3 (about 5 x 10^11). The queries are the 40 rows of
// bench_queries: a topic of a project, the
// project's share of the queries following its share of the rows, plus one vector of the pool at
// 0.5, normalised.
//
// The triggers of the tables it fills are off for the load, and every index of embeddings that
// backs no constraint, the HNSW index among them, is dropped first and built again after it, as
// coverage_corpus.sql does; VACUUM and ANALYZE are the caller's.
const benchCorpusSQL = `
begin;
do $$ begin
  if exists (select 1 from issues) or exists (select 1 from embeddings) then
    raise exception 'the bench corpus is built on a database with no issue and no embeddings row';
  end if;
end $$;
alter table issues disable trigger user;
alter table artifacts disable trigger user;
alter table artifact_versions disable trigger user;
alter table comments disable trigger user;
alter table messages disable trigger user;
alter table asks disable trigger user;
alter table embeddings disable trigger user;

create table bench_plan as
select round(11151 * s.scale)::int as issues, round(29531 * s.scale)::int as documents,
       round(92278 * s.scale)::int as comments, round(24193 * s.scale)::int as messages,
       round(6408 * s.scale)::int as asks
from (select current_setting('bench_corpus.scale')::numeric as scale) s;

create table bench_projects as
select n, 'C' || lpad(n::text, 2, '0') as key, (1.0 / n) / (select sum(1.0 / m) from generate_series(1, 20) m) as share
from generate_series(1, 20) n;
create table bench_bounds as
select key, n, coalesce(sum(share) over (order by n rows between unbounded preceding and 1 preceding), 0) as low,
       sum(share) over (order by n) as high
from bench_projects;

insert into projects (key, name, default_assignee) select key, 'Bench ' || key, 'alice' from bench_projects;

-- project_of(x) is the project whose share of [0, 1) holds x.
create function pg_temp.project_of(x float8) returns text language sql stable as $f$
  select key from bench_bounds where x >= low and x < high union all select 'C20' limit 1
$f$;

-- normals() is 1,536 standard normal draws (Box-Muller), and scaled_unit(x, scale) is x normalised
-- to the length scale. pgvector adds two vectors but multiplies none by a number, so each pool
-- vector is kept at the three lengths the rows and the queries add it at.
create function pg_temp.normals(seed int) returns float8[] language sql volatile as $f$
  select array_agg(sqrt(-2 * ln(1 - random())) * cos(2 * pi() * random()) order by d) from generate_series(1, 1536) d
$f$;
create function pg_temp.scaled_unit(x float8[], scale float8) returns vector language sql immutable as $f$
  select (select array_agg(e * scale / norm order by i) from unnest(x) with ordinality u(e, i))::real[]::vector
  from (select sqrt(sum(e * e)) as norm from unnest(x) e) n
$f$;
create table bench_normals as select g as n, pg_temp.normals(g) as x from generate_series(0, 4095) g;
create table bench_noise as
select n, pg_temp.scaled_unit(x, 0.7) as v07, pg_temp.scaled_unit(x, 0.35) as v035, pg_temp.scaled_unit(x, 0.2) as v02,
       pg_temp.scaled_unit(x, 0.5) as v05
from bench_normals;
create unique index on bench_noise (n);
create table bench_topics as
select p.key, t, pg_temp.scaled_unit(pg_temp.normals(p.n * 8 + t), 1) as v from bench_projects p cross join generate_series(0, 7) t;
create unique index on bench_topics (key, t);

-- vector_of(project, k) is the vector of the row k names, by its kind and id, in project. Each of
-- its four choices is a hash of k of its own, so no residue of a row number decides more than one
-- of them (a multiple of a number modulo 4,096 would give every project only 4,096 vectors, each
-- shared by dozens of rows).
create function pg_temp.vector_of(p text, k text) returns vector language sql stable as $f$
  select l2_normalize(topic.v + a.v07 + b.v035 + c.v02)
  from bench_topics topic, bench_noise a, bench_noise b, bench_noise c
  where topic.key = p and topic.t = abs(hashtext(k || ':t')) % 8
    and a.n = abs(hashtext(k || ':a')) % 4096 and b.n = abs(hashtext(k || ':b')) % 4096
    and c.n = abs(hashtext(k || ':c')) % 4096
$f$;

insert into issues (key, project_key, number, title, created_by, rank, created_at, updated_at)
select p.key || '-' || g, p.key, g, 'Bench issue ' || g || ' of ' || p.key, '{"kind": "user", "id": "alice"}'::jsonb,
       lpad(to_hex(g), 8, '0'), now() - make_interval(days => g % 400), now() - make_interval(days => g % 400)
from bench_plan c cross join generate_series(1, c.issues) g
cross join lateral (select pg_temp.project_of((g * 0.6180339887) - floor(g * 0.6180339887)) as key) p;

create table bench_issue_of as select project_key, array_agg(key order by number) as keys from issues group by project_key;

insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by, project_key)
select md5('doc:' || d)::uuid, o.keys[1 + (d % cardinality(o.keys))], 'doc-' || d, 'doc-' || d || '.md', 'doc', false,
       '{"kind": "user", "id": "alice"}'::jsonb, p.key
from bench_plan c cross join generate_series(1, c.documents) d
cross join lateral (select pg_temp.project_of((d * 0.7548776662) - floor(d * 0.7548776662)) as key) p
join bench_issue_of o on o.project_key = p.key;
insert into artifact_versions (artifact_id, number, markdown, mime, size)
select id, 1, 'A bench document.', 'text/markdown', 17 from artifacts where slug like 'doc-%';

insert into comments (id, issue_key, author, body)
select md5('comment:' || g)::uuid, o.keys[1 + (g % cardinality(o.keys))], '{"kind": "user", "id": "alice"}'::jsonb, 'A bench comment.'
from bench_plan c cross join generate_series(1, c.comments) g
cross join lateral (select pg_temp.project_of((g * 0.5698402910) - floor(g * 0.5698402910)) as key) p
join bench_issue_of o on o.project_key = p.key;
insert into messages (id, issue_key, author, body)
select md5('message:' || g)::uuid, o.keys[1 + (g % cardinality(o.keys))], '{"kind": "user", "id": "alice"}'::jsonb, 'A bench message.'
from bench_plan c cross join generate_series(1, c.messages) g
cross join lateral (select pg_temp.project_of((g * 0.4142135623) - floor(g * 0.4142135623)) as key) p
join bench_issue_of o on o.project_key = p.key;
insert into asks (id, issue_key, author, question)
select md5('ask:' || g)::uuid, o.keys[1 + (g % cardinality(o.keys))], '{"kind": "user", "id": "alice"}'::jsonb, 'A bench question?'
from bench_plan c cross join generate_series(1, c.asks) g
cross join lateral (select pg_temp.project_of((g * 0.3247179572) - floor(g * 0.3247179572)) as key) p
join bench_issue_of o on o.project_key = p.key;

create temp table bench_indexes as
select x.indexrelid::regclass::text as name, pg_get_indexdef(x.indexrelid) as definition
from pg_index x where x.indrelid = 'embeddings'::regclass
  and not exists (select 1 from pg_constraint k where k.conindid = x.indexrelid);
do $$ declare dropped record; begin
  for dropped in select name from bench_indexes loop execute 'drop index ' || dropped.name; end loop;
end $$;

insert into embeddings (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, embedded_at, covers, project_key)
select w.kind, w.id, w.text, md5(w.text), md5(w.text), current_setting('bench_corpus.model'),
       pg_temp.vector_of(w.project, w.kind || ':' || w.id), now(), w.covers, w.project
from (
  select 'issue' as kind, i.key as id, 'Bench issue ' || i.key as text, null::text[] as covers, i.project_key as project
    from issues i
  union all
  select 'document_block', a.id::text || '#b' || b, 'Block ' || b || ' of ' || a.slug, array['b' || b], a.project_key
    from artifacts a cross join generate_series(1, 25) b where a.slug like 'doc-%'
  union all
  select 'comment_block', 'comment/' || c.id::text || '#b' || b, 'Comment window ' || b, array['b' || b], i.project_key
    from comments c join issues i on i.key = c.issue_key cross join generate_series(1, 2) b
  union all
  select 'message_block', 'message/' || m.id::text || '#b' || b, 'Message window ' || b, array['b' || b], i.project_key
    from messages m join issues i on i.key = m.issue_key cross join generate_series(1, 2) b
  union all
  select 'ask_part', 'ask/' || k.id::text || '#p' || b, 'Ask part ' || b, array['p' || b], i.project_key
    from asks k join issues i on i.key = k.issue_key cross join generate_series(1, 2) b
) w;

create table bench_queries as
select q as n, l2_normalize(t.v + noise.v05) as v
from generate_series(0, 39) q
cross join lateral (select pg_temp.project_of((q + 0.5) / 40.0) as key) p
join bench_topics t on t.key = p.key and t.t = q % 8
join bench_noise noise on noise.n = (q * 2654435761) % 4096;

do $$ declare built record; begin
  for built in select definition from bench_indexes order by name loop execute built.definition; end loop;
end $$;

alter table issues enable trigger user;
alter table artifacts enable trigger user;
alter table artifact_versions enable trigger user;
alter table comments enable trigger user;
alter table messages enable trigger user;
alter table asks enable trigger user;
alter table embeddings enable trigger user;
commit;
`

// TestBuildMeaningSearchBenchCorpus builds benchCorpusSQL's corpus on DISPATCH_BENCH_BUILD_URL, at
// DISPATCH_BENCH_SCALE (1, production's size, when unset), and logs what it built: the rows of
// each kind, embeddings' size, and how long the load and the index builds took.
func TestBuildMeaningSearchBenchCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_BUILD_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_BENCH_BUILD_URL must name an empty database to build the bench corpus on")
	}
	scale := os.Getenv("DISPATCH_BENCH_SCALE")
	if scale == "" {
		scale = "1"
	}
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Pool.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, setting := range []string{
		"select set_config('bench_corpus.scale', '" + scale + "', false)",
		"select set_config('bench_corpus.model', '" + embed.BedrockModel + "', false)",
		"set maintenance_work_mem = '8GB'", "set max_parallel_maintenance_workers = 8", "set statement_timeout = 0",
	} {
		if _, err := conn.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	started := time.Now()
	if _, err := conn.Conn().PgConn().Exec(ctx, benchCorpusSQL).ReadAll(); err != nil {
		t.Fatalf("build the bench corpus: %v", err)
	}
	built := time.Since(started)
	if _, err := conn.Exec(ctx, "vacuum analyze"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	rows, err := conn.Query(ctx, `select kind, count(*) from embeddings group by kind order by kind`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatal(err)
		}
		t.Logf("bench corpus: %s rows=%d", kind, count)
	}
	rows.Close()
	var bytes int64
	var projects int
	if err := conn.QueryRow(ctx, `select (select coalesce((select sum(pg_total_relation_size(relid)) from pg_partition_tree('embeddings')), pg_total_relation_size('embeddings'))::bigint),
		(select count(*) from projects)`).
		Scan(&bytes, &projects); err != nil {
		t.Fatal(err)
	}
	t.Logf("bench corpus: scale=%s projects=%d embeddings_bytes=%d (%.1f GiB) built in %v", scale, projects, bytes, float64(bytes)/(1<<30), built)
	// Rows that share a vector tie in every nearest-neighbour read, and a tie is broken by
	// whichever row a scan meets first, which differs between two builds' plans of the same rows:
	// a recall or a rank comparison over such a corpus compares the tie-breaks. So the corpus
	// refuses itself where its rows do not each hold a vector of their own. The vectors are
	// counted by their digest, since a distinct over the vectors themselves sorts 6 KB a row.
	var stored, distinct int64
	if err := conn.QueryRow(ctx, `select count(*), count(distinct md5(embedding::text)) from embeddings`).Scan(&stored, &distinct); err != nil {
		t.Fatal(err)
	}
	t.Logf("bench corpus: %d rows hold %d distinct vectors", stored, distinct)
	if distinct < stored-stored/1000 {
		t.Fatalf("the corpus's %d rows hold only %d distinct vectors: its nearest-neighbour reads are tie-breaks", stored, distinct)
	}
}

// TestPartitionEmbeddingsOnBenchCorpus migrates DISPATCH_BENCH_DATABASE_URL with this build and
// runs the fill's whole pass over it, as a deployment's boot does, and logs how long each took and
// what the pass's own lines say: the rows it copied and in how many batches, and how long the swap
// held embeddings. Run by the build that partitions embeddings; on an earlier one it labels
// nothing and logs no copy.
func TestPartitionEmbeddingsOnBenchCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_BENCH_DATABASE_URL must name a bench corpus copy")
	}
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Pool.Close()
	started := time.Now()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Logf("bench partition: migrate took %v", time.Since(started))
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	started = time.Now()
	if _, err := database.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("fill: %v", err)
	}
	t.Logf("bench partition: the fill's pass took %v", time.Since(started))
	for _, record := range records.Records() {
		var attrs []string
		record.Attrs(func(attr slog.Attr) bool {
			attrs = append(attrs, attr.Key+"="+attr.Value.String())
			return true
		})
		t.Logf("bench partition: %s %s", record.Message, strings.Join(attrs, " "))
	}
	if _, err := database.Pool.Exec(ctx, "vacuum analyze embeddings"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
}

// TestMeaningSearchOnBenchCorpus measures a meaning search on DISPATCH_BENCH_DATABASE_URL at each
// width DISPATCH_BENCH_WIDTHS names (benchWidths), as the reader DISPATCH_BENCH_READER names
// (benchReader): the reader's 40 queries (benchReaderQueries) are searched once at every width, to
// warm and to record each answer, and then DISPATCH_BENCH_ROUNDS rounds (10 when unset; 0 records
// the answers alone), each round searching every query at every width in turn, so that whatever
// slows the machine during the run slows every width alike. It logs each width's p50, p95 and
// maximum, and writes each width's answers, kind/id in order, one JSON line a query, to
// DISPATCH_BENCH_HITS_DIR/<width>.jsonl when that is set, so two builds' answers, and two widths',
// can be compared rank for rank and against the exact nearest items (TestExactNearestOnBenchCorpus).
func TestMeaningSearchOnBenchCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_BENCH_DATABASE_URL must name a bench corpus copy")
	}
	rounds := 10
	if listed := os.Getenv("DISPATCH_BENCH_ROUNDS"); listed != "" {
		parsed, err := strconv.Atoi(listed)
		if err != nil || parsed < 0 {
			t.Fatalf("DISPATCH_BENCH_ROUNDS = %q", listed)
		}
		rounds = parsed
	}
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reader := benchReader(t, database)
	queries := benchReaderQueries(t, database, reader)
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	for i, vector := range queries {
		embedder.vectors[benchQueryText(i)] = vector
	}
	handler := newCorpusServer(t, database, embedder)
	widths := benchWidths(t)
	built := searchMeaningScan
	t.Cleanup(func() { searchMeaningScan = built })

	hits := make([][][]string, len(widths))
	for w, width := range widths {
		benchReadBack(t, database, width)
		searchMeaningScan = width.settings
		hits[w] = make([][]string, len(queries))
		empty := 0
		for i := range queries {
			hits[w][i] = benchSearch(t, handler, reader, i)
			if len(hits[w][i]) == 0 {
				empty++
			}
		}
		if empty == len(queries) {
			t.Fatalf("%s at width %s: no query answers a hit, so the bench measures nothing", reader.name, width.name)
		}
		t.Logf("bench hits: reader=%s width=%s: %d of %d queries answer no hit", reader.name, width.name, empty, len(queries))
	}
	latencies := make([][]time.Duration, len(widths))
	for range rounds {
		for w, width := range widths {
			searchMeaningScan = width.settings
			for i := range queries {
				started := time.Now()
				benchSearch(t, handler, reader, i)
				latencies[w] = append(latencies[w], time.Since(started))
			}
		}
	}
	dir := os.Getenv("DISPATCH_BENCH_HITS_DIR")
	for w, width := range widths {
		if measured := slices.Sorted(slices.Values(latencies[w])); len(measured) > 0 {
			p50 := measured[(len(measured)-1)/2]
			p95 := measured[(len(measured)*95+99)/100-1]
			t.Logf("bench search: reader=%s width=%s: %d meaning searches: p50=%v p95=%v max=%v",
				reader.name, width.name, len(measured), p50, p95, measured[len(measured)-1])
			if dir != "" {
				summary, err := json.Marshal(map[string]any{
					"searches": len(measured), "p50_ms": p50.Seconds() * 1000, "p95_ms": p95.Seconds() * 1000,
					"max_ms": measured[len(measured)-1].Seconds() * 1000,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, width.name+".latency.json"), summary, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		if dir != "" {
			benchWriteLines(t, filepath.Join(dir, width.name+".jsonl"), "hits", hits[w])
		}
	}
}

// TestExactNearestOnBenchCorpus reads the exact 20 nearest items of each of the reader's 40 queries
// (benchReader, benchReaderQueries) on DISPATCH_BENCH_DATABASE_URL, by a sequential scan of every
// vector the reader may read (benchExactNearest), and writes them, one JSON line a query, to
// DISPATCH_BENCH_EXACT_FILE: what each width's recall@20 is read against.
func TestExactNearestOnBenchCorpus(t *testing.T) {
	databaseURL, path := os.Getenv("DISPATCH_BENCH_DATABASE_URL"), os.Getenv("DISPATCH_BENCH_EXACT_FILE")
	if databaseURL == "" || path == "" {
		t.Skip("DISPATCH_BENCH_DATABASE_URL must name a bench corpus copy and DISPATCH_BENCH_EXACT_FILE the file to write")
	}
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reader := benchReader(t, database)
	queries := benchReaderQueries(t, database, reader)
	exact := make([][]string, len(queries))
	for i, vector := range queries {
		exact[i] = benchExactNearest(t, database, vector, 20, reader.projects)
		if len(exact[i]) == 0 {
			t.Fatalf("query %d of %s has no item above the meaning floor: its recall reads nothing", i, reader.name)
		}
	}
	benchWriteLines(t, path, "exact", exact)
	t.Logf("bench exact: reader=%s: the exact nearest items of %d queries written to %s", reader.name, len(queries), path)
}

// benchWidth is one setting of the meaning scan the bench measures: its name, the settings
// searchMeaningScan holds under it, and the parameters it sets in place of the build's own.
type benchWidth struct {
	name      string
	settings  []string
	overrides map[string]string
}

// benchWidths is the widths DISPATCH_BENCH_WIDTHS names, `;`-separated, each
// `<name>:<parameter>=<value>[,<parameter>=<value>...]`: the build's own searchMeaningScan with
// each named parameter's `set local` given that value, or added where the build sets none. Unset,
// it is one width, "built", the build's settings as they are.
func benchWidths(t *testing.T) []benchWidth {
	t.Helper()
	listed := os.Getenv("DISPATCH_BENCH_WIDTHS")
	if listed == "" {
		return []benchWidth{{name: "built", settings: searchMeaningScan}}
	}
	var widths []benchWidth
	for _, field := range strings.Split(listed, ";") {
		name, changes, ok := strings.Cut(strings.TrimSpace(field), ":")
		if !ok || name == "" || changes == "" {
			t.Fatalf("DISPATCH_BENCH_WIDTHS entry %q: want <name>:<parameter>=<value>[,...]", field)
		}
		width := benchWidth{name: name, settings: slices.Clone(searchMeaningScan), overrides: map[string]string{}}
		for _, change := range strings.Split(changes, ",") {
			parameter, value, ok := strings.Cut(change, "=")
			if !ok || parameter == "" || value == "" {
				t.Fatalf("DISPATCH_BENCH_WIDTHS entry %q: %q is no <parameter>=<value>", field, change)
			}
			width.overrides[parameter] = value
			setting := "set local " + parameter + " = " + value
			if at := slices.IndexFunc(width.settings, func(s string) bool { return strings.HasPrefix(s, "set local "+parameter+" = ") }); at >= 0 {
				width.settings[at] = setting
			} else {
				width.settings = append(width.settings, setting)
			}
		}
		widths = append(widths, width)
	}
	return widths
}

// benchReadBack reads, in a transaction holding width's settings as beginMeaningScan applies them,
// the scan's two parameters a width changes, logs them, and fails where a parameter width sets
// reads otherwise, so each width's lines say what the searches under it ran on.
func benchReadBack(t *testing.T, database *store.Store, width benchWidth) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, strings.Join(width.settings, "; ")); err != nil {
		t.Fatalf("width %s: apply %q: %v", width.name, width.settings, err)
	}
	read := map[string]string{}
	for _, parameter := range []string{"hnsw.ef_search", "hnsw.iterative_scan"} {
		var value string
		if err := tx.QueryRow(ctx, `select current_setting($1)`, parameter).Scan(&value); err != nil {
			t.Fatalf("width %s: read %s: %v", width.name, parameter, err)
		}
		read[parameter] = value
	}
	for parameter, want := range width.overrides {
		var value string
		if err := tx.QueryRow(ctx, `select current_setting($1)`, parameter).Scan(&value); err != nil || value != want {
			t.Fatalf("width %s: %s reads %q (%v), want %q", width.name, parameter, value, err, want)
		}
	}
	t.Logf("bench width: %s: hnsw.ef_search=%s hnsw.iterative_scan=%s", width.name, read["hnsw.ef_search"], read["hnsw.iterative_scan"])
}

// benchReaderOf is whose search the bench measures: alice, a full member no access list names, who
// reads every project, or a restricted member whose list names projects and no other.
type benchReaderOf struct {
	name     string
	login    string
	projects []string // nil: every project
}

// benchReader is the reader DISPATCH_BENCH_READER names. Unset, it is alice. Set, it is a
// comma-separated list of project keys, and the reader a restricted member (an outside
// collaborator) whose access list names those projects alone: every bench project is public, so
// those are all he sees, and his meaning arms read their partitions and the one of no project
// (searchReaderPartitions). He is written into the tables the access routes write, and writing him
// again changes nothing, so each build's copy of the corpus may be read by him in turn.
//
// A reader who is not open sees an item only where the graph store holds its node under a project
// he sees (refs.VisibleKind: a node with no row is unscoped, which only an open viewer passes), and
// benchCorpusSQL loads with the owners' triggers off, so it writes no node and no artifact's
// ref_key. So for his projects' items it writes both as the owners' writes would have: each
// artifact's ref_key (artifacts_ref_key's expression, migration 0071), and each issue's, document's,
// comment's, message's and ask's node, naming its project. That is what a deployment holds for
// every item (graphcensus keeps it so), and alice's search reads the same rows either way, since
// with no private project an open viewer passes a node with no row.
func benchReader(t *testing.T, database *store.Store) benchReaderOf {
	t.Helper()
	listed := os.Getenv("DISPATCH_BENCH_READER")
	if listed == "" {
		return benchReaderOf{name: "alice", login: "alice"}
	}
	projects := strings.Split(listed, ",")
	login := "bench-" + strings.ToLower(strings.Join(projects, "-")) + "@outside.example"
	seedPeople(t, database, login)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// The owners' triggers stay off for these writes, as they were for the load: an update of an
	// artifact would otherwise queue its embedding again and change the corpus being measured.
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatalf("write %s's items' nodes with the owners' triggers off: %v", login, err)
	}
	if _, err := tx.Exec(ctx, `
		update artifacts set ref_key = coalesce(issue_key, project_key) || '/' || slug
		 where ref_key is null and project_key = any($1)`, projects); err != nil {
		t.Fatalf("give %v's documents their ref_key: %v", projects, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event, project_key)
		select 'issue', i.key, 'issue', 'project', 'live', i.created_at, i.created_by, 0, i.project_key
		  from issues i where i.project_key = any($1)
		union all
		select 'artifact', a.ref_key, 'document', 'project', 'live', a.created_at, a.created_by, 0, a.project_key
		  from artifacts a where a.project_key = any($1)
		union all
		select 'comment', c.id::text, 'comment', 'project', 'live', c.created_at, c.author, 0, i.project_key
		  from comments c join issues i on i.key = c.issue_key where i.project_key = any($1)
		union all
		select 'message', m.id::text, 'message', 'project', 'live', m.created_at, m.author, 0, i.project_key
		  from messages m join issues i on i.key = m.issue_key where i.project_key = any($1)
		union all
		select 'ask', k.id::text, 'ask', 'project', 'live', k.created_at, k.author, 0, i.project_key
		  from asks k join issues i on i.key = k.issue_key where i.project_key = any($1)
		on conflict do nothing`, projects); err != nil {
		t.Fatalf("write %v's items' nodes: %v", projects, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %v's items' nodes: %v", projects, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, login); err != nil {
		t.Fatalf("make %s restricted: %v", login, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into project_access (project_key, principal_kind, principal, added_by)
		select key, 'person', $2, '{"kind": "user", "id": "alice"}' from projects where key = any($1)
		on conflict do nothing`, projects, login); err != nil {
		t.Fatalf("list %s on %v: %v", login, projects, err)
	}
	var kind string
	var named, unnamed int
	if err := database.Pool.QueryRow(ctx, `
		select (select member_kind from people where email = $1),
		       (select count(*) from project_access where principal_kind = 'person' and principal = $1),
		       (select count(*) from issues i where i.project_key = any($2)
		           and not exists (select 1 from graph_nodes n where n.kind = 'issue' and n.id = i.key and n.project_key = i.project_key))
		     + (select count(*) from artifacts a where a.project_key = any($2)
		           and not exists (select 1 from graph_nodes n where n.kind = 'artifact' and n.id = a.ref_key and n.project_key = a.project_key))`,
		login, projects).Scan(&kind, &named, &unnamed); err != nil || kind != "restricted" || named != len(projects) || unnamed != 0 {
		t.Fatalf("%s reads member kind %q, %d listings and %d issues or documents of his projects with no node (%v), want restricted, %d and 0",
			login, kind, named, unnamed, err, len(projects))
	}
	return benchReaderOf{name: "restricted:" + listed, login: login, projects: projects}
}

// benchReaderQueries is the reader's 40 queries: bench_queries for alice, and for a restricted
// member 40 of the same make over his own projects, taken in turn - a topic of the project plus
// one vector of the pool at 0.5, normalised - so that each has near items he may read, as a
// person's search of his own project's work has.
func benchReaderQueries(t *testing.T, database *store.Store, reader benchReaderOf) [][]float32 {
	t.Helper()
	if reader.projects == nil {
		return benchQueries(t, database)
	}
	return benchVectors(t, database, `
		select l2_normalize(t.v + noise.v05)::text
		  from generate_series(0, 39) q
		  join bench_topics t on t.key = ($1::text[])[1 + q % cardinality($1::text[])] and t.t = (q / cardinality($1::text[])) % 8
		  join bench_noise noise on noise.n = (q * 2654435761 + 1013904223) % 4096
		 order by q`, reader.projects)
}

// benchWriteLines writes one JSON line a query to path, {"query": i, field: values[i]}.
func benchWriteLines(t *testing.T, path, field string, values [][]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for i, value := range values {
		line, err := json.Marshal(map[string]any{"query": i, field: value})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(file, string(line)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// TestExplainMeaningSearchOnBenchCorpus logs the plan, with its timings and buffers, of the
// meaning statement a full member's search runs on DISPATCH_BENCH_DATABASE_URL, for the queries
// DISPATCH_BENCH_EXPLAIN names (0 and 39 when unset: the largest project's and the smallest's),
// each explained once to warm and then again: EXPLAIN (ANALYZE, BUFFERS) of searchQueryMeaning in
// a transaction holding the meaning scan's own settings (searchMeaningScan), with the viewer of an
// open full member of a deployment holding no private project, which reads every partition. The
// two builds' plans side by side are what says where a difference in their times goes.
func TestExplainMeaningSearchOnBenchCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_BENCH_DATABASE_URL must name a bench corpus copy")
	}
	which := []int{0, 39}
	if listed := os.Getenv("DISPATCH_BENCH_EXPLAIN"); listed != "" {
		which = nil
		for _, field := range strings.Split(listed, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil {
				t.Fatalf("DISPATCH_BENCH_EXPLAIN = %q", listed)
			}
			which = append(which, n)
		}
	}
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	queries := benchQueries(t, database)
	var projects []string
	rows, err := database.Pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, key)
	}
	rows.Close()
	alice := model.Actor{Kind: "user", ID: "alice", Viewer: &model.Viewer{Person: "alice", Open: true, Sees: projects}}
	for _, width := range benchWidths(t) {
		for _, i := range which {
			arguments := searchMeaningQueryArguments(
				searchParameters{text: benchQueryText(i), limit: 20, who: alice}, queries[i], (&fakeEmbedder{}).Identity())
			for round := range 2 {
				lines := benchExplain(t, database, width.settings, i, arguments)
				if round == 1 {
					t.Logf("bench explain: width %s, query %d, %d plan lines:\n%s", width.name, i, len(lines), strings.Join(lines, "\n"))
				}
			}
		}
	}
}

// benchExplain is the plan, line by line, of EXPLAIN (ANALYZE, BUFFERS) of searchQueryMeaning
// for query i with arguments, in a transaction holding settings.
func benchExplain(t *testing.T, database *store.Store, settings []string, i int, arguments []any) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	plan, err := tx.Query(ctx, "explain (analyze, buffers) "+searchQueryMeaning, arguments...)
	if err != nil {
		t.Fatalf("explain query %d: %v", i, err)
	}
	var lines []string
	for plan.Next() {
		var line string
		if err := plan.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	plan.Close()
	if err := plan.Err(); err != nil {
		t.Fatalf("explain query %d: %v", i, err)
	}
	return lines
}

// benchQueryText is query i's text: words no row holds, so the keyword legs match nothing and the
// page is the meaning leg's alone.
func benchQueryText(i int) string {
	return fmt.Sprintf("zorblax%c%c quandary", 'a'+i/26, 'a'+i%26)
}

func benchQueries(t *testing.T, database *store.Store) [][]float32 {
	t.Helper()
	return benchVectors(t, database, `select v::text from bench_queries order by n`)
}

// benchVectors is the vectors statement reads, one a row, as text.
func benchVectors(t *testing.T, database *store.Store, statement string, arguments ...any) [][]float32 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, arguments...)
	if err != nil {
		t.Fatalf("read the bench's query vectors: %v", err)
	}
	defer rows.Close()
	var queries [][]float32
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		fields := strings.Split(strings.Trim(text, "[]"), ",")
		vector := make([]float32, len(fields))
		for i, field := range fields {
			value, err := strconv.ParseFloat(field, 32)
			if err != nil {
				t.Fatalf("parse a query vector: %v", err)
			}
			vector[i] = float32(value)
		}
		queries = append(queries, vector)
	}
	if err := rows.Err(); err != nil || len(queries) == 0 {
		t.Fatalf("read the bench's query vectors: %d queries, %v", len(queries), err)
	}
	return queries
}

func benchSearch(t *testing.T, handler http.Handler, reader benchReaderOf, i int) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?"+url.Values{"q": {benchQueryText(i)}, "limit": {"20"}}.Encode(), nil, reader.login)
	if response.Code != http.StatusOK {
		t.Fatalf("search %d: status=%d body=%s", i, response.Code, response.Body.String())
	}
	answer := decodeBody[model.SearchResponse](t, response)
	if answer.Degraded != "" {
		t.Fatalf("search %d answered degraded (%s)", i, answer.Degraded)
	}
	return hnswHits(answer)
}

// benchExactNearest is the limit items nearest vector by their nearest vector, read exactly by a
// sequential scan, as search names them (kind/id), of the rows of projects and of no project, or
// of every row where projects is nil.
func benchExactNearest(t *testing.T, database *store.Store, vector []float32, limit int, projects []string) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_indexscan = off; set local enable_bitmapscan = off; set local statement_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	literal := make([]string, len(vector))
	for i, value := range vector {
		literal[i] = strconv.FormatFloat(float64(value), 'g', -1, 32)
	}
	rows, err := tx.Query(ctx, `
		select item from (
			select case kind when 'issue' then 'issue/' || id
			                 when 'document_block' then 'document/' || split_part(id, '#', 1)
			                 when 'comment_block' then 'comment/' || split_part(split_part(id, '#', 1), '/', 2)
			                 when 'message_block' then 'message/' || split_part(split_part(id, '#', 1), '/', 2)
			                 when 'ask_part' then 'ask/' || split_part(split_part(id, '#', 1), '/', 2) end as item,
			       max(1 - (embedding <=> $1::vector)) as r
			  from embeddings where embedding is not null and model = $2
			   and ($5::bool or project_key is null or project_key = any($6::text[]))
			 group by 1
		) items where item is not null and r >= $4 order by r desc limit $3`,
		"["+strings.Join(literal, ",")+"]", embed.BedrockModel, limit, searchMeaningFloor, projects == nil, append([]string{}, projects...))
	if err != nil {
		t.Fatalf("read the exact nearest items: %v", err)
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return items
}
