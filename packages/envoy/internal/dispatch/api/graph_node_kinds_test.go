package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestTheGraphNodeKindEnumIsTheRegistrysKinds: the OpenAPI document closes `GraphNode.kind` over
// every node kind a read of the graph answers, the kinds the read registry holds a loader for
// (refs.ResolvableKinds), so the TypeScript type `NODE_LABELS` is keyed by names each of them, and
// a child that resolves a kind later adds its member or fails here. A kind the registry holds a
// predicate for and no loader - a supertag or an attribute of the block schema - is the kind of
// no node an answer carries, so a member naming one fails here too. `state` and `visibility` are
// closed over the values the store takes. Both the document the emitter writes now and the one
// committed beside the clients are read: the first is what the model's tags say, the second what
// every generated client was written from. The shapes that embed a node (GraphNodeRead, the node
// of a references read) carry the same. The negative control is `supertag` written back into the
// tag, which fails naming the kind.
//
// `kind` is required, as every node answers it. `state` and `visibility` are not: this release
// adds them to a shape a server of the release before already answers, and that server answers
// every read of a node without them during a roll, so a reader must take a node that carries
// neither. This build sends both on every node all the same, which the marshaller shows on a node
// nothing filled: it writes every field that carries no `,omitempty`, so no answer of either shape
// leaves them out.
func TestTheGraphNodeKindEnumIsTheRegistrysKinds(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"kind":       refs.ResolvableKinds(),
		"state":      graphstore.NodeStates(),
		"visibility": {graphstore.VisibilityProject, graphstore.VisibilityAuthor},
	}
	added := []string{"state", "visibility"}
	rendered, _ := readOpenAPI(t)
	for name, document := range map[string][]byte{"the emitted document": rendered, "the committed document": committedOpenAPI(t)} {
		var read struct {
			Components struct {
				Schemas map[string]struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(document, &read); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		for _, schema := range []string{"GraphNode", "GraphNodeRead"} {
			declared, held := read.Components.Schemas[schema]
			if !held {
				t.Fatalf("%s declares no schema %s", name, schema)
			}
			for field, values := range want {
				got := slices.Sorted(slices.Values(declared.Properties[field].Enum))
				if !slices.Equal(got, slices.Sorted(slices.Values(values))) {
					t.Errorf("%s declares %s.%s as one of %v; the registry and the store hold %v", name, schema, field, got, values)
				}
				switch required := slices.Contains(declared.Required, field); {
				case slices.Contains(added, field) && required:
					t.Errorf("%s requires %s.%s, which a server of the release before does not send", name, schema, field)
				case !slices.Contains(added, field) && !required:
					t.Errorf("%s does not require %s.%s, which every node answers", name, schema, field)
				}
			}
		}
	}
	for schema, node := range map[string]any{"GraphNode": model.GraphNode{}, "GraphNodeRead": graphNodeRead{}} {
		encoded, err := json.Marshal(node)
		if err != nil {
			t.Fatalf("encode an empty %s: %v", schema, err)
		}
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &sent); err != nil {
			t.Fatalf("decode an empty %s: %v", schema, err)
		}
		for _, field := range added {
			if _, written := sent[field]; !written {
				t.Errorf("this build writes %s with no %s when nothing fills it: %s", schema, field, encoded)
			}
		}
	}
}
