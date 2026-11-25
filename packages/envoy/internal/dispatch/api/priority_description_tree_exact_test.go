package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A priority's description is drawn from the tree the server reads it into, and a writer's
// editor that was filled from that tree wrote the tree's reading back over the text: a reading
// that fell back draws a block Parse refuses, or the whole description, as text no write stores,
// so one keystroke wrote `\# Why` for `# Why`. Each read that carries `description_tree` carries
// `description_tree_exact` beside it, which says whether Parse takes the description whole, so
// the page fills a writer's editor from the markdown only where it does. Parse takes a heading,
// a list and stars whole; it refuses a description holding one block of HTML, which the reading
// draws as inline HTML, and emphasis nested 4,000 deep, which the reading holds as its own
// characters. A read without `Dispatch-Trees: 1` carries neither field.
func TestAPriorityDescriptionTreeSaysWhetherItIsExact(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	cases := []struct {
		id, description string
		exact           bool
	}{
		{"exact-heading", "# Why\n\nEvery machine runs the daemon.", true},
		{"exact-list", "- first host\n- second host", true},
		{"exact-stars", "Ship **every** host, *then* check.", true},
		{"refused-block", "## Plan\n\n<div>held as HTML</div>\n\n- first host\n- second host", false},
		{"nested-deep", strings.Repeat("*a ", 4_000) + "b" + strings.Repeat(" c*", 4_000), false},
	}
	for _, each := range cases {
		created := decodeBody[model.Priority](t, priorityTreesRequest(t, handler, http.MethodPost, "/api/v1/priorities",
			map[string]any{"id": each.id, "title": each.id, "description": each.description}, http.StatusCreated))
		requireExactness(t, "the create's answer of "+each.id, created, each.exact)
	}

	listing := decodeBody[model.Priorities](t, treesRequest(t, handler, "/api/v1/priorities", true))
	for _, each := range cases {
		requireExactness(t, "the listing's row of "+each.id, priorityRow(t, listing, each.id).Priority, each.exact)
	}

	// A write's answer reads the description it wrote: one Parse refuses, written over one it
	// takes whole, answers false.
	patched := decodeBody[model.Priority](t, priorityTreesRequest(t, handler, http.MethodPatch,
		"/api/v1/priorities/exact-heading", map[string]any{"description": "# Why\n\n<div>now HTML</div>"}, http.StatusOK))
	requireExactness(t, "the write's answer", patched, false)

	// Without the header there is no tree, and no word on one.
	plain := treesRequest(t, handler, "/api/v1/priorities", false)
	if strings.Contains(plain.Body.String(), "description_tree") {
		t.Errorf("the listing without Dispatch-Trees: 1 carries a description_tree field: %.300s", plain.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": "no-trees", "title": "No trees", "description": "# Why"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create without Dispatch-Trees: 1: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "description_tree") {
		t.Errorf("the create's answer without Dispatch-Trees: 1 carries a description_tree field: %s", response.Body.String())
	}
}

// requireExactness fails unless priority, read where where says, carries a description tree and
// says Parse takes its description whole exactly as want says.
func requireExactness(t *testing.T, where string, priority model.Priority, want bool) {
	t.Helper()
	if priority.DescriptionTree == nil {
		t.Fatalf("%s carries no description_tree", where)
	}
	if priority.DescriptionTreeExact == nil {
		t.Fatalf("%s carries a description_tree and no description_tree_exact", where)
	}
	if got := *priority.DescriptionTreeExact; got != want {
		t.Errorf("%s: description_tree_exact=%v, want %v", where, got, want)
	}
}
