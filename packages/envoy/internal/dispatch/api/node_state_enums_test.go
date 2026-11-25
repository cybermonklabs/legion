package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestEveryNodeStateAndVisibilityEnumIsTheStoresValues holds every schema of the OpenAPI document,
// wherever it sits - a component, a property of a nested object, an item of a list - to the values
// the store takes for a node's state and visibility: each such enum is written by hand on its Go
// field's tag, and a value the store gains would otherwise reach one answer's enum and not
// another's. A `visibility` enum is the two visibilities, graphstore.VisibilityProject and
// graphstore.VisibilityAuthor. A `state` enum that names only node states (graphstore.NodeStates)
// is every one of them on a schema the API answers; a request body (a schema named `...Input`)
// may take fewer, the states its route writes, and none the store does not hold. A `state` enum
// of something else - an ask's, a check's, a pinned block's - names no node state and is passed
// over. Both the document the emitter writes now and the one committed beside the clients are
// read.
func TestEveryNodeStateAndVisibilityEnumIsTheStoresValues(t *testing.T) {
	t.Parallel()
	states := slices.Sorted(slices.Values(graphstore.NodeStates()))
	visibilities := slices.Sorted(slices.Values([]string{graphstore.VisibilityProject, graphstore.VisibilityAuthor}))
	rendered, _ := readOpenAPI(t)
	for name, document := range map[string][]byte{"the emitted document": rendered, "the committed document": committedOpenAPI(t)} {
		var tree any
		if err := json.Unmarshal(document, &tree); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		held := 0
		walkOpenAPI(tree, "", func(path string, object map[string]any) {
			properties, _ := object["properties"].(map[string]any)
			for _, field := range []string{"state", "visibility"} {
				property, _ := properties[field].(map[string]any)
				listed, _ := property["enum"].([]any)
				if len(listed) == 0 {
					continue
				}
				values := make([]string, 0, len(listed))
				for _, value := range listed {
					text, _ := value.(string)
					values = append(values, text)
				}
				slices.Sort(values)
				at := path + "/properties/" + field
				if field == "visibility" {
					held++
					if !slices.Equal(values, visibilities) {
						t.Errorf("%s declares %s as one of %v; a node's visibility is one of %v", name, at, values, visibilities)
					}
					continue
				}
				if slices.ContainsFunc(values, func(value string) bool { return !slices.Contains(states, value) }) {
					continue
				}
				held++
				if strings.HasSuffix(path, "Input") {
					t.Logf("%s takes %s as one of %v, the node states its route writes", name, at, values)
					continue
				}
				if !slices.Equal(values, states) {
					t.Errorf("%s answers %s as one of %v; a node's state is one of %v", name, at, values, states)
				}
			}
		})
		if held == 0 {
			t.Fatalf("%s declares no node state or visibility enum anywhere: the walk read nothing worth checking", name)
		}
		t.Logf("%s holds %d node state and visibility enums to the store's values", name, held)
	}
}

// TestTheStructuralEdgeKindsNameEveryKindTheStoreAloneStates holds model.GraphEdgeKinds, the
// structural edge kinds a reference read takes in `?kind=` and a typed block's `block[]`
// attribute may never be named after, to the kinds graph_edges answers from the node and tuple
// store alone, with no projection row behind them: each pointer the store's type rules hold
// (graphstore.PointerKinds) and each placement (graphstore.PlacementKinds), the changed_by_run of
// a run of the command bar, and the authored_by of a reasoning node, is one refs.TupleKinds lists,
// the list the view's tuple arm renders, and one GraphEdgeKinds names, so a kind the store comes
// to state with no projection row reaches both lists or fails here. GraphEdgeKinds cannot take
// these kinds from graphstore, which imports model; this test is what holds it to them, and a
// pointer added to graphstore's pointerRules alone fails it.
func TestTheStructuralEdgeKindsNameEveryKindTheStoreAloneStates(t *testing.T) {
	t.Parallel()
	rendered := refs.TupleKinds()
	for _, kind := range slices.Concat(graphstore.PointerKinds(), graphstore.PlacementKinds(),
		[]string{graphstore.TupleChangedByRun, graphstore.TupleAuthoredBy}) {
		if !slices.Contains(rendered, kind) {
			t.Errorf("refs.TupleKinds %v leaves out %s, which graph_edges answers from the store alone", rendered, kind)
		}
		if !slices.Contains(model.GraphEdgeKinds, kind) {
			t.Errorf("model.GraphEdgeKinds %v leaves out %s, a kind graph_edges answers", model.GraphEdgeKinds, kind)
		}
	}
}
