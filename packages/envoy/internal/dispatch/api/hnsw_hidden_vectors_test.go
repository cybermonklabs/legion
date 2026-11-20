package api

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// hnswQuery is the text the HNSW row searches for: it shares no word with any issue the row writes,
// so only the meaning leg finds them, and the fake embedder answers queryVector() for it.
const hnswQuery = "zorblaxian quandary"

// hnswVisible is how many issues of PUB, the project alice sees, the row places farther from the
// query than every hidden one: fewer than one page of search (searchMaxLimit), so every hit is read.
const hnswVisible = 40

// hnswHiddenCounts are the numbers of PRIV's issues the HNSW tests place nearer the query than
// every visible one: DISPATCH_HNSW_HIDDEN_VECTORS, a comma-separated list, or 100 and 2,000 when it
// is unset, both well inside pgvector's hnsw.max_scan_tuples (20,000).
func hnswHiddenCounts(t *testing.T) []int {
	t.Helper()
	listed := os.Getenv("DISPATCH_HNSW_HIDDEN_VECTORS")
	if listed == "" {
		return []int{100, 2000}
	}
	var counts []int
	for _, field := range strings.Split(listed, ",") {
		count, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || count < 1 {
			t.Fatalf("DISPATCH_HNSW_HIDDEN_VECTORS = %q: %q is no positive count", listed, field)
		}
		counts = append(counts, count)
	}
	return counts
}

// hnswResidual names why TestAMeaningSearchFindsWhatItWouldWithoutNearerHiddenVectors is skipped.
const hnswResidual = "residual: a meaning search walks one HNSW graph that holds every project's vectors, " +
	"so vectors its reader may not see change which rows the walk reaches (TestMeasureAMeaningSearchBesideNearerHiddenVectors); " +
	"one HNSW graph per visibility domain closes it, and this row is switched on with that change"

// TestMeasureAMeaningSearchBesideNearerHiddenVectors (HNSW, a measurement): logs what alice's
// meaning search answers when hnswHiddenCounts vectors she may not see sit nearer the query than
// everything she sees, against the same seed without them, under the meaning transaction's own
// settings and under the scan's budgets raised far past them (hnsw.max_scan_tuples 1,000,000,
// hnsw.scan_mem_multiplier 64). It passes whatever it measures, and fails only when the seed
// without hidden vectors answers no hit, where it would measure nothing. The rule the
// measurement is for is TestAMeaningSearchFindsWhatItWouldWithoutNearerHiddenVectors.
//
// The seed is verification v16's with hnswVisible issues of PUB placed at cosine 0.60 down to
// 0.50 from the query and, in the seed measured, the hidden issues of PRIV at 0.95 down to 0.85.
// alice, who does not see PRIV, searches through an index-preferring pool (sequential scans and
// sorts off, as every plan test of this package reads, so the walk is embeddings_cosine's).
//
// Measured on 2026-11-20 against pgvector 0.8.7 (the test's own log): with no hidden vector alice
// reads 33 hits of PUB's 40, the walk's own reach; with 100 hidden vectors she reads 0, and with
// 2,000 she reads 0, under the meaning transaction's settings and under the raised budgets alike.
// The same geometry in a bare pgvector table (40 visible rows, then 1,000 or 5,000 hidden ones,
// ef_search 400 or 1,000, iterative_scan relaxed_order, strict_order or off, max_scan_tuples
// 1,000,000, scan_mem_multiplier 64) finds 40 visible rows with no hidden one and 0 with them,
// while an unfiltered walk returns 996 of 1,040 rows and 696 of 5,040: what ends the walk is the
// shared graph's reach, not the scan's budget. Run it as
// env DISPATCH_HNSW_HIDDEN_VECTORS=100,1000,5000 go test -v -run TestMeasureAMeaningSearchBesideNearerHiddenVectors ./internal/dispatch/api/
func TestMeasureAMeaningSearchBesideNearerHiddenVectors(t *testing.T) {
	without := hnswSearch(t, hnswSeed(t, 0), nil)
	if len(without.Results) == 0 {
		t.Fatalf("alice's search of the seed without hidden vectors holds no hit of PUB's %d: the measurement measures nothing", hnswVisible)
	}
	t.Logf("no hidden vector: alice's total %d, %d hits of PUB's %d", without.Total, len(without.Results), hnswVisible)
	for _, hidden := range hnswHiddenCounts(t) {
		seed := hnswSeed(t, hidden)
		for _, scan := range []struct {
			name     string
			settings map[string]string
		}{
			{"the meaning transaction's settings", nil},
			{"hnsw.max_scan_tuples 1000000 and hnsw.scan_mem_multiplier 64", map[string]string{"hnsw.max_scan_tuples": "1000000", "hnsw.scan_mem_multiplier": "64"}},
		} {
			with := hnswSearch(t, seed, scan.settings)
			t.Logf("%d hidden vectors nearer the query, %s: alice's total %d, %d hits, equal to the seed without them: %t",
				hidden, scan.name, with.Total, len(with.Results), with.Total == without.Total && slices.Equal(hnswHits(with), hnswHits(without)))
		}
	}
}

// TestAMeaningSearchFindsWhatItWouldWithoutNearerHiddenVectors (HNSW): a meaning search answers a
// reader what it would answer were the vectors the reader may not see absent, even when every one
// of them is nearer the query than everything the reader sees: over the seeds
// TestMeasureAMeaningSearchBesideNearerHiddenVectors reads, alice's total and hits with the hidden
// vectors equal those without them. It is skipped while hnswResidual stands: the candidate arms
// already hold their rows to the reader inside the iterative scan, before each arm's LIMIT, and
// still the hidden vectors change what the walk of the one shared graph reaches.
func TestAMeaningSearchFindsWhatItWouldWithoutNearerHiddenVectors(t *testing.T) {
	t.Skip(hnswResidual)
	without := hnswSearch(t, hnswSeed(t, 0), nil)
	if len(without.Results) == 0 {
		t.Fatalf("alice's search of the seed without hidden vectors holds no hit of PUB's %d: the comparison proves nothing", hnswVisible)
	}
	for _, hidden := range hnswHiddenCounts(t) {
		with := hnswSearch(t, hnswSeed(t, hidden), nil)
		if with.Total != without.Total || !slices.Equal(hnswHits(with), hnswHits(without)) {
			t.Errorf("with %d hidden vectors nearer the query, alice's search answers a total %d and hits\n\t%v\nwant what the seed without them answers, a total %d and hits\n\t%v",
				hidden, with.Total, hnswHits(with), without.Total, hnswHits(without))
		}
	}
}

// hnswSeed is verification v16's seed with the fake embedder answering queryVector() for
// hnswQuery, hnswVisible issues of PUB at cosine 0.60 down to 0.50 from it and hidden issues of
// PRIV at 0.95 down to 0.85, each embedded as embedqueue would have embedded it. The visible
// issues share a component apart from the query's, so they lie near each other and the index's
// graph links them, as text of one subject does.
func hnswSeed(t *testing.T, hidden int) privateSeed {
	t.Helper()
	embedder := &fakeEmbedder{vectors: map[string][]float32{hnswQuery: queryVector()}}
	seed := seedPrivateProjectWith(t, testServerOptions{embedder: embedder})
	hnswIssues(t, seed, seedPublic, hnswVisible, 0.60, 0.50, 0.60, 37)
	if hidden > 0 {
		hnswIssues(t, seed, seedPrivate, hidden, 0.95, 0.85, 0, 1)
	}
	return seed
}

// hnswIssues writes count issues of project, numbered from 1001, each with the graph node of the
// seed's own issue of project copied to its key (a node no write recorded is hidden while a
// project is private, refs.VisibleKind), and gives each the unit vector of cosine nearest down to
// farthest from queryVector() (dimension 0), in steps, with shared on the last dimension and the
// rest on an axis of its own (stride apart, so the visible and the hidden issues do not share axes
// in step, and never dimension 1, which defaultVector() holds), as the fake embedder's identity
// embedded it: what search reads of a row embedqueue embedded.
func hnswIssues(t *testing.T, seed privateSeed, project string, count int, nearest, farthest, shared float64, stride int) {
	t.Helper()
	ctx, pool := t.Context(), seed.database.Pool
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, status)
		select $1 || '-' || (1000 + g), $1, 1000 + g, 'Neighbour ' || g || ' of ' || $1, '{"kind":"user","id":"carol@example.com"}',
			'z' || lpad(g::text, 7, '0'), 'triage'
		from generate_series(1, $2::int) g
	`, project, count); err != nil {
		t.Fatalf("write %d issues of %s: %v", count, project, err)
	}
	noded, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
		select n.kind, i.key, n.supertag, n.visibility, n.state, n.attributes, n.made_at, n.made_by, n.source_event, n.project_key, n.parties
		from issues i join graph_nodes n on n.kind = 'issue' and n.id = $2
		where i.project_key = $1 and i.number > 1000
	`, project, seed.issue[project])
	if err != nil || noded.RowsAffected() != int64(count) {
		t.Fatalf("copy %s's node to %d issues of %s: %d copied, %v", seed.issue[project], count, project, noded.RowsAffected(), err)
	}
	embedded, err := pool.Exec(ctx, `
		update embeddings e
		set embedding = placed.vector, model = $6, embedded_hash = e.content_hash, embedded_at = now(), project_key = $1
		from (
			select i.key, (
				select array_agg(case
					when d = 1 then p.cosine
					when d = $7::int then $8::float8
					when d = p.axis then sqrt(1 - p.cosine * p.cosine - $8::float8 * $8::float8)
					else 0 end order by d)::real[]::vector
				from generate_series(1, $7::int) d
			) as vector
			from issues i
			cross join lateral (
				select $3::float8 - ($3::float8 - $4::float8) * (i.number - 1001) / greatest($2::int - 1, 1) as cosine,
					3 + ((i.number - 1001) * $5::int) % ($7::int - 3) as axis
			) p
			where i.project_key = $1 and i.number > 1000
		) placed
		where e.kind = 'issue' and e.id = placed.key
	`, project, count, nearest, farthest, stride, embed.BedrockModel, embed.Dimension, shared)
	if err != nil {
		t.Fatalf("embed %d issues of %s: %v", count, project, err)
	}
	if embedded.RowsAffected() != int64(count) {
		t.Fatalf("embedded %d of %s's %d issues, want every one: the issue trigger queued fewer embedding rows than issues", embedded.RowsAffected(), project, count)
	}
	if _, err := pool.Exec(ctx, `analyze embeddings`); err != nil {
		t.Fatalf("analyze embeddings: %v", err)
	}
}

// hnswSearch is alice's GET /api/v1/search for hnswQuery, a page of searchMaxLimit, served by the
// seed's server over a pool whose connections prefer indexes (sequential scans and sorts off, no
// JIT) and take settings besides.
func hnswSearch(t *testing.T, seed privateSeed, settings map[string]string) model.SearchResponse {
	t.Helper()
	params := map[string]string{"enable_seqscan": "off", "enable_sort": "off", "jit": "off"}
	maps.Copy(params, settings)
	deps := seed.deps
	deps.Store = &store.Store{Pool: storetest.Traced(t, seed.database.Pool, nil, params)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+strings.ReplaceAll(hnswQuery, " ", "+")+"&limit="+strconv.Itoa(searchMaxLimit), nil)
	request.Header = seed.readerRequest(t, callerAlice).Header
	recorder := httptest.NewRecorder()
	directServer(deps).search(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("alice's search = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	response := decodeBody[model.SearchResponse](t, recorder)
	if response.Degraded != "" {
		t.Fatalf("alice's search answered degraded (%s): the meaning leg did not run", response.Degraded)
	}
	return response
}

// hnswHits is each hit of a search answer as "kind/id", in its order.
func hnswHits(response model.SearchResponse) []string {
	hits := make([]string, len(response.Results))
	for i, result := range response.Results {
		hits[i] = result.Kind + "/" + result.ID
	}
	return hits
}
