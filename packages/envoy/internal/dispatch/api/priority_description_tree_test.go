package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A priority's description is markdown of any length (createPriorityInput.Description), and the
// priorities page shows it to every reader. Each read of it - the listing, and the answers of the
// create and of the write that stored it - carries the tree the reader is shown as
// `description_tree` under `Dispatch-Trees: 1`, as a comment's read carries `body_tree`, so the
// page parses no description. Emphasis nested 4,000 deep is the shape the browser's parser spends
// the square of its depth on (672,095,992 steps, about 55 s: cybermonklabs/legion#4437). The
// server's display read holds marks to maxInlineNesting and reads text nested past it as its own
// characters, so the tree holds every character of the description as text, under no mark. Each
// write stores that tree, and a read without the header carries none.
func TestAPriorityDescriptionNested4000DeepReadsBackAsATreeOfItsOwnText(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	// 24,001 characters: `*a ` opens one emphasis and ` c*` closes it, 4,000 times each.
	nested := func(marker string) string {
		return strings.Repeat(marker+"a ", 4_000) + "b" + strings.Repeat(" c"+marker, 4_000)
	}

	written := nested("*")
	created := priorityTreesRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": "nested-deep", "title": "Nested deep", "description": written}, http.StatusCreated)
	requireTextTree(t, "the create's answer", decodeBody[model.Priority](t, created).DescriptionTree, written)
	if _, held := storedTreeOf(t, database, written); !held {
		t.Error("the create stored no display tree of the description it wrote")
	}

	// The store starts with no priority (newTestHandlerWithStore), so the one created is row 0.
	requireTextTree(t, "the listing",
		model.DisplayTree(treeAt(t, handler, "/api/v1/priorities", "priorities.0.description_tree")), written)

	rewritten := nested("_")
	patched := priorityTreesRequest(t, handler, http.MethodPatch, "/api/v1/priorities/nested-deep",
		map[string]any{"description": rewritten}, http.StatusOK)
	requireTextTree(t, "the write's answer", decodeBody[model.Priority](t, patched).DescriptionTree, rewritten)
	if _, held := storedTreeOf(t, database, rewritten); !held {
		t.Error("the write stored no display tree of the description it wrote")
	}
}

// priorityTreesRequest makes one priority call as alice from the dashboard, which asks for the
// display trees, and fails unless it is answered want.
func priorityTreesRequest(t *testing.T, handler http.Handler, method, target string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	sent, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body of %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(sent))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d, want %d; body=%.500s", method, target, response.Code, want, response.Body.String())
	}
	return response
}

// requireTextTree fails unless tree, the description_tree the answer named by where carried, is
// text read as its own characters - every one of them, under no mark, which is how the display
// read takes marks nested past maxInlineNesting - and is the tree pmdoc.ReadForDisplay makes of
// text, as the tree of every other stored text is. The marks are counted first, so a reading
// that nested them stops the row before anything compares their thousands.
func requireTextTree(t *testing.T, where string, tree model.DisplayTree, text string) {
	t.Helper()
	if tree == nil {
		t.Fatalf("%s carries no description_tree", where)
	}
	var root snippetNode
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("%s: decode the description_tree: %v", where, err)
	}
	var words strings.Builder
	marks := 0
	var walk func(snippetNode)
	walk = func(node snippetNode) {
		words.WriteString(node.Text)
		marks += len(node.Marks)
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	if marks != 0 {
		t.Fatalf("%s: the description_tree carries %d marks, want the emphasis nested past maxInlineNesting read as text",
			where, marks)
	}
	if read := words.String(); read != text {
		t.Fatalf("%s: the description_tree holds %d characters of text, want the description's own %d (%.60q…)",
			where, len(read), len(text), read)
	}
	want := sameShape(t, pmdoc.DisplayJSON(text, pmdoc.DisplayLatest))
	if got := sameShape(t, json.RawMessage(tree)); !bytes.Equal(got, want) {
		t.Fatalf("%s: the description_tree is not ReadForDisplay's tree of the description:\n\t%.300s\nwant\n\t%.300s",
			where, got, want)
	}
}
