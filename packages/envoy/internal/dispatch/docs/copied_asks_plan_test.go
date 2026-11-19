package docs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The reads a settlement makes for its copied ask blocks run at every settlement of a copy, which
// never gets an ask row of its own. Each must reach the owner's documents through an index - the
// issue's through artifacts_issue_key, a project's unlinked documents through
// artifacts_project_documents, each document's asks under a block id through
// asks_block_id_unique, each source ask's part_of through an index of graph_tuples keyed on the
// ask's node and the document it names through artifacts_ref_key_key, and read events through
// events_ask_payload_id, never scanning a table that grows with every document, ask, tuple or
// event. So must the restoration of a decided ask's wording, which every settlement of a document
// holding one runs over its decided asks, reaching them through their primary key, and which the
// boot runs over every decided ask (a null $1).
//
// The part_of is held to no index by name: what holds it under either plan is that it reaches
// graph_tuples through an index whose condition keys the ask's node (from_id) and the index's own
// first key column, and scans no part of the table by sequence (requireTuplesKeyedOnTheAsk). The
// read binds no far end's kind, so only graph_tuples_as_of_out keys it; bound, that kind fit
// graph_tuples_as_of_in too, keyed on the far end's kind and the tuple's kind alone, which reads
// every part_of into every document, and once graph_tuples is analyzed the planner took it, so
// every read is explained both ways: as the fixture's writes leave the table, with no statistics,
// and analyzed.
func TestCopiedAskQueriesAvoidSequentialScans(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	keys := explaintest.NewIndexKeys(tx)
	issue, project := "DOC-1", "DOC"
	sourcesOfIssue, issueKey := copiedAskSourcesQuery(artifactOwner{IssueKey: &issue, Project: project})
	sourcesOfProject, projectKey := copiedAskSourcesQuery(artifactOwner{Project: project})
	copiesOnIssue, _ := owedCopiesQuery(artifactOwner{IssueKey: &issue, Project: project})
	copiesOnProject, _ := owedCopiesQuery(artifactOwner{Project: project})
	document := "00000000-0000-4000-8000-000000000000"
	blocks := []string{"decision"}
	patterns := []string{"%ask{#decision %"}
	queries := []struct {
		name     string
		sql      string
		relation string
		// index is the index the read must reach relation through, or explaintest.AnyIndex
		// for a read held to any index keyed on the ask's node (requireTuplesKeyedOnTheAsk).
		index string
		args  []any
	}{
		{"an issue document's copy sources", sourcesOfIssue, "artifacts", "artifacts_issue_key", []any{blocks, document, issueKey}},
		{"a project document's copy sources", sourcesOfProject, "artifacts", "artifacts_project_documents", []any{blocks, document, projectKey}},
		{"the copies of an issue document's retracted ask", copiesOnIssue, "artifacts", "artifacts_issue_key", []any{blocks, document, issueKey, patterns}},
		{"the copies of a project document's retracted ask", copiesOnProject, "artifacts", "artifacts_project_documents", []any{blocks, document, projectKey, patterns}},
		// The lateral read of each candidate's latest version: its unique (artifact_id, number).
		{"the latest versions of an issue's copies", copiesOnIssue, "artifact_versions", "artifact_versions_artifact_id_number_key", []any{blocks, document, issueKey, patterns}},
		{"the latest versions of a project's copies", copiesOnProject, "artifact_versions", "artifact_versions_artifact_id_number_key", []any{blocks, document, projectKey, patterns}},
		// Each candidate document's asks under the block ids.
		{"the asks of an issue's copy sources", sourcesOfIssue, "asks", "asks_block_id_unique", []any{blocks, document, issueKey}},
		{"the asks of a project's copy sources", sourcesOfProject, "asks", "asks_block_id_unique", []any{blocks, document, projectKey}},
		{"the asks of an issue's copies", copiesOnIssue, "asks", "asks_block_id_unique", []any{blocks, document, issueKey, patterns}},
		{"the asks of a project's copies", copiesOnProject, "asks", "asks_block_id_unique", []any{blocks, document, projectKey, patterns}},
		// Each source ask's part_of, through any index keyed on the ask's node, and the document
		// it names.
		{"the part_of of an issue's copy sources", sourcesOfIssue, "graph_tuples", explaintest.AnyIndex, []any{blocks, document, issueKey}},
		{"the part_of of a project's copy sources", sourcesOfProject, "graph_tuples", explaintest.AnyIndex, []any{blocks, document, projectKey}},
		{"the documents an issue's copy sources are part of", sourcesOfIssue, "artifacts", "artifacts_ref_key_key", []any{blocks, document, issueKey}},
		{"the documents a project's copy sources are part of", sourcesOfProject, "artifacts", "artifacts_ref_key_key", []any{blocks, document, projectKey}},
		{"what a copied ask asked and decided", copiedAskHistoryQuery, "events", "events_ask_payload_id", []any{[]string{"ask-id"}}},
		// The restoration guards each ask on the answer its plan read, which it is given beside
		// the ids (decidedWordingRestore).
		{"a settlement's decided asks", decidedWordingRestore, "asks", "asks_pkey", []any{[]string{document}, []*string{nil}}},
		{"a settlement's decided asks' events", decidedWordingRestore, "events", "events_ask_payload_id", []any{[]string{document}, []*string{nil}}},
		{"the boot's decided asks' events", decidedWordingRestore, "events", "events_ask_payload_id", []any{[]string(nil), []*string(nil)}},
	}
	// First as the fixture's writes leave graph_tuples, then once it is analyzed, on this test's
	// own database.
	for _, statistics := range []string{"without statistics", "with statistics"} {
		if statistics == "with statistics" {
			if _, err := tx.Exec(ctx, "analyze graph_tuples"); err != nil {
				t.Fatalf("analyze graph_tuples: %v", err)
			}
		}
		t.Run(statistics, func(t *testing.T) {
			for _, query := range queries {
				t.Run(query.name, func(t *testing.T) {
					var planJSON []byte
					if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
						t.Fatalf("explain query: %v", err)
					}
					var plans []struct {
						Plan json.RawMessage `json:"Plan"`
					}
					if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("decode explain output (%v): %s", err, planJSON)
					}
					if query.index == explaintest.AnyIndex {
						requireTuplesKeyedOnTheAsk(t, keys, plans[0].Plan, query.relation, planJSON)
						return
					}
					if explaintest.SeqScansRelation(t, plans[0].Plan, query.relation) {
						t.Fatalf("query plan sequentially scans %s; want %s:\n%s", query.relation, query.index, planJSON)
					}
					if !planUsesIndex(t, plans[0].Plan, query.index) {
						t.Fatalf("query plan does not read %s:\n%s", query.index, planJSON)
					}
				})
			}
		})
	}
}

// requireTuplesKeyedOnTheAsk holds plan to reading relation, graph_tuples, through an index keyed
// on the ask's node, whichever index that is: no sequential scan of it, at least one scan of one
// of its indexes, and every such scan under a condition that names the ask's node (from_id) and
// the first key column of the index it reads, the one column an index scan narrows on
// (explaintest.IndexKeys). planJSON is the whole answer, which a failure prints.
func requireTuplesKeyedOnTheAsk(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage, relation string, planJSON []byte) {
	t.Helper()
	if explaintest.SeqScansRelation(t, plan, relation) {
		t.Fatalf("query plan sequentially scans %s; want it read through an index keyed on the ask's node:\n%s", relation, planJSON)
	}
	keyed := false
	for _, scan := range explaintest.IndexScans(t, plan) {
		if !strings.HasPrefix(scan.Index, relation+"_") {
			continue
		}
		keyed = true
		if !strings.Contains(scan.Cond, "from_id") {
			t.Errorf("query plan reads %s on `%s`, which does not name the ask's node:\n%s", scan, scan.Cond, planJSON)
		}
		if firstKey := keys.FirstKey(t, scan.Index); !explaintest.CondNamesFirstKey(scan.Cond, firstKey) {
			t.Errorf("query plan reads %s on `%s`, which names nothing of that index's first key column `%s`:\n%s",
				scan, scan.Cond, firstKey, planJSON)
		}
	}
	if !keyed {
		t.Fatalf("query plan reads %s through no index of it:\n%s", relation, planJSON)
	}
}

// planUsesIndex reports whether a plan node, at any depth, reads index.
func planUsesIndex(t *testing.T, planJSON json.RawMessage, index string) bool {
	t.Helper()
	var node struct {
		IndexName string            `json:"Index Name"`
		Plans     []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.IndexName == index {
		return true
	}
	for _, child := range node.Plans {
		if planUsesIndex(t, child, index) {
			return true
		}
	}
	return false
}
