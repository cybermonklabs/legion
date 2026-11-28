package embedqueue

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestCoverageReadsNoWindowsTextAfterAnEmbedderChangeDropsEveryVector holds the recount's window
// statement, the one statement of the counts that reads every window row, to the bitmap of its two
// window indexes on the table an embedder change leaves: once the configured identity changes, the
// coverage pass's identity step (dropForeignVectors) takes every vector the old identity made out
// of its row, so every window row leaves the embedded half for the other one until the queue has
// embedded it again, which takes hours at production's size. Recounts run all that time, each
// under its bound (coverageRecountTimeout) and starting at most 11 minutes less that bound after
// the one before started.
//
// The test builds that table from the scale test's corpus at 0.005 x production's size
// (loadSmallCoverageCorpus): the identity step of another identity run over every window kind,
// which must drop a vector, then embeddings analyzed. The statement's plan there, under
// planCoverage's settings as the recount runs it (explainPlanned), must read embeddings by no Seq
// Scan, test no window's text in a filter of embeddings, and read the windows by a Bitmap Heap Scan
// over a BitmapOr of embeddings_window_embedded_since and embeddings_window_unembedded_since.
//
// An earlier version of main's coverage statement, which had the bitmap from the planner's
// underestimate of the embedded half, read the window arm of this table by one Bitmap Index Scan of
// embeddings_kind_project, by kind, with the blank test's regex and both halves' predicates as the
// heap scan's filter, over every window's text. INFRA-4241's tester measured it in a builds Job
// (8 CPUs, 16 GiB) on the scale test's corpus
// (dispatch://INFRA-4241/artifact/tester-r3-job-0ec6b4ad-log): at production's size that heap scan
// took 3,496 ms of the statement's 3,888 ms, and Coverage took 1,027 ms before the vectors were
// dropped, then 4,590 ms with the statistics stale and 3,728 ms analyzed; at twice that size
// 2,153 ms before, then 8,243 ms and 8,455 ms, so it reaches the route's 10 s bound at 2.3 times
// production's size, below the 4 times R2 of INFRA-4241 asks for. Five Coverage calls at once at
// twice the size each took 9.9 s.
func TestCoverageReadsNoWindowsTextAfterAnEmbedderChangeDropsEveryVector(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	loadSmallCoverageCorpus(t, database)

	// The embedder changes: the identity step drops every vector the old identity made.
	const newIdentity = "azure:example.services.ai.azure.com/embed-v-5-0"
	var dropped int64
	for _, kind := range windowCoverageKinds {
		step := coverStep{&dropped, dropForeignVectors, []any{kind, newIdentity, coverageStatementRows}}
		if err := step.run(ctx, Deps{Store: database}, kind); err != nil {
			t.Fatalf("drop the old identity's vectors of %s: %v", kind, err)
		}
	}
	if dropped == 0 {
		t.Fatalf("the identity step dropped no vector; the corpus holds none of another identity than %s", newIdentity)
	}
	mustExec(t, database.Pool, `analyze embeddings`)

	plan := explainPlanned(t, database)
	if explaintest.SeqScansRelation(t, plan, "embeddings") {
		t.Errorf("after the identity step dropped %d vectors, the recount's window statement reads embeddings by a Seq Scan:\n%s", dropped, plan)
	}
	var textFilters []string
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation == "embeddings" && strings.Contains(node.Filter, "text_snapshot") {
			textFilters = append(textFilters, node.Type+" (Filter: "+node.Filter+")")
		}
	}
	var bothIndexes bool
	for _, scan := range explaintest.BitmapHeapScans(t, plan, "embeddings") {
		if scan.OrOf(windowEmbeddedIndex, windowUnembeddedIndex) {
			bothIndexes = true
		}
	}
	if len(textFilters) > 0 {
		t.Errorf("after the identity step dropped %d vectors, the recount's window statement tests windows' text in a filter: %s\n%s", dropped, strings.Join(textFilters, "; "), plan)
	}
	if !bothIndexes {
		t.Errorf("after the identity step dropped %d vectors, the recount's window statement reads the windows through no BitmapOr of %s and %s:\n%s",
			dropped, windowEmbeddedIndex, windowUnembeddedIndex, plan)
	}
}
