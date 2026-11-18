package embedqueue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestCoverageReadsNoWindowsTextAfterAnEmbedderChangeDropsEveryVector holds the report to the
// bitmap of its two window indexes on the table an embedder change leaves: once the configured
// identity changes, the coverage pass's identity step (dropForeignVectors) takes every vector the
// old identity made out of its row, so every window row leaves the embedded half for the other one
// until the queue has embedded it again, which takes hours at production's size. That is the time
// the coverage route and its probe are there to watch.
//
// At 0ec6b4ad the planner then reads the window arm by one Bitmap Index Scan of
// embeddings_kind_project, by kind, with the blank test's regex and both halves' predicates as the
// heap scan's filter, over every window's text. INFRA-4241's tester measured it in a builds Job
// (8 CPUs, 16 GiB) on the scale test's corpus: at production's size that heap scan took 3,496 ms of
// the statement's 3,888 ms, and Coverage took 1,027 ms before the vectors were dropped, then
// 4,590 ms with the statistics stale and 3,728 ms analyzed; at twice that size 2,153 ms before,
// then 8,243 ms and 8,455 ms, so it reaches the route's 10 s bound at 2.3 times production's size,
// below the 4 times R2 of INFRA-4241 asks for. Five Coverage calls at once at twice the size each
// took 9.9 s.
func TestCoverageReadsNoWindowsTextAfterAnEmbedderChangeDropsEveryVector(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	script, err := os.ReadFile(filepath.Join("testdata", "coverage_corpus.sql"))
	if err != nil {
		t.Fatalf("read the corpus script: %v", err)
	}
	loadCoverageCorpus(t, database.Pool.Config().ConnString(), coverageScale{value: 0.005, text: "0.005"}, repositoryProse(t), string(script))
	mustExec(t, database.Pool, `analyze `+coverageScaleTables)

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
		t.Errorf("after the identity step dropped %d vectors, the coverage statement reads embeddings by a Seq Scan:\n%s", dropped, plan)
	}
	var root bitmapPlanNode
	if err := json.Unmarshal(plan, &root); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	var bothIndexes bool
	var textFilters []string
	root.walk(func(node bitmapPlanNode) {
		if node.RelationName != "embeddings" {
			return
		}
		if strings.Contains(node.Filter, "text_snapshot") {
			textFilters = append(textFilters, node.NodeType+" (Filter: "+node.Filter+")")
		}
		if node.NodeType != "Bitmap Heap Scan" {
			return
		}
		for _, child := range node.Plans {
			var indexes []string
			child.walk(func(scan bitmapPlanNode) {
				if scan.NodeType == "Bitmap Index Scan" {
					indexes = append(indexes, scan.IndexName)
				}
			})
			if child.NodeType == "BitmapOr" && slices.Contains(indexes, "embeddings_window_embedded") && slices.Contains(indexes, "embeddings_window_unembedded") {
				bothIndexes = true
			}
		}
	})
	if len(textFilters) > 0 {
		t.Errorf("after the identity step dropped %d vectors, the coverage statement tests windows' text in a filter: %s\n%s", dropped, strings.Join(textFilters, "; "), plan)
	}
	if !bothIndexes {
		t.Errorf("after the identity step dropped %d vectors, the coverage statement reads the windows through no BitmapOr of embeddings_window_embedded and embeddings_window_unembedded:\n%s", dropped, plan)
	}
}
