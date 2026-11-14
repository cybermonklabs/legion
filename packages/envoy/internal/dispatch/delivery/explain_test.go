package delivery

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// indexScanOf is the one scan of plan that reads relation through index, failing the test when
// the plan holds none or more than one.
func indexScanOf(t *testing.T, plan json.RawMessage, raw, relation, index string) explaintest.IndexScan {
	t.Helper()
	var found []explaintest.IndexScan
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index == index {
			found = append(found, scan)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the plan reads %s through %s %d times; want once:\n%s", relation, index, len(found), raw)
	}
	return found[0]
}

// scannedRelations is every relation a node of plan reads, once each.
func scannedRelations(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	var relations []string
	for _, scan := range explaintest.Scans(t, plan) {
		if !slices.Contains(relations, scan.Relation) {
			relations = append(relations, scan.Relation)
		}
	}
	return relations
}
