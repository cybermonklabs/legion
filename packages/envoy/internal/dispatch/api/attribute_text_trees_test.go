package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Three stored texts a writer types in a box that carries the reference picker, and that a
// route answers as an attribute of something else: an issue's reason for touching no component,
// on the issue read and on the architecture tree's Not architectural row, and a priority's
// description, on the priorities listing. Each carries its display tree beside it when, and only
// when, the request holds `Dispatch-Trees: 1`, the tree is the one pmdoc.ReadForDisplay makes of
// exactly that text, and the emitted document declares it where the answer carries it - so the
// dashboard draws each of them from the server's reading and parses none of them in the page.
func TestAReasonAndADescriptionCarryTheirTreesOnlyUnderTheHeader(t *testing.T) {
	t.Parallel()
	handler, _ := newComponentTreeServer(t)
	const (
		reason      = "Hiring, **not** code: see dispatch://CORE-1."
		description = "Ship the *daemon* first.\n\nThen the rest."
	)
	issue := createComponentIssue(t, handler, "Process work", nil, map[string]any{"mode": "none", "reason": reason})
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": "ship-the-daemon", "title": "Ship the daemon", "description": description,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the priority: status=%d body=%s", created.Code, created.Body.String())
	}

	node, _ := readOpenAPITree(t)
	document, _ := node.(map[string]any)
	for _, test := range []struct{ name, pattern, target, textPath, treePath string }{
		{"an issue's reason for touching no component", "/api/v1/issues/{key}",
			"/api/v1/issues/" + issue.Key, "components.reason", "components_reason_tree"},
		{"a Not architectural row's reason", "/api/v1/projects/{key}/architecture",
			"/api/v1/projects/CORE/architecture", "not_architectural.0.reason", "not_architectural.0.reason_tree"},
		{"a priority's description", "/api/v1/priorities",
			"/api/v1/priorities", "priorities.0.description", "priorities.0.description_tree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireDisplayTree(t, handler, test.target, test.textPath, test.treePath)
			requireTreeInAnswerSchema(t, document, test.pattern, test.treePath)
		})
	}
}

// The architecture tree and the priorities listing each answer a caller that asked for no trees
// the bytes the route answered before their texts gained trees, keys in the order it wrote
// them: the priorities listing the bytes of model.Priorities, and the architecture tree those of
// the shape that already carried its components' prose trees, which writes the components last
// (architectureTreeBefore). A shadow written in another order would move its key in every answer.
func TestTheShapedListingsAnswerTheirModelsBytesWithoutTheHeader(t *testing.T) {
	t.Parallel()
	handler, _ := newComponentTreeServer(t)
	createComponentIssue(t, handler, "Process work", nil, map[string]any{"mode": "none", "reason": "Hiring, **not** code."})
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": "ship-the-daemon", "title": "Ship the daemon", "description": "Ship the *daemon* first.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the priority: status=%d body=%s", created.Code, created.Body.String())
	}
	t.Run("the architecture tree", func(t *testing.T) {
		requireModelBytes[architectureTreeBefore](t, handler, "/api/v1/projects/CORE/architecture")
	})
	t.Run("the priorities listing", func(t *testing.T) {
		requireModelBytes[model.Priorities](t, handler, "/api/v1/priorities")
	})
}

// architectureTreeBefore is the shape GET /api/v1/projects/{key}/architecture answered before a
// Not architectural row carried the tree of its reason: the model, with its components shaped to
// carry their prose's tree, which encoding/json writes after the model's other fields.
type architectureTreeBefore struct {
	model.ArchitectureTree
	Components []architectureComponentRead `json:"components"`
}

// requireModelBytes fails unless the answer target gives a caller that asked for no trees is,
// byte for byte, the encoding of the model M it decodes into: the keys the model writes, in the
// order it writes them, and no other.
func requireModelBytes[M any](t *testing.T, handler http.Handler, target string) {
	t.Helper()
	bare := treesRequest(t, handler, target, false)
	if bare.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, bare.Code, bare.Body.String())
	}
	encoded := httptest.NewRecorder()
	WriteJSON(encoded, http.StatusOK, decodeBody[M](t, bare))
	if !bytes.Equal(bare.Body.Bytes(), encoded.Body.Bytes()) {
		t.Errorf("GET %s without the header answers\n\t%s\nwhere its model encodes as\n\t%s",
			target, bare.Body.String(), encoded.Body.String())
	}
}
